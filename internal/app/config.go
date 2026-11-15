package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

const guestHome = "/home/agent"

var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,40}$`)

type Mount struct {
	Host     string `json:"host"`
	Guest    string `json:"guest"`
	ReadOnly bool   `json:"read_only"`
}
type MCP struct {
	Name     string   `json:"name"`
	Command  []string `json:"command,omitempty"`
	URL      string   `json:"url,omitempty"`
	Host     bool     `json:"host,omitempty"`
	Port     int      `json:"port,omitempty"`
	Disabled bool     `json:"disabled,omitempty"`
}
type Config struct {
	Version int      `json:"version"`
	CPUs    int      `json:"cpus"`
	Memory  int      `json:"memory_gib"`
	Disk    int      `json:"disk_gib"`
	Token   string   `json:"token"`
	Mounts  []Mount  `json:"mounts"`
	Hosts   []string `json:"hosts"`
	MCPs    []MCP    `json:"mcp"`
}
type App struct {
	Root, Name, Dir string
	Restart         bool
}

func New(root, name string, restart bool) (*App, error) {
	if !validName.MatchString(name) {
		return nil, fmt.Errorf("invalid VM name %q", name)
	}
	if root == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		root = filepath.Join(home, ".agents")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	root, err = canonicalPath(root)
	if err != nil {
		return nil, err
	}
	return &App{Root: root, Name: name, Dir: filepath.Join(root, "vms", name), Restart: restart}, nil
}

// Resolve existing ancestors too: /tmp on macOS is a symlink to /private/tmp.
func canonicalPath(p string) (string, error) {
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	parent := filepath.Dir(p)
	if parent == p {
		return "", err
	}
	base, err := canonicalPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, filepath.Base(p)), nil
}
func (a *App) configPath() string { return filepath.Join(a.Dir, "config.json") }
func (a *App) load() (Config, error) {
	var c Config
	b, err := os.ReadFile(a.configPath())
	if err != nil {
		return c, fmt.Errorf("read VM config (run agents create first): %w", err)
	}
	err = json.Unmarshal(b, &c)
	if err == nil && c.Version != 1 {
		err = fmt.Errorf("unsupported config version %d", c.Version)
	}
	return c, err
}
func atomicWrite(file string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(file), ".agents-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = f.Chmod(mode); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), file)
}
func (a *App) save(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(a.configPath(), append(b, '\n'), 0600)
}
func newToken() (string, error) {
	b := make([]byte, 32)
	_, err := rand.Read(b)
	return hex.EncodeToString(b), err
}

func (a *App) lock() (func(), error) {
	if err := os.MkdirAll(a.Dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(a.Dir, "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another agents command is updating this VM")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func hostPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		p = filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return "", fmt.Errorf("not a directory: %s", p)
	}
	return p, nil
}
func guestPath(p, host string) (string, error) {
	if p == "" {
		p = path.Join(guestHome, "dev", filepath.Base(host))
	} else if p == "~" || strings.HasPrefix(p, "~/") {
		p = guestHome + strings.TrimPrefix(p, "~")
	} else if !path.IsAbs(p) {
		p = path.Join(guestHome, "dev", p)
	}
	p = path.Clean(p)
	if !(strings.HasPrefix(p, guestHome+"/dev/") || strings.HasPrefix(p, "/mnt/")) {
		return "", fmt.Errorf("mount destination must be below ~/dev/ or /mnt/ (refusing to cover VM tools or credentials)")
	}
	return p, nil
}
func contains(parent, child string) bool {
	return child == parent || strings.HasPrefix(child, strings.TrimSuffix(parent, "/")+"/")
}
func filepathAbsExport(p string) (string, error) {
	if resolved, e := hostPath(p); e == nil {
		return resolved, nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		p = filepath.Join(h, strings.TrimPrefix(p, "~"))
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return canonicalPath(abs)
}
func validateMount(c Config, m Mount) error {
	for _, x := range c.Mounts {
		if contains(x.Host, m.Host) || contains(m.Host, x.Host) || contains(x.Guest, m.Guest) || contains(m.Guest, x.Guest) {
			return fmt.Errorf("mount overlaps %s -> %s", x.Host, x.Guest)
		}
	}
	return nil
}
func (a *App) validateHostExport(host string) error {
	if contains(host, a.Root) || contains(a.Root, host) {
		return errors.New("cannot export agents state, credentials or a parent of its state directory")
	}
	return nil
}
func sortConfig(c *Config) {
	sort.Strings(c.Hosts)
	sort.Slice(c.Mounts, func(i, j int) bool { return c.Mounts[i].Guest < c.Mounts[j].Guest })
}
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func commandLine(args []string) string {
	out := make([]string, len(args))
	for i, s := range args {
		out[i] = quote(s)
	}
	return strings.Join(out, " ")
}
