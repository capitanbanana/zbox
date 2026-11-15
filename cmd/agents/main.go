package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"agents-sandbox/internal/app"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("agents", version)
		return
	}
	if err := app.Main(os.Args[1:]); err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "agents:", err)
		os.Exit(1)
	}
}
