package runner

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"
)

// SchemaVersion identifies the persisted runner provenance contract.
const SchemaVersion = 1

const (
	SourceBase            = "base"
	SourcePortableDefault = "portable-default"
	SourceDefault         = "default"
	SourceInline          = "inline"
	SourceLinux           = "linux"
	SourceMacOS           = "macos"
	SourceWindows         = "windows"
)

// Spec is one shell executable plus the arguments that make its final argv
// element the command string. It deliberately models argv rather than a shell-
// split string so configuration never needs lossy quoting or reparsing.
type Spec struct {
	Executable string   `yaml:"executable" json:"executable"`
	Args       []string `yaml:"args" json:"args"`
	present    map[string]bool
}

func (s *Spec) UnmarshalYAML(value *yaml.Node) error {
	if err := rejectUnknownMappingFields(value, "runner", "executable", "args"); err != nil {
		return err
	}
	type rawSpec Spec
	var raw rawSpec
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*s = Spec(raw)
	s.present = mappingPresence(value)
	return nil
}

// Clone returns an independently mutable runner specification.
func (s Spec) Clone() Spec {
	return Spec{Executable: s.Executable, Args: append([]string(nil), s.Args...), present: clonePresence(s.present)}
}

// Overlay applies only fields explicitly present in a parsed mapping. A
// programmatically constructed spec replaces both fields.
func (s Spec) Overlay(override Spec) Spec {
	if override.present == nil {
		return override.Clone()
	}
	result := s.Clone()
	if override.present["executable"] {
		result.Executable = override.Executable
	}
	if override.present["args"] {
		result.Args = append([]string(nil), override.Args...)
	}
	result.present = nil
	return result
}

// Override replaces a command string, a runner, or both for one platform.
// Run is a pointer so an absent value inherits while an explicit empty value
// remains distinguishable and is rejected only if that platform is resolved.
type Override struct {
	Run     *string `yaml:"run,omitempty" json:"run,omitempty"`
	Runner  *Spec   `yaml:"runner,omitempty" json:"runner,omitempty"`
	form    yamlForm
	present map[string]bool
}

func (o *Override) UnmarshalYAML(value *yaml.Node) error {
	value = dereferenceYAMLNode(value)
	if value.Kind == yaml.ScalarNode {
		if value.Tag != "!!str" {
			return fmt.Errorf("platform command override must be a string or mapping")
		}
		run := value.Value
		o.Run = &run
		o.Runner = nil
		o.form = yamlFormScalar
		o.present = map[string]bool{"run": true}
		return nil
	}
	if err := rejectUnknownMappingFields(value, "platform command override", "run", "runner"); err != nil {
		return err
	}
	type rawOverride Override
	var raw rawOverride
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*o = Override(raw)
	o.form = yamlFormMapping
	o.present = mappingPresence(value)
	return nil
}

// Command is a shell command with an optional inline runner and per-platform
// overrides. A YAML scalar decodes as Run for compatibility with existing
// commands.* configuration.
type Command struct {
	Run     string    `yaml:"run" json:"run"`
	Runner  *Spec     `yaml:"runner,omitempty" json:"runner,omitempty"`
	Linux   *Override `yaml:"linux,omitempty" json:"linux,omitempty"`
	MacOS   *Override `yaml:"macos,omitempty" json:"macos,omitempty"`
	Windows *Override `yaml:"windows,omitempty" json:"windows,omitempty"`
	form    yamlForm
	present map[string]bool
}

func (c *Command) UnmarshalYAML(value *yaml.Node) error {
	value = dereferenceYAMLNode(value)
	if value.Kind == yaml.ScalarNode {
		if value.Tag == "!!null" {
			*c = Command{}
			c.form = yamlFormScalar
			c.present = map[string]bool{"run": true}
			return nil
		}
		// The legacy string fields accepted YAML-typed scalars such as an
		// unquoted `true` and decoded them to their source text. Preserve that
		// behavior while adding the structured mapping form.
		var run string
		if err := value.Decode(&run); err != nil {
			return fmt.Errorf("command must be a scalar or mapping: %w", err)
		}
		*c = Command{Run: run, form: yamlFormScalar, present: map[string]bool{"run": true}}
		return nil
	}
	if err := rejectUnknownMappingFields(value, "command", "run", "runner", "linux", "macos", "windows"); err != nil {
		return err
	}
	type rawCommand Command
	var raw rawCommand
	if err := value.Decode(&raw); err != nil {
		return err
	}
	*c = Command(raw)
	c.form = yamlFormMapping
	c.present = mappingPresence(value)
	return nil
}

