package tools

import (
	"github.com/abhijitkrm/cometcli/internal/toolkit"
	"github.com/abhijitkrm/cometcli/internal/tools/chain"
	"github.com/abhijitkrm/cometcli/internal/tools/evmtool"
	"github.com/abhijitkrm/cometcli/internal/tools/fleet"
	"github.com/abhijitkrm/cometcli/internal/tools/fs"
	"github.com/abhijitkrm/cometcli/internal/tools/govtool"
	"github.com/abhijitkrm/cometcli/internal/tools/keystool"
	"github.com/abhijitkrm/cometcli/internal/tools/montool"
	"github.com/abhijitkrm/cometcli/internal/tools/nettool"
	"github.com/abhijitkrm/cometcli/internal/tools/networktool"
	"github.com/abhijitkrm/cometcli/internal/tools/node"
	"github.com/abhijitkrm/cometcli/internal/tools/runbooktool"
	"github.com/abhijitkrm/cometcli/internal/tools/sectool"
	"github.com/abhijitkrm/cometcli/internal/tools/shell"
	"github.com/abhijitkrm/cometcli/internal/tools/snaptool"
	"github.com/abhijitkrm/cometcli/internal/tools/triage"
	"github.com/abhijitkrm/cometcli/internal/tools/txtool"
	"github.com/abhijitkrm/cometcli/internal/tools/upgradetool"
	"github.com/abhijitkrm/cometcli/internal/tools/val"
	"github.com/abhijitkrm/cometcli/internal/tools/waittool"
	"github.com/abhijitkrm/cometcli/internal/tools/web"
)

// RegisterAll registers every tool in the catalog.
func RegisterAll(r *toolkit.Registry) {
	node.Register(r)
	val.Register(r)
	chain.Register(r)
	govtool.Register(r)
	evmtool.Register(r)
	keystool.Register(r)
	sectool.Register(r)
	montool.Register(r)
	upgradetool.Register(r)
	snaptool.Register(r)
	runbooktool.Register(r)
	fleet.Register(r)
	nettool.Register(r)
	networktool.Register(r)
	txtool.Register(r)
	waittool.Register(r)
	triage.Register(r)
	// general-purpose agent tools
	shell.Register(r)
	fs.Register(r)
	web.Register(r)
}
