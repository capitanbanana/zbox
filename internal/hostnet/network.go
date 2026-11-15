// Package hostnet identifies explicitly trusted host networks. A matching LAN
// permits direct egress; missing or ambiguous observations never do.
package hostnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

type Identity struct {
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
	MAC     string `json:"gateway_mac"`
}

type Network struct {
	Identity
	Interface string `json:"interface"`
	Index     int    `json:"interface_index"`
	Source    string `json:"source"`
}

type Exception struct {
	Name string `json:"name"`
	Identity
}

func (i Identity) Valid() bool {
	p, e := netip.ParsePrefix(i.Subnet)
	g, ge := netip.ParseAddr(i.Gateway)
	m, me := normalizeMAC(i.MAC)
	return e == nil && ge == nil && me == nil && p.Addr().Is4() && g.Is4() && p == p.Masked() && p.Contains(g) && g.IsGlobalUnicast() && m == i.MAC
}

func (n Network) Valid() bool {
	s, e := netip.ParseAddr(n.Source)
	p, _ := netip.ParsePrefix(n.Subnet)
	return n.Identity.Valid() && e == nil && s.Is4() && s.IsGlobalUnicast() && p.Contains(s) && n.Interface != "" && n.Index > 0
}

func Match(n Network, rules []Exception) string {
	if !n.Valid() {
		return ""
	}
	for _, r := range rules {
		if r.Name != "" && r.Identity.Valid() && r.Identity == n.Identity {
			return r.Name
		}
	}
	return ""
}

func parseRoute(b string) (gateway, iface string, err error) {
	for _, line := range strings.Split(b, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch key {
		case "gateway":
			if gateway != "" {
				return "", "", errors.New("ambiguous default gateway")
			}
			gateway = strings.TrimSpace(value)
		case "interface":
			if iface != "" {
				return "", "", errors.New("ambiguous default interface")
			}
			iface = strings.TrimSpace(value)
		}
	}
	ip := net.ParseIP(gateway)
	if ip == nil || ip.To4() == nil || !ip.IsGlobalUnicast() || iface == "" || strings.ContainsAny(iface, " /\t\r\n") {
		return "", "", errors.New("no usable IPv4 default route")
	}
	return gateway, iface, nil
}

func normalizeMAC(raw string) (string, error) {
	parts := strings.Split(raw, ":")
	if len(parts) != 6 {
		return "", errors.New("invalid gateway MAC")
	}
	for i, part := range parts {
		if len(part) < 1 || len(part) > 2 {
			return "", errors.New("invalid gateway MAC")
		}
		n, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return "", errors.New("invalid gateway MAC")
		}
		parts[i] = fmt.Sprintf("%02x", n)
	}
	m, err := net.ParseMAC(strings.Join(parts, ":"))
	if err != nil || m[0]&1 != 0 || m.String() == "00:00:00:00:00:00" {
		return "", errors.New("invalid gateway MAC")
	}
	return m.String(), nil
}

func parseARP(b, gateway, iface string) (string, error) {
	f := strings.Fields(b)
	if len(f) < 7 || f[1] != "("+gateway+")" || f[2] != "at" || f[4] != "on" || f[5] != iface || strings.Contains(strings.TrimSpace(b), "\n") {
		return "", errors.New("gateway MAC is unavailable or ambiguous")
	}
	return normalizeMAC(f[3])
}
