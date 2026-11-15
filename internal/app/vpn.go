package app

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/capitanbanana/zbox/internal/gateway"
	"github.com/capitanbanana/zbox/internal/hostnet"
	"github.com/capitanbanana/zbox/internal/proxy"
	"github.com/capitanbanana/zbox/internal/vpn"
	"gopkg.in/yaml.v3"
)

func (a *App) networkName() string { return fmt.Sprintf("ag%x", sha256.Sum256([]byte(a.Name)))[:10] }
func (a *App) networkDir() string  { return filepath.Join(a.Root, "lima", "_networks", a.networkName()) }
func (a *App) networkSocket(kind string) string {
	return filepath.Join(a.networkDir(), a.networkName()+"_"+kind+".sock")
}
func (a *App) networkPIDFile() string {
	return filepath.Join(a.networkDir(), "usernet_"+a.networkName()+".pid")
}

func singBoxBinary() (string, error) {
	p := settingEnv("SING_BOX")
	if p == "" {
		p = "sing-box"
	}
	resolved, e := exec.LookPath(p)
	if e != nil {
		return "", errors.New("sing-box is required on the Mac: brew install sing-box (or set ZBOX_SING_BOX)")
	}
	return filepath.Abs(resolved)
}

func (a *App) vpn(args []string) error {
	if len(args) > 0 && args[0] == "bypass" {
		return a.vpnBypass(args[1:])
	}
	if len(args) == 0 || args[0] == "status" {
		if len(args) > 1 {
			return errors.New("vpn status takes no arguments")
		}
		c, e := a.load()
		if e != nil {
			return e
		}
		a.printVPN(c)
		return nil
	}
	action := args[0]
	if action != "add" && action != "update" && action != "delete" {
		return errors.New("usage: zbox vpn add|update VLESS_URI|--stdin | delete | status")
	}
	c, e := a.load()
	if e != nil {
		return e
	}
	if action == "add" && c.VPN != nil {
		return errors.New("VPN already exists; use vpn update")
	}
	if action != "add" && c.VPN == nil {
		return errors.New("no VPN profile configured")
	}
	var p *vpn.Profile
	if action == "delete" {
		if len(args) != 1 {
			return errors.New("vpn delete takes no arguments")
		}
	} else {
		if len(args) != 2 {
			return errors.New("pass a VLESS URI or --stdin")
		}
		raw := args[1]
		if raw == "--stdin" {
			b, e := io.ReadAll(io.LimitReader(os.Stdin, 16385))
			if e != nil {
				return e
			}
			if len(b) > 16384 {
				return errors.New("VLESS URI is too long")
			}
			raw = string(b)
		}
		p, e = vpn.Parse(raw)
		if e != nil {
			return e
		}
		binary, e := singBoxBinary()
		if e != nil {
			return e
		}
		b, e := p.SingBox(1080, c.Token)
		if e != nil {
			return e
		}
		file := filepath.Join(a.Dir, "vpn-check.json")
		if e = atomicWrite(file, b, 0600); e != nil {
			return e
		}
		check := exec.Command(binary, "check", "-c", file)
		result := check.Run()
		_ = os.Remove(file)
		if result != nil {
			return errors.New("sing-box rejected the generated VPN configuration; check version/transport support (1.13.x recommended)")
		}
	}
	state, e := a.state()
	if e != nil {
		return e
	}
	running := state == "Running"
	if running && !a.Restart {
		return errors.New("VPN changes require a stopped VM; use zbox --restart vpn … to restart all sessions safely")
	}
	if running {
		if e = a.stop(); e != nil {
			return e
		}
	} else {
		if e = a.stopGateway(); e != nil {
			return e
		}
	}
	c.VPN = p
	// Enabling/updating records the fail-closed intent first. If Lima editing
	// fails, a later start still requires the gateway. Disabling does the reverse:
	// keep the intent until the stopped VM has accepted ordinary networking.
	if p != nil {
		if e = a.save(c); e != nil {
			return e
		}
	}
	if e = a.syncVPNNetwork(c); e != nil {
		return e
	}
	if p == nil {
		if e = a.save(c); e != nil {
			return e
		}
	}
	_ = os.Remove(filepath.Join(a.Dir, "vpn-runtime.json"))
	if p == nil {
		fmt.Println("VPN removed. The next VM start uses ordinary Lima networking.")
	} else {
		fmt.Println("VPN configured:", p.Summary(), "— host-enforced kill switch; network/MCP exceptions retained.")
	}
	if running {
		return a.start()
	}
	return nil
}