func (c Command) MarshalYAML() (any, error) {
	if c.Runner == nil && c.Linux == nil && c.MacOS == nil && c.Windows == nil {
		return c.Run, nil
	}
	type rawCommand Command
	return rawCommand(c), nil
}

// Clone returns an independently mutable command definition.
func (c Command) Clone() Command {
	cloned := Command{
		Run:     c.Run,
		Runner:  cloneSpec(c.Runner),
		Linux:   cloneOverride(c.Linux),
		MacOS:   cloneOverride(c.MacOS),
		Windows: cloneOverride(c.Windows),
		form:    c.form,
	}
	cloned.present = clonePresence(c.present)
	return cloned
}

// Canonical returns the semantic command without YAML presence metadata. Use
// it at persistence boundaries so equivalent scalar/mapping inputs compare
// identically after decoding.
func (c Command) Canonical() Command {
	canonical := c.Clone()
	canonical.form = yamlFormUnknown
	canonical.present = nil
	clearSpecPresence(canonical.Runner)
	clearOverridePresence(canonical.Linux)
	clearOverridePresence(canonical.MacOS)
	clearOverridePresence(canonical.Windows)
	return canonical
}

// Equal compares command behavior rather than YAML spelling or presence
// metadata, so a legacy scalar and an equivalent mapping are equal.
func (c Command) Equal(other Command) bool {
	return c.Run == other.Run && specsEqual(c.Runner, other.Runner) &&
		overridesEqual(c.Linux, other.Linux) && overridesEqual(c.MacOS, other.MacOS) && overridesEqual(c.Windows, other.Windows)
}

// IsZero reports whether a command has no script, runner, or platform fields.
func (c Command) IsZero() bool {
	return c.Run == "" && c.Runner == nil && c.Linux == nil && c.MacOS == nil && c.Windows == nil
}

// ValidateRunners checks every configured runner, including inactive platform
// overrides, before the command enters a persisted policy snapshot.
func (c Command) ValidateRunners() error {
	checks := []struct {
		name string
		spec *Spec
	}{
		{name: "inline", spec: c.Runner},
		{name: "linux", spec: overrideRunner(c.Linux)},
		{name: "macos", spec: overrideRunner(c.MacOS)},
		{name: "windows", spec: overrideRunner(c.Windows)},
	}
	for _, check := range checks {
		if check.spec == nil {
			continue
		}
		if err := ValidateSpec(*check.spec); err != nil {
			return fmt.Errorf("%s runner: %w", check.name, err)
		}
	}
	return nil
}

// Overlay applies a parsed override without losing inherited nested fields.
// Scalar values replace the whole command; mapping values merge only fields
// explicitly present in YAML.
func (c Command) Overlay(override Command) Command {
	if override.form == yamlFormScalar || override.form == yamlFormUnknown {
		return override.Clone()
	}
	result := c.Clone()
	if override.has("run") {
		result.Run = override.Run
	}
	if override.has("runner") {
		result.Runner = overlaySpec(result.Runner, override.Runner)
	}
	if override.has("linux") {
		result.Linux = overlayOverride(result.Linux, override.Linux)
	}
	if override.has("macos") {
		result.MacOS = overlayOverride(result.MacOS, override.MacOS)
	}
	if override.has("windows") {
		result.Windows = overlayOverride(result.Windows, override.Windows)
	}
	result.form = yamlFormMapping
	result.present = nil
	return result
}

func (c Command) has(field string) bool { return c.present != nil && c.present[field] }

func cloneSpec(spec *Spec) *Spec {
	if spec == nil {
		return nil
	}
	value := spec.Clone()
	cloned := &value
	return cloned
}

func cloneOverride(override *Override) *Override {
	if override == nil {
		return nil
	}
	cloned := &Override{Runner: cloneSpec(override.Runner), form: override.form, present: clonePresence(override.present)}
	if override.Run != nil {
		run := *override.Run
		cloned.Run = &run
	}
	return cloned
}

