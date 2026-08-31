package agent

import "strings"

// withoutFlagValues removes value-taking flags that a first-class controller
// option replaces. Other arguments retain their original order.
func withoutFlagValues(args []string, flags ...string) []string {
	blocked := make(map[string]bool, len(flags))
	shortFlags := make([]string, 0, len(flags))
	for _, flag := range flags {
		blocked[flag] = true
		if len(flag) == 2 && strings.HasPrefix(flag, "-") {
			shortFlags = append(shortFlags, flag)
		}
	}

	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, _, hasAttachedValue := strings.Cut(arg, "=")
		if !blocked[name] {
			remove := false
			for _, flag := range shortFlags {
				if strings.HasPrefix(arg, flag) && len(arg) > len(flag) {
					remove = true
					break
				}
			}
			if remove {
				continue
			}
			filtered = append(filtered, arg)
			continue
		}
		if !hasAttachedValue && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
			i++
		}
	}
	return filtered
}

// withoutCodexConfigKey removes only one typed `-c key=value` override while
// preserving every unrelated Codex config entry and its original order.
func withoutCodexConfigKey(args []string, key string) []string {
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "-c" || arg == "--config" {
			if i+1 < len(args) && codexConfigAssignmentKey(args[i+1]) == key {
				i++
				continue
			}
			filtered = append(filtered, arg)
			continue
		}
		for _, prefix := range []string{"-c ", "--config ", "-c=", "--config="} {
			if strings.HasPrefix(arg, prefix) && codexConfigAssignmentKey(strings.TrimPrefix(arg, prefix)) == key {
				arg = ""
				break
			}
		}
		if arg != "" {
			filtered = append(filtered, arg)
		}
	}
	return filtered
}

func codexConfigAssignmentKey(value string) string {
	key, _, ok := strings.Cut(strings.TrimSpace(value), "=")
	if !ok {
		return ""
	}
	return strings.TrimSpace(key)
}
