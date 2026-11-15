package app

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/capitanbanana/zbox/internal/guest"
	"github.com/capitanbanana/zbox/internal/policy"
	"github.com/capitanbanana/zbox/internal/proxy"
)

type daemonState struct {
	Control string `json:"control"`
	SOCKS   string `json:"socks"`
}

func (a *App) daemonStatePath() string { return filepath.Join(a.Dir, "daemon.json") }
func (a *App) daemonRequest(method, path string) ([]byte, error) {
	b, err := os.ReadFile(a.daemonStatePath())
	if err != nil {
		return nil, err
	}
	var s daemonState
	if err = json.Unmarshal(b, &s); err != nil {
		return nil, err
	}
	h, _, err := net.SplitHostPort(s.Control)
	if err != nil || h != "127.0.0.1" {
		return nil, errors.New("invalid daemon address")
	}
	c, err := a.load()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, "http://"+s.Control+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	cl := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("daemon returned %s", resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4096))
}

func (a *App) startDaemon() error {
	if _, err := a.daemonRequest("GET", "/health"); err == nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(a.Dir, "proxy.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	c := exec.Command(exe, "--state-dir", a.Root, "--vm", a.Name, "_daemon")
	c.Stdin = nil
	c.Stdout = f
	c.Stderr = f
	c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = c.Start(); err != nil {
		return err
	}
	_ = c.Process.Release()
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if _, err = a.daemonRequest("GET", "/health"); err == nil {
			return nil
		}
	}
	return fmt.Errorf("host proxy did not start; inspect %s", filepath.Join(a.Dir, "proxy.log"))
}

func (a *App) stopDaemon() error {
	// Shutdown is sent via a separate host-only Unix socket, never through the
	// policy endpoint forwarded to the VM. No PID-based killing of stale files.
	sock := filepath.Join(a.Dir, "control.sock")
	c, err := net.DialTimeout("unix", sock, time.Second)
	if err != nil {
		if _, e := a.daemonRequest("GET", "/health"); e == nil {
			return fmt.Errorf("daemon is live but host control socket is unavailable: %w", err)
		}
		return nil
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	_, err = c.Write([]byte("stop\n"))
	if err != nil {
		return err
	}
	_, err = io.ReadAll(c)
	if err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
		if _, e := a.daemonRequest("GET", "/health"); e != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return errors.New("daemon shutdown timed out")
}

func (a *App) daemon() error {
	lock, err := os.OpenFile(filepath.Join(a.Dir, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return errors.New("daemon already running")
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	socksL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer socksL.Close()
	httpL, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer httpL.Close()
	sock := filepath.Join(a.Dir, "control.sock")
	_ = os.Remove(sock)
	unixL, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	defer unixL.Close()
	_ = os.Chmod(sock, 0600)
	go func() {
		for {
			conn, e := unixL.Accept()
			if e != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			line, _ := bufio.NewReader(io.LimitReader(conn, 32)).ReadString('\n')
			if strings.TrimSpace(line) == "stop" {
				cancel()
				conn.Close()
				return
			}
			conn.Close()
		}
	}()

	go func() { _ = proxy.ServeSOCKS(socksL, c.Token, a.hostDial) }()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			http.Error(w, "read only", 405)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+c.Token)) != 1 {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, "ok\n")
		case "/policy":
			current, e := a.load()
			if e != nil {
				http.Error(w, "policy unavailable", 503)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(guest.Policy{Hosts: current.Hosts})
		default:
			http.NotFound(w, r)
		}
	})
	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	defer srv.Close()
	go func() { _ = srv.Serve(httpL) }()
	state := daemonState{Control: httpL.Addr().String(), SOCKS: socksL.Addr().String()}
	b, _ := json.Marshal(state)
	if err = atomicWrite(a.daemonStatePath(), b, 0600); err != nil {
		return err
	}
	defer os.Remove(a.daemonStatePath())
	done := make(chan struct{})
	go func() { defer close(done); a.tunnelLoop(ctx, state) }()
	<-ctx.Done()
	<-done
	return nil
}

func (a *App) tunnelArgs(c Config, s daemonState) []string {
	args := []string{"-F", a.sshConfig(), "-N", "-T", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ForwardAgent=no", "-o", "ExitOnForwardFailure=yes", "-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=5",
		"-R", "127.0.0.1:" + guest.HostSOCKSPort + ":" + s.SOCKS, "-R", "127.0.0.1:" + guest.HostControlPort + ":" + s.Control}
	for _, m := range c.MCPs {
		if m.Host && !m.Disabled {
			addr, _ := hostMCPAddress(m.URL)
			args = append(args, "-R", fmt.Sprintf("127.0.0.1:%d:%s", m.Port, addr))
		}
	}
	return append(args, "lima-"+a.Name)
}

func (a *App) tunnelLoop(ctx context.Context, s daemonState) {
	for ctx.Err() == nil {
		c, err := a.load()
		if err != nil {
			log.Print(err)
			return
		}
		args := a.tunnelArgs(c, s)
		childCtx, cancel := context.WithCancel(ctx)
		cmd := exec.CommandContext(childCtx, "ssh", args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err = cmd.Start(); err != nil {
			cancel()
			log.Print(err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
			continue
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		tick := time.NewTicker(time.Second)
	loop:
		for {
			select {
			case <-ctx.Done():
				cancel()
				<-done
				break loop
			case err = <-done:
				if err != nil {
					log.Printf("tunnel disconnected: %v", err)
				}
				break loop
			case <-tick.C:
				next, e := a.load()
				if e != nil || strings.Join(a.tunnelArgs(next, s), "\x00") != strings.Join(args, "\x00") {
					cancel()
					<-done
					break loop
				}
			}
		}
		tick.Stop()
		cancel()
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func (a *App) waitGuest() error {
	for i := 0; i < 20; i++ {
		c := a.ssh(false, "curl --silent --fail --max-time 2 --noproxy '*' -H \"Authorization: Bearer $(python3 -c 'import json; print(json.load(open(\"/home/agent/.config/agents/credentials.json\"))[\"token\"])')\" http://127.0.0.1:"+guest.HostControlPort+"/health >/dev/null && systemctl --user is-active --quiet agents-proxy.service")
		if err := c.Run(); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("guest proxy/tunnel not ready; inspect %s", filepath.Join(a.Dir, "proxy.log"))
}

func (a *App) hostDial(ctx context.Context, addr string) (net.Conn, error) {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	current, err := a.load()
	if err != nil {
		return nil, err
	}
	if !policy.Match(current.Hosts, h) {
		log.Printf("DENY %s", strconv.Quote(addr))
		return nil, errors.New("not in host proxy allowlist")
	}
	// Loopback, link-local and unspecified targets are never reachable through
	// the generic VPN proxy. Host MCP access requires its own explicit tunnel.
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, h)
	if err != nil {
		return nil, err
	}
	for _, ip := range ips {
		if ip.IP.IsLoopback() || ip.IP.IsUnspecified() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsMulticast() {
			return nil, errors.New("local host address is blocked; use mcp add --host")
		}
	}
	var last error
	for _, ip := range ips {
		_, p, _ := net.SplitHostPort(addr)
		up, e := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", net.JoinHostPort(ip.IP.String(), p))
		if e == nil {
			log.Printf("HOST %s", strconv.Quote(addr))
			return up, nil
		}
		last = e
	}
	if last == nil {
		last = errors.New("no DNS addresses")
	}
	return nil, last
}
