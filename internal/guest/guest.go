package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/capitanbanana/zbox/internal/policy"
	"github.com/capitanbanana/zbox/internal/proxy"
)

const HTTPPort = "18080"
const SOCKSPort = "18082"
const HostSOCKSPort = "18083"
const HostControlPort = "18084"

type Credentials struct {
	Token string `json:"token"`
}
type Policy struct {
	Hosts []string `json:"hosts"`
}

func Run(file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var cred Credentials
	if err = json.Unmarshal(b, &cred); err != nil {
		return err
	}
	if cred.Token == "" {
		return fmt.Errorf("empty host token")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	dial := splitDial(client, "http://127.0.0.1:"+HostControlPort+"/policy", "127.0.0.1:"+HostSOCKSPort, cred.Token)
	l, err := net.Listen("tcp", "127.0.0.1:"+SOCKSPort)
	if err != nil {
		return err
	}
	defer l.Close()
	go func() { _ = proxy.ServeSOCKS(l, "", dial) }()
	s := &http.Server{Addr: "127.0.0.1:" + HTTPPort, Handler: proxy.HTTPHandler(dial), ReadHeaderTimeout: 15 * time.Second, MaxHeaderBytes: 64 << 10}
	return s.ListenAndServe()
}

func splitDial(client *http.Client, policyURL, socksEndpoint, token string) proxy.DialFunc {
	return func(ctx context.Context, addr string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		// Fetch on each connection: changes apply immediately; fail closed if the
		// policy service is unavailable, never leak a corporate lookup to guest DNS.
		req, err := http.NewRequestWithContext(ctx, "GET", policyURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("host policy unavailable: %s", resp.Status)
		}
		var p Policy
		if err = json.NewDecoder(resp.Body).Decode(&p); err != nil {
			return nil, err
		}
		if policy.Match(p.Hosts, host) {
			return proxy.SOCKSDial(ctx, socksEndpoint, token, addr)
		}
		return (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", addr)
	}
}

func Env() []string {
	h := "http://127.0.0.1:" + HTTPPort
	s := "socks5h://127.0.0.1:" + SOCKSPort
	return []string{"HTTP_PROXY=" + h, "HTTPS_PROXY=" + h, "http_proxy=" + h, "https_proxy=" + h, "ALL_PROXY=" + s, "all_proxy=" + s, "NO_PROXY=localhost,127.0.0.1,::1", "no_proxy=localhost,127.0.0.1,::1", "CODEX_HOME=/home/agent/.config/agents/codex", "PI_CODING_AGENT_DIR=/home/agent/.config/agents/pi"}
}

func Profile() string {
	var b strings.Builder
	for _, v := range Env() {
		fmt.Fprintf(&b, "export %s\n", v)
	}
	return b.String()
}
