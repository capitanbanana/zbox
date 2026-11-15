package proxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

// SOCKSUDP opens an authenticated SOCKS5 UDP association. No packet can fall
// back to a direct destination if either the control or data channel fails.
func SOCKSUDP(ctx context.Context, endpoint, token, target string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(target)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 || len(host) > 255 || len(token) > 255 {
		return nil, errors.New("invalid SOCKS UDP address or token")
	}
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, "tcp", endpoint)
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
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	method := byte(0)
	if token != "" {
		method = 2
	}
	if _, err = c.Write([]byte{5, 1, method}); err != nil {
		return nil, err
	}
	b := make([]byte, 2)
	if _, err = io.ReadFull(c, b); err != nil {
		return nil, err
	}
	if b[0] != 5 || b[1] != method {
		return nil, errors.New("SOCKS UDP authentication rejected")
	}
	if method == 2 {
		msg := append([]byte{1, 6}, []byte("agents")...)
		msg = append(msg, byte(len(token)))
		msg = append(msg, token...)
		if _, err = c.Write(msg); err != nil {
			return nil, err
		}
		if _, err = io.ReadFull(c, b); err != nil {
			return nil, err
		}
		if b[0] != 1 || b[1] != 0 {
			return nil, errors.New("SOCKS UDP authentication failed")
		}
	}
	if _, err = c.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return nil, err
	}
	h := make([]byte, 4)
	if _, err = io.ReadFull(c, h); err != nil {
		return nil, err
	}
	if h[0] != 5 || h[1] != 0 {
		return nil, errors.New("SOCKS UDP association rejected")
	}
	relay, err := readHost(c, h[3])
	if err != nil {
		return nil, err
	}
	if _, err = io.ReadFull(c, b); err != nil {
		return nil, err
	}
	relayPort := binary.BigEndian.Uint16(b)
	peer, _, _ := net.SplitHostPort(endpoint)
	ip := net.ParseIP(relay)
	if ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
		return nil, errors.New("SOCKS UDP relay must be loopback")
	}
	if relayPort == 0 {
		return nil, errors.New("invalid SOCKS UDP relay port")
	}
	data, err := (&net.Dialer{}).DialContext(ctx, "udp", net.JoinHostPort(peer, strconv.Itoa(int(relayPort))))
	if err != nil {
		return nil, err
	}
	if !stopCancel() {
		data.Close()
		return nil, ctx.Err()
	}
	header := append([]byte{0, 0, 0, 3, byte(len(host))}, []byte(host)...)
	header = append(header, byte(p>>8), byte(p))
	u := &udpAssociation{Conn: data, control: c, header: header}
	_ = c.SetDeadline(time.Time{})
	go func() { _, _ = io.Copy(io.Discard, c); data.Close() }()
	ok = true
	return u, nil
}

type udpAssociation struct {
	net.Conn
	control net.Conn
	header  []byte
}

func (u *udpAssociation) Close() error { _ = u.control.Close(); return u.Conn.Close() }
func (u *udpAssociation) Write(b []byte) (int, error) {
	packet := append(append([]byte{}, u.header...), b...)
	_, err := u.Conn.Write(packet)
	if err != nil {
		return 0, err
	}
	return len(b), nil
}
func (u *udpAssociation) Read(b []byte) (int, error) {
	packet := make([]byte, 65535)
	n, err := u.Conn.Read(packet)
	if err != nil {
		return 0, err
	}
	packet = packet[:n]
	if n < 4 || packet[0] != 0 || packet[1] != 0 || packet[2] != 0 {
		return 0, errors.New("invalid or fragmented SOCKS UDP packet")
	}
	offset := 4
	switch packet[3] {
	case 1:
		offset += 4
	case 4:
		offset += 16
	case 3:
		if n < 5 {
			return 0, io.ErrUnexpectedEOF
		}
		offset += 1 + int(packet[4])
	default:
		return 0, errors.New("invalid SOCKS UDP address")
	}
	offset += 2
	if n < offset {
		return 0, io.ErrUnexpectedEOF
	}
	if n-offset > len(b) {
		return 0, io.ErrShortBuffer
	}
	return copy(b, packet[offset:]), nil
}
