package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/capitanbanana/zbox/internal/proxy"
)

func TestSplitRoutingHotUpdatesAndFailure(t *testing.T) {
	var enabled atomic.Bool
	var unavailable atomic.Bool
	var calls atomic.Int32
	policy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Error("missing auth")
		}
		if unavailable.Load() {
			http.Error(w, "down", 503)
			return
		}
		p := Policy{}
		if enabled.Load() {
			p.Hosts = []string{"127.0.0.1", "*.corp.invalid"}
		}
		_ = json.NewEncoder(w).Encode(p)
	}))
	defer policy.Close()
	dest, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer dest.Close()
	go func() {
		for {
			c, e := dest.Accept()
			if e != nil {
				return
			}
			c.Close()
		}
	}()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	go func() {
		_ = proxy.ServeSOCKS(l, "secret", func(ctx context.Context, addr string) (net.Conn, error) {
			calls.Add(1)
			if addr != "service.corp.invalid:443" && addr != dest.Addr().String() {
				return nil, fmt.Errorf("wrong hostname %s", addr)
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", dest.Addr().String())
		})
	}()
	dial := splitDial(&http.Client{Timeout: time.Second}, policy.URL, l.Addr().String(), "secret")
	check := func(addr string) {
		t.Helper()
		c, e := dial(context.Background(), addr)
		if e != nil {
			t.Fatal(e)
		}
		c.Close()
	}
	check(dest.Addr().String())
	if calls.Load() != 0 {
		t.Fatal("direct route used host")
	}
	enabled.Store(true)
	check(dest.Addr().String())
	check("service.corp.invalid:443")
	if calls.Load() != 2 {
		t.Fatal("policy update did not affect routing")
	}
	enabled.Store(false)
	check(dest.Addr().String())
	if calls.Load() != 2 {
		t.Fatal("removed rule still active")
	}
	unavailable.Store(true)
	if c, e := dial(context.Background(), dest.Addr().String()); e == nil {
		c.Close()
		t.Fatal("fell back to direct with unavailable policy")
	}
}
