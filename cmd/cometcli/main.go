package main

import (
	"fmt"
	"os"

	"github.com/abhijitkrm/cometcli/internal/cli"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools"
)

func main() {
	reg := toolkit.NewRegistry()
	tools.RegisterAll(reg)
	root := cli.NewRoot(reg, extraCommands(reg))
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
