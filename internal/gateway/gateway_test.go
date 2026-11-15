package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func TestEthernetHasOnlyVPNForwarders(t *testing.T) {
	tcpCalls := make(chan string, 8)
	udpCalls := make(chan string, 8)
	n, e := New(func(_ context.Context, a string) (net.Conn, error) { tcpCalls <- a; return nil, errors.New("VPN down") }, func(_ context.Context, a string) (net.Conn, error) { udpCalls <- a; return nil, errors.New("VPN down") })
	if e != nil {
		t.Fatal(e)
	}
	defer n.Close()
	vm, wire := net.Pipe()
	defer vm.Close()
	defer wire.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = n.Switch.Accept(ctx, wire, types.QemuProtocol) }()
	go func() { _, _ = io.Copy(io.Discard, vm) }()
	send := func(dst string, protocol layers.IPProtocol) {
		t.Helper()
		sm, _ := net.ParseMAC("52:55:55:00:00:01")
		dm, _ := net.ParseMAC(mac)
		eth := &layers.Ethernet{SrcMAC: sm, DstMAC: dm, EthernetType: layers.EthernetTypeIPv4}
		ip := &layers.IPv4{Version: 4, IHL: 5, TTL: 64, SrcIP: net.ParseIP("192.168.105.10").To4(), DstIP: net.ParseIP(dst).To4(), Protocol: protocol}
		var transport gopacket.SerializableLayer
		if protocol == layers.IPProtocolTCP {
			v := &layers.TCP{SrcPort: 40000, DstPort: 443, SYN: true, Seq: 1234, Window: 65535}
			_ = v.SetNetworkLayerForChecksum(ip)
			transport = v
		} else {
			v := &layers.UDP{SrcPort: 40001, DstPort: 9999}
			_ = v.SetNetworkLayerForChecksum(ip)
			transport = v
		}
		buf := gopacket.NewSerializeBuffer()
		if e = gopacket.SerializeLayers(buf, gopacket.SerializeOptions{FixLengths: true, ComputeChecksums: true}, eth, ip, transport, gopacket.Payload("test")); e != nil {
			t.Fatal(e)
		}
		frame := buf.Bytes()
		header := make([]byte, 4)
		binary.BigEndian.PutUint32(header, uint32(len(frame)))
		_ = vm.SetWriteDeadline(time.Now().Add(time.Second))
		if _, e = vm.Write(append(header, frame...)); e != nil {
			t.Fatal(e)
		}
	}
	send("198.51.100.10", layers.IPProtocolTCP)
	select {
	case a := <-tcpCalls:
		if a != "198.51.100.10:443" {
			t.Fatal(a)
		}
	case <-time.After(time.Second):
		t.Fatal("TCP did not enter VPN dialer")
	}
	send("198.51.100.11", layers.IPProtocolUDP)
	select {
	case a := <-udpCalls:
		if a != "198.51.100.11:9999" {
			t.Fatal(a)
		}
	case <-time.After(time.Second):
		t.Fatal("UDP did not enter VPN dialer")
	}
	// A guest cannot reach host loopback or use the gateway as a generic proxy.
	send("127.0.0.1", layers.IPProtocolTCP)
	send(IP, layers.IPProtocolTCP)
	select {
	case a := <-tcpCalls:
		t.Fatal("local address escaped", a)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestDNSFailureNeverUsesSystemResolver(t *testing.T) {
	var calls []string
	// All upstream hostnames must go to the supplied VPN dialer. An unavailable
	// tunnel cannot cause DNS to fall back to net.DefaultResolver or host UDP.
	dial := func(_ context.Context, a string) (net.Conn, error) {
		calls = append(calls, a)
		return nil, errors.New("VPN down")
	}
	r := dnsQuestion()
	if _, e := resolveDNS(context.Background(), r, dial); e == nil {
		t.Fatal("DNS succeeded without VPN")
	}
	if len(calls) != 2 || calls[0] != "cloudflare-dns.com:443" || calls[1] != "dns.google:443" {
		t.Fatal(calls)
	}
}
