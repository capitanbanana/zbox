package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agents-sandbox/internal/vpn"
)

func fakeVPNTools(t *testing.T, state, edit string) {
	t.Helper()
	bin := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'limactl version 2.1.1';;\nlist) echo '{\"status\":\"" + state + "\"}';;\nedit) " + edit + ";;\n*) exit 99;;\nesac\n"
	if e := os.WriteFile(filepath.Join(bin, "limactl"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(bin, "sing-box"), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AGENTS_SING_BOX", filepath.Join(bin, "sing-box"))
}

const dummyVPN = "vless://11111111-2222-4333-8444-555555555555@vpn.example:443?security=tls"

func TestVPNChangesRequireStoppedVM(t *testing.T) {
	a := testApp(t)
	fakeVPNTools(t, "Running", "exit 99")
	if e := a.vpn([]string{"add", dummyVPN}); e == nil || !strings.Contains(e.Error(), "restart") {
		t.Fatal(e)
	}
	c, _ := a.load()
	if c.VPN != nil {
		t.Fatal("saved VPN despite refusal")
	}
}

func TestVPNFailedEditKeepsFailClosedIntent(t *testing.T) {
	a := testApp(t)
	fakeVPNTools(t, "Stopped", "exit 99")
	if e := a.vpn([]string{"add", dummyVPN}); e == nil {
		t.Fatal("edit should fail")
	}
	c, _ := a.load()
	if c.VPN == nil {
		t.Fatal("partial enable forgot VPN intent")
	}
	if e := a.vpn([]string{"delete"}); e == nil {
		t.Fatal("delete edit should fail")
	}
	c, _ = a.load()
	if c.VPN == nil {
		t.Fatal("partial disable forgot VPN intent")
	}
}

func TestVPNLifecycleKeepsExceptionsAndPrivateConfig(t *testing.T) {
	a := testApp(t)
	fakeVPNTools(t, "Stopped", "exit 0")
	c, _ := a.load()
	c.Hosts = []string{"*.corp.example"}
	c.MCPs = []MCP{{Name: "host-service", URL: "http://127.0.0.1:8080/mcp", Host: true}}
	_ = a.save(c)
	for _, cmd := range []string{"add", "update", "delete"} {
		args := []string{cmd}
		if cmd != "delete" {
			args = append(args, dummyVPN)
		}
		if e := a.vpn(args); e != nil {
			t.Fatal(e)
		}
		c, _ = a.load()
		if len(c.Hosts) != 1 || len(c.MCPs) != 1 {
			t.Fatal("lost exceptions")
		}
		if (c.VPN == nil) != (cmd == "delete") {
			t.Fatal("wrong VPN state")
		}
		st, _ := os.Stat(a.configPath())
		if st.Mode().Perm() != 0600 {
			t.Fatal("credentials not private")
		}
	}
}

func TestVPNRejectsExtraNICAndHostDNS(t *testing.T) {
	a := testApp(t)
	fakeVPNTools(t, "Stopped", "exit 0")
	if e := a.installNetworkGuard(); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(a.Root, "lima", a.Name)
	_ = os.MkdirAll(dir, 0700)
	good := "networks:\n- lima: " + a.networkName() + "\nhostResolver:\n  enabled: false\ndns: [192.168.105.2]\n"
	for i, s := range []string{good, strings.Replace(good, "false", "true", 1), strings.Replace(good, "hostResolver:", "- vzNAT: true\nhostResolver:", 1), strings.Replace(good, "192.168.105.2", "1.1.1.1", 1)} {
		_ = os.WriteFile(filepath.Join(dir, "lima.yaml"), []byte(s), 0600)
		if e := a.verifyVPNNetwork(); (e == nil) != (i == 0) {
			t.Fatalf("case %d: %v", i, e)
		}
	}
	// The fallback guard survives Lima-style lease writes: a future stock
	// usernet cannot decode valid leases and start an unrestricted NIC.
	p := filepath.Join(a.networkDir(), "leases.json")
	if e := os.WriteFile(p, []byte(`{"192.168.105.1":"00:00:00:00:00:01"}`), 0600); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(p)
	if e != nil || len(b) != 0 {
		t.Fatal("fallback guard lost", e)
	}
	_ = os.Remove(p)
	if a.verifyNetworkGuard() == nil {
		t.Fatal("missing guard accepted")
	}
}

func TestVPNProfileRoundTrip(t *testing.T) {
	a := testApp(t)
	c, _ := a.load()
	c.VPN, _ = vpn.Parse(dummyVPN)
	if e := a.save(c); e != nil {
		t.Fatal(e)
	}
	r, e := a.load()
	if e != nil || r.VPN == nil || r.VPN.UUID != c.VPN.UUID {
		t.Fatal("profile lost", e)
	}
}
