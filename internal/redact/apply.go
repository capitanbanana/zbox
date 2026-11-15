package redact

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Candidate is one redaction rule candidate. Secret candidates keep only a
// SHA-256 hash of the value: the plaintext never reaches disk.
type Candidate struct {
	Value       string `json:"value,omitempty"`
	Category    string `json:"category"`
	Replacement string `json:"replacement,omitempty"`
	Context     string `json:"context,omitempty"`
	Source      string `json:"source,omitempty"`
	Hash        string `json:"hash,omitempty"`
	Extractor   string `json:"extractor,omitempty"`
}

// Config is the persisted redaction configuration of one VM.
type Config struct {
	Updated string      `json:"updated,omitempty"`
	Rules   []Candidate `json:"rules"`
}

var validCategories = map[string]bool{
	"NAME": true, "EMAIL": true, "KEY": true, "IP": true, "HOST": true,
	"PROJECT": true, "PATH": true, "CUSTOM": true,
}

// ParseCandidates accepts an LLM reply or a hand-written JSON array. Anything
// that is not a JSON array is rejected so a prose preamble cannot be mistaken
// for findings.
func ParseCandidates(b []byte) ([]Candidate, error) {
	trimmed := strings.TrimSpace(string(b))
	start := strings.Index(trimmed, "[")
	end := strings.LastIndex(trimmed, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("no JSON array found in input")
	}
	var raw []Candidate
	if err := json.Unmarshal([]byte(trimmed[start:end+1]), &raw); err != nil {
		return nil, fmt.Errorf("parse findings: %w", err)
	}
	out := make([]Candidate, 0, len(raw))
	for _, c := range raw {
		c.Value = strings.TrimSpace(c.Value)
		c.Category = strings.ToUpper(strings.TrimSpace(c.Category))
		if c.Value == "" && c.Hash == "" {
			continue
		}
		if !validCategories[c.Category] {
			c.Category = "CUSTOM"
		}
		c.Source = "llm"
		out = append(out, c)
	}
	return out, nil
}

// MergeCandidates combines harvested facts with LLM findings, dropping
// duplicates and anything whose replacement equals the value itself.
func MergeCandidates(legend Legend, seed []byte, r Report, findings []Candidate) []Candidate {
	var out []Candidate
	out = append(out, harvestCandidates(legend, seed, r)...)
	out = append(out, findings...)

	seen := map[string]bool{}
	merged := out[:0]
	for _, c := range out {
		if c.Hash != "" {
			key := "h\x00" + c.Hash
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, c)
			continue
		}
		if strings.TrimSpace(c.Value) == "" {
			continue
		}
		key := c.Category + "\x00" + strings.ToLower(c.Value)
		if seen[key] {
			continue
		}
		seen[key] = true
		if c.Replacement == "" && c.Category != "KEY" && c.Category != "IP" && c.Category != "CUSTOM" {
			c.Replacement = legend.Replacement(seed, c.Category, c.Value)
		}
		if strings.EqualFold(c.Replacement, c.Value) {
			continue
		}
		merged = append(merged, c)
	}
	sort.Slice(merged, func(i, j int) bool {
		if merged[i].Category != merged[j].Category {
			return merged[i].Category < merged[j].Category
		}
		return strings.ToLower(merged[i].Value) < strings.ToLower(merged[j].Value)
	})
	return merged
}

// harvestCandidates turns harvested machine facts into rule candidates.
func harvestCandidates(legend Legend, seed []byte, r Report) []Candidate {
	var out []Candidate
	add := func(value, category, source string) {
		value = strings.TrimSpace(value)
		if value == "" || strings.EqualFold(value, legend.Username) {
			return
		}
		out = append(out, Candidate{
			Value:       value,
			Category:    category,
			Replacement: legend.Replacement(seed, category, value),
			Source:      source,
		})
	}
	add(r.Hostname, "HOST", "auto")
	for _, p := range r.Paths {
		add(p, "PATH", "auto")
	}
	for _, id := range r.Identities {
		if id.Name != "" {
			add(id.Name, "NAME", sourceOr(id.Source, "git"))
		}
		if id.Email != "" {
			add(id.Email, "EMAIL", sourceOr(id.Source, "git"))
		}
	}
	for _, h := range r.InternalHosts {
		add(h, "HOST", "auto")
	}
	for _, remote := range r.Remotes {
		add(remoteIdentity(remote), "HOST", "git")
	}
	for _, s := range r.Secrets {
		if s.Value == "" {
			continue
		}
		out = append(out, Candidate{
			Category:  "KEY",
			Context:   s.Path + " (" + s.Name + ")",
			Source:    "secret",
			Hash:      sha256Hex(s.Value),
			Extractor: secretValueRE,
		})
	}
	return out
}

