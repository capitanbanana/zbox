package app

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/capitanbanana/zbox/internal/guest"
	"github.com/capitanbanana/zbox/internal/redact"
)

func envSet(env []string, key, value string) []string {
	out := []string{}
	for _, s := range env {
		if !strings.HasPrefix(s, key+"=") {
			out = append(out, s)
		}
	}
	return append(out, key+"="+value)
}
func (a *App) lima(args ...string) *exec.Cmd {
	c := exec.Command("limactl", args...)
	c.Env = envSet(os.Environ(), "LIMA_HOME", filepath.Join(a.Root, "lima"))
	return c
}
func run(c *exec.Cmd) error {
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
func output(c *exec.Cmd) ([]byte, error) {
	var errout bytes.Buffer
	c.Stderr = &errout
	b, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("%s: %w\n%s", c.Args[0], err, errout.String())
	}
	return b, nil
}
func (a *App) state() (string, error) {
	b, err := output(a.lima("list", "--json", a.Name))
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return "Absent", nil
	}
	var row struct{ Status string }
	if err = json.Unmarshal(b, &row); err != nil {
		return "", err
	}
	return row.Status, nil
}
func (a *App) sshConfig() string { return filepath.Join(a.Root, "lima", a.Name, "ssh.config") }
func (a *App) ssh(tty bool, command string) *exec.Cmd {
	args := []string{"-F", a.sshConfig(), "-o", "ForwardAgent=no", "-o", "ForwardX11=no", "-o", "ClearAllForwardings=yes"}
	if tty {
		args = append(args, "-t")
	}
	args = append(args, "lima-"+a.Name, command)
	return exec.Command("ssh", args...)
}

func (a *App) template(c Config) ([]byte, error) {
	mounts := []map[string]any{}
	for _, m := range c.Mounts {
		mounts = append(mounts, map[string]any{"location": m.Host, "mountPoint": m.Guest, "writable": !m.ReadOnly})
	}
	t := map[string]any{
		"base": []string{"template:_images/ubuntu-lts"}, "vmType": "vz", "mountType": "virtiofs", "mountInotify": true,
		"cpus": c.CPUs, "memory": fmt.Sprintf("%dGiB", c.Memory), "disk": fmt.Sprintf("%dGiB", c.Disk),
		"user": map[string]any{"name": guestUser(c), "home": c.Home()}, "mounts": mounts,
		"ssh":        map[string]any{"loadDotSSHPubKeys": false, "forwardAgent": false, "forwardX11": false},
		"containerd": map[string]bool{"system": false, "user": false}, "propagateProxyEnv": false,
		"portForwards": []map[string]any{{"guestIP": "0.0.0.0", "guestIPMustBeZero": false, "proto": "any", "ignore": true}},
		"provision": []map[string]string{{"mode": "system", "script": `#!/bin/bash
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
if test -f /var/lib/agents/bootstrap-system-v2; then exit 0; fi
apt-get update
apt-get install -y --no-install-recommends ca-certificates curl git ripgrep tmux jq python3 python3-venv build-essential xz-utils
case "$(uname -m)" in
  aarch64) node_arch=arm64 ;;
  x86_64) node_arch=x64 ;;
  *) echo 'Unsupported Node architecture' >&2; exit 1 ;;
esac
node_tmp=$(mktemp -d)
trap 'rm -rf "$node_tmp"' EXIT
cd "$node_tmp"
node_base=https://nodejs.org/dist/latest-v22.x
curl -fsSL --retry 3 "$node_base/SHASUMS256.txt" -o SHASUMS256.txt
archive=$(awk -v suffix="-linux-$node_arch.tar.xz" 'substr($2,length($2)-length(suffix)+1)==suffix {print $2}' SHASUMS256.txt)
[[ "$archive" =~ ^node-v22\.[0-9]+\.[0-9]+-linux-(arm64|x64)\.tar\.xz$ ]]
curl -fsSL --retry 3 "$node_base/$archive" -o "$archive"
awk -v file="$archive" '$2==file {print}' SHASUMS256.txt > checksum.txt
sha256sum --check checksum.txt
tar -xJf "$archive" --strip-components=1 -C /usr/local
mkdir -p /var/lib/agents
touch /var/lib/agents/bootstrap-system-v2
`}, {"mode": "user", "script": harnessProvision}},
	}
	return json.MarshalIndent(t, "", "  ")
}

