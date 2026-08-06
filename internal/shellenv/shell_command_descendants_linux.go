//go:build linux

package shellenv

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var (
	childSubreaperOnce sync.Once
	childSubreaperErr  error
)

// startDescendantDiscovery makes this process a child subreaper and returns the
// teardown-time collector.
//
// Linux needs no fork notification at all, which is the whole point of doing it
// this way. PR_SET_CHILD_SUBREAPER redirects orphan reparenting from init to the
// nearest living subreaper ancestor, so ancestry is never destroyed rather than
// being raced against. While an agent leader is alive its own orphans stay
// inside its subtree; only when the leader itself dies do they land on us, which
// is precisely the moment teardown looks.
//
// The attribute is inherited across fork and preserved across exec, and it is
// idempotent, so setting it once per process is enough.
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

// stop runs at teardown, immediately after the leader was reaped, and collects
// the orphans that just reparented onto us. See recordAdoptedOrphans for why
// what we see now is this leader's and not a concurrently running run's.
func (s *subreaperDiscovery) stop() {
	s.stopOnce.Do(func() {
		s.descendants.recordAdoptedOrphans(sampleProcessTable())
	})
}

// sampleProcessTable reads pid, ppid, pgid, session and start time from /proc.
// It is taken once at teardown, not on a schedule.
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

// reapAdoptedDescendant collects an orphan this process adopted as a subreaper.
// Without it every reaped escapee would linger as a zombie for the daemon's
// lifetime, trading a CPU leak for a pid-table leak.
//
// Only pids identified as adopted reach here. That restriction is what makes it
// safe: a bare wait(-1) would race os/exec and could consume the exit status a
// Cmd.Wait is blocked on, and an adopted orphan is by construction not an
// os/exec child.
func reapAdoptedDescendant(pid int) {
	if pid <= 1 {
		return
	}
	var status unix.WaitStatus
	for attempt := 0; attempt < adoptedReapAttempts; attempt++ {
		waited, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
		if waited == pid || err != nil {
			return // collected, or never ours to collect
		}
		unix.Nanosleep(&unix.Timespec{Nsec: int64(adoptedReapPause)}, nil)
	}
}

const (
	adoptedReapAttempts = 20
	adoptedReapPause    = 5_000_000 // 5ms in nanoseconds
)
