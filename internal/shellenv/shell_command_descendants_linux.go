//go:build linux

package shellenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

var (
	childSubreaperOnce sync.Once
	childSubreaperErr  error
)

// descendantDiscoveryTracksLiveLeader reports whether discovery learns
// descendants while the leader is still running. Linux does not: there is no
// unprivileged fork notification, so everything is enumerated once at teardown.
const descendantDiscoveryTracksLiveLeader = false

// startDescendantDiscovery makes this process a child subreaper and returns the
// teardown-time collector.
//
// Linux needs no fork notification at all, which is the whole point of doing it
// this way. PR_SET_CHILD_SUBREAPER redirects orphan reparenting from init to the
// nearest living subreaper ancestor, so ancestry is never destroyed rather than
// being raced against, and teardown can enumerate what landed on us.
//
// The attribute is preserved across exec but, per prctl(2), is NOT inherited
// across fork or clone, and nothing we spawn sets it for itself. Every orphan
// under this process therefore reparents onto the daemon directly and
// immediately, including orphans of a pipeline that is still running. Which
// invocation an orphan belongs to is decided by recordAdoptedOrphans, not by the
// shape of the process tree; do not reintroduce the assumption that a live
// leader shields its own subtree.
//
// The prctl is idempotent, so setting it once per process is enough.
func startDescendantDiscovery(d *ShellCommandDescendants) descendantDiscovery {
	childSubreaperOnce.Do(func() {
		childSubreaperErr = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	})
	if childSubreaperErr != nil {
		d.noteLostDiscovery("child subreaper unavailable", d.leaderPID, childSubreaperErr)
		return nil
	}
	return &subreaperDiscovery{descendants: d}
}

type subreaperDiscovery struct {
	descendants *ShellCommandDescendants
	stopOnce    sync.Once
}

// stop runs at teardown and collects the orphans this process adopted. Orphans
// of a concurrently running invocation are sitting in the same place, so
// ownership is proved per pid rather than assumed; see recordAdoptedOrphans.
func (s *subreaperDiscovery) stop() {
	s.stopOnce.Do(func() {
		s.descendants.recordAdoptedOrphans(sampleProcessTable(), s.descendants.carriesInvocationToken)
	})
}

// processCarriesDescendantToken reports whether pid's environment still holds
// token. /proc/<pid>/environ is the process's initial environment, which fork
// and exec propagate down the whole subtree, so it answers "did this come from
// that invocation" without needing an ancestry that is already gone.
//
// Every failure - an exited process, an unreadable environment, a zombie whose
// environment the kernel has already dropped - reads as "not ours", which leaves
// the process running instead of terminating a stranger's work.
func processCarriesDescendantToken(pid int, token string) bool {
	if pid <= 1 || token == "" {
		return false
	}
	environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	if err != nil {
		return false
	}
	want := descendantTokenEnv + "=" + token
	for _, entry := range strings.Split(string(environ), "\x00") {
		if entry == want {
			return true
		}
	}
	return false
}

// sampleProcessTable reads pid, ppid, pgid, session and start time from /proc.
// It is only ever taken during teardown - never on a schedule - and the
// process-group path reaches it only once a group has proved to still have a
// member, so a clean exit pays for no snapshot at all.
func sampleProcessTable() map[int]procEntry {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	table := make(map[int]procEntry, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue // exited between the readdir and the read
		}
		parsed, ok := parseProcPIDStat(string(stat))
		if !ok {
			continue
		}
		table[pid] = parsed
	}
	return table
}

// parseProcPIDStat pulls ppid, pgrp, session, state and starttime out of one
// /proc/<pid>/stat line. The executable name in field 2 is wrapped in
// parentheses and may itself contain spaces and parentheses, so fields are
// counted from the last closing parenthesis rather than split naively.
func parseProcPIDStat(stat string) (procEntry, bool) {
	close := strings.LastIndexByte(stat, ')')
	if close < 0 || close+2 > len(stat) {
		return procEntry{}, false
	}
	// After the comm field the remaining columns are stat(5) fields 3 onwards:
	// state, ppid, pgrp, session, ... with starttime as field 22.
	fields := strings.Fields(stat[close+1:])
	const (
		stateIdx     = 0
		ppidIdx      = 1
		pgrpIdx      = 2
		sessionIdx   = 3
		starttimeIdx = 19
	)
	if len(fields) <= starttimeIdx {
		return procEntry{}, false
	}
	ppid, err := strconv.Atoi(fields[ppidIdx])
	if err != nil {
		return procEntry{}, false
	}
	pgid, err := strconv.Atoi(fields[pgrpIdx])
	if err != nil {
		return procEntry{}, false
	}
	sid, err := strconv.Atoi(fields[sessionIdx])
	if err != nil {
		return procEntry{}, false
	}
	startedAt, err := strconv.ParseUint(fields[starttimeIdx], 10, 64)
	if err != nil {
		return procEntry{}, false
	}
	return procEntry{
		ppid:      ppid,
		pgid:      pgid,
		sid:       sid,
		startedAt: startedAt,
		zombie:    fields[stateIdx] == "Z",
	}, true
}

// collectAdoptedDescendant makes one non-blocking attempt to collect an orphan
// this process adopted as a subreaper, reporting whether it was collected.
// Without collection every reaped escapee would linger as a zombie for the
// daemon's lifetime, trading a CPU leak for a pid-table leak.
//
// Always waiting on an exact pid is what makes it safe. A bare wait(-1) would
// race os/exec and could consume the exit status a Cmd.Wait is blocked on;
// waiting on a specific pid that os/exec does not own cannot, and a pid that is
// not our child fails with ECHILD without disturbing anything.
func collectAdoptedDescendant(pid int) bool {
	if pid <= 1 {
		return false
	}
	var status unix.WaitStatus
	collected, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
	return err == nil && collected == pid
}

// collectAdoptedGroupOrphans collects the members of leaderPID's process group
// that this process adopted as a subreaper and that have since exited.
//
// The subreaper obliges us to wait on what we adopt, and process-group cleanup
// is where the obligation falls due for descendants that never left the group:
// a worker backgrounded by a configured build/test/lint shell is orphaned the
// moment that shell exits, reparents onto the daemon rather than init, and after
// the group SIGTERM stays a zombie that nothing collects. That costs a pid slot
// for the daemon's lifetime, and - because kill(-pgid, 0) still succeeds for a
// group whose only remaining member is a zombie - it would also make every such
// teardown burn the whole grace window before SIGKILLing a corpse.
//
// Attribution here needs no token: Setpgid makes the group id the leader's pid,
// so the only member of it that os/exec owns is the leader itself, and every
// other member that is a direct child of ours was adopted rather than spawned.
//
// within bounds how long it waits for stragglers still on their way out; zero
// makes it a single pass. It returns as soon as the group is empty either way.
func collectAdoptedGroupOrphans(leaderPID int, within time.Duration) {
	if leaderPID <= 1 {
		return
	}
	self := os.Getpid()
	deadline := time.Now().Add(within)
	for {
		for pid, entry := range sampleProcessTable() {
			if pid == leaderPID || pid == self || !entry.zombie {
				continue
			}
			if entry.ppid != self || entry.pgid != leaderPID {
				continue
			}
			collectAdoptedDescendant(pid)
		}
		if unix.Kill(-leaderPID, 0) == unix.ESRCH || !time.Now().Before(deadline) {
			return
		}
		time.Sleep(processGroupTerminationPollInterval)
	}
}
