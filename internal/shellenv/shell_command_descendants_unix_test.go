//go:build unix

package shellenv

import (
	"os"
	"os/exec"
	"reflect"
	"syscall"
	"testing"
	"time"
)

// TestSignallableEscapee pins the guards that keep a descendant sweep from
// turning into a self-inflicted outage. Signalling our own process group would
// take the daemon down along with the leak.
func TestSignallableEscapee(t *testing.T) {
	const leaderPGID, ownPGID = 4000, 500

	tests := []struct {
		name       string
		pid        int
		pgid       int
		leaderPGID int
		want       bool
	}{
		{name: "escaped into its own group", pid: 4100, pgid: 4100, leaderPGID: leaderPGID, want: true},
		{name: "child of an escapee", pid: 4101, pgid: 4100, leaderPGID: leaderPGID, want: true},
		{name: "still in the leader group", pid: 4050, pgid: leaderPGID, leaderPGID: leaderPGID, want: false},
		{name: "our own process group", pid: 4200, pgid: ownPGID, leaderPGID: leaderPGID, want: false},
		{name: "init", pid: 1, pgid: 1, leaderPGID: leaderPGID, want: false},
		{name: "group id zero", pid: 4300, pgid: 0, leaderPGID: leaderPGID, want: false},
		{name: "our own pid", pid: os.Getpid(), pgid: 4400, leaderPGID: leaderPGID, want: false},
		// leaderPGID 0 means "no leader group to compare against", used by the
		// adopted-orphan path once the leader is already gone.
		{name: "no leader group to compare", pid: 4500, pgid: 4500, leaderPGID: 0, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := signallableEscapee(tt.pid, tt.pgid, tt.leaderPGID, ownPGID); got != tt.want {
				t.Fatalf("signallableEscapee(%d, %d, %d, %d) = %v, want %v",
					tt.pid, tt.pgid, tt.leaderPGID, ownPGID, got, tt.want)
			}
		})
	}
}

// TestSurvivingEscapees_RequiresMatchingStartTime is the pid-reuse guard: a
// recorded pid that now belongs to a different process must never be signalled.
func TestSurvivingEscapees_RequiresMatchingStartTime(t *testing.T) {
	recorded := []trackedDescendant{
		{pid: 4100, pgid: 4100, startedAt: 1000, escaped: true},
		{pid: 4101, pgid: 4100, startedAt: 1000, escaped: true},
		{pid: 4102, pgid: 4102, startedAt: 1000, escaped: true},
	}
	table := map[int]procEntry{
		// Same process: still a victim.
		4100: {ppid: 1, pgid: 4100, startedAt: 1000},
		// Pid recycled by something unrelated: must be left alone.
		4101: {ppid: 900, pgid: 900, startedAt: 7777},
		// 4102 has exited and is absent entirely.
	}

	victims := survivingEscapees(recorded, table)
	if len(victims) != 1 || victims[0].pid != 4100 {
		t.Fatalf("survivingEscapees() = %#v, want only pid 4100", victims)
	}

	if got := survivingEscapees(recorded, nil); got != nil {
		t.Fatalf("survivingEscapees() with an unreadable process table = %#v, want nil", got)
	}
}

func TestDescendantClosure(t *testing.T) {
	table := map[int]procEntry{
		1:    {ppid: 1}, // self-parented root must not loop
		9000: {ppid: 1},
		9001: {ppid: 9000},
		9002: {ppid: 9001},
		9003: {ppid: 9000},
		9500: {ppid: 1},
	}
	got := descendantClosure(table, []int{9000})
	want := []int{9001, 9002, 9003}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descendantClosure() = %v, want %v", got, want)
	}
	if got := descendantClosure(table, []int{9500}); got != nil {
		t.Fatalf("descendantClosure() for a childless seed = %v, want nil", got)
	}
}

