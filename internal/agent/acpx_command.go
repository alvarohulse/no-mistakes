package agent

import (
	"fmt"
	"slices"
	"strings"
)

// composeACPTargetCommand translates controller args into the raw command acpx
// spawns. Cursor root flags precede its acp subcommand; arbitrary raw commands
// use append semantics because no-mistakes cannot infer their CLI grammar.
func composeACPTargetCommand(target, rawCommand string, extraArgs []string, model string) (string, error) {
	if len(extraArgs) == 0 && model == "" {
		return rawCommand, nil
	}
	if rawCommand == "" {
		return "", fmt.Errorf("target %q has no raw command to receive a configured model or agent_args_override; configure acp_registry_overrides.%s", target, target)
	}

	command, err := splitACPXCommandLine(rawCommand)
	if err != nil {
		return "", err
	}
	spawnArgs := extraArgs
	if model != "" {
		spawnArgs = withoutFlagValues(spawnArgs, "-m", "--model")
		spawnArgs = append(spawnArgs, "--model", model)
	}

	insertAt := len(command)
	if target == "cursor" {
		insertAt = uniqueCommandTokenIndex(command, "acp")
		if insertAt < 0 {
			return "", fmt.Errorf("cursor raw command must contain exactly one acp subcommand so target arguments can be placed safely")
		}
	}
	command = slices.Insert(command, insertAt, spawnArgs...)

	for i, arg := range command {
		command[i] = quoteACPXCommandArg(arg)
	}
	return strings.Join(command, " "), nil
}

func uniqueCommandTokenIndex(command []string, token string) int {
	index := -1
	for i := 1; i < len(command); i++ {
		if command[i] == token {
			if index >= 0 {
				return -1
			}
			index = i
		}
	}
	return index
}

// splitACPXCommandLine mirrors acpx's --agent tokenizer. The raw command is
// parsed by acpx directly rather than by a platform shell.
func splitACPXCommandLine(value string) ([]string, error) {
	var parts []string
	var current strings.Builder
	var quote rune
	escaping := false
	hasPart := false

	flush := func() {
		if hasPart {
			parts = append(parts, current.String())
			current.Reset()
			hasPart = false
		}
	}
	for _, char := range value {
		if escaping {
			current.WriteRune(char)
			escaping = false
			hasPart = true
			continue
		}
		if char == '\\' && quote != '\'' {
			escaping = true
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
				hasPart = true
			} else {
				current.WriteRune(char)
				hasPart = true
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			hasPart = true
			continue
		}
		if isECMAScriptWhitespace(char) {
			flush()
			continue
		}
		current.WriteRune(char)
		hasPart = true
	}
	if escaping {
		current.WriteRune('\\')
		hasPart = true
	}
	if quote != 0 {
		return nil, fmt.Errorf("invalid ACP raw command: unterminated quote")
	}
	flush()
	if len(parts) == 0 || parts[0] == "" {
		return nil, fmt.Errorf("invalid ACP raw command: empty command")
	}
	return parts, nil
}

func quoteACPXCommandArg(arg string) string {
	if arg != "" && strings.IndexFunc(arg, acpxCommandArgNeedsQuotes) < 0 {
		return arg
	}
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"")
	return "\"" + replacer.Replace(arg) + "\""
}

func acpxCommandArgNeedsQuotes(char rune) bool {
	return isECMAScriptWhitespace(char) || char == '\'' || char == '"' || char == '\\'
}

// isECMAScriptWhitespace matches the current ECMAScript \s character set used
// by acpx's command tokenizer. unicode.IsSpace is intentionally broader: it
// includes U+0085, which acpx retains inside an argument.
func isECMAScriptWhitespace(char rune) bool {
	if char >= '\u0009' && char <= '\u000d' {
		return true
	}
	if char >= '\u2000' && char <= '\u200a' {
		return true
	}
	switch char {
	case '\u0020', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	default:
		return false
	}
}
