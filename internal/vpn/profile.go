// Package vpn parses VLESS share links and generates a single-exit sing-box
// configuration. There is deliberately no direct/fallback outbound.
package vpn

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/capitanbanana/zbox/internal/policy"
)

type Profile struct {
	Server      string   `json:"server"`
	Port        int      `json:"port"`
	UUID        string   `json:"uuid"`
	Security    string   `json:"security"`
	Transport   string   `json:"transport"`
	SNI         string   `json:"sni,omitempty"`
	Fingerprint string   `json:"fingerprint,omitempty"`
	PublicKey   string   `json:"public_key,omitempty"`
	ShortID     string   `json:"short_id,omitempty"`
	Flow        string   `json:"flow,omitempty"`
	ALPN        []string `json:"alpn,omitempty"`
	Host        string   `json:"host,omitempty"`
	Path        string   `json:"path,omitempty"`
	ServiceName string   `json:"service_name,omitempty"`
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Never return the supplied URI in an error: it contains authentication data.
func Parse(raw string) (*Profile, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return nil, errors.New("invalid VLESS URI")
	}
	if u.Scheme != "vless" || u.User == nil || !uuidPattern.MatchString(u.User.Username()) {
		return nil, errors.New("expected vless://UUID@server:port?…")
	}
	if _, ok := u.User.Password(); ok {
		return nil, errors.New("VLESS URI must not contain a password")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("transport path must be supplied in the path query parameter")
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, errors.New("VLESS requires an explicit port between 1 and 65535")
	}
	host, err := policy.Normalize(u.Hostname())
	if err != nil || strings.HasPrefix(host, "*.") {
		return nil, errors.New("invalid VLESS server hostname")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid VLESS query encoding")
	}
	known := map[string]bool{"security": true, "type": true, "encryption": true, "sni": true, "fp": true, "pbk": true, "sid": true, "flow": true, "alpn": true, "host": true, "path": true, "serviceName": true, "headerType": true, "allowInsecure": true}
	for k, v := range q {
		if !known[k] {
			return nil, errors.New("unsupported VLESS query parameter (supported: security,type,encryption,sni,fp,pbk,sid,flow,alpn,host,path,serviceName,headerType,allowInsecure)")
		}
		if len(v) != 1 {
			return nil, errors.New("duplicate VLESS query parameter")
		}
	}
	if s := q.Get("encryption"); s != "" && s != "none" {
		return nil, errors.New("only VLESS encryption=none is supported")
	}
	if s := q.Get("allowInsecure"); s != "" && s != "0" && s != "false" {
		return nil, errors.New("insecure TLS verification is not supported")
	}
	if s := q.Get("headerType"); s != "" && s != "none" {
		return nil, errors.New("TCP header obfuscation is not supported")
	}
	p := &Profile{Server: host, Port: port, UUID: strings.ToLower(u.User.Username()), Security: q.Get("security"), Transport: q.Get("type"), SNI: q.Get("sni"), Fingerprint: q.Get("fp"), PublicKey: q.Get("pbk"), ShortID: q.Get("sid"), Flow: q.Get("flow"), Host: q.Get("host"), Path: q.Get("path"), ServiceName: q.Get("serviceName")}
	if p.Security == "" {
		p.Security = "none"
	}
	if p.Transport == "" || p.Transport == "raw" {
		p.Transport = "tcp"
	}
	switch p.Transport {
	case "tcp", "ws", "grpc", "http", "httpupgrade":
	default:
		return nil, errors.New("unsupported VLESS transport; supported: tcp, ws, grpc, http, httpupgrade")
	}
	switch p.Security {
	case "none", "tls", "reality":
	default:
		return nil, errors.New("unsupported VLESS security; supported: none, tls, reality")
	}
	if p.Flow != "" && (p.Flow != "xtls-rprx-vision" || p.Transport != "tcp" || p.Security == "none") {
		return nil, errors.New("xtls-rprx-vision requires TCP with TLS or REALITY")
	}
	if p.Security == "reality" {
		key, e := base64.RawURLEncoding.DecodeString(p.PublicKey)
		if e != nil || len(key) != 32 {
			return nil, errors.New("REALITY requires a 32-byte base64url public key (pbk)")
		}
		sid, e := hex.DecodeString(p.ShortID)
		if e != nil || len(sid) > 8 {
			return nil, errors.New("REALITY sid must be even-length hex, at most 16 characters")
		}
		if p.SNI == "" {
			return nil, errors.New("REALITY requires sni")
		}
		if p.Fingerprint == "" {
			p.Fingerprint = "chrome"
		}
	}
	if p.Security != "reality" && (p.PublicKey != "" || p.ShortID != "") {
		return nil, errors.New("pbk/sid require REALITY")
	}
	if p.Security == "none" && (p.SNI != "" || p.Fingerprint != "" || q.Get("alpn") != "") {
		return nil, errors.New("TLS options require security=tls or reality")
	}
	if p.Security != "none" && p.SNI == "" {
		p.SNI = p.Server
	}
	if q.Get("alpn") != "" {
		p.ALPN = strings.Split(q.Get("alpn"), ",")
	}
	if p.Transport == "tcp" && (p.Host != "" || p.Path != "" || p.ServiceName != "") {
		return nil, errors.New("transport host/path/serviceName cannot be used with TCP")
	}
	if p.Transport != "grpc" && p.ServiceName != "" {
		return nil, errors.New("serviceName requires grpc")
	}
	if p.Transport == "grpc" && (p.Host != "" || p.Path != "") {
		return nil, errors.New("gRPC uses serviceName, not host/path")
	}
	return p, nil
}

func (p *Profile) Summary() string {
	return fmt.Sprintf("%s (%s/%s)", net.JoinHostPort(p.Server, strconv.Itoa(p.Port)), p.Transport, p.Security)
}

func (p *Profile) SingBox(port int, token string) ([]byte, error) {
	out := map[string]any{"type": "vless", "tag": "vpn", "server": p.Server, "server_port": p.Port, "uuid": p.UUID}
	if p.Flow != "" {
		out["flow"] = p.Flow
	}
	if p.Security != "none" {
		tls := map[string]any{"enabled": true, "server_name": p.SNI}
		if p.Fingerprint != "" {
			tls["utls"] = map[string]any{"enabled": true, "fingerprint": p.Fingerprint}
		}
		if len(p.ALPN) > 0 {
			tls["alpn"] = p.ALPN
		}
		if p.Security == "reality" {
			tls["reality"] = map[string]any{"enabled": true, "public_key": p.PublicKey, "short_id": p.ShortID}
		}
		out["tls"] = tls
	}
	if p.Transport != "tcp" {
		tr := map[string]any{"type": p.Transport}
		if p.Path != "" {
			tr["path"] = p.Path
		}
		if p.Host != "" {
			switch p.Transport {
			case "ws":
				tr["headers"] = map[string]string{"Host": p.Host}
			case "http":
				tr["host"] = []string{p.Host}
			default:
				tr["host"] = p.Host
			}
		}
		if p.ServiceName != "" {
			tr["service_name"] = p.ServiceName
		}
		out["transport"] = tr
	}
	return json.MarshalIndent(map[string]any{"log": map[string]any{"level": "error"}, "inbounds": []any{map[string]any{"type": "socks", "listen": "127.0.0.1", "listen_port": port, "users": []any{map[string]string{"username": "agents", "password": token}}}}, "outbounds": []any{out}, "route": map[string]any{"final": "vpn"}}, "", "  ")
}
