package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/capitanbanana/zbox/internal/redact"
)

// legendApp returns a test app whose VM already has a legend and one exported
// project, so rule generation can be exercised without a VM.
func legendApp(t *testing.T) (*App, string) {
	t.Helper()
	a := testApp(t)
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	c.Persona = &redact.Legend{
		Username:    "ivy.crane",
		FullName:    "Ivy Crane",
		Email:       "ivy.crane@quietpost.example",
		GitHub:      "ivy-crane",
		Hostname:    "quietpost",
		Domain:      "quietpost.example",
		TimeZone:    "Europe/Berlin",
		MachineID:   "0123456789abcdef0123456789abcdef",
		UserID:      "0123456789abcdef0123456789abcdef",
		NumStartups: 1,
	}
	c.PersonaSeed = "9f2c1d4b6a8e0f3c5d7b9a1e2f4c6d8b"
	c.Mounts = []Mount{{Host: project, Guest: "/home/ivy.crane/proj"}}
	if err = a.save(c); err != nil {
		t.Fatal(err)
	}
	return a, project
}

func writeFindings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "findings.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Without --add, discovery only reports: nothing is stored and no rules file is
// written, because an uncovered value needs a human decision.
func TestRedactRulesWithoutAddWritesNothing(t *testing.T) {
	a, project := legendApp(t)
	findings := writeFindings(t, `[{"value":"Dana Whitlock","category":"NAME"}]`)

	if err := a.redactRules([]string{"--new", findings}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, rulesFileName)); !os.IsNotExist(err) {
		t.Fatalf("%s written without --add: %v", rulesFileName, err)
	}
	c, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Redact != nil && len(c.Redact.Rules) != 0 {
		t.Fatalf("rules stored without --add: %+v", c.Redact.Rules)
	}
}

// --add is the auto-add path: it stores the discovered values, replaces them
// with legend values, and regenerates the per-project rdx rules file.
func TestRedactRulesAddStoresLegendRules(t *testing.T) {
	a, project := legendApp(t)
	findings := writeFindings(t, `[
		{"value":"Dana Whitlock","category":"NAME","context":"src/notes.md:4"},
		{"value":"dana@corp.example","category":"EMAIL"}]`)

	if err := a.redactRules([]string{"--new", findings, "--add"}); err != nil {
		t.Fatal(err)
	}
	c, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Redact == nil || len(c.Redact.Rules) != 2 {
		t.Fatalf("rules = %+v, want the two discoveries", c.Redact)
	}
	for _, r := range c.Redact.Rules {
		if strings.EqualFold(r.Replacement, r.Value) {
			t.Errorf("rule %q keeps the original value", r.Value)
		}
		if r.Category == "NAME" && r.Replacement != "Ivy Crane" {
			t.Errorf("NAME replacement = %q, want the legend name", r.Replacement)
		}
		if r.Category == "EMAIL" && r.Replacement != "ivy.crane@quietpost.example" {
			t.Errorf("EMAIL replacement = %q, want the legend address", r.Replacement)
		}
	}
	b, err := os.ReadFile(filepath.Join(project, rulesFileName))
	if err != nil {
		t.Fatal(err)
	}
	rules := string(b)
	for _, want := range []string{"pattern:", "replacement: Ivy Crane", "ivy.crane@quietpost.example"} {
		if !strings.Contains(rules, want) {
			t.Errorf("%s missing %q:\n%s", rulesFileName, want, rules)
		}
	}
	exclude, err := os.ReadFile(filepath.Join(project, ".git", "info", "exclude"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(exclude), "/"+rulesFileName) {
		t.Errorf("git exclude missing the rules file: %q", exclude)
	}
}

// A second --add over already covered values must not duplicate rules, which
// keeps prompt prefixes stable across runs.
func TestRedactRulesAddIsIdempotent(t *testing.T) {
	a, _ := legendApp(t)
	first := writeFindings(t, `[{"value":"Dana Whitlock","category":"NAME"}]`)
	if err := a.redactRules([]string{"--new", first, "--add"}); err != nil {
		t.Fatal(err)
	}
	second := writeFindings(t, `[{"value":"Dana Whitlock","category":"NAME"}]`)
	if err := a.redactRules([]string{"--new", second, "--add"}); err != nil {
		t.Fatal(err)
	}
	c, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Redact.Rules) != 1 {
		t.Fatalf("rules = %+v, want a single rule after re-adding", c.Redact.Rules)
	}
}

// A suspected credential is matched by hash, so --add can accept a finding that
// carries only a digest and must never write the value.
func TestRedactRulesAddKeepsSecretsHashed(t *testing.T) {
	a, project := legendApp(t)
	findings := writeFindings(t, `[{"hash":"e3b0c44298fc1c149afbf4c8996fb924","extractor":"generic-api-key","category":"KEY"}]`)

	if err := a.redactRules([]string{"--new", findings, "--add"}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(project, rulesFileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"hashed: true", "pattern: e3b0c44298fc1c149afbf4c8996fb924", "hash_extractor: generic-api-key"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("%s missing %q:\n%s", rulesFileName, want, b)
		}
	}
	// A second add of the same digest must not duplicate the rule.
	if err := a.redactRules([]string{"--new", findings, "--add"}); err != nil {
		t.Fatal(err)
	}
	c, err := a.load()
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Redact.Rules) != 1 {
		t.Fatalf("rules = %+v, want a single hashed rule", c.Redact.Rules)
	}
}
