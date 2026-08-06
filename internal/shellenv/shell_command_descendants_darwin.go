//go:build darwin

package shellenv

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// kqueueStopIdent is the EVFILT_USER identifier used to wake the blocked
// discovery loop at teardown. Any value works; it shares no namespace with the
// pids registered under EVFILT_PROC.
const kqueueStopIdent = 1

// descendantProcEvents are the process transitions worth waking up for.
// NOTE_FORK says the tree grew, NOTE_EXEC says a child replaced its image (a
// tool-call shell exec'ing the work), NOTE_EXIT says it shrank.
const descendantProcEvents = unix.NOTE_FORK | unix.NOTE_EXEC | unix.NOTE_EXIT

// kqueueDiscovery learns the descendant tree from kernel process events.
//
// The obvious tool for this is EVFILT_PROC with NOTE_TRACK, which on FreeBSD
// reports each fork with the new child's pid and auto-registers that child.
// XNU does not implement it: registering NOTE_TRACK fails with ENOTSUP on
// macOS 26.3 / xnu-12377, while NOTE_FORK, NOTE_EXEC and NOTE_EXIT all attach
// normally. golang.org/x/sys exports the constant regardless, which is a
// userspace header fact and not kernel support.
//
// So the kernel tells us that something forked but not what. The loop blocks in
// kevent with no timeout - costing nothing while the tree is quiet - and on each
// wakeup takes one kern.proc.all snapshot (about 95us, one syscall, no fork) to
// learn the new pids and subscribe to them in turn.
//
// This narrows the discovery window to wakeup-to-snapshot rather than closing
// it. A grandchild forked and orphaned inside that window is still missed; the
// sentinel in the shared file is what makes such a miss visible.
//
// registered is keyed by pid but records process identity, because the kernel
// drops an EVFILT_PROC registration when its process exits: a bare "already
// subscribed" set would treat a recycled pid as covered and stay blind to the
// new process's forks for the rest of the invocation.
type kqueueDiscovery struct {
	descendants *ShellCommandDescendants
	kq          int
	registered  map[int]uint64
	done        chan struct{}
	stopOnce    sync.Once
	kqClosed    bool
}

// descendantDiscoveryTracksLiveLeader reports whether discovery learns
// descendants while the leader is still running. kqueue process events do, which
// is what lets teardown reach an escapee whose ancestry is gone by then.
const descendantDiscoveryTracksLiveLeader = true

func startDescendantDiscovery(d *ShellCommandDescendants) descendantDiscovery {
	kq, err := unix.Kqueue()
	if err != nil {
		d.noteLostDiscovery("kqueue unavailable", d.leaderPID, err)
		return nil
	}

	wake := unix.Kevent_t{
		Ident:  kqueueStopIdent,
		Filter: unix.EVFILT_USER,
		Flags:  unix.EV_ADD | unix.EV_CLEAR,
	}
	if _, err := unix.Kevent(kq, []unix.Kevent_t{wake}, nil, nil); err != nil {
		d.noteLostDiscovery("kqueue wakeup registration failed", d.leaderPID, err)
		_ = unix.Close(kq)
		return nil
	}

	discovery := &kqueueDiscovery{
		descendants: d,
		kq:          kq,
		registered:  make(map[int]uint64),
		done:        make(chan struct{}),
	}
	if err := discovery.subscribe(d.leaderPID); err != nil {
		d.noteLostDiscovery("cannot subscribe to agent process events", d.leaderPID, err)
		_ = unix.Close(kq)
		return nil
	}

	go discovery.loop()
	return discovery
}

func (k *kqueueDiscovery) loop() {
	defer close(k.done)
	// The leader may already have forked between Start and subscription, so take
	// one snapshot before parking on the queue.
	k.snapshot()

	events := make([]unix.Kevent_t, 64)
	for {
		// A nil timeout blocks indefinitely: no timer, no wakeups of our own.
		n, err := unix.Kevent(k.kq, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			k.descendants.noteLostDiscovery("kqueue wait failed", k.descendants.leaderPID, err)
			return
		}
		stopping := false
		for i := 0; i < n; i++ {
			if events[i].Filter == unix.EVFILT_USER {
				stopping = true
			}
		}
		// Snapshot before honouring the stop so the final wakeup still collects
		// whatever the leader spawned on its way out.
		k.snapshot()
		if stopping {
			return
		}
	}
}

