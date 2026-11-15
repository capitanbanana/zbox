package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Identity is a person name/email pair found on the host, with its provenance.
type Identity struct {
	Name   string
	Email  string
	Source string
}

// Secret is a suspected credential. Only a masked preview is kept in memory;
// rules for it are written as a SHA-256 hash, never as plaintext.
type Secret struct {
	Path  string
	Name  string
	Value string
}

// Report is the machine-fact harvest for one VM configuration.
type Report struct {
	User          string
	Home          string
	Hostname      string
	Paths         []string
	Identities    []Identity
	Remotes       []string
	InternalHosts []string
	Secrets       []Secret
}

// ScanLimits bounds filesystem work so `zbox redact audit` stays cheap.
type ScanLimits struct {
	MaxFiles    int
	MaxFileSize int64
}

// DefaultLimits is the production scan budget per mounted repository.
var DefaultLimits = ScanLimits{MaxFiles: 4000, MaxFileSize: 512 << 10}

var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, "node_modules": true,
	"vendor": true, "dist": true, "build": true, ".venv": true, "venv": true,
	"__pycache__": true, ".next": true, "target": true, ".cache": true,
	".zbox": true, ".agents": true,
}

var (
	internalHostRE = regexp.MustCompile(`(?i)\b[a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)*\.(?:local|internal|lan|corp|intra)\b`)
	secretAssignRE = regexp.MustCompile(`(?im)^\s*(?:export\s+)?([A-Za-z0-9_.-]*(?:KEY|TOKEN|SECRET|PASSWORD|PASSWD|CREDENTIAL|CREDS|AUTH)[A-Za-z0-9_.-]*)\s*[:=]\s*["']?([A-Za-z0-9+/_.:@=-]{16,})`)
	secretFileRE   = regexp.MustCompile(`(?i)(?:\.env(?:\.[\w.-]+)?$|\.pem$|\.key$|\.p12$|\.pfx$|(?:^|/)credentials(?:\.json)?$|(?:^|/)id_(?:rsa|ed25519|ecdsa)$|(?:^|/)\.netrc$)`)
	// secretValueRE is the hash extractor shared by generated secret rules. It
	// is intentionally broad: hashed matching only fires on an exact digest.
	secretValueRE = `[A-Za-z0-9+/_.:@=-]{16,}`
)

// Harvest collects machine facts for the given host repository paths.
func Harvest(roots []string) Report {
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	r := Report{
		User:     os.Getenv("USER"),
		Home:     home,
		Hostname: host,
	}
	if host != "" {
		r.Hostname = host
	}
	if home != "" {
		r.Paths = append(r.Paths, home)
	}
	seenHost := map[string]bool{}
	seenIdent := map[string]bool{}
	seenSecret := map[string]bool{}
	addIdent := func(name, email, source string) {
		name, email = strings.TrimSpace(name), strings.TrimSpace(email)
		if name == "" && email == "" {
			return
		}
		key := name + "\x00" + email
		if seenIdent[key] {
			return
		}
		seenIdent[key] = true
		r.Identities = append(r.Identities, Identity{Name: name, Email: email, Source: source})
	}
	addHost := func(value string) {
		value = strings.TrimSuffix(strings.ToLower(value), ".")
		if value == "" || seenHost[value] {
			return
		}
		seenHost[value] = true
		r.InternalHosts = append(r.InternalHosts, value)
	}
	addSecret := func(path, name, value string) {
		if value == "" && path == "" {
			return
		}
		key := path + "\x00" + name + "\x00" + value
		if seenSecret[key] {
			return
		}
		seenSecret[key] = true
		r.Secrets = append(r.Secrets, Secret{Path: path, Name: name, Value: value})
	}

	if host != "" {
		addHost(host)
	}
	seenRemotes := map[string]bool{}
	for _, root := range roots {
		for _, line := range gitLines(root, "config", "--local", "--get", "user.name") {
			addIdent(line, "", "git-config:"+root)
		}
		for _, line := range gitLines(root, "config", "--local", "--get", "user.email") {
			addIdent("", line, "git-config:"+root)
		}
		for _, line := range gitLines(root, "log", "-n", "300", "--format=%an%x00%ae") {
			parts := strings.SplitN(line, "\x00", 2)
			if len(parts) == 2 {
				addIdent(parts[0], parts[1], "git-log:"+root)
			}
		}
		for _, line := range gitLines(root, "remote", "-v") {
			fields := strings.Fields(line)
			if len(fields) < 2 || seenRemotes[fields[1]] {
				continue
			}
			seenRemotes[fields[1]] = true
			r.Remotes = append(r.Remotes, fields[1])
		}
		scanTree(root, DefaultLimits, addHost, addSecret)
	}
	sort.Strings(r.InternalHosts)
	sort.Strings(r.Remotes)
	sort.Slice(r.Identities, func(i, j int) bool {
		if r.Identities[i].Source != r.Identities[j].Source {
			return r.Identities[i].Source < r.Identities[j].Source
		}
		return r.Identities[i].Name < r.Identities[j].Name
	})
	sort.Slice(r.Secrets, func(i, j int) bool { return r.Secrets[i].Path < r.Secrets[j].Path })
	return r
}

// git runs a git subcommand in a repository and returns non-empty stdout lines.
func gitLines(root string, args ...string) []string {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var lines []string
	for line := range strings.SplitSeq(string(out), "\n") {
		if line = strings.TrimRight(line, "\r"); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// scanTree walks a repository, reporting internal hostnames and suspected
// secrets. It is bounded: skipped directories, size and file-count limits.
func scanTree(root string, limits ScanLimits, addHost func(string), addSecret func(path, name, value string)) {
	budget := limits.MaxFiles
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if budget <= 0 {
			return filepath.SkipAll
		}
		budget--
		info, err := d.Info()
		if err != nil || info.Size() > limits.MaxFileSize || info.Size() == 0 {
			return nil
		}
		name := d.Name()
		isSecretFile := secretFileRE.MatchString(name)
		if !isSecretFile && !hasTextExtension(name) {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || looksBinary(data) {
			return nil
		}
		text := string(data)
		for _, m := range internalHostRE.FindAllString(text, -1) {
			addHost(m)
		}
		if !isSecretFile {
			return nil
		}
		found := false
		for _, m := range secretAssignRE.FindAllStringSubmatch(text, -1) {
			addSecret(p, m[1], m[2])
			found = true
		}
		if !found {
			// Key material and credential dumps rarely carry a KEY= label.
			addSecret(p, "key-material", "")
		}
		return nil
	})
}

// hasTextExtension keeps the scan limited to files that can carry identifiers.
func hasTextExtension(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".txt", ".yaml", ".yml", ".json", ".toml", ".ini", ".conf",
		".sh", ".bash", ".zsh", ".go", ".py", ".js", ".ts", ".rb", ".rs",
		".java", ".c", ".h", ".env", ".example", ".cfg", ".properties":
		return true
	}
	// Extensionless build files and env files.
	switch strings.ToLower(name) {
	case "makefile", "dockerfile", ".env", ".gitconfig", ".npmrc", ".netrc":
		return true
	}
	return false
}

func looksBinary(data []byte) bool {
	limit := min(len(data), 800)
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

// sha256Hex hashes a secret for a hashed rdx rule.
func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
