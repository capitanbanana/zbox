package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func echo(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	return l.Addr().String()
}
func socksServer(t *testing.T, token string, d DialFunc) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close() })
	go func() { _ = ServeSOCKS(l, token, d) }()
	return l.Addr().String()
}
func TestSOCKSAuthDomainAndDeny(t *testing.T) {
	target := echo(t)
	var called atomic.Int32
	endpoint := socksServer(t, "secret", func(ctx context.Context, addr string) (net.Conn, error) {
		called.Add(1)
		if addr != "corp.example:443" {
			return nil, fmt.Errorf("denied")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", target)
	})
	for _, token := range []string{"", "wrong"} {
		if c, e := SOCKSDial(context.Background(), endpoint, token, "corp.example:443"); e == nil {
			c.Close()
			t.Fatal("accepted bad auth")
		}
	}
	if called.Load() != 0 {
		t.Fatal("dialed before authentication")
	}
	if c, e := SOCKSDial(context.Background(), endpoint, "secret", "other.example:443"); e == nil {
		c.Close()
		t.Fatal("bypassed allowlist")
	}
	c, e := SOCKSDial(context.Background(), endpoint, "secret", "corp.example:443")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("hello"))
	b := make([]byte, 5)
	if _, e = io.ReadFull(c, b); e != nil || string(b) != "hello" {
		t.Fatalf("echo %q %v", b, e)
	}
}
func TestHTTPProxy(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" {
			t.Error("leaked proxy credentials")
		}
		fmt.Fprint(w, "ok:"+r.URL.Path)
	}))
	defer origin.Close()
	dial := func(ctx context.Context, a string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", a)
	}
	p := httptest.NewServer(HTTPHandler(dial))
	defer p.Close()
	u, _ := url.Parse(p.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}}
	req, _ := http.NewRequest("GET", origin.URL+"/test", nil)
	req.Header.Set("Proxy-Authorization", "secret")
	resp, e := client.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok:/test" {
		t.Fatal(string(b))
	}
}
func TestCONNECTPreservesBufferedBytes(t *testing.T) {
	target := echo(t)
	p := httptest.NewServer(HTTPHandler(func(ctx context.Context, a string) (net.Conn, error) {
		if a != target {
			return nil, fmt.Errorf("bad destination")
		}
		return (&net.Dialer{}).DialContext(ctx, "tcp", a)
	}))
	defer p.Close()
	c, e := net.Dial("tcp", strings.TrimPrefix(p.URL, "http://"))
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(2 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\nhello", target, target)
	r := bufio.NewReader(c)
	resp, e := http.ReadResponse(r, &http.Request{Method: "CONNECT"})
	if e != nil || resp.StatusCode != 200 {
		t.Fatalf("CONNECT: %v %v", resp, e)
	}
	b := make([]byte, 5)
	if _, e = io.ReadFull(r, b); e != nil || string(b) != "hello" {
		t.Fatalf("buffered bytes: %q %v", b, e)
	}
}
func TestSOCKSRejectsUDP(t *testing.T) {
	endpoint := socksServer(t, "", func(context.Context, string) (net.Conn, error) { t.Error("dialed UDP"); return nil, fmt.Errorf("no") })
	c, e := net.Dial("tcp", endpoint)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte{5, 1, 0})
	b := make([]byte, 2)
	_, _ = io.ReadFull(c, b)
	_, _ = c.Write([]byte{5, 3, 0, 1})
	b = make([]byte, 10)
	_, e = io.ReadFull(c, b)
	if e != nil || b[1] != 7 {
		t.Fatalf("UDP not rejected: %v %v", b, e)
	}
}