func (a *App) create(c Config) error {
	if runtime.GOOS != "darwin" {
		return errors.New("VM lifecycle currently supports macOS with Lima VZ")
	}
	if _, err := os.Stat(a.configPath()); err == nil {
		return errors.New("VM configuration already exists; use zbox start")
	} else if !os.IsNotExist(err) {
		return err
	}
	if c.CPUs < 1 || c.Memory < 2 || c.Disk < 10 {
		return errors.New("minimum resources: 1 CPU, 2 GiB memory, 10 GiB disk")
	}
	if err := os.MkdirAll(filepath.Join(a.Root, "lima"), 0700); err != nil {
		return err
	}
	var err error
	c.Version = 1
	c.Token, err = newToken()
	if err != nil {
		return err
	}
	c.Hosts = []string{}
	c.Mounts = []Mount{}
	c.MCPs = []MCP{}
	seed := make([]byte, 32)
	if _, err = rand.Read(seed); err != nil {
		return err
	}
	legend := redact.NewLegend(seed, time.Now())
	c.PersonaSeed = hex.EncodeToString(seed)
	c.Persona = &legend
	b, err := a.template(c)
	if err != nil {
		return err
	}
	file := filepath.Join(a.Dir, "template.yaml")
	if err = atomicWrite(file, b, 0600); err != nil {
		return err
	}
	if err = run(a.lima("create", "--tty=false", "--name", a.Name, file)); err != nil {
		return err
	}
	if err = a.save(c); err != nil {
		return err
	}
	fmt.Println("Created", a.Name, "(stopped). Run zbox start to provision and start it.")
	return nil
}

func (a *App) syncMounts(c Config) error {
	m := []map[string]any{}
	for _, x := range c.Mounts {
		m = append(m, map[string]any{"location": x.Host, "mountPoint": x.Guest, "writable": !x.ReadOnly})
	}
	b, _ := json.Marshal(m)
	return run(a.lima("edit", "--tty=false", "--set", ".mounts = "+string(b), a.Name))
}

