package redact

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func testLegend(t *testing.T) (Legend, []byte) {
	t.Helper()
	seed := []byte("0123456789abcdef0123456789abcdef")
	return NewLegend(seed, time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)), seed
}

func TestLegendIsDeterministicAndNeutral(t *testing.T) {
	legend, seed := testLegend(t)
	again := NewLegend(seed, time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC))
	if legend != again {
		t.Fatalf("legend differs across runs:\n%+v\n%+v", legend, again)
	}
	if legend.Username == "" || legend.Hostname == "" || legend.Email == "" {
		t.Fatalf("incomplete legend: %+v", legend)
	}
	if legend.Home() != "/home/"+legend.Username {
		t.Fatalf("home = %q", legend.Home())
	}
	if legend.NumStartups < 5 || legend.NumStartups > 20 {
		t.Fatalf("numStartups = %d", legend.NumStartups)
	}
	other := NewLegend([]byte("another-seed-of-32-bytes-long-xx"), time.Now())
	if other.Username == legend.Username && other.Hostname == legend.Hostname {
		t.Fatal("different seeds produced the same legend")
	}
}

func TestClaudeJSONShape(t *testing.T) {
	legend, _ := testLegend(t)
	var doc map[string]any
	if err := json.Unmarshal(legend.ClaudeJSON(), &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"numStartups", "firstStartTime", "machineID", "userID", "anonymousId"} {
		if _, ok := doc[key]; !ok {
			t.Fatalf("missing %s in %s", key, legend.ClaudeJSON())
		}
	}
	if account, ok := doc["oauthAccount"].(map[string]any); !ok || len(account) != 0 {
		t.Fatalf("oauthAccount must be empty, got %v", doc["oauthAccount"])
	}
	if !strings.HasPrefix(doc["anonymousId"].(string), "claudecode.v1.") {
		t.Fatalf("anonymousId = %v", doc["anonymousId"])
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHarvestFindsHostsAndSecrets(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "README.md"), "deploy to build-01.corp and notes\n")
	writeFile(t, filepath.Join(root, ".env"), "PROBE_API_KEY=sk-live-0123456789abcdef\n")
	writeFile(t, filepath.Join(root, "prod.env"), "DEPLOY_TOKEN=sk-live-fedcba9876543210\n")
	writeFile(t, filepath.Join(root, ".env.example"), "PROBE_API_KEY=changeme\n")
	writeFile(t, filepath.Join(root, "node_modules", ".env"), "PROBE_API_KEY=sk-ignored-0123456789abcdef\n")

	legend, seed := testLegend(t)
	report := Harvest([]string{root})
	if len(report.InternalHosts) == 0 {
		t.Fatal("no internal hosts found")
	}
	found := false
	for _, h := range report.InternalHosts {
		if h == "build-01.corp" {
			found = true
		}
	}
	if !found {
		t.Fatalf("hosts = %v", report.InternalHosts)
	}
	rules := MergeCandidates(legend, seed, report, nil)
	hashes := map[string]Candidate{}
	for _, rule := range rules {
		if rule.Hash != "" {
			hashes[rule.Hash] = rule
		}
	}
	for _, value := range []string{"sk-live-0123456789abcdef", "sk-live-fedcba9876543210"} {
		rule, ok := hashes[sha256Hex(value)]
		if !ok {
			t.Fatalf("%s was not turned into a hashed rule: %+v", value, rules)
		}
		if rule.Value != "" {
			t.Fatalf("secret %q kept in clear text", value)
		}
		if strings.Contains(rule.Context, "sk-live") {
			t.Fatalf("secret value leaked into the context: %q", rule.Context)
		}
	}
	if _, ok := hashes[sha256Hex("sk-ignored-0123456789abcdef")]; ok {
		t.Fatal("scanned node_modules")
	}
}

func TestAuditPackageWithholdsSecretsAndListsLegend(t *testing.T) {
	legend, _ := testLegend(t)
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "README.md"), "# Project\nmaintainer: dev@example.com\n")
	report := Report{
		User: "dvl", Home: "/Users/dvl", Hostname: "laptop",
		Paths:   []string{"/Users/dvl"},
		Secrets: []Secret{{Path: filepath.Join(root, ".env"), Name: "PROBE_API_KEY", Value: "sk-live-0123456789abcdef"}},
	}
	pkg := AuditPackage(legend, report, []Project{{Host: root, Guest: "/home/x/dev/project", Name: "project"}})
	if !strings.Contains(pkg, "JSON array only") {
		t.Fatal("missing instructions")
	}
	if !strings.Contains(pkg, legend.Username) {
		t.Fatal("legend values missing")
	}
	if !strings.Contains(pkg, "dev@example.com") {
		t.Fatal("project document missing")
	}
	if strings.Contains(pkg, "sk-live-0123456789abcdef") {
		t.Fatal("secret value leaked into the audit package")
	}
	if !strings.Contains(pkg, "values withheld") {
		t.Fatal("secret locations not listed")
	}
}

