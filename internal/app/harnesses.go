package app

import (
	_ "embed"
	"fmt"
	"os"
	"strings"

	"github.com/capitanbanana/zbox/internal/guest"
)

// One installer is used both at first boot and to upgrade older VM layouts.
// It installs missing tools only; it does not update working installations.
//
//go:embed harnesses.sh
var harnessProvision string

func (a *App) ensureHarnesses() error {
	c, err := a.load()
	if err != nil {
		return err
	}
	// A healthy older VM can reuse its running proxy, but needs new exported
	// environment settings without restarting that proxy or other sessions.
	if err := a.upload(strings.NewReader(guest.Profile(c.Home())), c.Home()+"/.config/agents/proxy.env", "600"); err != nil {
		return err
	}
	check := `test -x "$HOME/.local/bin/claude" && test -x "$HOME/.local/bin/codex" && test -x "$HOME/.local/bin/pi" && test -x "$HOME/.local/bin/ori" && test -x "$HOME/.local/share/uv/tools/claude-code-redact/bin/rdx"`
	command := "if " + check + "; then exit 0; fi; " +
		`export PATH="$HOME/.local/bin:$PATH"; . "$HOME/.config/agents/proxy.env"; exec bash -s`
	cmd := a.ssh(false, command)
	cmd.Stdin = strings.NewReader(harnessProvision)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("guest CLI installation incomplete; fix guest connectivity/Node and rerun zbox start: %w", err)
	}
	// rdx arrives with the harness set, after its unit was enabled; start it now
	// so the proxy is live in this session. A missing binary is not an error.
	restart := a.ssh(false, "systemctl --user restart rdx-proxy.service >/dev/null 2>&1 || true")
	if err := restart.Run(); err != nil {
		return err
	}
	return nil
}

func harnessCommand(harness string, args []string, c Config) []string {
	if harness == "openrouter" {
		harness = "ori"
	}
	cmd := []string{harness}
	if harness == "ori" {
		// Ori forwards harness options. Preserve its subcommand position, and
		// apply the same explicit MCP configuration to its Claude launcher.
		if len(args) > 0 && args[0] == "claude" {
			cmd = append(cmd, "claude")
			cmd = append(cmd, mcpArgs("claude", c)...)
			return append(cmd, args[1:]...)
		}
		return append(cmd, args...)
	}
	cmd = append(cmd, mcpArgs(harness, c)...)
	return append(cmd, args...)
}