func clearSpecPresence(spec *Spec) {
	if spec != nil {
		spec.present = nil
	}
}

func clearOverridePresence(override *Override) {
	if override == nil {
		return
	}
	override.form = yamlFormUnknown
	override.present = nil
	clearSpecPresence(override.Runner)
}

func overrideRunner(override *Override) *Spec {
	if override == nil {
		return nil
	}
	return override.Runner
}

func overlaySpec(base, override *Spec) *Spec {
	if override == nil {
		return nil
	}
	if base == nil {
		return cloneSpec(override)
	}
	result := base.Overlay(*override)
	return &result
}

func overlayOverride(base, override *Override) *Override {
	if override == nil {
		return nil
	}
	if override.form == yamlFormScalar || base == nil {
		return cloneOverride(override)
	}
	result := cloneOverride(base)
	if override.present["run"] {
		if override.Run == nil {
			result.Run = nil
		} else {
			run := *override.Run
			result.Run = &run
		}
	}
	if override.present["runner"] {
		result.Runner = overlaySpec(result.Runner, override.Runner)
	}
	result.form = yamlFormMapping
	result.present = nil
	return result
}

func specsEqual(a, b *Spec) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Executable == b.Executable && reflect.DeepEqual(a.Args, b.Args)
}

func overridesEqual(a, b *Override) bool {
	if a == nil || b == nil {
		return a == b
	}
	if (a.Run == nil) != (b.Run == nil) || !specsEqual(a.Runner, b.Runner) {
		return false
	}
	return a.Run == nil || *a.Run == *b.Run
}

type yamlForm uint8

const (
	yamlFormUnknown yamlForm = iota
	yamlFormScalar
	yamlFormMapping
)

func mappingPresence(value *yaml.Node) map[string]bool {
	value = dereferenceYAMLNode(value)
	present := make(map[string]bool)
	if value == nil || value.Kind != yaml.MappingNode {
		return present
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		if value.Content[i].Value == "<<" {
			collectMergedPresence(value.Content[i+1], present)
			continue
		}
		present[value.Content[i].Value] = true
	}
	return present
}

func collectMergedPresence(value *yaml.Node, present map[string]bool) {
	value = dereferenceYAMLNode(value)
	if value == nil {
		return
	}
	if value.Kind == yaml.SequenceNode {
		for _, item := range value.Content {
			for field := range mappingPresence(item) {
				present[field] = true
			}
		}
		return
	}
	for field := range mappingPresence(value) {
		present[field] = true
	}
}

func clonePresence(present map[string]bool) map[string]bool {
	if present == nil {
		return nil
	}
	cloned := make(map[string]bool, len(present))
	for field, value := range present {
		cloned[field] = value
	}
	return cloned
}

func rejectUnknownMappingFields(value *yaml.Node, label string, allowed ...string) error {
	value = dereferenceYAMLNode(value)
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("%s must be a mapping", label)
	}
	known := make(map[string]bool, len(allowed))
	for _, field := range allowed {
		known[field] = true
	}
	for i := 0; i+1 < len(value.Content); i += 2 {
		field := value.Content[i].Value
		if field == "<<" {
			if err := rejectUnknownMergedFields(value.Content[i+1], label, allowed...); err != nil {
				return err
			}
			continue
		}
		if !known[field] {
			return fmt.Errorf("field %s not found in type %s", field, label)
		}
	}
	return nil
}

func rejectUnknownMergedFields(value *yaml.Node, label string, allowed ...string) error {
	value = dereferenceYAMLNode(value)
	if value.Kind == yaml.SequenceNode {
		for _, item := range value.Content {
			if err := rejectUnknownMappingFields(item, label, allowed...); err != nil {
				return err
			}
		}
		return nil
	}
	return rejectUnknownMappingFields(value, label, allowed...)
}

func dereferenceYAMLNode(value *yaml.Node) *yaml.Node {
	for value != nil && (value.Kind == yaml.DocumentNode || value.Kind == yaml.AliasNode) {
		if value.Kind == yaml.DocumentNode {
			if len(value.Content) == 0 {
				return value
			}
			value = value.Content[0]
			continue
		}
		value = value.Alias
	}
	return value
}
