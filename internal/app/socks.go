package app

import (
	"errors"
	"flag"
	"fmt"
	"os/exec"

	"agents-sandbox/internal/guest"
)

func parseSOCKSPort(args []string) (int, error) {
	f := flag.NewFlagSet("socks", flag.ContinueOnError)
	port := f.Int("port", 1080, "Mac loopback port")
	if err := f.Parse(args); err != nil {
		return 0, err
	}
	if f.NArg() != 0 || *port < 1 || *port > 65535 {
		return 0, errors.New("usage: agents socks [--port 1..65535]")
	}
	return *port, nil
}

// Forward the existing guest split-routing proxy, not an SSH dynamic proxy:
// ssh -D would skip the guest's domain policy and corporate DNS handling.
// A separate SSH connection owns the listener, so Ctrl+C cannot leave it on
// Lima's persistent control master or interrupt the infrastructure tunnels.
func (a *App) browserSOCKS(port int) *exec.Cmd {
	return exec.Command("ssh", "-F", a.sshConfig(), "-N", "-T",
		"-o", "ControlMaster=no", "-o", "ControlPath=none",
		"-o", "ForwardAgent=no", "-o", "ForwardX11=no",
		"-o", "ExitOnForwardFailure=yes", "-o", "ConnectTimeout=5",
		"-o", "ServerAliveInterval=10", "-o", "ServerAliveCountMax=3",
		"-L", fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%s", port, guest.SOCKSPort),
		"lima-"+a.Name)
}
