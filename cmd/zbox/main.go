package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/capitanbanana/zbox/internal/app"
)

var version = "dev"

func main() {
	if len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("zbox", version)
		return
	}
	if err := app.Main(os.Args[1:]); err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			if err != e {
				fmt.Fprintln(os.Stderr, "zbox:", err)
			}
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "zbox:", err)
		os.Exit(1)
	}
}
