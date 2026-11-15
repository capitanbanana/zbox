// Package gateway is the only Ethernet gateway of VPN-enabled Lima VMs.
// Its TCP/UDP forwarding functions use policy-controlled dialers: VPN unless
// the host recognizes an explicitly trusted LAN. IPv6 and external ICMP drop.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/capitanbanana/zbox/internal/proxy"
	"github.com/balajiv113/fd"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/forwarder"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/miekg/dns"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const IP = "192.168.105.2"
const Subnet = "192.168.105.0/24"
const mac = "5a:94:ef:e4:0c:dd"

type Network struct {
	Stack   *stack.Stack
	Switch  *tap.Switch
	Pool    *tap.IPPool
	DHCP    *dhcp.Server
	Ports   *forwarder.PortsForwarder
	closers []interface{ Close() error }
}

func New(tcpDial, udpDial proxy.DialFunc) (*Network, error) {
	n := &Network{}
	ok := false
	defer func() {
		if !ok {
			n.Close()
		}
	}()
	_, poolSubnet, _ := net.ParseCIDR(Subnet)
	pool := tap.NewIPPool(poolSubnet)
	n.Pool = pool
	pool.Reserve(net.ParseIP(IP), mac)
	link, err := tap.NewLinkEndpoint(false, 1500, mac, IP, []string{IP})
	if err != nil {
		return nil, err
	}
	n.Switch = tap.NewSwitch(false)
	link.Connect(n.Switch)
	n.Switch.Connect(link)
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol, icmp.NewProtocol4}})
	n.Stack = s
	if e := s.CreateNIC(1, link); e != nil {
		return nil, errors.New(e.String())
	}
	if e := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4Slice(net.ParseIP(IP).To4()).WithPrefix()}, stack.AddressProperties{}); e != nil {
		return nil, errors.New(e.String())
	}
	_ = s.SetSpoofing(1, true)
	_ = s.SetPromiscuousMode(1, true)
	_, cidr, _ := net.ParseCIDR(Subnet)
	sub, _ := tcpip.NewSubnet(tcpip.AddrFrom4Slice(cidr.IP.To4()), tcpip.MaskFromBytes(cidr.Mask))
	s.SetRouteTable([]tcpip.Route{{Destination: sub, NIC: 1}})
	tf := tcp.NewForwarder(s, 0, 128, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		if blocked(id.LocalAddress.String()) {
			r.Complete(true)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		up, e := tcpDial(ctx, net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort))))
		if e != nil {
			r.Complete(true)
			return
		}
		var q waiter.Queue
		ep, te := r.CreateEndpoint(&q)
		r.Complete(false)
		if te != nil {
			up.Close()
			return
		}
		go proxy.Relay(gonet.NewTCPConn(&q, ep), up)
	})
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tf.HandlePacket)
	uf := udp.NewForwarder(s, func(r *udp.ForwarderRequest) {
		id := r.ID()
		if blocked(id.LocalAddress.String()) {
			return
		}
		var q waiter.Queue
		ep, e := r.CreateEndpoint(&q)
		if e != nil {
			return
		}
		down := gonet.NewUDPConn(&q, ep)
		go func() {
			defer down.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			up, e := udpDial(ctx, net.JoinHostPort(id.LocalAddress.String(), strconv.Itoa(int(id.LocalPort))))
			if e != nil {
				return
			}
			defer up.Close()
			relayPackets(down, up)
		}()
	})
	s.SetTransportProtocolHandler(udp.ProtocolNumber, uf.HandlePacket)
	// No ICMP forwarder and no IPv6 protocol: neither can escape to the host.
	n.DHCP, err = dhcp.New(&types.Configuration{Subnet: Subnet, GatewayIP: IP, GatewayMacAddress: mac, MTU: 1500}, s, pool)
	if err != nil {
		return nil, err
	}
	go func() { _ = n.DHCP.Serve() }()
	n.Ports = forwarder.NewPortsForwarder(s)
	if err = n.startDNS(tcpDial); err != nil {
		return nil, err
	}
	ok = true
	return n, nil
}

func blocked(host string) bool {
	ip := net.ParseIP(host)
	_, sub, _ := net.ParseCIDR(Subnet)
	return ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || sub.Contains(ip) || host == "255.255.255.255"
}
func relayPackets(a, b net.Conn) {
	done := make(chan struct{})
	copyPackets := func(dst, src net.Conn) {
		buf := make([]byte, 65507)
		for {
			_ = src.SetReadDeadline(time.Now().Add(60 * time.Second))
			n, e := src.Read(buf)
			if e != nil {
				return
			}
			if _, e = dst.Write(buf[:n]); e != nil {
				return
			}
		}
	}
	go func() { copyPackets(a, b); a.Close(); b.Close(); close(done) }()
	copyPackets(b, a)
	a.Close()
	b.Close()
	<-done
}
func (n *Network) Close() {
	for _, c := range n.closers {
		_ = c.Close()
	}
	if n.DHCP != nil {
		_ = n.DHCP.Underlying.Close()
	}
	if n.Stack != nil {
		n.Stack.Close()
	}
}

