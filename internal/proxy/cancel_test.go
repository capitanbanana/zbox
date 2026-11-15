package proxy

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestCancellationInterruptsSOCKSHandshake(t *testing.T) {
	for _, kind := range []string{"tcp", "udp"} {
		t.Run(kind, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			started := make(chan struct{})
			go func() {
				c, err := listener.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				if _, err = io.ReadFull(c, make([]byte, 3)); err != nil {
					return
				}
				close(started)
				_, _ = io.Copy(io.Discard, c) // Never finish the SOCKS greeting.
			}()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var c net.Conn
				var err error
				if kind == "tcp" {
					c, err = SOCKSDial(ctx, listener.Addr().String(), "token", "example.com:443")
				} else {
					c, err = SOCKSUDP(ctx, listener.Addr().String(), "token", "1.1.1.1:53")
				}
				if c != nil {
					c.Close()
				}
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("no handshake")
			}
			cancel()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("canceled handshake succeeded")
				}
			case <-time.After(time.Second):
				t.Fatal("canceled handshake remained open")
			}
		})
	}
}
