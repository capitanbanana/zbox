package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/capitanbanana/zbox/internal/gateway"
	"github.com/capitanbanana/zbox/internal/hostnet"
	"github.com/capitanbanana/zbox/internal/vpn"
)

func detectedHome(context.Context) (hostnet.Network, error) {
	return hostnet.Network{Identity: hostnet.Identity{Subnet: "10.77.0.0/24", Gateway: "10.77.0.1", MAC: "02:00:00:00:00:01"}, Interface: "en0", Index: 6, Source: "10.77.0.10"}, nil
}

func TestBypassCLIAndVPNLifecycle(t *testing.T) {
	a := testApp(t)
	if err := a.vpnBypassWithDetector([]string{"add", "home"}, detectedHome); err != nil {
		t.Fatal(err)
	}
	c, err := a.load()
	if err != nil || len(c.VPNBypass) != 1 {
		t.Fatal(c.VPNBypass, err)
	}
	st, err := os.Stat(a.configPath())
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("config permissions", err)
	}
	for _, name := range []string{"home", "duplicate-lan", "../bad"} {
		if err := a.vpnBypassWithDetector([]string{"add", name}, detectedHome); err == nil {
			t.Fatal("accepted duplicate or invalid name", name)
		}
	}
	fakeVPNTools(t, "Stopped", "exit 0")
	for _, cmd := range []string{"add", "update", "delete"} {
		args := []string{cmd}
		if cmd != "delete" {
			args = append(args, dummyVPN)
		}
		if err := a.vpn(args); err != nil {
			t.Fatal(err)
		}
		c, err = a.load()
		if err != nil || len(c.VPNBypass) != 1 {
			t.Fatal("lost bypass rules", err)
		}
	}
	if err := a.vpnBypassWithDetector([]string{"list"}, detectedHome); err != nil {
		t.Fatal(err)
	}
	if err := a.vpnBypassWithDetector([]string{"delete", "home"}, detectedHome); err != nil {
		t.Fatal(err)
	}
	c, _ = a.load()
	if len(c.VPNBypass) != 0 {
		t.Fatal("rule not deleted")
	}
}

func TestBypassCannotLearnUnknownNetwork(t *testing.T) {
	for _, detect := range []func(context.Context) (hostnet.Network, error){
		func(context.Context) (hostnet.Network, error) { return hostnet.Network{}, errors.New("no route") },
		func(context.Context) (hostnet.Network, error) { return hostnet.Network{}, nil },
	} {
		a := testApp(t)
		if err := a.vpnBypassWithDetector([]string{"add", "home"}, detect); err == nil {
			t.Fatal("trusted unknown network")
		}
		c, _ := a.load()
		if len(c.VPNBypass) != 0 {
			t.Fatal("saved unknown network")
		}
	}
}

func TestBypassChangesRefreshRunningGateway(t *testing.T) {
	a := testApp(t)
	// Darwin Unix sockets have a short pathname limit; t.TempDir includes the
	// whole test name, so use a dedicated short root for the live API socket.
	root, err := os.MkdirTemp("/tmp", "ab-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	a.Root = root
	if err := os.MkdirAll(a.networkDir(), 0700); err != nil {
		t.Fatal(err)
	}
	c, _ := a.load()
	c.VPN, _ = vpn.Parse(dummyVPN)
	if err := a.save(c); err != nil {
		t.Fatal(err)
	}
	// No limactl is involved: these changes must not restart sessions.
	listener, err := gateway.ListenUnix(a.networkSocket("ep"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	seen := make(chan int, 2)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/agents/refresh" {
			http.Error(w, "unexpected", 400)
			return
		}
		latest, err := a.load()
		if err != nil {
			http.Error(w, "config unavailable", 500)
			return
		}
		seen <- len(latest.VPNBypass)
		_ = json.NewEncoder(w).Encode(gatewayHealth{Mode: "agents-vpn-v1", Status: hostnet.Status{Egress: "vpn"}})
	})}
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	for i, cmd := range []string{"add", "delete"} {
		if err := a.vpnBypassWithDetector([]string{cmd, "home"}, detectedHome); err != nil {
			t.Fatal(err)
		}
		select {
		case n := <-seen:
			if n != 1-i {
				t.Fatal("refresh before save", n)
			}
		case <-time.After(time.Second):
			t.Fatal("no live refresh")
		}
	}
}
