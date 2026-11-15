package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

func hostMCPAddress(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("MCP URL must use http or https")
	}
	if u.User != nil || u.Fragment != "" {
		return "", errors.New("credentials/fragments in MCP URLs are not supported")
	}
	h := u.Hostname()
	if h != "localhost" && h != "127.0.0.1" {
		return "", errors.New("--host requires a localhost or 127.0.0.1 URL")
	}
	p := u.Port()
	if p == "" {
		p = "80"
		if u.Scheme == "https" {
			p = "443"
		}
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid MCP port")
	}
	return net.JoinHostPort("127.0.0.1", p), nil
}
func guestMCPURL(m MCP) string {
	if !m.Host {
		return m.URL
	}
	u, _ := url.Parse(m.URL)
	u.Host = net.JoinHostPort(u.Hostname(), strconv.Itoa(m.Port))
	return u.String()
}
func validateMCP(m MCP) error {
	if !validName.MatchString(m.Name) {
		return errors.New("invalid MCP name")
	}
	if (len(m.Command) > 0) == (m.URL != "") {
		return errors.New("provide either --url URL or -- COMMAND ARG…")
	}
	if m.Host {
		_, err := hostMCPAddress(m.URL)
		return err
	}
	if m.URL != "" {
		u, err := url.Parse(m.URL)
		if err != nil {
			return err
		}
		if (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return errors.New("MCP URL must be http(s), without embedded credentials")
		}
	}
	return nil
}
func (a *App) addMCP(m MCP) error {
	if err := validateMCP(m); err != nil {
		return err
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	used := map[int]bool{}
	for _, x := range c.MCPs {
		if x.Name == m.Name {
			return errors.New("MCP already exists; remove it first")
		}
		used[x.Port] = true
	}
	if m.Host {
		for m.Port = 19000; used[m.Port]; m.Port++ {
		}
		if m.Port >= 20000 {
			return errors.New("too many host MCP tunnels")
		}
	}
	c.MCPs = append(c.MCPs, m)
	if err = a.save(c); err != nil {
		return err
	}
	fmt.Println("MCP saved. Harness configuration applies on the next session; host tunnels update automatically.")
	return nil
}

func claudeMCP(c Config) []byte {
	servers := map[string]any{}
	for _, m := range c.MCPs {
		if m.Disabled {
			continue
		}
		if m.URL != "" {
			servers[m.Name] = map[string]any{"type": "http", "url": guestMCPURL(m)}
		} else {
			servers[m.Name] = map[string]any{"type": "stdio", "command": m.Command[0], "args": m.Command[1:]}
		}
	}
	b, _ := json.Marshal(map[string]any{"mcpServers": servers})
	return b
}
func tomlString(s string) string { b, _ := json.Marshal(s); return string(b) }
func mcpArgs(harness string, c Config) []string {
	if harness == "claude" {
		return []string{"--strict-mcp-config", "--mcp-config", guestHome + "/.config/agents/mcp.json"}
	}
	if harness != "codex" {
		return nil
	}
	entries := []string{}
	for _, m := range c.MCPs {
		if m.Disabled {
			continue
		}
		fields := []string{}
		if m.URL != "" {
			fields = append(fields, "url = "+tomlString(guestMCPURL(m)))
		} else {
			fields = append(fields, "command = "+tomlString(m.Command[0]))
			args := []string{}
			for _, s := range m.Command[1:] {
				args = append(args, tomlString(s))
			}
			fields = append(fields, "args = ["+strings.Join(args, ", ")+"]")
		}
		entries = append(entries, tomlString(m.Name)+" = { "+strings.Join(fields, ", ")+" }")
	}
	return []string{"-c", "mcp_servers={" + strings.Join(entries, ", ") + "}"}
}
func (a *App) writeMCP(c Config) error {
	if err := a.upload(bytes.NewReader(claudeMCP(c)), guestHome+"/.config/agents/mcp.json", "600"); err != nil {
		return err
	}
	// Codex recursively merges -c tables. A managed CODEX_HOME prevents removed
	// servers in a previous user config from silently remaining active. Auth and
	// history in this directory are preserved; only config.toml is generated.
	args := mcpArgs("codex", c)
	return a.upload(strings.NewReader("# Managed by agents. Pass other settings with codex -c.\n"+args[1]+"\n"), guestHome+"/.config/agents/codex/config.toml", "600")
}
