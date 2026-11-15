package hostnet

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func testRouter(t *testing.T) (*Router, *Network, *[]Exception, *error, *error, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	n := homeNetwork()
	rules := []Exception{{Name: "home", Identity: n.Identity}}
	var detectErr, rulesErr error
	var direct, vpn atomic.Int32
	dial := func(counter *atomic.Int32) (net.Conn, error) {
		counter.Add(1)
		a, b := net.Pipe()
		t.Cleanup(func() { b.Close() })
		return a, nil
	}
	r := &Router{
		Detect: func(context.Context) (Network, error) { return n, detectErr },
		Rules:  func() ([]Exception, error) { return rules, rulesErr },
		Direct: func(context.Context, Network, string, string) (net.Conn, error) { return dial(&direct) },
		TCP:    func(context.Context, string) (net.Conn, error) { return dial(&vpn) },
		UDP:    func(context.Context, string) (net.Conn, error) { return dial(&vpn) },
	}
	t.Cleanup(r.Close)
	return r, &n, &rules, &detectErr, &rulesErr, &direct, &vpn
}

func assertClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("connection survived policy change")
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("connection was not closed")
	}
}

func TestRouterTransitionsCloseTCPAndUDP(t *testing.T) {
	r, n, rules, _, _, direct, vpn := testRouter(t)
	ctx := context.Background()
	a, err := r.Dial(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Dial(ctx, "udp", "1.1.1.1:53")
	if err != nil {
		t.Fatal(err)
	}
	if direct.Load() != 2 || vpn.Load() != 0 || r.Status().Bypass != "home" {
		t.Fatal(r.Status())
	}
	n.MAC = "02:00:00:00:00:02" // Same subnet, different router.
	if s := r.Refresh(ctx); s.Egress != "vpn" {
		t.Fatal(s)
	}
	assertClosed(t, a)
	assertClosed(t, b)
	c, err := r.Dial(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	if vpn.Load() != 1 {
		t.Fatal("not routed through VPN")
	}
	*n = homeNetwork()
	if s := r.Refresh(ctx); s.Egress != "direct" {
		t.Fatal(s)
	}
	assertClosed(t, c)
	d, err := r.Dial(ctx, "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	*rules = nil
	if s := r.Refresh(ctx); s.Egress != "vpn" {
		t.Fatal(s)
	}
	assertClosed(t, d)
}

func TestRouterUnknownOrUnreadableNeverDirect(t *testing.T) {
	for _, cause := range []string{"detect", "rules", "invalid"} {
		t.Run(cause, func(t *testing.T) {
			r, n, _, detectErr, rulesErr, direct, vpn := testRouter(t)
			c, err := r.Dial(context.Background(), "tcp", "example.com:443")
			if err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "detect":
				*detectErr = errors.New("offline")
			case "rules":
				*rulesErr = errors.New("bad config")
			case "invalid":
				n.MAC = ""
			}
			d, err := r.Dial(context.Background(), "udp", "1.1.1.1:53")
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			assertClosed(t, c)
			if direct.Load() != 1 || vpn.Load() != 1 || r.Status().Egress != "vpn" {
				t.Fatal("failure opened direct access")
			}
		})
	}
}

func TestRouterNoFallbackAndHomeWorksWithDeadVPN(t *testing.T) {
	r, n, _, _, _, direct, _ := testRouter(t)
	dead := func(context.Context, string) (net.Conn, error) { return nil, errors.New("VPN dead") }
	r.TCP, r.UDP = dead, dead
	c, err := r.Dial(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal("home depends on VPN", err)
	}
	c.Close()
	n.MAC = "02:00:00:00:00:02"
	for _, protocol := range []string{"tcp", "udp"} {
		if c, err := r.Dial(context.Background(), protocol, "1.1.1.1:53"); err == nil {
			c.Close()
			t.Fatal("VPN failure bypassed")
		}
	}
	if direct.Load() != 1 {
		t.Fatal("direct fallback")
	}
	*n = homeNetwork()
	r.Direct = func(context.Context, Network, string, string) (net.Conn, error) {
		return nil, errors.New("direct failed")
	}
	var fallback atomic.Bool
	r.TCP = func(context.Context, string) (net.Conn, error) {
		fallback.Store(true)
		return nil, errors.New("fallback")
	}
	if _, err := r.Dial(context.Background(), "tcp", "example.com:443"); err == nil || fallback.Load() {
		t.Fatal("direct failure retried via VPN")
	}
}

func TestRouterChangeCancelsPendingDialAndRejectsLateSuccess(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "late-success"}[late], func(t *testing.T) {
			r, n, _, _, _, _, _ := testRouter(t)
			started, done := make(chan struct{}), make(chan error, 1)
			var peer net.Conn
			r.Direct = func(ctx context.Context, _ Network, _, _ string) (net.Conn, error) {
				close(started)
				<-ctx.Done()
				if late {
					var a net.Conn
					a, peer = net.Pipe()
					return a, nil
				}
				return nil, ctx.Err()
			}
			go func() {
				c, e := r.Dial(context.Background(), "tcp", "1.1.1.1:443")
				if c != nil {
					c.Close()
				}
				done <- e
			}()
			<-started
			n.Source = "10.77.0.11" // Even a new address on the trusted LAN resets flows.
			r.Refresh(context.Background())
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("stale connection accepted")
				}
			case <-time.After(time.Second):
				t.Fatal("pending dial not canceled")
			}
			if peer != nil {
				defer peer.Close()
				assertClosed(t, peer)
			}
		})
	}
}

func TestWatchClosesIdleConnectionsWhenNetworkChanges(t *testing.T) {
	r, _, _, _, _, _, _ := testRouter(t)
	var away atomic.Bool
	r.Detect = func(context.Context) (Network, error) {
		n := homeNetwork()
		if away.Load() {
			n.MAC = "02:00:00:00:00:02"
		}
		return n, nil
	}
	c, err := r.Dial(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.Watch(ctx) }()
	defer func() { cancel(); <-done }()
	away.Store(true)
	_ = c.SetReadDeadline(time.Now().Add(4 * time.Second))
	_, err = c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("idle connection survived")
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("watcher did not close connection")
	}
	if r.Status().Egress != "vpn" {
		t.Fatal(r.Status())
	}
}

func TestCanceledRequestDoesNotInvalidateNetwork(t *testing.T) {
	r, _, _, _, _, direct, _ := testRouter(t)
	c, err := r.Dial(context.Background(), "tcp", "example.com:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Dial(ctx, "tcp", "example.com:443"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if direct.Load() != 1 || r.Status().Egress != "direct" {
		t.Fatal("canceled request altered egress")
	}
	r.mu.Lock()
	count := len(r.connections)
	r.mu.Unlock()
	if count != 1 {
		t.Fatal("canceled request closed active connection")
	}
}