// A dedicated named user-v2 NIC replaces Lima's default unfiltered NIC.
func (a *App) syncVPNNetwork(c Config) error {
	if c.VPN != nil {
		if e := a.installNetworkGuard(); e != nil {
			return e
		}
	}
	if e := os.MkdirAll(filepath.Join(a.Root, "lima", "_config"), 0700); e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(a.Root, "networks.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	file := filepath.Join(a.Root, "lima", "_config", "networks.yaml")
	cfg := map[string]any{}
	b, e := os.ReadFile(file)
	if e == nil {
		if e = yaml.Unmarshal(b, &cfg); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return e
	}
	nets, _ := cfg["networks"].(map[string]any)
	if nets == nil {
		nets = map[string]any{}
	}
	cfg["networks"] = nets
	if c.VPN != nil {
		nets[a.networkName()] = map[string]any{"mode": "user-v2", "gateway": gateway.IP, "netmask": "255.255.255.0"}
	} else {
		delete(nets, a.networkName())
	}
	b, e = yaml.Marshal(cfg)
	if e != nil {
		return e
	}
	if e = atomicWrite(file, b, 0600); e != nil {
		return e
	}
	setting := `.networks = [] | .hostResolver.enabled = true | .dns = []`
	if c.VPN != nil {
		netJSON, _ := json.Marshal([]map[string]string{{"lima": a.networkName()}})
		setting = ".networks = " + string(netJSON) + ` | .hostResolver.enabled = false | .dns = ["` + gateway.IP + `"]`
	}
	return run(a.lima("edit", "--tty=false", "--set", setting, a.Name))
}

type gatewayHealth struct {
	hostnet.Status
	Mode       string `json:"mode"`
	PID        int    `json:"pid"`
	VPNPID     int    `json:"vpn_pid"`
	VPNRunning bool   `json:"vpn_process_running"`
}

func (a *App) gatewayRequest(method, route string) (gatewayHealth, error) {
	var h gatewayHealth
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", a.networkSocket("ep"))
	}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(method, "http://gateway"+route, nil)
	r, e := client.Do(req)
	if e != nil {
		return h, e
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return h, errors.New("VPN gateway unavailable")
	}
	e = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&h)
	if e == nil && h.Mode != "agents-vpn-v1" {
		e = errors.New("unexpected network gateway; refusing unfiltered networking")
	}
	return h, e
}
func (a *App) printVPN(c Config) {
	if c.VPN == nil {
		fmt.Println("VPN: disabled")
		return
	}
	fmt.Println("VPN:", c.VPN.Summary())
	h, e := a.gatewayRequest("GET", "/agents/health")
	if e != nil {
		fmt.Println("  gateway: stopped/unavailable; external traffic cannot bypass VPN in a configured running VM")
	} else if h.Egress == "direct" {
		fmt.Printf("  egress: DIRECT via trusted network %q; VPN process running: %t\n", h.Bypass, h.VPNRunning)
	} else if !h.VPNRunning {
		fmt.Println("  VPN process: DOWN; kill switch active")
	} else {
		fmt.Println("  gateway: running; VPN process running (remote reachability is not asserted)")
	}
	if e == nil && h.Reason != "" {
		fmt.Println("  policy:", h.Reason)
	}
	fmt.Printf("  bypass networks: %d (zbox vpn bypass list)\n", len(c.VPNBypass))
	fmt.Println("  default TCP/UDP and DNS: VPN unless a trusted network matches; raw IPv6/external ICMP: blocked; network/MCP: explicit host exceptions")
}