// snapshot reads the process table once and subscribes to every descendant not
// already registered, so their forks wake us too. This is the userspace stand-in
// for the recursive auto-registration NOTE_TRACK would have done in the kernel.
func (k *kqueueDiscovery) snapshot() {
	table := sampleProcessTable()
	if len(table) == 0 {
		return
	}
	// Registrations die with their process, so forget the ones whose process is
	// gone. That both bounds the map and makes a recycled pid look unregistered,
	// which it is.
	for pid, startedAt := range k.registered {
		if entry, ok := table[pid]; !ok || entry.startedAt != startedAt {
			delete(k.registered, pid)
		}
	}
	for _, pid := range k.descendants.recordDescendants(table) {
		if _, ok := k.registered[pid]; ok {
			continue
		}
		if err := k.subscribe(pid); err != nil {
			// ESRCH only means the process exited between the snapshot and the
			// subscription, which is not a leak. Anything else is a descendant we
			// are now blind to, and is reported rather than swallowed - this is the
			// NOTE_TRACKERR case that the kernel would have raised for us.
			if !errors.Is(err, unix.ESRCH) {
				k.descendants.noteLostDiscovery("cannot subscribe to descendant process events", pid, err)
			}
			continue
		}
		k.registered[pid] = table[pid].startedAt
	}
}

func (k *kqueueDiscovery) subscribe(pid int) error {
	if pid <= 0 {
		return unix.ESRCH
	}
	event := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ENABLE | unix.EV_CLEAR,
		Fflags: descendantProcEvents,
	}
	_, err := unix.Kevent(k.kq, []unix.Kevent_t{event}, nil, nil)
	return err
}

func (k *kqueueDiscovery) stop() {
	k.stopOnce.Do(func() {
		trigger := unix.Kevent_t{
			Ident:  kqueueStopIdent,
			Filter: unix.EVFILT_USER,
			Fflags: unix.NOTE_TRIGGER,
		}
		if _, err := unix.Kevent(k.kq, []unix.Kevent_t{trigger}, nil, nil); err != nil {
			// Without the wakeup the loop would park forever, so close the queue to
			// force kevent to fail out instead of deadlocking teardown. The loop
			// goroutine observes EBADF and finishes, and the descriptor must not be
			// closed a second time below: the daemon opens files, pipes and sockets
			// constantly, so the number would by then belong to something else.
			_ = unix.Close(k.kq)
			k.kqClosed = true
		}
	})
	<-k.done
	if !k.kqClosed {
		_ = unix.Close(k.kq)
	}
}

// sampleProcessTable reads pid, ppid, pgid and start time for every process in
// one sysctl. No fork, no ps: roughly 95us for ~700 processes, which is what
// makes it affordable to run on every kernel wakeup.
//
// Darwin's kinfo_proc carries no session id, so procEntry.sid stays zero here.
// Only the Linux adopted-orphan path needs it, and macOS never adopts because it
// has no child-subreaper facility.
func sampleProcessTable() map[int]procEntry {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil
	}
	table := make(map[int]procEntry, len(procs))
	for i := range procs {
		proc := &procs[i]
		pid := int(proc.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		table[pid] = procEntry{
			ppid:      int(proc.Eproc.Ppid),
			pgid:      int(proc.Eproc.Pgid),
			startedAt: uint64(proc.Proc.P_starttime.Sec)*1_000_000 + uint64(proc.Proc.P_starttime.Usec),
			zombie:    proc.Proc.P_stat == zombieProcessState,
		}
	}
	return table
}

// zombieProcessState is SZOMB from sys/proc.h: exited, awaiting collection.
const zombieProcessState = 5

// collectAdoptedDescendant and collectAdoptedGroupOrphans are no-ops on Darwin.
// Nothing is ever adopted here - there is no PR_SET_CHILD_SUBREAPER equivalent,
// so orphans go straight to init and init collects them.
func collectAdoptedDescendant(int) bool { return false }

func collectAdoptedGroupOrphans(int, time.Duration) {}

// processCarriesDescendantToken is unused on Darwin: the invocation token exists
// to attribute adopted orphans, and Darwin adopts none.
func processCarriesDescendantToken(int, string) bool { return false }
