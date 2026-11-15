package proxy

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"
)

func TestSOCKSUDPAssociationAndControlLifetime(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	packet, e := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if e != nil {
		t.Fatal(e)
	}
	defer packet.Close()
	stop := make(chan struct{})
	defer close(stop)
	closed := make(chan struct{})
	go func() {
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		b := make([]byte, 3)
		_, _ = io.ReadFull(c, b)
		if string(b) != string([]byte{5, 1, 2}) {
			return
		}
		_, _ = c.Write([]byte{5, 2})
		auth := make([]byte, 2)
		_, _ = io.ReadFull(c, auth)
		name := make([]byte, int(auth[1]))
		_, _ = io.ReadFull(c, name)
		_, _ = io.ReadFull(c, auth[:1])
		token := make([]byte, int(auth[0]))
		_, _ = io.ReadFull(c, token)
		if string(name) != "agents" || string(token) != "test-token" {
			return
		}
		_, _ = c.Write([]byte{1, 0})
		request := make([]byte, 10)
		_, _ = io.ReadFull(c, request)
		if request[1] != 3 {
			return
		}
		reply := []byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint16(reply[8:], uint16(packet.LocalAddr().(*net.UDPAddr).Port))
		_, _ = c.Write(reply)
		<-closed
	}()
	go func() {
		b := make([]byte, 1024)
		_ = packet.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, peer, e := packet.ReadFromUDP(b)
		if e == nil {
			_, _ = packet.WriteToUDP(b[:n], peer)
		}
		<-stop
	}()
	c, e := SOCKSUDP(context.Background(), l.Addr().String(), "test-token", "198.51.100.5:9999")
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, e = c.Write([]byte("hello")); e != nil {
		t.Fatal(e)
	}
	b := make([]byte, 64)
	n, e := c.Read(b)
	if e != nil || string(b[:n]) != "hello" {
		t.Fatal("datagram mismatch", e)
	}
	close(closed)
	if _, e = c.Read(b); e == nil {
		t.Fatal("UDP survived loss of SOCKS control channel")
	}
}
