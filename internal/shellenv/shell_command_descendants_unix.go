//go:build unix

package shellenv

import (
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// procEntry is one row of a platform process-table snapshot.
//
// startedAt is a platform-specific process identity, never a wall clock: it is
// only ever compared against another reading of the same field on the same host,
// which is exactly what is needed to tell "still the process I recorded" from
// "that pid was recycled".
type procEntry struct {
	ppid      int
	pgid      int
	sid       int // zero when the platform snapshot does not carry a session id
	startedAt uint64
	zombie    bool
}

// trackedDescendant is one descendant discovered beneath a shell command.
type trackedDescendant struct {
	pid       int
	pgid      int
	startedAt uint64
	// escaped marks a descendant outside the leader's process group, which is
	// the set TerminateShellCommandGroup structurally cannot reach.
	escaped bool
	// adopted marks a descendant this process inherited as a subreaper, which
	// means we owe it a wait() once it dies or it lingers as a zombie.
	adopted bool
	zombie  bool
}

// descendantDiscovery is the platform half of descendant tracking. stop ends
// discovery, performs any final platform-specific collection, and blocks until
// discovery has finished.
type descendantDiscovery interface {
	stop()
}

// ShellCommandDescendants tracks the descendants of a command configured with
// ConfigureShellCommand that escape its process group, so teardown can reap
// them.
//
// Why this exists: Setpgid plus kill(-pgid) reaps a leader's whole subtree only
// while the subtree stays in that group. A child that calls setsid becomes the
// leader of a brand-new session and process group, so kill(-leaderPGID) never
// reaches it or anything it goes on to spawn. That is not a hypothetical - agent
// CLIs spawn every tool-call shell detached, so work an agent backgrounds is
// always outside the group.
//
// No post-hoc sweep can find those processes: the moment the leader exits they
// reparent away, which erases the ancestry that would identify them. They have
// to be discovered while the leader is still alive. Discovery is driven by the
// kernel, never by a timer - see the platform files for how each does it.
//
// Discovery narrows the window but does not close it; see Terminate for how a
// miss is made visible instead of silent.
//
// The zero value is not usable; call PrepareShellCommandDescendants. A nil
// *ShellCommandDescendants is a valid no-op receiver throughout.
type ShellCommandDescendants struct {
	cmd        *exec.Cmd
	grace      time.Duration
	leaderPID  int
	leaderPGID int
	sentinel   *descendantSentinel
	discovery  descendantDiscovery

	mu      sync.Mutex
	tracked map[int]trackedDescendant

	terminateOnce sync.Once
}

// PrepareShellCommandDescendants sets up descendant tracking for cmd. It must be
// called BEFORE StartShellCommand, because it installs the inherited sentinel
// descriptor that later answers "did the sweep actually get everything?".
//
// Pair it with Watch immediately after a successful start, and Terminate at
// teardown. Returns nil for a nil command, and every method tolerates a nil
// receiver, so callers need no branching.
func PrepareShellCommandDescendants(cmd *exec.Cmd, processTerminationGrace time.Duration) *ShellCommandDescendants {
	if cmd == nil {
		return nil
	}
	if processTerminationGrace <= 0 {
		processTerminationGrace = DefaultProcessTerminationGrace
	}
	return &ShellCommandDescendants{
		cmd:      cmd,
		grace:    processTerminationGrace,
		sentinel: newDescendantSentinel(cmd),
		tracked:  make(map[int]trackedDescendant),
	}
}

// Watch begins kernel-driven discovery. Call it immediately after a successful
// StartShellCommand; before that there is no pid to watch. It also drops this
// process's copy of the sentinel write end, so the only remaining holders are
// the command and whatever it spawns.
func (d *ShellCommandDescendants) Watch() {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return
	}
	d.sentinel.releaseParentEnd()
	d.leaderPID = d.cmd.Process.Pid
	// Capture the leader's group now, while it is certain to be alive. Every
	// later classification compares against it, including snapshots taken after
	// the leader has exited and dropped out of the process table.
	if pgid, err := syscall.Getpgid(d.leaderPID); err == nil {
		d.leaderPGID = pgid
	}
	d.discovery = startDescendantDiscovery(d)
}

// Terminate stops discovery and reaps every recorded escapee that is still the
// process it was when recorded: SIGTERM first, then SIGKILL for anything still
// alive after the grace period.
//
// Afterwards it consults the sentinel. The sentinel cannot name pids, so it
// never decides what to kill; its only job is to answer whether anything still
// holds the inherited descriptor once the sweep is done. If something does, that
// is a descendant discovery missed, and it is logged loudly - the difference
// between a leak we know about and a silent one.
//
// Safe to call more than once, and on a nil receiver.
func (d *ShellCommandDescendants) Terminate() {
	if d == nil {
		return
	}
	d.terminateOnce.Do(d.terminate)
}

