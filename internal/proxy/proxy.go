// Package proxy provides TCP-only SOCKS5 and HTTP CONNECT proxies.
package proxy

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type DialFunc func(context.Context, string) (net.Conn, error)

func Relay(a, b net.Conn) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(a, b)
		if c, ok := a.(interface{ CloseWrite() error }); ok {
			_ = c.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(b, a)
	if c, ok := b.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	}
	// Do not retain a half-closed stream forever.
	_ = a.SetDeadline(time.Now().Add(30 * time.Second))
	_ = b.SetDeadline(time.Now().Add(30 * time.Second))
	<-done
}

func ServeSOCKS(l net.Listener, token string, dial DialFunc) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() { defer c.Close(); _ = socks(c, token, dial) }()
	}
}

func socks(c net.Conn, token string, dial DialFunc) error {
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	h := make([]byte, 2)
	if _, err := io.ReadFull(c, h); err != nil {
		return err
	}
	if h[0] != 5 || h[1] == 0 {
		return errors.New("invalid SOCKS greeting")
	}
	methods := make([]byte, int(h[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return err
	}
	method := byte(0)
	if token != "" {
		method = 2
	}
	found := false
	for _, m := range methods {
		if m == method {
			found = true
		}
	}
	if !found {
		_, _ = c.Write([]byte{5, 255})
		return errors.New("authentication required")
	}
	if _, err := c.Write([]byte{5, method}); err != nil {
		return err
	}
	if method == 2 {
		if _, err := io.ReadFull(c, h); err != nil {
			return err
		}
		if h[0] != 1 {
			return errors.New("bad auth")
		}
		u := make([]byte, int(h[1]))
		if _, err := io.ReadFull(c, u); err != nil {
			return err
		}
		if _, err := io.ReadFull(c, h[:1]); err != nil {
			return err
		}
		p := make([]byte, int(h[0]))
		if _, err := io.ReadFull(c, p); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare(p, []byte(token)) != 1 {
			_, _ = c.Write([]byte{1, 1})
			return errors.New("bad token")
		}
		if _, err := c.Write([]byte{1, 0}); err != nil {
			return err
		}
	}
	r := make([]byte, 4)
	if _, err := io.ReadFull(c, r); err != nil {
		return err
	}
	if r[0] != 5 || r[1] != 1 || r[2] != 0 {
		_, _ = c.Write([]byte{5, 7, 0, 1, 0, 0, 0, 0, 0, 0})
		return errors.New("only CONNECT is supported")
	}
	host, err := readHost(c, r[3])
	if err != nil {
		return err
	}
	if _, err = io.ReadFull(c, h); err != nil {
		return err
	}
	port := binary.BigEndian.Uint16(h)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	up, err := dial(ctx, net.JoinHostPort(host, strconv.Itoa(int(port))))
	if err != nil {
		_, _ = c.Write([]byte{5, 2, 0, 1, 0, 0, 0, 0, 0, 0})
		return err
	}
	defer up.Close()
	if _, err = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return err
	}
	_ = c.SetDeadline(time.Time{})
	Relay(c, up)
	return nil
}

func readHost(r io.Reader, typ byte) (string, error) {
	var n int
	switch typ {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		b := []byte{0}
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		n = int(b[0])
		if n == 0 {
			return "", errors.New("empty hostname")
		}
	default:
		return "", errors.New("unsupported address type")
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	if typ == 3 {
		return string(b), nil
	}
	return net.IP(b).String(), nil
}

// SOCKSDial always transmits the original DNS name; DNS is resolved on the host.
func SOCKSDial(ctx context.Context, endpoint, token, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, errors.New("invalid port")
	}
	if len(host) > 255 {
		return nil, errors.New("hostname too long")
	}
	c, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	stopCancel := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopCancel()
	ok := false
	defer func() {
		if !ok {
			c.Close()
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	if d, yes := ctx.Deadline(); yes && d.Before(deadline) {
		deadline = d
	}
	_ = c.SetDeadline(deadline)
	m := byte(0)
	if token != "" {
		m = 2
	}
	if _, err = c.Write([]byte{5, 1, m}); err != nil {
		return nil, err
	}
	b := make([]byte, 2)
	if _, err = io.ReadFull(c, b); err != nil {
		return nil, err
	}
	if b[0] != 5 || b[1] != m {
		return nil, errors.New("SOCKS auth negotiation failed")
	}
	if m == 2 {
		if len(token) > 255 {
			return nil, errors.New("token too long")
		}
		a := append([]byte{1, 6}, []byte("agents")...)
		a = append(a, byte(len(token)))
		a = append(a, []byte(token)...)
		if _, err = c.Write(a); err != nil {
			return nil, err
		}
		if _, err = io.ReadFull(c, b); err != nil {
			return nil, err
		}
		if b[0] != 1 || b[1] != 0 {
			return nil, errors.New("SOCKS authentication failed")
		}
	}
	request := append([]byte{5, 1, 0, 3, byte(len(host))}, []byte(host)...)
	request = append(request, byte(p>>8), byte(p))
	if _, err = c.Write(request); err != nil {
		return nil, err
	}
	h := make([]byte, 4)
	if _, err = io.ReadFull(c, h); err != nil {
		return nil, err
	}
	if h[0] != 5 || h[1] != 0 {
		return nil, fmt.Errorf("SOCKS connection denied (code %d)", h[1])
	}
	if _, err = readHost(c, h[3]); err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(c, b); err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	if !stopCancel() {
		return nil, ctx.Err()
	}
	ok = true
	return c, nil
}

type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c bufferedConn) Read(p []byte) (int, error) { return c.r.Read(p) }

func HTTPHandler(dial DialFunc) http.Handler {
	transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, addr) }, DisableKeepAlives: true, ResponseHeaderTimeout: 30 * time.Second}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			if _, _, err := net.SplitHostPort(r.Host); err != nil {
				http.Error(w, "host:port required", 400)
				return
			}
			up, err := dial(r.Context(), r.Host)
			if err != nil {
				http.Error(w, "connection denied or unavailable", 502)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				up.Close()
				http.Error(w, "hijack unavailable", 500)
				return
			}
			c, rw, err := hj.Hijack()
			if err != nil {
				up.Close()
				return
			}
			if _, err = rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
				c.Close()
				up.Close()
				return
			}
			if err = rw.Flush(); err != nil {
				c.Close()
				up.Close()
				return
			}
			Relay(bufferedConn{c, rw.Reader}, up)
			return
		}
		if r.URL.Scheme != "http" || r.URL.Host == "" {
			http.Error(w, "absolute HTTP proxy URL required", 400)
			return
		}
		r = r.Clone(r.Context())
		r.RequestURI = ""
		r.Close = true
		stripHop(r.Header)
		resp, err := transport.RoundTrip(r)
		if err != nil {
			http.Error(w, "connection denied or unavailable", 502)
			return
		}
		defer resp.Body.Close()
		stripHop(resp.Header)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})
}

func stripHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, k := range strings.Split(v, ",") {
			h.Del(strings.TrimSpace(k))
		}
	}
	for _, k := range []string{"Connection", "Proxy-Connection", "Proxy-Authorization", "Proxy-Authenticate", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		h.Del(k)
	}
}
