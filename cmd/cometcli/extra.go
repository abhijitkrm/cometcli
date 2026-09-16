package main

import (
	"github.com/spf13/cobra"

	"github.com/abhijitkrm/cometcli/internal/cli"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// extraCommands returns hand-written commands that aren't schema-generated
// tools: doctor, agent, ask.
func extraCommands(reg *toolkit.Registry) []*cobra.Command {
	return []*cobra.Command{
		cli.DoctorCmd(reg),
		cli.AskCmd(reg),
		cli.AgentCmd(reg),
	}
}
