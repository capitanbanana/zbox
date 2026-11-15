package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Lima normally hides provisioning output behind "boot scripts must have
// finished". Follow this boot's journal as soon as SSH becomes available.
func (a *App) runLimaStart() error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.followBootLog(ctx, os.Stderr)
	}()
	defer func() {
		cancel()
		<-done
	}()
	fmt.Fprintln(os.Stderr, "[zbox] Booting VM. First boot installs system dependencies; progress appears below once SSH is ready.")
	return run(a.lima("start", "--tty=false", a.Name))
}

func (a *App) followBootLog(ctx context.Context, out io.Writer) {
	for ctx.Err() == nil {
		ssh := a.ssh(false, "exec sudo -n journalctl -b -u cloud-final.service --follow -n 20 --no-pager -o cat")
		// A fresh connection avoids racing Lima's own ControlMaster setup.
		args := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=2", "-o", "ControlMaster=no", "-o", "ControlPath=none"}
		args = append(args, ssh.Args[1:]...)
		cmd := exec.CommandContext(ctx, ssh.Path, args...)
		cmd.Stdout = out
		cmd.Stderr = io.Discard // Connection failures are expected during boot.
		cmd.WaitDelay = time.Second
		_ = cmd.Run()
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}