func TestShellCommandDescendants_RecordTracksOnlyEscapees(t *testing.T) {
	ownPGID, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("Getpgid(0): %v", err)
	}
	const leader, inGroup, escapee, grandchild, unrelated = 9000, 9001, 9002, 9003, 9500
	for _, pid := range []int{leader, inGroup, escapee, grandchild, unrelated} {
		if pid == ownPGID || pid == os.Getpid() {
			t.Skipf("synthetic pid %d collides with this process", pid)
		}
	}

	table := map[int]procEntry{
		leader:     {ppid: 8000, pgid: leader, startedAt: 10},
		inGroup:    {ppid: leader, pgid: leader, startedAt: 11},
		escapee:    {ppid: leader, pgid: escapee, startedAt: 12},
		grandchild: {ppid: escapee, pgid: escapee, startedAt: 13},
		unrelated:  {ppid: 8000, pgid: 8000, startedAt: 14},
	}

	d := &ShellCommandDescendants{leaderPID: leader, leaderPGID: leader, tracked: make(map[int]trackedDescendant)}
	closure := d.recordDescendants(table)

	// The whole subtree is returned so discovery can subscribe to each member,
	// including the ones the group kill already covers.
	if want := []int{inGroup, escapee, grandchild}; !reflect.DeepEqual(closure, want) {
		t.Fatalf("recordDescendants() closure = %v, want %v", closure, want)
	}
	// In-group descendants are tracked but not marked escaped: the group kill
	// already covers them, and they are only kept so a later setsid can be
	// noticed.
	if tracked, ok := d.tracked[inGroup]; !ok || tracked.escaped {
		t.Errorf("descendant %d inside the leader's group = %#v, want tracked but not escaped", inGroup, tracked)
	}
	if _, ok := d.tracked[unrelated]; ok {
		t.Error("tracked a process outside the leader's subtree")
	}
	for _, pid := range []int{escapee, grandchild} {
		if tracked, ok := d.tracked[pid]; !ok || !tracked.escaped {
			t.Errorf("did not record escaped descendant %d", pid)
		}
	}
}

// TestShellCommandDescendants_RecordNoticesLateSetsid is the regression test for
// a gap the sentinel caught: setsid raises no process event, so a tool shell
// that forks first and leaves the process group afterwards looks perfectly
// in-group at the instant the fork wakes discovery. Only re-classifying known
// descendants on later snapshots finds it.
func TestShellCommandDescendants_RecordNoticesLateSetsid(t *testing.T) {
	const leader, toolShell = 9000, 9001
	d := &ShellCommandDescendants{
		leaderPID:  leader,
		leaderPGID: leader,
		tracked:    make(map[int]trackedDescendant),
	}

	// First snapshot, taken on the fork event: the child is still in the group.
	d.recordDescendants(map[int]procEntry{
		leader:    {ppid: 8000, pgid: leader, startedAt: 10},
		toolShell: {ppid: leader, pgid: leader, startedAt: 11},
	})
	if tracked := d.tracked[toolShell]; tracked.escaped {
		t.Fatalf("tool shell marked escaped while still in the leader's group: %#v", tracked)
	}

	// Later snapshot, after setsid: same process, now leading its own group.
	d.recordDescendants(map[int]procEntry{
		leader:    {ppid: 8000, pgid: leader, startedAt: 10},
		toolShell: {ppid: leader, pgid: toolShell, startedAt: 11},
	})
	if tracked := d.tracked[toolShell]; !tracked.escaped {
		t.Fatalf("tool shell %#v not re-classified as escaped after leaving the group", tracked)
	}
}

// TestShellCommandDescendants_RecordWithoutLeaderGroupStaysSilent guards the
// case where the leader's group was never observed: with nothing to compare
// against, classifying would mean guessing at which group to signal.
func TestShellCommandDescendants_RecordWithoutLeaderGroupStaysSilent(t *testing.T) {
	d := &ShellCommandDescendants{leaderPID: 9000, tracked: make(map[int]trackedDescendant)}
	table := map[int]procEntry{
		9000: {ppid: 8000, pgid: 9000, startedAt: 10},
		9001: {ppid: 9000, pgid: 9001, startedAt: 11},
	}
	if closure := d.recordDescendants(table); closure != nil {
		t.Fatalf("recordDescendants() = %v, want nil without a known leader group", closure)
	}
	if len(d.tracked) != 0 {
		t.Fatalf("tracked %#v without a known leader group", d.tracked)
	}
}