func (a *App) startGateway() error {
	if _, e := a.gatewayRequest("GET", "/agents/health"); e == nil {
		return nil
	}
	if _, e := singBoxBinary(); e != nil {
		return e
	}
	if e := os.MkdirAll(a.networkDir(), 0700); e != nil {
		return e
	}
	if len(a.networkSocket("fd")) > 100 {
		return errors.New("VPN network socket path exceeds macOS limit; use a shorter --state-dir")
	}
	exe, e := os.Executable()
	if e != nil {
		return e
	}
	log, e := os.OpenFile(filepath.Join(a.Dir, "vpn.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer log.Close()
	cmd := exec.Command(exe, "--state-dir", a.Root, "--vm", a.Name, "_gateway")
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if e = cmd.Start(); e != nil {
		return e
	}
	_ = cmd.Process.Release()
	for i := 0; i < 100; i++ {
		time.Sleep(100 * time.Millisecond)
		if h, e := a.gatewayRequest("GET", "/agents/health"); e == nil && h.VPNRunning {
			return nil
		}
	}
	return fmt.Errorf("VPN gateway failed to start; see %s (no VM was started)", filepath.Join(a.Dir, "vpn.log"))
}
func (a *App) stopGateway() error {
	if _, e := a.gatewayRequest("GET", "/agents/health"); e != nil {
		return nil
	}
	_, e := a.gatewayRequest("POST", "/agents/stop")
	if e != nil {
		return e
	}
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, e = a.gatewayRequest("GET", "/agents/health"); e != nil {
			return nil
		}
	}
	return errors.New("VPN gateway did not stop")
}

// Keep a live owner PID throughout Lima reconciliation. Even if the gateway
// dies between our health check and VM boot, Lima must not spawn its standard
// unfiltered usernet. The CLI waits for limactl before restoring the gateway PID.
func (a *App) guardedVMStart() error {
	h, e := a.gatewayRequest("GET", "/agents/health")
	if e != nil {
		return e
	}
	if e = atomicWrite(a.networkPIDFile(), []byte(strconv.Itoa(os.Getpid())), 0600); e != nil {
		return e
	}
	e = run(a.lima("start", "--tty=false", a.Name))
	if current, check := a.gatewayRequest("GET", "/agents/health"); check == nil && current.PID == h.PID {
		if writeErr := atomicWrite(a.networkPIDFile(), []byte(strconv.Itoa(h.PID)), 0600); e == nil {
			e = writeErr
		}
	} else if e == nil {
		e = errors.New("VPN gateway stopped during boot; VM has no external network")
	}
	return e
}

func (a *App) runGateway() error {
	lock, e := os.OpenFile(filepath.Join(a.Dir, "gateway.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return errors.New("VPN gateway already running")
	}
	c, e := a.load()
	if e != nil {
		return e
	}
	if c.VPN == nil {
		return errors.New("VPN not configured")
	}
	binary, e := singBoxBinary()
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return e
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	endpoint := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	b, e := c.VPN.SingBox(port, c.Token)
	if e != nil {
		return e
	}
	file := filepath.Join(a.Dir, "vpn-runtime.json")
	if e = atomicWrite(file, b, 0600); e != nil {
		return e
	}
	child := exec.CommandContext(ctx, binary, "run", "-c", file)
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if e = child.Start(); e != nil {
		return e
	}
	var alive atomic.Bool
	alive.Store(true)
	childDone := make(chan struct{})
	go func() { _ = child.Wait(); alive.Store(false); close(childDone) }()
	defer func() { cancel(); <-childDone }()
	ready := false
	for i := 0; i < 100; i++ {
		if !alive.Load() {
			return errors.New("sing-box exited during startup")
		}
		conn, err := net.DialTimeout("tcp", endpoint, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !ready {
		return errors.New("sing-box did not open its SOCKS listener")
	}
	tcpDial := func(ctx context.Context, addr string) (net.Conn, error) {
		if !alive.Load() {
			return nil, errors.New("VPN is down")
		}
		return proxy.SOCKSDial(ctx, endpoint, c.Token, addr)
	}
	udpDial := func(ctx context.Context, addr string) (net.Conn, error) {
		if !alive.Load() {
			return nil, errors.New("VPN is down")
		}
		return proxy.SOCKSUDP(ctx, endpoint, c.Token, addr)
	}
	router := &hostnet.Router{
		Detect: hostnet.Detect, Direct: hostnet.Direct,
		Rules: func() ([]hostnet.Exception, error) { current, err := a.load(); return current.VPNBypass, err },
		TCP:   tcpDial, UDP: udpDial,
	}
	router.Refresh(ctx) // Decide before the VM can attach to its NIC.
	defer router.Close()
	go router.Watch(ctx)
	n, e := gateway.New(
		func(ctx context.Context, addr string) (net.Conn, error) { return router.Dial(ctx, "tcp", addr) },
		func(ctx context.Context, addr string) (net.Conn, error) { return router.Dial(ctx, "udp", addr) },
	)
	if e != nil {
		return e
	}
	defer n.Close()
	fdListener, e := gateway.ListenUnix(a.networkSocket("fd"))
	if e != nil {
		return e
	}
	defer fdListener.Close()
	go func() { _ = n.ServeFD(ctx, fdListener) }()
	epListener, e := gateway.ListenUnix(a.networkSocket("ep"))
	if e != nil {
		return e
	}
	defer epListener.Close()
	handler := n.API(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(gatewayHealth{Status: router.Status(), Mode: "agents-vpn-v1", PID: os.Getpid(), VPNPID: child.Process.Pid, VPNRunning: alive.Load()})
	})
	mux := http.NewServeMux()
	mux.Handle("/", handler)
	mux.HandleFunc("/agents/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		s := router.Refresh(r.Context())
		_ = json.NewEncoder(w).Encode(gatewayHealth{Status: s, Mode: "agents-vpn-v1", PID: os.Getpid(), VPNPID: child.Process.Pid, VPNRunning: alive.Load()})
	})
	mux.HandleFunc("/agents/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST required", 405)
			return
		}
		_ = json.NewEncoder(w).Encode(gatewayHealth{Mode: "agents-vpn-v1"})
		go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	defer server.Close()
	if e = atomicWrite(a.networkPIDFile(), []byte(strconv.Itoa(os.Getpid())), 0600); e != nil {
		return e
	}
	defer os.Remove(a.networkPIDFile())
	go func() { _ = server.Serve(epListener) }()
	<-ctx.Done()
	return nil
}

func (a *App) verifyVPNNetwork() error {
	if e := a.verifyNetworkGuard(); e != nil {
		return e
	}
	// Reject out-of-band Lima configuration that would add an unfiltered NIC.
	b, e := os.ReadFile(filepath.Join(a.Root, "lima", a.Name, "lima.yaml"))
	if e != nil {
		return e
	}
	var cfg struct {
		Networks []struct {
			Lima   string `yaml:"lima"`
			Socket string `yaml:"socket"`
			VZNAT  bool   `yaml:"vzNAT"`
		} `yaml:"networks"`
		HostResolver struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"hostResolver"`
		DNS []string `yaml:"dns"`
	}
	if e = yaml.Unmarshal(b, &cfg); e != nil {
		return e
	}
	if len(cfg.Networks) != 1 || cfg.Networks[0].Lima != a.networkName() || cfg.Networks[0].Socket != "" || cfg.Networks[0].VZNAT || cfg.HostResolver.Enabled || strings.Join(cfg.DNS, ",") != gateway.IP {
		return errors.New("VPN VM networking differs from the guarded configuration; stop the VM and run zbox start")
	}
	return nil
}

// Lima 2.1 reads leases before spawning its stock (unfiltered) usernet. An
// EOF lease source prevents that fallback even after SIGKILL, across reboots,
// and during reconciliation for another VM. Lima's stop-time lease writes go
// harmlessly to /dev/null, keeping the guard intact. Never depend on a stale PID.
func (a *App) installNetworkGuard() error {
	version, e := output(a.lima("--version"))
	if e != nil {
		return e
	}
	if !strings.HasPrefix(strings.TrimSpace(string(version)), "limactl version 2.1.") {
		return errors.New("VPN gateway integration requires Lima 2.1.x; other versions need a fail-closed compatibility check")
	}
	if e = os.MkdirAll(a.networkDir(), 0700); e != nil {
		return e
	}
	p := filepath.Join(a.networkDir(), "leases.json")
	if target, err := os.Readlink(p); err == nil && target == os.DevNull {
		return nil
	}
	tmp := p + ".guard"
	_ = os.Remove(tmp)
	if e = os.Symlink(os.DevNull, tmp); e != nil {
		return e
	}
	defer os.Remove(tmp)
	return os.Rename(tmp, p)
}

func (a *App) verifyNetworkGuard() error {
	p, e := os.Readlink(filepath.Join(a.networkDir(), "leases.json"))
	if e != nil || p != os.DevNull {
		return errors.New("VPN network fallback guard missing; stop VM and run zbox start")
	}
	return nil
}
