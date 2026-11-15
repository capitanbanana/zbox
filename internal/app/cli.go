package app

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/capitanbanana/zbox/internal/guest"
	"github.com/capitanbanana/zbox/internal/policy"
)

const help = `zbox — Claude, Codex, Pi and Ori inside a Lima VM

Usage: zbox [--vm NAME] [--state-dir DIR] [--restart] COMMAND

  create [--cpus 4] [--memory 8] [--disk 60]  Create a stopped VM
  start / stop / delete                     Manage VM and proxy infrastructure
  status                                    Show VM, mounts, routing and MCP
  claude DIR [ARGS…]                         Mount DIR and run Claude Code
  codex DIR [ARGS…]                          Mount DIR and run Codex
  pi DIR [ARGS…]                             Mount DIR and run Pi
  ori DIR [ARGS…]                            Mount DIR and run OpenRouter Ori
  openrouter DIR [ARGS…]                     Alias for ori
  ssh [COMMAND ARGS…]                        SSH into VM or run a command
  shell [COMMAND ARGS…]                      Alias for ssh
  socks [--port 1080]                        Browse through VM; Ctrl+C closes tunnel
  network                                   Interactive routing-list editor
  network list / add HOST… / remove HOST…    Route listed hosts through Mac
  vpn add|update VLESS_URI|--stdin            Configure VLESS with a kill switch
  vpn delete / status                       Remove or inspect VPN
  vpn bypass add NAME                       Trust current LAN for direct egress
  vpn bypass list / current / delete NAME    Inspect or remove trusted networks
  mounts list                               List host exports
  mounts add HOST [GUEST] [--ro] [--restart]  Default: ~/dev/<folder-name>
  mounts remove HOST [--restart]             Remove an export
  mcp list                                  Show managed MCP servers
  mcp add NAME --url URL [--host]            Add an HTTP MCP server
  mcp add NAME -- COMMAND [ARGS…]            Run stdio MCP inside the VM
  mcp remove / enable / disable NAME         Update managed MCP servers

Global flags precede the command. Arguments after a harness DIR are passed
unchanged (one optional -- separator is stripped). New mounts on a running VM
require --restart, which ends active sessions. Host directories are RW unless
--ro is used. With VPN configured, default egress uses VLESS unless a trusted
bypass network matches; network/MCP
entries remain explicit host exceptions. VPN changes require a stopped VM
or --restart. Without VPN, network rules are split routing, not a firewall.
`

