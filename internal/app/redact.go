package app

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/capitanbanana/zbox/internal/redact"
)

// rulesFileName is the per-project rules file rdx loads from the project root.
const rulesFileName = ".redaction_rules"

// redactCommand implements `zbox redact …`.
func (a *App) redactCommand(args []string) error {
	cmd := "show"
	if len(args) > 0 {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "audit":
		return a.redactAudit(args)
	case "apply":
		return a.redactApply(args)
	case "rules":
		return a.redactRules(args)
	case "discover":
		return a.redactDiscover(args)
	case "show", "list":
		c, err := a.load()
		if err != nil {
			return err
		}
		printRedaction(c)
		return nil
	default:
		return fmt.Errorf("unknown redact command %q; run zbox help", cmd)
	}
}

// redactAudit prints the portable audit package: instructions plus harvested
// host facts and project documents, ready to paste into any LLM.
func (a *App) redactAudit(args []string) error {
	out := os.Stdout
	if len(args) == 2 && args[0] == "--out" {
		f, err := os.OpenFile(args[1], os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		defer f.Close()
		out = f
	} else if len(args) != 0 {
		return errors.New("usage: zbox redact audit [--out FILE]")
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	legend, err := a.legend(c)
	if err != nil {
		return err
	}
	roots := []string{}
	for _, m := range c.Mounts {
		roots = append(roots, m.Host)
	}
	report := redact.Harvest(roots)
	projects := make([]redact.Project, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		projects = append(projects, redact.Project{Host: m.Host, Guest: m.Guest, Name: filepath.Base(m.Guest)})
	}
	pkg := redact.AuditPackage(legend, report, projects)
	if _, err := io.WriteString(out, pkg); err != nil {
		return err
	}
	if out != os.Stdout {
		fmt.Printf("Audit package written to %s (%d bytes)\n", args[1], len(pkg))
	} else {
		fmt.Fprintf(os.Stderr, "\nPaste everything above into an LLM, then run:\n  zbox redact apply --file findings.json\n")
	}
	return nil
}

// redactApply merges an LLM reply (or hand-written JSON) with harvest output,
// shows the resulting table, and stores it in the VM configuration.
func (a *App) redactApply(args []string) error {
	file, yes := "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--file", "-f":
			if i+1 >= len(args) {
				return errors.New("--file needs a path")
			}
			i++
			file = args[i]
		case "--yes", "-y":
			yes = true
		default:
			return fmt.Errorf("unknown redact apply argument %q", args[i])
		}
	}
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		b, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	findings, err := redact.ParseCandidates(b)
	if err != nil {
		return err
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	legend, err := a.legend(c)
	if err != nil {
		return err
	}
	seed, err := personaSeed(c)
	if err != nil {
		return err
	}
	roots := []string{}
	for _, m := range c.Mounts {
		roots = append(roots, m.Host)
	}
	report := redact.Harvest(roots)
	rules := redact.MergeCandidates(legend, seed, report, findings)
	fmt.Printf("%d findings from the LLM, %d rules after merge with the harvest:\n\n", len(findings), len(rules))
	printRuleTable(rules)
	if !yes {
		fmt.Print("\nWrite these rules into the VM configuration? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
			return errors.New("aborted, nothing saved")
		}
	}
	c.Redact = &redact.Config{Updated: time.Now().UTC().Format(time.RFC3339), Rules: rules}
	if err := a.save(c); err != nil {
		return err
	}
	if err := writeRules(c); err != nil {
		return err
	}
	fmt.Printf("\nSaved %d rules; %s regenerated for %d mount(s).\n", len(rules), rulesFileName, len(c.Mounts))
	return nil
}

// discoverScript asks the installed rdx tool to scan one project directory and
// prints its findings as JSON. Discovery stays inside the VM.
const discoverScript = `import json, sys
from dataclasses import asdict
from pathlib import Path
from rdx.discover import discover
report = discover(Path(sys.argv[1]), quiet=True)
print(json.dumps([asdict(f) for f in report.findings]))`

// discoverMount runs `rdx discover` inside the VM for one mount and returns
// values that the approved rules do not cover yet.
func (a *App) discoverMount(c Config, m Mount, approved []redact.Candidate) ([]redact.Candidate, error) {
	tool := c.Home() + "/.local/share/uv/tools/claude-code-redact/bin/python"
	cmd := a.ssh(false, "cd "+quote(m.Guest)+" && "+quote(tool)+" -c "+quote(discoverScript)+" "+quote(m.Guest))
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("rdx discover in %s: %w", m.Guest, err)
	}
	found, err := redact.ParseDiscover(out)
	if err != nil {
		return nil, err
	}
	var uncovered []redact.Candidate
	for _, f := range found {
		if !redact.CoveredBy(approved, f.Value) && !redact.CoveredBy(uncovered, f.Value) {
			uncovered = append(uncovered, f)
		}
	}
	return uncovered, nil
}

