package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestEffectiveConfigPublishPushOptionPreservesTriState(t *testing.T) {
	trueValue := true
	falseValue := false
	tests := []struct {
		name  string
		value *bool
	}{
		{name: "unset"},
		{name: "true", value: &trueValue},
		{name: "false", value: &falseValue},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			option := formatEffectiveConfigPublishPushOption(tt.value)
			var options []string
			if option != "" {
				options = []string{option}
			}
			got, err := parseEffectiveConfigPublishPushOptions(options)
			if err != nil {
				t.Fatal(err)
			}
			if tt.value == nil {
				if got != nil {
					t.Fatalf("publication override = %v, want nil", got)
				}
				return
			}
			if got == nil || *got != *tt.value {
				t.Fatalf("publication override = %v, want %t", got, *tt.value)
			}
		})
	}
}

func TestEffectiveConfigPublishPushOptionRejectsInvalidValue(t *testing.T) {
	if _, err := parseEffectiveConfigPublishPushOptions([]string{effectiveConfigPublishPushOptionPrefix + "sometimes"}); err == nil {
		t.Fatal("invalid effective-config publication push option was accepted")
	}
}

func TestRunCommandsRejectConflictingEffectiveConfigPublicationFlags(t *testing.T) {
	for name, cmd := range map[string]commandFactory{
		"axi run": newAxiRunCmd,
		"rerun":   newRerunCmd,
	} {
		t.Run(name, func(t *testing.T) {
			command := cmd()
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			command.SetArgs([]string{"--publish-effective-config", "--no-publish-effective-config"})
			err := command.Execute()
			if err == nil || !strings.Contains(err.Error()+output.String(), "mutually exclusive") {
				t.Fatalf("conflicting publication flags error = %v, output = %q", err, output.String())
			}
		})
	}
}

func TestRunCommandEffectiveConfigPublicationFlagsProduceExplicitValues(t *testing.T) {
	tests := []struct {
		name string
		flag string
		want bool
	}{
		{name: "positive", flag: "publish-effective-config", want: true},
		{name: "negative", flag: "no-publish-effective-config", want: false},
	}
	for commandName, newCommand := range map[string]commandFactory{
		"axi run": newAxiRunCmd,
		"rerun":   newRerunCmd,
	} {
		for _, tt := range tests {
			t.Run(commandName+" "+tt.name, func(t *testing.T) {
				command := newCommand()
				if err := command.Flags().Set(tt.flag, "true"); err != nil {
					t.Fatal(err)
				}
				publish, err := command.Flags().GetBool("publish-effective-config")
				if err != nil {
					t.Fatal(err)
				}
				noPublish, err := command.Flags().GetBool("no-publish-effective-config")
				if err != nil {
					t.Fatal(err)
				}
				got, err := effectiveConfigPublishOverride(command, publish, noPublish)
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || *got != tt.want {
					t.Fatalf("publication override = %v, want %t", got, tt.want)
				}
			})
		}
	}
}

type commandFactory func() *cobra.Command