func (d *ShellCommandDescendants) terminate() {
	if d.discovery != nil {
		d.discovery.stop()
	}
	// Drop our own copy of the write end before consulting the sentinel. Watch
	// normally does this, but a command that failed to start never got there, and
	// while we hold it we are ourselves a holder.
	d.sentinel.releaseParentEnd()
	d.reapEscapees()
	if d.leaderPID != 0 {
		d.reportSurvivors()
	}
	d.sentinel.close()
}

// recordDescendants walks the descendant closure of the leader plus every
// descendant already known, classifies each one, and returns the newly walked
// closure so platform discovery can subscribe to each member's own events.
//
// Every known descendant is re-classified on each call, not just the new ones.
// That is load-bearing: setsid raises no process event of its own, so a tool
// shell that forks and only then leaves the process group looked perfectly
// in-group at the moment we first saw it. Re-checking is how it is noticed at
// all, and the final snapshot at teardown is the backstop for one that never
// forks again.
//
// Seeding from known descendants as well as the leader matters for the same
// reason in reverse: once an intermediate process dies its children leave the
// leader's subtree, but they stay reachable from the descendant already
// recorded. Seeds are re-verified by start time first, so a recycled pid can
// never drag unrelated processes into the closure.
func (d *ShellCommandDescendants) recordDescendants(table map[int]procEntry) []int {
	if len(table) == 0 {
		return nil
	}
	ownPGID, err := syscall.Getpgid(0)
	if err != nil {
		return nil
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	if d.leaderPGID == 0 {
		// Never observed the leader's group, so nothing can be classified against
		// it. Staying silent beats guessing and signalling the wrong group.
		return nil
	}

	seeds := make([]int, 0, len(d.tracked)+1)
	known := make([]int, 0, len(d.tracked))
	if _, ok := table[d.leaderPID]; ok {
		seeds = append(seeds, d.leaderPID)
	}
	for pid, tracked := range d.tracked {
		if entry, ok := table[pid]; ok && entry.startedAt == tracked.startedAt {
			seeds = append(seeds, pid)
			known = append(known, pid)
		}
	}
	if len(seeds) == 0 {
		return nil
	}

	closure := descendantClosure(table, seeds)
	for _, pid := range append(known, closure...) {
		entry := table[pid]
		d.tracked[pid] = trackedDescendant{
			pid:       pid,
			pgid:      entry.pgid,
			startedAt: entry.startedAt,
			escaped:   signallableEscapee(pid, entry.pgid, d.leaderPGID, ownPGID),
			zombie:    entry.zombie,
		}
	}
	return closure
}

// descendantClosure returns every process reachable by parent links from seeds,
// excluding the seeds themselves, in ascending pid order.
func descendantClosure(table map[int]procEntry, seeds []int) []int {
	children := make(map[int][]int, len(table))
	for pid, entry := range table {
		if pid == entry.ppid {
			continue // init and any self-parented row: following it would not terminate
		}
		children[entry.ppid] = append(children[entry.ppid], pid)
	}

	seen := make(map[int]bool, len(seeds))
	for _, seed := range seeds {
		seen[seed] = true
	}
	queue := append([]int(nil), seeds...)
	var closure []int
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, child := range children[pid] {
			if seen[child] {
				continue
			}
			seen[child] = true
			closure = append(closure, child)
			queue = append(queue, child)
		}
	}
	sort.Ints(closure)
	return closure
}

// signallableEscapee reports whether a descendant both escaped the leader's
// process group and is safe to signal. The guards are load-bearing: signalling
// our own process group would take the daemon down along with the leak, and
// pid/pgid 0 and 1 are the calling group and init. A leaderPGID of zero means
// "no leader group to compare against", used by the adopted-orphan path where
// the leader is already gone.
func signallableEscapee(pid, pgid, leaderPGID, ownPGID int) bool {
	if leaderPGID != 0 && pgid == leaderPGID {
		return false // the group kill already covers it
	}
	if pid <= 1 || pgid <= 1 {
		return false
	}
	if pgid == ownPGID || pid == os.Getpid() {
		return false
	}
	return true
}