func (a *App) guestBinary() (string, error) {
	if p := settingEnv("GUEST_BINARY"); p != "" {
		return filepath.Abs(p)
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	p := filepath.Join(filepath.Dir(exe), "zbox-linux-"+runtime.GOARCH)
	if _, err = os.Stat(p); err != nil {
		return "", fmt.Errorf("guest helper missing: %s; run make build or set ZBOX_GUEST_BINARY", p)
	}
	return p, nil
}

func (a *App) upload(reader io.Reader, destination string, mode string) error {
	c := a.ssh(false, "umask 077; mkdir -p "+quote(filepathDirGuest(destination))+" && cat > "+quote(destination+".tmp")+" && chmod "+mode+" "+quote(destination+".tmp")+" && mv "+quote(destination+".tmp")+" "+quote(destination))
	c.Stdin = reader
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}
func filepathDirGuest(s string) string {
	i := strings.LastIndex(s, "/")
	if i < 0 {
		return "."
	}
	return s[:i]
}

func (a *App) installGuest(c Config) error {
	p, err := a.guestBinary()
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	home := c.Home()
	if err = a.upload(f, home+"/.local/bin/agents-guest", "700"); err != nil {
		return err
	}
	b, _ := json.Marshal(guest.Credentials{Token: c.Token})
	if err = a.upload(bytes.NewReader(b), home+"/.config/agents/credentials.json", "600"); err != nil {
		return err
	}
	if err = a.upload(strings.NewReader(guest.Profile(home)), home+"/.config/agents/proxy.env", "600"); err != nil {
		return err
	}
	unit := `[Unit]
Description=Zbox split-routing proxy
After=network.target
[Service]
ExecStart=` + home + `/.local/bin/agents-guest _guest ` + home + `/.config/agents/credentials.json
Restart=on-failure
RestartSec=2
[Install]
WantedBy=default.target
`
	if err = a.upload(strings.NewReader(unit), home+"/.config/systemd/user/agents-proxy.service", "600"); err != nil {
		return err
	}
	if err = a.upload(strings.NewReader(rdxEnv()), home+"/.config/agents/rdx.env", "600"); err != nil {
		return err
	}
	if err = a.upload(strings.NewReader(rdxUnit), home+"/.config/systemd/user/rdx-proxy.service", "600"); err != nil {
		return err
	}
	if err = a.uploadBootstrap(c); err != nil {
		return err
	}
	// rdx is provisioned by the harness installer, so on a first boot its unit
	// has no binary yet; the proxy service must still come up.
	cmd := "systemctl --user daemon-reload" +
		" && systemctl --user enable agents-proxy.service rdx-proxy.service" +
		" && systemctl --user restart agents-proxy.service" +
		" && { systemctl --user restart rdx-proxy.service || true; }"
	return run(a.ssh(false, cmd))
}

// guestUser is the guest account name: the persona's username where one
// exists, and the legacy "agent" for VMs created before personas existed.
func guestUser(c Config) string {
	if c.Persona != nil && c.Persona.Username != "" {
		return c.Persona.Username
	}
	return "agent"
}

// rdxEnv is the plain KEY=VALUE environment for the guest redaction proxy. rdx
// has to reach the provider itself, so it inherits the guest routing proxy.
func rdxEnv() string {
	proxy := "http://127.0.0.1:" + guest.HTTPPort
	return "RDX_UPSTREAM_URL=https://api.anthropic.com\n" +
		"RDX_TIMEOUT=600\n" +
		"HTTP_PROXY=" + proxy + "\n" +
		"HTTPS_PROXY=" + proxy + "\n" +
		"http_proxy=" + proxy + "\n" +
		"https_proxy=" + proxy + "\n" +
		"NO_PROXY=127.0.0.1,localhost\n"
}

// rdxUnit runs the redaction proxy as a user service. It deliberately avoids
// `rdx setup`, which would rewrite project settings and CLAUDE.md.
const rdxUnit = `[Unit]
Description=Zbox redaction proxy
After=network.target agents-proxy.service
[Service]
EnvironmentFile=%h/.config/agents/rdx.env
ExecStart=%h/.local/share/uv/tools/claude-code-redact/bin/python -m uvicorn rdx.proxy.server:app --host 127.0.0.1 --port ` + guest.RDXPort + ` --log-level warning
Restart=on-failure
RestartSec=3
[Install]
WantedBy=default.target
`

// uploadBootstrap seeds ~/.claude.json exactly once, before Claude Code has
// ever run on this VM and therefore before anything could reach the provider.
// After that the file belongs to the tool and is never rewritten: rewriting it
// under a logged-in session would look like tampering.
func (a *App) uploadBootstrap(c Config) error {
	if c.Persona == nil {
		return nil
	}
	home := c.Home()
	dest := home + "/.claude.json"
	command := "umask 077; mkdir -p " + quote(home) + " && if test -e " + quote(dest) + "; then cat >/dev/null; else cat > " + quote(dest) + ".tmp && chmod 600 " + quote(dest) + ".tmp && mv " + quote(dest) + ".tmp " + quote(dest) + "; fi"
	cmd := a.ssh(false, command)
	cmd.Stdin = bytes.NewReader(c.Persona.ClaudeJSON())
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

func (a *App) start() error {
	c, err := a.load()
	if err != nil {
		return err
	}
	if _, err = a.guestBinary(); err != nil {
		return err
	}
	if err = writeRules(c); err != nil {
		return err
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	if c.VPN != nil {
		if state == "Running" {
			if err = a.verifyVPNNetwork(); err != nil {
				return err
			}
			if _, err = a.gatewayRequest("GET", "/agents/health"); err != nil {
				return errors.New("VPN gateway is unavailable; VM remains blocked, run zbox stop then zbox start")
			}
		} else {
			if err = a.syncVPNNetwork(c); err != nil {
				return err
			}
			if err = a.stopGateway(); err != nil {
				return err
			}
			if err = a.startGateway(); err != nil {
				return err
			}
			if err = a.verifyVPNNetwork(); err != nil {
				return err
			}
		}
	}
	if state == "Running" {
		if _, e := a.daemonRequest("GET", "/health"); e == nil {
			if e = a.waitGuest(); e == nil {
				return a.ensureHarnesses()
			}
		}
	}
	if state != "Running" {
		if err = a.syncMounts(c); err != nil {
			return err
		}
		if c.VPN != nil {
			err = a.guardedVMStart()
		} else {
			err = run(a.lima("start", "--tty=false", a.Name))
		}
		if err != nil {
			return err
		}
	}
	if err = a.installGuest(c); err != nil {
		return err
	}
	if err = a.startDaemon(); err != nil {
		return err
	}
	if err = a.waitGuest(); err != nil {
		return err
	}
	return a.ensureHarnesses()
}

func (a *App) stop() error {
	if _, err := a.load(); err != nil {
		return err
	}
	if err := a.stopDaemon(); err != nil {
		return err
	}
	state, err := a.state()
	if err != nil {
		return err
	}
	if state == "Stopped" || state == "Absent" {
		return a.stopGateway()
	}
	if err = run(a.lima("stop", "--tty=false", a.Name)); err != nil {
		return err
	}
	return a.stopGateway()
}

func (a *App) delete() error {
	if err := a.stop(); err != nil {
		return err
	}
	if c, err := a.load(); err == nil && c.VPN != nil {
		c.VPN = nil
		if err = a.syncVPNNetwork(c); err != nil {
			return err
		}
	}
	if err := run(a.lima("delete", "--tty=false", a.Name)); err != nil {
		return err
	}
	// Keep the lock inode stable; only remove this VM's managed state.
	entries, err := os.ReadDir(a.Dir)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() != "lock" {
			if err = os.RemoveAll(filepath.Join(a.Dir, e.Name())); err != nil {
				return err
			}
		}
	}
	fmt.Println("Deleted", a.Name, "; host mounts were not deleted.")
	return nil
}

func (a *App) changeMounts(c Config) error {
	state, err := a.state()
	if err != nil {
		return err
	}
	running := state == "Running"
	if running && !a.Restart {
		return errors.New("new virtiofs mounts require a VM restart; stop it first or use zbox --restart mounts add … (ends running sessions)")
	}
	if running {
		if err = a.stop(); err != nil {
			return err
		}
	}
	// Lima is authoritative for mount application; save only after it accepts.
	if err = a.syncMounts(c); err != nil {
		return err
	}
	sortConfig(&c)
	if err = a.save(c); err != nil {
		return err
	}
	if running {
		return a.start()
	}
	return nil
}

func (a *App) addMount(host, dest string, ro bool) (string, error) {
	host, err := hostPath(host)
	if err != nil {
		return "", err
	}
	if err = a.validateHostExport(host); err != nil {
		return "", err
	}
	c, err := a.load()
	if err != nil {
		return "", err
	}
	g, err := guestPath(dest, host, c.Home())
	if err != nil {
		return "", err
	}
	for _, m := range c.Mounts {
		if m.Host == host && m.Guest == g && m.ReadOnly == ro {
			return g, nil
		}
	}
	m := Mount{Host: host, Guest: g, ReadOnly: ro}
	if err = validateMount(c, m); err != nil {
		return "", err
	}
	c.Mounts = append(c.Mounts, m)
	if err = a.changeMounts(c); err != nil {
		return "", err
	}
	return g, nil
}

func (a *App) sessionMount(host string) (string, error) {
	p, err := hostPath(host)
	if err != nil {
		return "", err
	}
	c, err := a.load()
	if err != nil {
		return "", err
	}
	for _, m := range c.Mounts {
		if contains(m.Host, p) {
			rel, err := filepath.Rel(m.Host, p)
			if err != nil {
				return "", err
			}
			return filepath.Join(m.Guest, rel), nil
		}
	}
	return a.addMount(p, "", false)
}

func (a *App) sessionCommand(harness, dir string, args []string, c Config) string {
	cmd := harnessCommand(harness, args, c)
	env := "export PATH=\"$HOME/.local/bin:$PATH\"; . \"$HOME/.config/agents/proxy.env\"; "
	if harness == "ori" || harness == "openrouter" {
		// Ori can hide/replace Codex auth while configuring OpenRouter. Keep
		// that state separate from ordinary zbox codex sessions.
		env += "export CODEX_HOME=" + quote(c.Home()+"/.config/agents/ori-codex") + "; "
	}
	return env + "cd " + quote(dir) + " && exec " + commandLine(cmd)
}

func (a *App) session(harness, dir string, args []string) (*exec.Cmd, error) {
	g, err := a.sessionMount(dir)
	if err != nil {
		return nil, err
	}
	if err = a.start(); err != nil {
		return nil, err
	}
	c, err := a.load()
	if err != nil {
		return nil, err
	}
	if err = a.writeMCP(c); err != nil {
		return nil, err
	}
	fmt.Fprintf(os.Stderr, "[%s VM] %s\n", a.Name, g)
	for _, m := range c.Mounts {
		mode := "RW"
		if m.ReadOnly {
			mode = "RO"
		}
		fmt.Fprintf(os.Stderr, "  mount %s: %s -> %s\n", mode, m.Host, m.Guest)
	}
	mode := "direct by default (host/LAN not filtered)"
	if c.VPN != nil {
		mode = "VPN only, kill switch active"
		if h, err := a.gatewayRequest("GET", "/agents/health"); err != nil {
			mode = "gateway unavailable, external traffic blocked"
		} else if h.Egress == "direct" {
			mode = fmt.Sprintf("DIRECT via trusted network %q", h.Bypass)
		} else if !h.VPNRunning {
			mode = "VPN down, kill switch active"
		}
	}
	fmt.Fprintf(os.Stderr, "  network: %s; via Mac SOCKS exceptions: %s\n", mode, strings.Join(c.Hosts, ", "))
	for _, m := range c.MCPs {
		if m.Disabled {
			continue
		}
		kind := "guest stdio"
		if m.URL != "" {
			kind = "remote HTTP"
		}
		if m.Host {
			kind = fmt.Sprintf("host TCP tunnel, guest port %d", m.Port)
		}
		fmt.Fprintf(os.Stderr, "  MCP %s: %s\n", m.Name, kind)
	}
	return a.ssh(isTerminal(os.Stdin), a.sessionCommand(harness, g, args, c)), nil
}