// warnNewIdentity scans a freshly added mount while the VM is running. It never
// fails the command: discovery is advice, and an uncovered value needs a human
// decision about its replacement.
func (a *App) warnNewIdentity(guestDir string) {
	c, err := a.load()
	if err != nil || c.Persona == nil {
		return
	}
	if state, err := a.state(); err != nil || state != "Running" {
		return
	}
	var approved []redact.Candidate
	if c.Redact != nil {
		approved = c.Redact.Rules
	}
	for _, m := range c.Mounts {
		if m.Guest != guestDir {
			continue
		}
		found, err := a.discoverMount(c, m, approved)
		if err != nil || len(found) == 0 {
			return
		}
		fmt.Printf("\n%d value(s) in this folder are not covered by the redaction rules:\n\n", len(found))
		printRuleTable(found)
		fmt.Println("\nReview them with: zbox redact discover")
		fmt.Println("Accept them with:  zbox redact discover --add")
	}
}

// redactDiscover runs `rdx discover` inside the VM for every mount and reports
// values that no rule covers yet. It never talks to a model provider.
func (a *App) redactDiscover(args []string) error {
	add := false
	for _, arg := range args {
		switch arg {
		case "--add":
			add = true
		default:
			return fmt.Errorf("unknown redact discover argument %q", arg)
		}
	}
	if err := a.start(); err != nil {
		return err
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	var approved []redact.Candidate
	if c.Redact != nil {
		approved = c.Redact.Rules
	}
	var uncovered []redact.Candidate
	for _, m := range c.Mounts {
		found, err := a.discoverMount(c, m, approved)
		if err != nil {
			return err
		}
		for _, f := range found {
			if !redact.CoveredBy(uncovered, f.Value) {
				uncovered = append(uncovered, f)
			}
		}
	}
	if len(uncovered) == 0 {
		fmt.Println("Discovery found nothing outside the current rules.")
		return nil
	}
	fmt.Printf("%d discovered value(s) not covered by the current rules:\n\n", len(uncovered))
	printRuleTable(uncovered)
	if !add {
		fmt.Println("\nReview them, then add with: zbox redact discover --add")
		return nil
	}
	legend, err := a.legend(c)
	if err != nil {
		return err
	}
	seed, err := personaSeed(c)
	if err != nil {
		return err
	}
	var empty redact.Report
	c.Redact = &redact.Config{
		Updated: time.Now().UTC().Format(time.RFC3339),
		Rules:   redact.MergeCandidates(legend, seed, empty, append(approved, uncovered...)),
	}
	if err := a.save(c); err != nil {
		return err
	}
	if err := writeRules(c); err != nil {
		return err
	}
	fmt.Printf("Added %d rule(s); %s regenerated.\n", len(uncovered), rulesFileName)
	return nil
}

// redactRules compares `rdx discover` findings with the approved rules and
// prints only the values that are not covered yet. It never writes rules on
// its own: an uncovered value needs a human decision about its replacement.
func (a *App) redactRules(args []string) error {
	file, add := "", false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--new", "-n":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				file = args[i]
			}
		case "--add":
			add = true
		default:
			return fmt.Errorf("unknown redact rules argument %q", args[i])
		}
	}
	if file == "" && !add {
		return errors.New("usage: zbox redact rules --new FINDINGS.json [--add]")
	}
	var b []byte
	var err error
	if file != "" {
		b, err = os.ReadFile(file)
	} else {
		b, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		return err
	}
	findings, err := redact.ParseCandidates(b)
	if err != nil {
		return err
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	var approved []redact.Candidate
	if c.Redact != nil {
		approved = c.Redact.Rules
	}
	var uncovered []redact.Candidate
	for _, f := range findings {
		if f.Hash != "" {
			if !redact.CoveredByHash(approved, f.Hash) {
				uncovered = append(uncovered, f)
			}
			continue
		}
		if !redact.CoveredBy(approved, f.Value) {
			uncovered = append(uncovered, f)
		}
	}
	if len(uncovered) == 0 {
		fmt.Println("No new findings: every discovered value is already covered by the rules.")
		return nil
	}
	fmt.Printf("%d new finding(s) not covered by the current rules:\n\n", len(uncovered))
	printRuleTable(uncovered)
	if !add {
		fmt.Println("\nAdd them with: zbox redact rules --new FINDINGS.json --add")
		return nil
	}
	legend, err := a.legend(c)
	if err != nil {
		return err
	}
	seed, err := personaSeed(c)
	if err != nil {
		return err
	}
	var empty redact.Report
	c.Redact = &redact.Config{
		Updated: time.Now().UTC().Format(time.RFC3339),
		Rules:   redact.MergeCandidates(legend, seed, empty, append(approved, uncovered...)),
	}
	if err := a.save(c); err != nil {
		return err
	}
	if err := writeRules(c); err != nil {
		return err
	}
	fmt.Printf("Added %d rule(s); %s regenerated.\n", len(uncovered), rulesFileName)
	return nil
}