func sourceOr(source, fallback string) string {
	if source == "" {
		return fallback
	}
	return source
}

// remoteIdentity extracts the "host:owner" identity out of a git remote URL,
// which is the part that identifies the account rather than the project.
func remoteIdentity(remote string) string {
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	remote = strings.TrimPrefix(remote, "git@")
	remote = strings.TrimPrefix(remote, "ssh://git@")
	remote = strings.TrimPrefix(remote, "https://")
	remote = strings.TrimPrefix(remote, "http://")
	parts := strings.SplitN(remote, ":", 2)
	if len(parts) != 2 {
		parts = strings.SplitN(remote, "/", 2)
	}
	if len(parts) != 2 {
		return ""
	}
	owner := strings.Trim(strings.SplitN(parts[1], "/", 2)[0], " ")
	host := strings.ToLower(parts[0])
	switch host {
	case "github.com", "gitlab.com", "bitbucket.org", "codeberg.org":
		if owner == "" {
			return ""
		}
		return host + ":" + owner
	}
	return ""
}

// Mask renders a candidate for terminal display without revealing secrets.
func (c Candidate) Mask() string {
	if c.Hash != "" {
		return "sha256:" + c.Hash[:8] + "…"
	}
	return c.Value
}

// CoveredBy reports whether a discovered finding value is already handled.
func CoveredBy(rules []Candidate, value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return true
	}
	hash := sha256Hex(value)
	for _, r := range rules {
		if r.Hash != "" && r.Hash == hash {
			return true
		}
		if r.Value == "" {
			continue
		}
		if strings.EqualFold(r.Value, value) {
			return true
		}
		if len(r.Value) > 3 && strings.Contains(strings.ToLower(value), strings.ToLower(r.Value)) {
			return true
		}
	}
	return false
}

// discovery is one `rdx discover` finding as emitted by the guest helper.
type discovery struct {
	File     string  `json:"file"`
	Line     int     `json:"line"`
	Value    string  `json:"value"`
	Category string  `json:"category"`
	Method   string  `json:"method"`
	Score    float64 `json:"score"`
}

// ParseDiscover converts `rdx discover` JSON into rule candidates. The values
// stay local: discovery output is never sent to a model provider.
func ParseDiscover(b []byte) ([]Candidate, error) {
	var raw []discovery
	if err := json.Unmarshal(bytesTrimJSON(b), &raw); err != nil {
		return nil, fmt.Errorf("parse discovery output: %w", err)
	}
	out := make([]Candidate, 0, len(raw))
	for _, f := range raw {
		value := strings.TrimSpace(f.Value)
		if value == "" {
			continue
		}
		category := strings.ToUpper(f.Category)
		if !validCategories[category] {
			category = "CUSTOM"
		}
		out = append(out, Candidate{
			Value:    value,
			Category: category,
			Context:  fmt.Sprintf("%s:%d (%s)", f.File, f.Line, f.Method),
			Source:   "discover",
		})
	}
	return out, nil
}

// bytesTrimJSON strips any leading text before the JSON array.
func bytesTrimJSON(b []byte) []byte {
	s := strings.TrimSpace(string(b))
	if i := strings.Index(s, "["); i > 0 {
		s = s[i:]
	}
	if j := strings.LastIndex(s, "]"); j >= 0 {
		s = s[:j+1]
	}
	return []byte(s)
}
