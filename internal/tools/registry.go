package tools

import (
	"github.com/abhijitkrm/cometcli/internal/tools/chain"
	"github.com/abhijitkrm/cometcli/internal/tools/evmtool"
	"github.com/abhijitkrm/cometcli/internal/tools/keystool"
	"github.com/abhijitkrm/cometcli/internal/tools/node"
	"github.com/abhijitkrm/cometcli/internal/tools/sectool"
	"github.com/abhijitkrm/cometcli/internal/tools/val"
	"github.com/abhijitkrm/cometcli/internal/toolkit"
)

// RegisterAll registers every tool in the catalog.
func RegisterAll(r *toolkit.Registry) {
	node.Register(r)
	val.Register(r)
	chain.Register(r)
	evmtool.Register(r)
	keystool.Register(r)
	sectool.Register(r)
}