// TestShellCommandDescendants_RecordSeedsFromKnownDescendants covers the case
// that makes discovery survive an intermediate death: once a tool shell is
// known, its children stay reachable even after the leader is gone.
func TestShellCommandDescendants_RecordSeedsFromKnownDescendants(t *testing.T) {
	const leader, escapee, lateChild = 9000, 9002, 9004
	d := &ShellCommandDescendants{
		leaderPID:  leader,
		leaderPGID: leader,
		tracked: map[int]trackedDescendant{
			escapee: {pid: escapee, pgid: escapee, startedAt: 12, escaped: true},
		},
	}
	// Leader absent: it has exited. The escapee is still alive with the same
	// start time, so its new child must still be found.
	table := map[int]procEntry{
		escapee:   {ppid: 1, pgid: escapee, startedAt: 12},
		lateChild: {ppid: escapee, pgid: escapee, startedAt: 15},
	}
	if closure := d.recordDescendants(table); !reflect.DeepEqual(closure, []int{lateChild}) {
		t.Fatalf("recordDescendants() closure = %v, want [%d]", closure, lateChild)
	}
	if _, ok := d.tracked[lateChild]; !ok {
		t.Errorf("did not record %d, spawned by a known escapee after the leader exited", lateChild)
	}

	// A recycled seed pid must not drag unrelated processes in.
	recycledTracker := &ShellCommandDescendants{
		leaderPID:  leader,
		leaderPGID: leader,
		tracked: map[int]trackedDescendant{
			escapee: {pid: escapee, pgid: escapee, startedAt: 12, escaped: true},
		},
	}
	recycled := map[int]procEntry{
		escapee:   {ppid: 1, pgid: escapee, startedAt: 999999}, // a different process now
		lateChild: {ppid: escapee, pgid: escapee, startedAt: 15},
	}
	if closure := recycledTracker.recordDescendants(recycled); closure != nil {
		t.Fatalf("recordDescendants() followed a recycled seed pid: %v", closure)
	}
}

// TestShellCommandDescendants_RecordForgetsDeadDescendants keeps the per-wakeup
// cost proportional to the live subtree rather than to every process the agent
// ever spawned: discovery re-classifies the whole tracked set on every fork,
// exec and exit event, and a long agent invocation drives thousands of
// short-lived compile and test processes through it.
func TestShellCommandDescendants_RecordForgetsDeadDescendants(t *testing.T) {
	const leader, shortLived, survivor = 9000, 9001, 9002
	d := &ShellCommandDescendants{
		leaderPID:  leader,
		leaderPGID: leader,
		tracked:    make(map[int]trackedDescendant),
	}
	d.recordDescendants(map[int]procEntry{
		leader:     {ppid: 8000, pgid: leader, startedAt: 10},
		shortLived: {ppid: leader, pgid: shortLived, startedAt: 11},
		survivor:   {ppid: leader, pgid: survivor, startedAt: 12},
	})
	if len(d.tracked) != 2 {
		t.Fatalf("tracked = %#v, want both descendants", d.tracked)
	}

	// shortLived has exited; survivor is still there under the same identity.
	d.recordDescendants(map[int]procEntry{
		leader:   {ppid: 8000, pgid: leader, startedAt: 10},
		survivor: {ppid: leader, pgid: survivor, startedAt: 12},
	})
	if _, ok := d.tracked[shortLived]; ok {
		t.Errorf("kept exited descendant %d; tracking grows with every process ever spawned", shortLived)
	}
	if _, ok := d.tracked[survivor]; !ok {
		t.Errorf("dropped live descendant %d", survivor)
	}
}

