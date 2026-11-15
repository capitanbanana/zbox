package app

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func testApp(t *testing.T) *App {
	t.Helper()
	a, e := New(t.TempDir(), "test", false)
	if e != nil {
		t.Fatal(e)
	}
	if e = a.save(Config{Version: 1, Token: "test", CPUs: 2, Memory: 4, Disk: 20}); e != nil {
		t.Fatal(e)
	}
	return a
}
func TestTemplateHasNoImplicitExports(t *testing.T) {
	a := testApp(t)
	c, _ := a.load()
	b, e := a.template(c)
	if e != nil {
		t.Fatal(e)
	}
	var v map[string]any
	if e = json.Unmarshal(b, &v); e != nil {
		t.Fatal(e)
	}
	if len(v["mounts"].([]any)) != 0 {
		t.Fatal("implicit mounts")
	}
	for _, base := range v["base"].([]any) {
		if strings.Contains(base.(string), "mounts") {
			t.Fatal("inherits host home")
		}
	}
	ssh := v["ssh"].(map[string]any)
	if ssh["forwardAgent"] != false || ssh["loadDotSSHPubKeys"] != false {
		t.Fatal("implicit SSH credentials")
	}
	if v["propagateProxyEnv"] != false {
		t.Fatal("host env propagation")
	}
}
func TestMountBoundaries(t *testing.T) {
	c := Config{Mounts: []Mount{{Host: "/host/project", Guest: "/home/agent/dev/project"}}}
	for _, m := range []Mount{{Host: "/host/project/sub", Guest: "/mnt/other"}, {Host: "/host", Guest: "/mnt/other"}, {Host: "/other", Guest: "/home/agent/dev/project/sub"}} {
		if validateMount(c, m) == nil {
			t.Fatal("accepted overlap", m)
		}
	}
	if e := validateMount(c, Mount{Host: "/host/project2", Guest: "/mnt/other"}); e != nil {
		t.Fatal(e)
	}
	a := testApp(t)
	if a.validateHostExport(filepath.Dir(a.Root)) == nil {
		t.Fatal("exported state ancestor")
	}
}
func TestGuestMountDestinations(t *testing.T) {
	for _, x := range []struct{ in, want string }{{"", "/home/agent/dev/project"}, {"foo", "/home/agent/dev/foo"}, {"~/dev/x", "/home/agent/dev/x"}, {"/mnt/x", "/mnt/x"}} {
		g, e := guestPath(x.in, "/host/project", "")
		if e != nil || g != x.want {
			t.Fatalf("%q -> %q %v", x.in, g, e)
		}
	}
	for _, p := range []string{"/", "~", "/etc", "/home/agent/.ssh", "../../.ssh"} {
		if _, e := guestPath(p, "/host/project", ""); e == nil {
			t.Fatal("accepted", p)
		}
	}
}
func TestGuestMountDestinationsFollowPersonaHome(t *testing.T) {
	g, e := guestPath("", "/host/project", "/home/mirak42")
	if e != nil || g != "/home/mirak42/dev/project" {
		t.Fatalf("persona default mount -> %q %v", g, e)
	}
	if _, e := guestPath("/home/agent/dev/x", "/host/project", "/home/mirak42"); e == nil {
		t.Fatal("accepted a path outside the persona home")
	}
}
func TestRuleTableTruncationKeepsRunes(t *testing.T) {
	long := "Лукшто Дмитрий Очень Длинное Имя"
	cut := truncateRunes(long, 10)
	if !utf8.ValidString(cut) {
		t.Fatalf("truncation produced invalid UTF-8: %q", cut)
	}
	if utf8.RuneCountInString(cut) != 10 {
		t.Fatalf("truncated to %d runes: %q", utf8.RuneCountInString(cut), cut)
	}
	if got := truncateRunes("short", 10); got != "short" {
		t.Fatalf("short value changed: %q", got)
	}
}
func TestArgumentQuoting(t *testing.T) {
	args := []string{"a b", "'quote'", "$(touch /tmp/agents-should-not-exist)", "`id`", "", "line\nbreak", "--flag=value"}
	script := "python3 -c 'import sys,json; print(json.dumps(sys.argv[1:]))' " + commandLine(args)
	b, e := exec.Command("sh", "-c", script).Output()
	if e != nil {
		t.Fatal(e)
	}
	var got []string
	if e = json.Unmarshal(b, &got); e != nil {
		t.Fatal(e)
	}
	if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
		t.Fatalf("changed args: %#v", got)
	}
}
func TestInteractiveNetworkPersistsAndValidates(t *testing.T) {
	a := testApp(t)
	var out bytes.Buffer
	e := a.networkInteractive(strings.NewReader("+ *.selectel.ru\n+ https://bad\n+ git.corp\n- git.corp\nquit\n"), &out)
	if e != nil {
		t.Fatal(e)
	}
	c, _ := a.load()
	if len(c.Hosts) != 1 || c.Hosts[0] != "*.selectel.ru" {
		t.Fatal(c.Hosts)
	}
	if !strings.Contains(out.String(), "Error:") {
		t.Fatal("missing invalid rule error")
	}
	st, _ := os.Stat(a.configPath())
	if st.Mode().Perm() != 0600 {
		t.Fatal("config not private")
	}
}
func TestMCPHostIsExplicit(t *testing.T) {
	a := testApp(t)
	if e := a.addMCP(MCP{Name: "host-service", URL: "http://localhost:8080/mcp", Host: true}); e != nil {
		t.Fatal(e)
	}
	c, _ := a.load()
	m := c.MCPs[0]
	if guestMCPURL(m) != "http://localhost:19000/mcp" {
		t.Fatal(guestMCPURL(m))
	}
	args := strings.Join(a.tunnelArgs(c, daemonState{SOCKS: "127.0.0.1:1", Control: "127.0.0.1:2"}), " ")
	if !strings.Contains(args, "127.0.0.1:19000:127.0.0.1:8080") {
		t.Fatal(args)
	}
	m.Disabled = true
	c.MCPs[0] = m
	if strings.Contains(strings.Join(a.tunnelArgs(c, daemonState{}), " "), "19000") {
		t.Fatal("disabled tunnel enabled")
	}
	if e := validateMCP(MCP{Name: "x", URL: "http://corp:80", Host: true}); e == nil {
		t.Fatal("host tunnel accepted non-loopback")
	}
}
func TestMCPConfigs(t *testing.T) {
	c := Config{MCPs: []MCP{{Name: "stdio", Command: []string{"npx", "-y", "package", "arg with spaces"}}, {Name: "remote", URL: "https://example.com/mcp"}, {Name: "off", URL: "https://off.test", Disabled: true}}}
	var v map[string]any
	if e := json.Unmarshal(claudeMCP(c), &v); e != nil {
		t.Fatal(e)
	}
	servers := v["mcpServers"].(map[string]any)
	if len(servers) != 2 {
		t.Fatal(servers)
	}
	args := mcpArgs("codex", c)
	if strings.Contains(args[1], "off.test") || !strings.Contains(args[1], `"arg with spaces"`) {
		t.Fatal(args)
	}
}
func TestMutationsAreLocked(t *testing.T) {
	a := testApp(t)
	unlock, e := a.lock()
	if e != nil {
		t.Fatal(e)
	}
	defer unlock()
	if u, e := a.lock(); e == nil {
		u()
		t.Fatal("acquired concurrent mutation lock")
	}
}

