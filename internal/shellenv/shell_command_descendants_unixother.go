//go:build unix && !darwin && !linux

package shellenv

import "time"

// The remaining unix targets have neither a usable kqueue process filter nor
// PR_SET_CHILD_SUBREAPER, so there is no way to learn about a descendant that
// setsid its way out of the process group without falling back to a timer.
// Discovery is therefore disabled rather than approximated: the process-group
// cleanup in shell_command_unix.go still applies, and the sentinel still reports
// loudly when something survives it.

// descendantDiscoveryTracksLiveLeader reports whether discovery learns
// descendants while the leader is still running. With discovery disabled, it
// learns none.
const descendantDiscoveryTracksLiveLeader = false

func startDescendantDiscovery(*ShellCommandDescendants) descendantDiscovery { return nil }

func sampleProcessTable() map[int]procEntry { return nil }

func collectAdoptedDescendant(int) bool { return false }

func collectAdoptedGroupOrphans(int, time.Duration) {}

func processCarriesDescendantToken(int, string) bool { return false }
