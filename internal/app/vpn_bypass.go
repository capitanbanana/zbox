package app

import (
	"context"
	"errors"
	"fmt"

	"agents-sandbox/internal/hostnet"
)

func (a *App) vpnBypass(args []string) error {
	return a.vpnBypassWithDetector(args, hostnet.Detect)
}

func (a *App) vpnBypassWithDetector(args []string, detect func(context.Context) (hostnet.Network, error)) error {
	if len(args) == 1 && args[0] == "current" {
		n, err := detect(context.Background())
		if err != nil {
			return err
		}
		fmt.Printf("Network: %s, gateway %s (%s), interface %s, source %s\n", n.Subnet, n.Gateway, n.MAC, n.Interface, n.Source)
		return nil
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	if !((len(args) == 1 && args[0] == "list") || (len(args) == 2 && (args[0] == "add" || args[0] == "delete"))) {
		return errors.New("usage: agents vpn bypass current | list | add NAME | delete NAME")
	}
	c, err := a.load()
	if err != nil {
		return err
	}
	if args[0] == "list" {
		if c.VPN == nil {
			fmt.Println("VPN disabled; bypass rules are inactive.")
		}
		if len(c.VPNBypass) == 0 {
			fmt.Println("No bypass networks. Default egress uses VLESS when configured.")
			return nil
		}
		for _, r := range c.VPNBypass {
			fmt.Printf("%s\t%s\tgateway %s\t%s\n", r.Name, r.Subnet, r.Gateway, r.MAC)
		}
		return nil
	}
	name := args[1]
	if !validName.MatchString(name) {
		return errors.New("invalid bypass network name")
	}
	index := -1
	for i, r := range c.VPNBypass {
		if r.Name == name {
			index = i
			break
		}
	}
	if args[0] == "add" {
		if index >= 0 {
			return errors.New("bypass network already exists; delete it before relearning")
		}
		n, err := detect(context.Background())
		if err != nil {
			return fmt.Errorf("cannot learn trusted network: %w", err)
		}
		if !n.Valid() {
			return errors.New("cannot learn an incomplete network identity")
		}
		for _, r := range c.VPNBypass {
			if r.Identity == n.Identity {
				return fmt.Errorf("current network already saved as %q", r.Name)
			}
		}
		c.VPNBypass = append(c.VPNBypass, hostnet.Exception{Name: name, Identity: n.Identity})
	} else {
		if index < 0 {
			return errors.New("bypass network does not exist")
		}
		c.VPNBypass = append(c.VPNBypass[:index], c.VPNBypass[index+1:]...)
	}
	if err = a.save(c); err != nil {
		return err
	}
	fmt.Printf("Bypass network %s: %s.\n", name, args[0])
	if c.VPN == nil {
		fmt.Println("Rule saved; it becomes active when VPN is configured.")
		return nil
	}
	h, err := a.gatewayRequest("POST", "/agents/refresh")
	if err != nil {
		fmt.Println("Saved. Gateway did not acknowledge refresh; it reloads rules on its next check or start.")
	} else {
		fmt.Printf("Current egress: %s", h.Egress)
		if h.Bypass != "" {
			fmt.Printf(" (%s)", h.Bypass)
		}
		fmt.Println()
	}
	return nil
}
