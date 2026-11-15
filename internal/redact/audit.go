package redact

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Project is one exported repository, viewed from both sides of the VM.
type Project struct {
	Host  string
	Guest string
	Name  string
}

// auditInstruction is the static prompt half of the portable audit package.
// It must be self-contained: the reply is copied back into `zbox redact apply`.
const auditInstruction = `You are auditing a machine-fact package for a disposable development VM.
The VM must present a fictional identity ("legend") to model providers, so
anything tied to the real person, employer, or internal network has to be
replaced before traffic leaves the machine.

Find identity leaks in the material below and reply with a JSON array only,
no prose, no code fences:

[{"value": "<exact string as it appears>", "category": "<NAME|EMAIL|HOST|PATH|PROJECT|KEY|IP|CUSTOM>", "context": "<file:line or short explanation>"}]

Rules:
- "value" must be copied verbatim from the material; never invent, normalize,
  generalize or summarize values, and never output regexes.
- Report personal names, personal e-mail addresses, usernames, personal home
  paths (/Users/<name>, /home/<name>), internal hostnames and intranet domains
  (*.local, *.internal, *.lan, *.corp, custom company domains), employer and
  customer names, and credentials visible in the material.
- Do NOT report the project itself: repository name, product naming, code
  structure, file layout, dependency names, public domains
  (github.com, api.anthropic.com, registry.npmjs.org, pypi.org,
  nodejs.org, openrouter.ai), well-known open-source authors, URLs whose
  host is public documentation, or generic words.
- A value that is already one of the legend values listed under
  "Already neutral" must not be reported.
- Prefer fewer, high-confidence findings over guesses. An empty array is a
  valid answer.

`

const (
	auditFileBudget  = 3000
	auditTotalBudget = 60000
	auditFileLimit   = 40
)

// auditCandidateFiles are the common project documents that carry identity.
var auditCandidateFiles = []string{
	"README.md", "README", "CLAUDE.md", "AGENTS.md", "RDX.md",
	"package.json", "go.mod", "pyproject.toml", "Cargo.toml", "Gemfile",
	"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
	"Dockerfile", "Makefile", ".env.example", ".env.sample", ".env.template",
	"config.example.yml", "config.example.yaml",
}

// AuditPackage renders the portable package: instructions, harvested facts and
// project documents. It is meant for copy/paste into any LLM; secrets are
// listed by location only, never by value.
func AuditPackage(legend Legend, r Report, projects []Project) string {
	var b strings.Builder
	b.WriteString(auditInstruction)
	b.WriteString("\n## Already neutral (the legend)\n")
	for _, line := range LegendFacts(legend) {
		fmt.Fprintf(&b, "- %s\n", line)
	}
	b.WriteString("\n## Machine facts harvested on the host\n")
	fmt.Fprintf(&b, "- host user: %s\n", orUnknown(r.User))
	fmt.Fprintf(&b, "- host home: %s\n", orUnknown(r.Home))
	fmt.Fprintf(&b, "- hostname: %s\n", orUnknown(r.Hostname))
	if len(r.Identities) > 0 {
		fmt.Fprintln(&b, "- git identities:")
		for _, id := range r.Identities {
			fmt.Fprintf(&b, "  - name=%s email=%s (%s)\n", orDash(id.Name), orDash(id.Email), id.Source)
		}
	}
	if len(r.Remotes) > 0 {
		fmt.Fprintln(&b, "- git remotes:")
		for _, remote := range r.Remotes {
			fmt.Fprintf(&b, "  - %s\n", remote)
		}
	}
	if len(r.InternalHosts) > 0 {
		fmt.Fprintln(&b, "- internal hostnames:")
		for _, h := range r.InternalHosts {
			fmt.Fprintf(&b, "  - %s\n", h)
		}
	}
	if len(r.Secrets) > 0 {
		fmt.Fprintln(&b, "- suspected credentials (values withheld):")
		for _, s := range r.Secrets {
			fmt.Fprintf(&b, "  - %s (%s)\n", s.Path, s.Name)
		}
	}
	if len(r.Paths) > 0 {
		fmt.Fprintln(&b, "- host paths:")
		for _, p := range r.Paths {
			fmt.Fprintf(&b, "  - %s\n", p)
		}
	}

	budget := auditTotalBudget
	files := 0
	for _, p := range sortedProjects(projects) {
		if files >= auditFileLimit || budget <= 0 {
			break
		}
		wroteHeader := false
		for _, name := range auditCandidateFiles {
			if files >= auditFileLimit || budget <= 0 {
				break
			}
			path := filepath.Join(p.Host, name)
			info, err := os.Stat(path)
			if err != nil || info.IsDir() || info.Size() > auditFileBudget*4 {
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil || looksBinary(data) {
				continue
			}
			text := strings.TrimSpace(string(data))
			if text == "" {
				continue
			}
			if len(text) > auditFileBudget {
				text = text[:auditFileBudget] + "\n… truncated"
			}
			if !wroteHeader {
				fmt.Fprintf(&b, "\n## Project %s (%s)\n", p.Name, p.Guest)
				wroteHeader = true
			}
			files++
			budget -= len(text)
			fmt.Fprintf(&b, "\n### %s\n```\n%s\n```\n", name, text)
		}
	}
	if files == 0 {
		b.WriteString("\n## Project documents\n(none found in exported repositories)\n")
	}
	return b.String()
}

// LegendFacts lists the legend values the auditor must not report.
func LegendFacts(l Legend) []string {
	var out []string
	for _, f := range []string{l.Username, l.FullName, l.Email, l.GitHub, l.Hostname, l.Domain, l.TimeZone} {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

func sortedProjects(projects []Project) []Project {
	out := append([]Project(nil), projects...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "(unknown)"
	}
	return s
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