func (n *Network) startDNS(dial proxy.DialFunc) error {
	addr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4Slice(net.ParseIP(IP).To4()), Port: 53}
	pc, e := gonet.DialUDP(n.Stack, &addr, nil, ipv4.ProtocolNumber)
	if e != nil {
		return e
	}
	n.closers = append(n.closers, pc)
	l, err := gonet.ListenTCP(n.Stack, addr, ipv4.ProtocolNumber)
	if err != nil {
		return err
	}
	n.closers = append(n.closers, l)
	h := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		ctx, cancel := context.WithTimeout(context.Background(), 18*time.Second)
		defer cancel()
		if answer, err := resolveDNS(ctx, r, dial); err == nil {
			_ = w.WriteMsg(answer)
			return
		}
		answer := new(dns.Msg)
		answer.SetRcode(r, dns.RcodeServerFailure)
		_ = w.WriteMsg(answer)
	})
	go func() { _ = (&dns.Server{PacketConn: pc, Handler: h}).ActivateAndServe() }()
	go func() { _ = (&dns.Server{Listener: l, Handler: h}).ActivateAndServe() }()
	return nil
}

// Resolve through encrypted DNS over the selected egress. In VPN mode the
// SOCKS dialer also sends the DoH hostname to VLESS. On a trusted LAN, only
// the DoH provider hostname is bootstrapped using the Mac resolver.
func resolveDNS(ctx context.Context, r *dns.Msg, dial proxy.DialFunc) (*dns.Msg, error) {
	wire, err := r.Pack()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{Proxy: nil, DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return dial(ctx, addr) }, DisableKeepAlives: true, TLSHandshakeTimeout: 8 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 8 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("DNS redirects disabled") }}
	for _, endpoint := range []string{"https://cloudflare-dns.com/dns-query", "https://dns.google/dns-query"} {
		req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(wire))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Content-Type", "application/dns-message")
		req.Header.Set("Accept", "application/dns-message")
		resp, e := client.Do(req)
		if e != nil {
			continue
		}
		b, e := io.ReadAll(io.LimitReader(resp.Body, 65536))
		resp.Body.Close()
		answer := new(dns.Msg)
		if e == nil && resp.StatusCode == 200 && answer.Unpack(b) == nil && answer.Id == r.Id {
			return answer, nil
		}
	}
	return nil, errors.New("gateway DNS unavailable")
}

func (n *Network) DialGuest(ctx context.Context, addr string) (net.Conn, error) {
	h, p, e := net.SplitHostPort(addr)
	if e != nil {
		return nil, e
	}
	port, e := strconv.ParseUint(p, 10, 16)
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(h).To4()
	if ip == nil {
		return nil, errors.New("guest address must be IPv4")
	}
	return gonet.DialContextTCP(ctx, n.Stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4Slice(ip), Port: uint16(port)}, ipv4.ProtocolNumber)
}

// API is reachable over a private host Unix socket only, never from the guest.
func (n *Network) API(health http.HandlerFunc) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/agents/health", health)
	m.Handle("/services/dhcp/", http.StripPrefix("/services/dhcp", n.DHCP.Mux()))
	m.Handle("/services/forwarder/", http.StripPrefix("/services/forwarder", n.Ports.Mux()))
	m.HandleFunc("/services/dns/add", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }) // no host DNS forwarding
	m.HandleFunc("/extension/wait_port", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
		defer cancel()
		addr := net.JoinHostPort(r.URL.Query().Get("ip"), r.URL.Query().Get("port"))
		for {
			c, e := n.DialGuest(ctx, addr)
			if e == nil {
				c.Close()
				w.WriteHeader(200)
				return
			}
			select {
			case <-ctx.Done():
				http.Error(w, "guest port unavailable", 408)
				return
			case <-time.After(200 * time.Millisecond):
			}
		}
	})
	m.HandleFunc("/leases", func(w http.ResponseWriter, r *http.Request) { _ = json.NewEncoder(w).Encode(n.Pool.Leases()) })
	return m
}

func (n *Network) ServeFD(ctx context.Context, l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			return err
		}
		go func() {
			defer c.Close()
			_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
			files, e := fd.Get(c.(*net.UnixConn), 1, []string{"vm"})
			if e != nil {
				return
			}
			defer func() {
				for _, f := range files {
					f.Close()
				}
			}()
			if len(files) != 1 {
				return
			}
			conn, e := net.FileConn(files[0])
			if e != nil {
				return
			}
			defer conn.Close()
			go func() { <-ctx.Done(); conn.Close() }()
			_ = n.Switch.Accept(ctx, conn, types.BessProtocol)
		}()
	}
}

func ListenUnix(path string) (net.Listener, error) {
	if len(path) > 100 {
		return nil, fmt.Errorf("VPN socket path too long; use a shorter --state-dir")
	}
	_ = os.Remove(path)
	l, e := net.Listen("unix", path)
	if e == nil {
		e = os.Chmod(path, 0600)
		if e != nil {
			l.Close()
		}
	}
	return l, e
}