// recordAdoptedOrphans records descendants this process inherited as a
// subreaper. It is the Linux collection step, kept in this shared file rather
// than the Linux one so it stays unit-testable on a macOS development machine.
//
// Attribution is safe for a specific, load-bearing reason. The subreaper
// attribute is inherited, so a live agent leader is itself the nearest subreaper
// ancestor of its own subtree: orphans of a concurrently running step reparent
// to THAT step's leader, not to this process. An orphan only reaches us when its
// own leader dies. Since this runs immediately after our leader was reaped, the
// adopted strangers visible now are ours. This matters because the daemon runs
// pipelines concurrently with no cap (internal/daemon/manager.go starts a
// goroutine per run and serialises only per repo+branch), so getting attribution
// wrong would mean one run's teardown killing another run's work.
//
// The session test is what separates an adopted orphan from a process we
// deliberately spawned: os/exec never calls setsid and ConfigureShellCommand
// only sets a process group, so our own children share our session. A child of
// ours in a different session was adopted, not spawned. A descendant that
// escaped by setpgid alone is therefore not collected on this path; that is a
// known narrowing, and the sentinel is what makes such a miss visible.
func (d *ShellCommandDescendants) recordAdoptedOrphans(table map[int]procEntry) {
	if len(table) == 0 {
		return
	}
	self := os.Getpid()
	selfEntry, ok := table[self]
	if !ok || selfEntry.sid <= 0 {
		return
	}
	ownPGID, err := syscall.Getpgid(0)
	if err != nil {
		return
	}

	var roots []int
	for pid, entry := range table {
		if !adoptedOrphan(pid, entry, self, selfEntry.sid) {
			continue
		}
		if !signallableEscapee(pid, entry.pgid, 0, ownPGID) {
			continue
		}
		roots = append(roots, pid)
	}
	if len(roots) == 0 {
		return
	}
	sort.Ints(roots)

	d.mu.Lock()
	defer d.mu.Unlock()
	for _, pid := range append(roots, descendantClosure(table, roots)...) {
		entry := table[pid]
		if !signallableEscapee(pid, entry.pgid, 0, ownPGID) {
			continue
		}
		d.tracked[pid] = trackedDescendant{
			pid:       pid,
			pgid:      entry.pgid,
			startedAt: entry.startedAt,
			escaped:   true,
			adopted:   true,
			zombie:    entry.zombie,
		}
	}
}

func adoptedOrphan(pid int, entry procEntry, self, selfSID int) bool {
	if pid == self || entry.ppid != self {
		return false
	}
	return entry.sid > 0 && entry.sid != selfSID
}

func (d *ShellCommandDescendants) reapEscapees() {
	d.mu.Lock()
	recorded := make([]trackedDescendant, 0, len(d.tracked))
	for _, descendant := range d.tracked {
		if descendant.escaped {
			recorded = append(recorded, descendant)
		}
	}
	d.mu.Unlock()
	if len(recorded) == 0 {
		return
	}

	victims := survivingEscapees(recorded, sampleProcessTable())
	if len(victims) == 0 {
		return
	}
	for _, victim := range victims {
		if victim.zombie {
			continue // already dead, only waiting to be collected below
		}
		signalEscapee(victim, syscall.SIGTERM)
	}

	deadline := time.Now().Add(d.grace)
	for {
		alive := aliveEscapees(victims)
		if len(alive) == 0 {
			break
		}
		if !time.Now().Before(deadline) {
			for _, victim := range alive {
				signalEscapee(victim, syscall.SIGKILL)
			}
			break
		}
		time.Sleep(processGroupTerminationPollInterval)
	}

	// Anything adopted as a subreaper is our child in the kernel's eyes, so it
	// stays a zombie until collected. Only adopted pids are waited on: waiting on
	// a pid os/exec owns would steal the exit status its Wait is blocked for.
	for _, victim := range victims {
		if victim.adopted {
			reapAdoptedDescendant(victim.pid)
		}
	}
}

// survivingEscapees filters recorded escapees down to those still present in a
// fresh snapshot under the same identity. An empty or failed snapshot yields no
// victims: without confirmation a recorded pid may since have been recycled by
// an unrelated process, and killing that is far worse than leaving a leak.
func survivingEscapees(recorded []trackedDescendant, table map[int]procEntry) []trackedDescendant {
	if len(table) == 0 {
		return nil
	}
	victims := make([]trackedDescendant, 0, len(recorded))
	for _, descendant := range recorded {
		entry, ok := table[descendant.pid]
		if !ok || entry.startedAt != descendant.startedAt {
			continue
		}
		descendant.pgid = entry.pgid
		descendant.zombie = entry.zombie
		victims = append(victims, descendant)
	}
	// Descending pid is a poor proxy for tree depth, but a stable order keeps
	// signalling deterministic and failures reproducible.
	sort.Slice(victims, func(i, j int) bool { return victims[i].pid > victims[j].pid })
	return victims
}