// TestAdoptedOrphan pins the Linux attribution rule. Our own children share our
// session, so a child of ours in a different session was adopted after its real
// parent died rather than spawned by us.
func TestAdoptedOrphan(t *testing.T) {
	const self, selfSID = 700, 700

	tests := []struct {
		name  string
		pid   int
		entry procEntry
		want  bool
	}{
		{name: "orphan from an escaped session", pid: 9100, entry: procEntry{ppid: self, sid: 9100}, want: true},
		{name: "command we spawned", pid: 9101, entry: procEntry{ppid: self, sid: selfSID}, want: false},
		{name: "not our child", pid: 9102, entry: procEntry{ppid: 42, sid: 9102}, want: false},
		{name: "ourselves", pid: self, entry: procEntry{ppid: self, sid: selfSID}, want: false},
		{name: "session id unavailable", pid: 9103, entry: procEntry{ppid: self, sid: 0}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := adoptedOrphan(tt.pid, tt.entry, self, selfSID); got != tt.want {
				t.Fatalf("adoptedOrphan(%d, %#v) = %v, want %v", tt.pid, tt.entry, got, tt.want)
			}
		})
	}
}

// TestRecordAdoptedOrphans_OnlyAdoptsThisInvocationsOrphans is the cross-run
// guard. PR_SET_CHILD_SUBREAPER is not inherited across fork, so a concurrently
// running pipeline's orphans reparent onto this process immediately, while that
// run is still using them. Teardown must terminate only the orphans it can prove
// came from its own invocation, or one run's cleanup kills another run's work.
func TestRecordAdoptedOrphans_OnlyAdoptsThisInvocationsOrphans(t *testing.T) {
	self := os.Getpid()
	ownPGID, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("Getpgid(0): %v", err)
	}
	// Any session id of our own will do: what matters is that the orphans lead
	// sessions of their own, which is what marks them adopted rather than spawned.
	const selfSID = 700
	const ours, theirs, ourGrandchild = 9100, 9200, 9101
	for _, pid := range []int{ours, theirs, ourGrandchild} {
		if pid == ownPGID || pid == self {
			t.Skipf("synthetic pid %d collides with this process", pid)
		}
	}

	// Both are adopted orphans by every structural test available: our direct
	// child, leading a session of its own. Only the token tells them apart.
	table := map[int]procEntry{
		self:          {ppid: 1, pgid: ownPGID, sid: selfSID, startedAt: 1},
		ours:          {ppid: self, pgid: ours, sid: ours, startedAt: 10},
		ourGrandchild: {ppid: ours, pgid: ours, sid: ours, startedAt: 11},
		theirs:        {ppid: self, pgid: theirs, sid: theirs, startedAt: 12},
	}

	d := &ShellCommandDescendants{tracked: make(map[int]trackedDescendant)}
	d.recordAdoptedOrphans(table, func(pid int) bool { return pid == ours })

	if tracked, ok := d.tracked[ours]; !ok || !tracked.escaped || !tracked.adopted {
		t.Errorf("orphan %d from this invocation = %#v, want adopted and escaped", ours, tracked)
	}
	// Ancestry carries ownership: a descendant of a proven orphan is ours even if
	// it dropped the token itself.
	if _, ok := d.tracked[ourGrandchild]; !ok {
		t.Errorf("did not record %d, spawned by an orphan proven to be ours", ourGrandchild)
	}
	if tracked, ok := d.tracked[theirs]; ok {
		t.Errorf("adopted %d from a concurrently running invocation: %#v", theirs, tracked)
	}

	// No proof available at all must adopt nothing rather than everything.
	empty := &ShellCommandDescendants{tracked: make(map[int]trackedDescendant)}
	empty.recordAdoptedOrphans(table, func(int) bool { return false })
	if len(empty.tracked) != 0 {
		t.Errorf("adopted %#v without any ownership proof", empty.tracked)
	}
}