func TestStateSymlinkCannotBypassExportProtection(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if e := os.Mkdir(real, 0700); e != nil {
		t.Fatal(e)
	}
	alias := filepath.Join(dir, "alias")
	if e := os.Symlink(real, alias); e != nil {
		t.Fatal(e)
	}
	a, e := New(filepath.Join(alias, "not-created-yet"), "test", false)
	if e != nil {
		t.Fatal(e)
	}
	if a.validateHostExport(real) == nil {
		t.Fatal("symlink bypassed state export protection")
	}
}

func TestRunningMountChangeRestartsAutomaticallyAndPreservesConfigOnFailure(t *testing.T) {
	a := testApp(t)
	bin := t.TempDir()
	fake := filepath.Join(bin, "limactl")
	log := filepath.Join(bin, "calls")
	if e := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$1\" >> "+quote(log)+"\nif [ \"$1\" = list ]; then printf '{\"status\":\"Running\"}'; else exit 99; fi\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c, _ := a.load()
	c.Mounts = []Mount{{Host: "/host/path", Guest: "/mnt/path"}}
	if e := a.changeMounts(c); e == nil || !strings.Contains(e.Error(), "exit status 99") {
		t.Fatalf("unexpected result %v", e)
	}
	if calls, err := os.ReadFile(log); err != nil || !strings.Contains(string(calls), "stop\n") {
		t.Fatalf("did not automatically stop for mount change: %v %s", err, calls)
	}
	saved, _ := a.load()
	if len(saved.Mounts) != 0 {
		t.Fatal("saved mount despite failed stop")
	}
}

func TestHostProxyPolicyCannotGrantLoopback(t *testing.T) {
	a := testApp(t)
	for _, addr := range []string{"127.0.0.1:80", "localhost:80", "unlisted.invalid:443"} {
		if c, e := a.hostDial(context.Background(), addr); e == nil {
			c.Close()
			t.Fatal("unlisted target accepted", addr)
		}
	}
	c, _ := a.load()
	c.Hosts = []string{"127.0.0.1", "localhost", "::1", "0.0.0.0", "169.254.169.254"}
	if e := a.save(c); e != nil {
		t.Fatal(e)
	}
	for _, addr := range []string{"127.0.0.1:80", "localhost:80", "[::1]:80", "0.0.0.0:80", "169.254.169.254:80"} {
		conn, e := a.hostDial(context.Background(), addr)
		if e == nil {
			conn.Close()
			t.Fatal("local host target accepted", addr)
		}
		if !strings.Contains(e.Error(), "blocked") {
			t.Fatalf("expected policy block for %s, got %v", addr, e)
		}
	}
}

func TestDevNullIsNotTerminal(t *testing.T) {
	f, e := os.Open(os.DevNull)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	if isTerminal(f) {
		t.Fatal("/dev/null mistaken for a TTY")
	}
}
