package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"agents-sandbox/internal/app"
)

func main() {
	if err := app.Main(os.Args[1:]); err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "agents:", err)
		os.Exit(1)
	}
}