func aliveEscapees(victims []trackedDescendant) []trackedDescendant {
	alive := victims[:0:0]
	for _, victim := range victims {
		if victim.zombie {
			continue
		}
		if syscall.Kill(victim.pid, 0) != syscall.ESRCH {
			alive = append(alive, victim)
		}
	}
	return alive
}

// signalEscapee prefers a group kill when the escapee leads its own group: the
// negative pid reaches members spawned since discovery, and it is safe precisely
// because the group leader was just confirmed to be the process we recorded.
func signalEscapee(escapee trackedDescendant, signal syscall.Signal) {
	if escapee.pid == escapee.pgid {
		if err := syscall.Kill(-escapee.pgid, signal); err == nil {
			return
		}
	}
	_ = syscall.Kill(escapee.pid, signal)
}

// noteLostDiscovery records that the kernel or the platform told us we lost
// track of something. A dropped subscription is a known unknown, and swallowing
// it is how a leak becomes invisible.
func (d *ShellCommandDescendants) noteLostDiscovery(reason string, pid int, err error) {
	slog.Warn("descendant tracking incomplete",
		"reason", reason, "leader_pid", d.leaderPID, "pid", pid, "error", err)
}

// descendantSentinelSettle bounds how long a survivor report waits for
// descendants that are already dying to actually go. The process-group kill
// escalates to SIGKILL at the end of its own grace window, so a member can still
// be on its way out when the sweep finishes; reporting it as a leak would be a
// false alarm. Nothing waits in the clean case - the pipe is already at EOF.
const descendantSentinelSettle = time.Second

// reportSurvivors is the backstop. The sweep kills what discovery found; the
// sentinel independently reports whether anything is left.
func (d *ShellCommandDescendants) reportSurvivors() {
	if !d.sentinel.holdersRemainAfter(descendantSentinelSettle) {
		return
	}
	d.mu.Lock()
	tracked := len(d.tracked)
	d.mu.Unlock()
	slog.Warn("processes survived step teardown: descendant sweep missed at least one",
		"leader_pid", d.leaderPID, "tracked_descendants", tracked,
		"detail", "a descendant still holds the inherited sentinel descriptor after the sweep, "+
			"so it escaped the process group without being discovered and is still running")
}

// descendantSentinel is an inherited pipe used purely as a detector. Every
// descendant inherits the write end across fork and exec, so the read end
// reaches EOF only once the last holder is gone.
//
// It deliberately never drives a kill: a pipe cannot name pids. Its answer is
// also one-directional. "Holders remain" is proof that something survived. "No
// holders" is not proof of a clean sweep, because a descendant is free to close
// the descriptor. The useful direction is the one that catches leaks.
type descendantSentinel struct {
	read            *os.File
	write           *os.File
	parentCloseOnce sync.Once
	closeOnce       sync.Once
}

func newDescendantSentinel(cmd *exec.Cmd) *descendantSentinel {
	read, write, err := os.Pipe()
	if err != nil {
		return nil
	}
	cmd.ExtraFiles = append(cmd.ExtraFiles, write)
	return &descendantSentinel{read: read, write: write}
}

// releaseParentEnd drops this process's copy of the write end. Until it is
// closed we are ourselves a holder, and the sentinel could never report EOF.
func (s *descendantSentinel) releaseParentEnd() {
	if s == nil {
		return
	}
	s.parentCloseOnce.Do(func() { _ = s.write.Close() })
}

func (s *descendantSentinel) close() {
	if s == nil {
		return
	}
	s.releaseParentEnd()
	s.closeOnce.Do(func() { _ = s.read.Close() })
}

// holdersRemainAfter reports holders only if they are still there once anything
// mid-exit has had a bounded chance to finish dying. It returns immediately when
// the pipe is already at EOF, so a clean teardown costs nothing.
func (s *descendantSentinel) holdersRemainAfter(settle time.Duration) bool {
	if s == nil {
		return false
	}
	deadline := time.Now().Add(settle)
	for {
		if !s.holdersRemain() {
			return false
		}
		if !time.Now().Before(deadline) {
			return true
		}
		time.Sleep(processGroupTerminationPollInterval)
	}
}

// holdersRemain reports whether any process still holds the write end. Nothing
// is ever written to the pipe, so readability can only mean end-of-file.
func (s *descendantSentinel) holdersRemain() bool {
	if s == nil {
		return false
	}
	conn, err := s.read.SyscallConn()
	if err != nil {
		return false
	}
	remain := false
	controlErr := conn.Control(func(fd uintptr) {
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
		n, pollErr := unix.Poll(fds, 0)
		if pollErr != nil {
			return
		}
		// n == 0 means neither readable nor hung up: a writer is still open.
		remain = n == 0
	})
	if controlErr != nil {
		return false
	}
	return remain
}
