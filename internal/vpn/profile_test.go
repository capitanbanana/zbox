package vpn

import (
	"encoding/json"
	"strings"
	"testing"
)

const testURI = "vless://11111111-2222-4333-8444-555555555555@vpn.example:443"

func TestRealityGRPC(t *testing.T) {
	p, e := Parse(testURI + "?type=grpc&security=reality&encryption=none&sni=cdn.example&fp=firefox&pbk=AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA&sid=0123456789abcdef&alpn=h2&serviceName=stream")
	if e != nil {
		t.Fatal(e)
	}
	b, e := p.SingBox(1080, "test-token")
	if e != nil {
		t.Fatal(e)
	}
	var c map[string]any
	if e = json.Unmarshal(b, &c); e != nil {
		t.Fatal(e)
	}
	outs := c["outbounds"].([]any)
	if len(outs) != 1 {
		t.Fatal("fallback outbound")
	}
	out := outs[0].(map[string]any)
	if out["type"] != "vless" || out["transport"].(map[string]any)["service_name"] != "stream" {
		t.Fatal(out)
	}
	if c["route"].(map[string]any)["final"] != "vpn" {
		t.Fatal("no VPN final")
	}
	if strings.Contains(p.Summary(), p.UUID) || strings.Contains(p.Summary(), p.PublicKey) {
		t.Fatal("summary exposes credentials")
	}
}
func TestVLESSValidationAndRedaction(t *testing.T) {
	for _, suffix := range []string{"?type=xhttp", "?security=reality", "?flow=xtls-rprx-vision", "?type=tcp&type=ws", "?security=tls&allowInsecure=1", "?security=none&sni=cdn.example", "?type=tcp&path=/ws", "?encryption=unknown", "?unsupported=secret", "?sid=12"} {
		_, e := Parse(testURI + suffix)
		if e == nil {
			t.Errorf("accepted %s", suffix)
		} else if strings.Contains(e.Error(), "11111111") || strings.Contains(e.Error(), "secret") {
			t.Fatal("credential leak", e)
		}
	}
	for _, s := range []string{"vless://secret%@host:443", "vless://secret:password@host:443", strings.Replace(testURI, ":443", ":99999", 1)} {
		if _, e := Parse(s); e == nil {
			t.Fatal("accepted invalid link")
		}
	}
}
func TestSupportedTransports(t *testing.T) {
	for _, transport := range []string{"tcp", "raw", "ws", "grpc", "http", "httpupgrade"} {
		p, e := Parse(testURI + "?security=tls&type=" + transport)
		if e != nil {
			t.Fatal(e)
		}
		if p.SNI != "vpn.example" {
			t.Fatal("missing TLS server verification name")
		}
		b, e := p.SingBox(1080, "test")
		if e != nil || strings.Contains(string(b), `"insecure": true`) {
			t.Fatalf("bad TLS config %v", e)
		}
	}
}