// TestRecordAdoptedOrphans_CollectsUnattributableZombies covers the one case
// that is exempt from the ownership proof: a zombie's environment is gone, so it
// can never carry the token, and leaving it uncollected would leak a pid slot
// for the daemon's lifetime. Collecting one cannot harm whoever it belonged to.
func TestRecordAdoptedOrphans_CollectsUnattributableZombies(t *testing.T) {
	self := os.Getpid()
	ownPGID, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("Getpgid(0): %v", err)
	}
	const selfSID = 700
	const zombie = 9300
	if zombie == ownPGID || zombie == self {
		t.Skipf("synthetic pid %d collides with this process", zombie)
	}

	table := map[int]procEntry{
		self:   {ppid: 1, pgid: ownPGID, sid: selfSID, startedAt: 1},
		zombie: {ppid: self, pgid: zombie, sid: zombie, startedAt: 10, zombie: true},
	}
	d := &ShellCommandDescendants{tracked: make(map[int]trackedDescendant)}
	d.recordAdoptedOrphans(table, func(int) bool { return false })

	if tracked, ok := d.tracked[zombie]; !ok || !tracked.adopted {
		t.Fatalf("adopted zombie %d = %#v, want recorded for collection", zombie, tracked)
	}
}

// TestPrepareShellCommandDescendants_InstallsInvocationToken pins that the
// identity actually reaches the process tree, since attribution is worthless
// without it, and that two invocations never share one.
func TestPrepareShellCommandDescendants_InstallsInvocationToken(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "true")
	cmd.Env = []string{"PATH=/usr/bin"}
	d := PrepareShellCommandDescendants(cmd, time.Second)
	if d == nil || d.token == "" {
		t.Fatal("PrepareShellCommandDescendants did not mint an invocation token")
	}
	t.Cleanup(d.Terminate)

	var found bool
	for _, entry := range cmd.Env {
		if entry == descendantTokenEnv+"="+d.token {
			found = true
		}
	}
	if !found {
		t.Fatalf("command environment %v does not carry the invocation token", cmd.Env)
	}
	if got, want := len(cmd.Env), 2; got != want {
		t.Fatalf("command environment length = %d, want %d: the caller's environment must survive", got, want)
	}

	other := PrepareShellCommandDescendants(exec.Command("/bin/sh", "-c", "true"), time.Second)
	t.Cleanup(other.Terminate)
	if other.token == d.token {
		t.Fatal("two invocations were given the same token; they could not be told apart")
	}
}

func TestShellCommandDescendants_NilAndUnstartedAreNoops(t *testing.T) {
	var nilDescendants *ShellCommandDescendants
	nilDescendants.Watch()
	nilDescendants.Terminate()

	if got := PrepareShellCommandDescendants(nil, 0); got != nil {
		t.Fatalf("PrepareShellCommandDescendants(nil) = %#v, want nil", got)
	}

	// Prepared but never started: Watch has no pid to work with, and Terminate
	// must still tidy up rather than panic.
	cmd := exec.Command("/bin/sh", "-c", "true")
	d := PrepareShellCommandDescendants(cmd, 0)
	d.Watch()
	d.Terminate()
	d.Terminate()
}

// TestPrepareShellCommandDescendants_InstallsInheritableSentinel checks the
// descriptor actually reaches the child, which is the whole basis of the
// backstop.
func TestPrepareShellCommandDescendants_InstallsInheritableSentinel(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "test -e /dev/fd/3")
	before := len(cmd.ExtraFiles)
	d := PrepareShellCommandDescendants(cmd, time.Second)
	if d == nil {
		t.Fatal("PrepareShellCommandDescendants returned nil for a valid command")
	}
	if len(cmd.ExtraFiles) != before+1 {
		t.Fatalf("ExtraFiles = %d, want %d", len(cmd.ExtraFiles), before+1)
	}
	if err := cmd.Run(); err != nil {
		t.Fatalf("child could not see the inherited sentinel descriptor: %v", err)
	}
	d.Terminate()
}
