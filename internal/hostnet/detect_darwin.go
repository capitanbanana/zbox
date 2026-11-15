package hostnet

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"syscall"
	"time"
)

func Detect(ctx context.Context) (Network, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var n Network
	b, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return n, errors.New("cannot read default route")
	}
	n.Gateway, n.Interface, err = parseRoute(string(b))
	if err != nil {
		return n, err
	}
	iface, err := net.InterfaceByName(n.Interface)
	if err != nil || iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return n, errors.New("default interface unavailable")
	}
	n.Index = iface.Index
	addrs, err := iface.Addrs()
	if err != nil {
		return n, err
	}
	for _, addr := range addrs {
		ip, subnet, e := net.ParseCIDR(addr.String())
		if e == nil && ip.To4() != nil && subnet.Contains(net.ParseIP(n.Gateway)) {
			if n.Subnet != "" {
				return n, errors.New("ambiguous interface subnet")
			}
			n.Subnet, n.Source = subnet.String(), ip.String()
		}
	}
	b, err = exec.CommandContext(ctx, "/usr/sbin/arp", "-n", "-i", n.Interface, n.Gateway).Output()
	if err != nil {
		return n, errors.New("cannot read gateway MAC")
	}
	n.MAC, err = parseARP(string(b), n.Gateway, n.Interface)
	if err != nil {
		return n, err
	}
	// A route change during the observation invalidates the entire snapshot.
	b, err = exec.CommandContext(ctx, "/sbin/route", "-n", "get", "default").Output()
	if err != nil {
		return n, errors.New("cannot recheck default route")
	}
	g, i, err := parseRoute(string(b))
	if err != nil || g != n.Gateway || i != n.Interface || !n.Valid() {
		return n, errors.New("network changed or could not be identified")
	}
	return n, nil
}

// Bind direct sockets to the observed address and physical interface. Never
// let the OS silently migrate them to a different egress interface.
func Direct(ctx context.Context, n Network, network, address string) (net.Conn, error) {
	if !n.Valid() {
		return nil, errors.New("invalid direct network")
	}
	d := net.Dialer{Timeout: 15 * time.Second}
	switch network {
	case "tcp":
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(n.Source)}
	case "udp":
		d.LocalAddr = &net.UDPAddr{IP: net.ParseIP(n.Source)}
	default:
		return nil, errors.New("unsupported direct protocol")
	}
	d.Control = func(_, _ string, raw syscall.RawConn) error {
		var optionErr error
		err := raw.Control(func(fd uintptr) {
			optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, n.Index)
		})
		if err != nil {
			return err
		}
		return optionErr
	}
	return d.DialContext(ctx, network+"4", address)
}