// legend returns the VM legend, failing when the VM predates it.
func (a *App) legend(c Config) (redact.Legend, error) {
	if c.Persona == nil {
		return redact.Legend{}, fmt.Errorf("VM %q has no legend; run zbox create to make a new VM", a.Name)
	}
	return *c.Persona, nil
}

func personaSeed(c Config) ([]byte, error) {
	if c.PersonaSeed == "" {
		return nil, errors.New("VM has no persona seed")
	}
	seed, err := hex.DecodeString(c.PersonaSeed)
	if err != nil || len(seed) == 0 {
		return nil, errors.New("VM persona seed is corrupt")
	}
	return seed, nil
}

// writeRules regenerates `.redaction_rules` inside each mounted project, where
// rdx looks for it, and keeps the file out of git in the host repository.
func writeRules(c Config) error {
	if c.Redact == nil {
		return nil
	}
	data, err := redact.RulesFile(*c.Redact)
	if err != nil {
		return err
	}
	for _, m := range c.Mounts {
		path := filepath.Join(m.Host, rulesFileName)
		if err := redact.WriteRulesFile(path, data); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		excludeRulesFile(m.Host)
	}
	return nil
}

// excludeRulesFile adds the rules file to .git/info/exclude, which is local to
// the clone and never shows up as a tracked change.
func excludeRulesFile(repo string) {
	gitDir := filepath.Join(repo, ".git")
	if info, err := os.Stat(gitDir); err != nil || !info.IsDir() {
		return
	}
	path := filepath.Join(gitDir, "info", "exclude")
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), rulesFileName) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	if len(b) > 0 && !strings.HasSuffix(string(b), "\n") {
		fmt.Fprintln(f)
	}
	fmt.Fprintf(f, "# zbox: rdx rules are generated per VM\n/%s\n", rulesFileName)
}

func printRedaction(c Config) {
	if c.Persona == nil {
		fmt.Println("Legend: not configured (VM created before personas existed)")
	} else {
		p := c.Persona
		fmt.Printf("Legend: %s <%s> as %s on %s\n", p.FullName, p.Email, p.Username, p.Hostname)
	}
	if c.Redact == nil || len(c.Redact.Rules) == 0 {
		fmt.Println("Redaction rules: none (run zbox redact audit)")
		return
	}
	fmt.Printf("Redaction rules: %d (updated %s)\n\n", len(c.Redact.Rules), c.Redact.Updated)
	printRuleTable(c.Redact.Rules)
}

// truncateRunes shortens s to limit runes, so a multi-byte value never turns
// into invalid UTF-8 on the way to a terminal.
func truncateRunes(s string, limit int) string {
	if utf8.RuneCountInString(s) <= limit {
		return s
	}
	runes := []rune(s)
	return string(runes[:limit-1]) + "…"
}

func printRuleTable(rules []redact.Candidate) {
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	fmt.Fprintf(w, "%-8s %-32s %-28s %-10s %s\n", "CATEGORY", "VALUE", "REPLACEMENT", "SOURCE", "CONTEXT")
	for _, r := range rules {
		value := truncateRunes(r.Mask(), 30)
		repl := r.Replacement
		if repl == "" {
			repl = "(auto-token)"
		}
		repl = truncateRunes(repl, 26)
		fmt.Fprintf(w, "%-8s %-32s %-28s %-10s %s\n", r.Category, value, repl, r.Source, r.Context)
	}
}