func Main(args []string) error {
	if len(args) > 0 && args[0] == "_guest" {
		if len(args) != 2 {
			return errors.New("_guest needs credentials path")
		}
		return guest.Run(args[1])
	}
	fs := flag.NewFlagSet("zbox", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	root := fs.String("state-dir", settingEnv("HOME"), "state directory")
	name := fs.String("vm", "default", "VM name")
	restart := fs.Bool("restart", false, "allow restarting for mounts/VPN")
	fs.Usage = func() { fmt.Print(help) }
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	args = fs.Args()
	if len(args) == 0 || args[0] == "help" {
		fmt.Print(help)
		return nil
	}
	a, err := New(*root, *name, *restart)
	if err != nil {
		return err
	}
	cmd := args[0]
	args = args[1:]
	if cmd == "_daemon" {
		return a.daemon()
	}
	if cmd == "_gateway" {
		return a.runGateway()
	}
	if cmd == "status" {
		if len(args) != 0 {
			return errors.New("status takes no arguments")
		}
		return a.status()
	}
	if cmd == "network" && len(args) == 0 {
		return a.networkInteractive(os.Stdin, os.Stdout)
	}
	unlock, err := a.lock()
	if err != nil {
		return err
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	switch cmd {
	case "create":
		f := flag.NewFlagSet("create", flag.ContinueOnError)
		cpus := f.Int("cpus", 4, "CPUs")
		mem := f.Int("memory", 8, "memory GiB")
		disk := f.Int("disk", 60, "disk GiB")
		if err = f.Parse(args); err != nil {
			return err
		}
		if f.NArg() != 0 {
			return errors.New("unexpected create arguments")
		}
		return a.create(Config{CPUs: *cpus, Memory: *mem, Disk: *disk})
	case "start", "stop", "delete":
		if len(args) != 0 {
			return fmt.Errorf("%s takes no arguments", cmd)
		}
		switch cmd {
		case "start":
			return a.start()
		case "stop":
			return a.stop()
		default:
			return a.delete()
		}
	case "claude", "codex", "pi", "ori", "openrouter":
		if len(args) == 0 {
			return fmt.Errorf("usage: zbox %s DIR [ARGS…]", cmd)
		}
		forward := args[1:]
		if len(forward) > 0 && forward[0] == "--" {
			forward = forward[1:]
		}
		var child *exec.Cmd
		child, err = a.session(cmd, args[0], forward)
		if err != nil {
			return err
		}
		unlock()
		unlock = nil
		return run(child)
	case "ssh", "shell":
		if err = a.start(); err != nil {
			return err
		}
		c, e := a.load()
		if e != nil {
			return e
		}
		if e = a.writeMCP(c); e != nil {
			return e
		}
		if len(args) > 0 && args[0] == "--" {
			args = args[1:]
		}
		if len(args) == 0 {
			args = []string{"bash", "-l"}
		}
		command := "export PATH=\"$HOME/.local/bin:$PATH\"; . \"$HOME/.config/agents/proxy.env\"; cd " + quote(guestHome) + " && exec " + commandLine(args)
		unlock()
		unlock = nil
		return run(a.ssh(isTerminal(os.Stdin), command))
	case "socks":
		port, e := parseSOCKSPort(args)
		if e != nil {
			return e
		}
		if e = a.start(); e != nil {
			return e
		}
		unlock()
		unlock = nil
		fmt.Fprintf(os.Stderr, "Opening VM SOCKS5 at 127.0.0.1:%d (DNS through proxy).\nUse socks5h://127.0.0.1:%d; Ctrl+C closes this tunnel, leaving the VM running.\n", port, port)
		return run(a.browserSOCKS(port))
	case "network":
		return a.network(args, os.Stdout)
	case "vpn":
		return a.vpn(args)
	case "mounts":
		return a.mounts(args)
	case "mcp":
		return a.mcp(args)
	default:
		return fmt.Errorf("unknown command %q; run zbox help", cmd)
	}
}

func (a *App) status() error {
	c, err := a.load()
	if err != nil {
		return err
	}
	s, err := a.state()
	if err != nil {
		return err
	}
	proxyStatus := "stopped"
	if _, err = a.daemonRequest("GET", "/health"); err == nil {
		proxyStatus = "running"
	}
	fmt.Printf("VM: %s (%s) | host proxy: %s\nState: %s\n", a.Name, s, proxyStatus, a.Dir)
	printMounts(c)
	printNetwork(c, os.Stdout)
	printMCP(c)
	a.printVPN(c)
	if c.VPN == nil {
		fmt.Println("Network is split routing, not an egress firewall. Direct VM traffic may inherit host VPN routes.")
		fmt.Println("Host/LAN services are not filtered by this wrapper; reachability follows Lima networking.")
	}
	fmt.Println("RW exports affect original files. Host applications executing exported code retain their host permissions.")
	return nil
}

func printMounts(c Config) {
	fmt.Println("Mounts:")
	for _, m := range c.Mounts {
		mode := "RW"
		if m.ReadOnly {
			mode = "RO"
		}
		fmt.Printf("  %s  %s -> %s\n", mode, m.Host, m.Guest)
	}
	if len(c.Mounts) == 0 {
		fmt.Println("  (none)")
	}
}
func (a *App) mounts(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		c, e := a.load()
		if e != nil {
			return e
		}
		printMounts(c)
		return nil
	}
	cmd := args[0]
	rest := []string{}
	ro := false
	for _, s := range args[1:] {
		switch s {
		case "--ro":
			ro = true
		case "--restart":
			a.Restart = true
		default:
			rest = append(rest, s)
		}
	}
	switch cmd {
	case "add":
		if len(rest) < 1 || len(rest) > 2 {
			return errors.New("usage: zbox mounts add HOST [GUEST] [--ro] [--restart]")
		}
		dest := ""
		if len(rest) == 2 {
			dest = rest[1]
		}
		g, e := a.addMount(rest[0], dest, ro)
		if e == nil {
			fmt.Println("Mount configured at", g)
		}
		return e
	case "remove":
		if len(rest) != 1 {
			return errors.New("usage: zbox mounts remove HOST [--restart]")
		}
		p, e := filepathAbsExport(rest[0])
		if e != nil {
			return e
		}
		c, e := a.load()
		if e != nil {
			return e
		}
		out := []Mount{}
		found := false
		for _, m := range c.Mounts {
			if m.Host == p {
				found = true
			} else {
				out = append(out, m)
			}
		}
		if !found {
			return errors.New("mount not found")
		}
		c.Mounts = out
		return a.changeMounts(c)
	default:
		return errors.New("mounts: expected list, add, remove")
	}
}

func printNetwork(c Config, w io.Writer) {
	fmt.Fprintln(w, "Hosts routed through Mac (all TCP ports; *.domain excludes the apex):")
	for _, h := range c.Hosts {
		fmt.Fprintln(w, "  "+h)
	}
	if len(c.Hosts) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
}
func (a *App) network(args []string, w io.Writer) error {
	c, err := a.load()
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "list" {
		printNetwork(c, w)
		return nil
	}
	if len(args) < 2 || (args[0] != "add" && args[0] != "remove") {
		return errors.New("usage: zbox network list | add HOST… | remove HOST…")
	}
	set := map[string]bool{}
	for _, h := range c.Hosts {
		set[h] = true
	}
	for _, h := range args[1:] {
		h, err = policy.Normalize(h)
		if err != nil {
			return err
		}
		if args[0] == "add" {
			set[h] = true
		} else {
			delete(set, h)
		}
	}
	c.Hosts = []string{}
	for h := range set {
		c.Hosts = append(c.Hosts, h)
	}
	sort.Strings(c.Hosts)
	if err = a.save(c); err != nil {
		return err
	}
	printNetwork(c, w)
	fmt.Fprintln(w, "Applied to new connections. Existing connections remain open.")
	return nil
}
func (a *App) networkInteractive(r io.Reader, w io.Writer) error {
	c, err := a.load()
	if err != nil {
		return err
	}
	printNetwork(c, w)
	fmt.Fprintln(w, "Commands: + *.selectel.ru | - host.example | list | quit (changes save immediately)")
	s := bufio.NewScanner(r)
	for {
		fmt.Fprint(w, "network> ")
		if !s.Scan() {
			return s.Err()
		}
		line := strings.Fields(s.Text())
		if len(line) == 0 {
			continue
		}
		switch line[0] {
		case "q", "quit", "exit":
			return nil
		case "+":
			line[0] = "add"
		case "-":
			line[0] = "remove"
		case "ls":
			line[0] = "list"
		}
		unlock, e := a.lock()
		if e == nil {
			e = a.network(line, w)
			unlock()
		}
		if e != nil {
			fmt.Fprintln(w, "Error:", e)
		}
	}
}

func printMCP(c Config) {
	fmt.Println("MCP:")
	for _, m := range c.MCPs {
		state := "enabled"
		if m.Disabled {
			state = "disabled"
		}
		location := "VM stdio"
		value := commandLine(m.Command)
		if m.URL != "" {
			location = "remote HTTP"
			value = m.URL
		}
		if m.Host {
			location = fmt.Sprintf("HOST TCP tunnel :%d", m.Port)
		}
		fmt.Printf("  %s [%s, %s] %s\n", m.Name, state, location, value)
	}
	if len(c.MCPs) == 0 {
		fmt.Println("  (none)")
	}
}
func (a *App) mcp(args []string) error {
	if len(args) == 0 || args[0] == "list" {
		c, e := a.load()
		if e != nil {
			return e
		}
		printMCP(c)
		return nil
	}
	if len(args) < 2 {
		return errors.New("mcp: name required")
	}
	action, name := args[0], args[1]
	if action == "add" {
		m := MCP{Name: name}
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--":
				m.Command = args[i+1:]
				i = len(args)
			case "--host":
				m.Host = true
			case "--url":
				i++
				if i >= len(args) {
					return errors.New("--url requires a URL")
				}
				m.URL = args[i]
			default:
				return fmt.Errorf("unknown MCP argument %q", args[i])
			}
		}
		return a.addMCP(m)
	}
	if len(args) != 2 {
		return errors.New("unexpected MCP arguments")
	}
	c, e := a.load()
	if e != nil {
		return e
	}
	for i := range c.MCPs {
		if c.MCPs[i].Name != name {
			continue
		}
		switch action {
		case "enable":
			c.MCPs[i].Disabled = false
		case "disable":
			c.MCPs[i].Disabled = true
		case "remove":
			c.MCPs = append(c.MCPs[:i], c.MCPs[i+1:]...)
		default:
			return errors.New("mcp: expected list, add, remove, enable, disable")
		}
		return a.save(c)
	}
	return errors.New("MCP not found")
}