func TestRulesFileIsRdxCompatibleAndStable(t *testing.T) {
	legend, seed := testLegend(t)
	cfg := Config{Rules: MergeCandidates(legend, seed, Report{
		Hostname: "laptop",
		Paths:    []string{"/Users/dvl"},
		Identities: []Identity{
			{Name: "Dev L", Email: "dev@example.com", Source: "git-config"},
		},
		Remotes: []string{"git@github.com:dvl/project.git"},
	}, nil)}
	first, err := RulesFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := RulesFile(cfg)
	if string(first) != string(second) {
		t.Fatal("rules file is not deterministic")
	}
	var doc struct {
		Rules []map[string]any `yaml:"rules"`
	}
	if err = yaml.Unmarshal(first, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Rules) == 0 {
		t.Fatal("no rules rendered")
	}
	ids := map[string]bool{}
	for _, rule := range doc.Rules {
		id, _ := rule["id"].(string)
		if id == "" || ids[id] {
			t.Fatalf("bad or duplicate id %q", id)
		}
		ids[id] = true
		if category, _ := rule["category"].(string); category == "" {
			t.Fatalf("rule %q has no category", id)
		}
	}
	// GitHub remotes need both ssh and https spellings.
	if !strings.Contains(string(first), `github\.com:dvl`) || !strings.Contains(string(first), `github\.com/dvl`) {
		t.Fatalf("github remote rules missing:\n%s", first)
	}
	if !strings.Contains(string(first), "replacement: ") {
		t.Fatal("no replacement values rendered")
	}
}

func TestSecretRuleIsHashed(t *testing.T) {
	legend, seed := testLegend(t)
	value := "sk-live-0123456789abcdef"
	cfg := Config{Rules: MergeCandidates(legend, seed, Report{
		Secrets: []Secret{{Path: ".env", Name: "PROBE_API_KEY", Value: value}},
	}, nil)}
	out, err := RulesFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), value) {
		t.Fatal("secret plaintext in rules file")
	}
	if !strings.Contains(string(out), "hashed: true") || !strings.Contains(string(out), sha256Hex(value)) {
		t.Fatalf("missing hashed rule:\n%s", out)
	}
	if !strings.Contains(string(out), "hash_extractor: ") {
		t.Fatal("hashed rule without extractor matches only the whole text")
	}
}

func TestParseCandidatesRejectsProse(t *testing.T) {
	if _, err := ParseCandidates([]byte("Sure! Here are the findings:")); err == nil {
		t.Fatal("prose accepted as findings")
	}
	got, err := ParseCandidates([]byte("Intro\n[{\"value\":\"Dev L\",\"category\":\"name\"}]\nThanks"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Category != "NAME" {
		t.Fatalf("got %+v", got)
	}
}

func TestCoveredBy(t *testing.T) {
	rules := []Candidate{{Value: "dev@example.com", Category: "EMAIL"}, {Hash: sha256Hex("s3cret-value-0001"), Category: "KEY"}}
	if !CoveredBy(rules, "dev@example.com") || !CoveredBy(rules, "s3cret-value-0001") {
		t.Fatal("covered value reported as new")
	}
	if CoveredBy(rules, "other@example.com") {
		t.Fatal("unknown value reported as covered")
	}
}

func TestCoveredByHash(t *testing.T) {
	hash := sha256Hex("s3cret-value-0001")
	rules := []Candidate{{Value: "dev@example.com", Category: "EMAIL"}, {Hash: strings.ToUpper(hash), Category: "KEY"}}
	if !CoveredByHash(rules, hash) {
		t.Fatal("known digest reported as new")
	}
	if CoveredByHash(rules, sha256Hex("other-value-0002")) {
		t.Fatal("unknown digest reported as covered")
	}
	if CoveredByHash(rules, "") {
		t.Fatal("empty digest reported as covered")
	}
}

func TestParseDiscover(t *testing.T) {
	out := []byte(`[{"file":"a.py","line":3,"value":"dev@example.com","category":"EMAIL","method":"builtin","score":1.0}]`)
	got, err := ParseDiscover(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Context != "a.py:3 (builtin)" {
		t.Fatalf("got %+v", got)
	}
}
