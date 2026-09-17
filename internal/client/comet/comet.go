// Package comet wraps the CometBFT RPC client with the specific calls
// cometcli tools need.
package comet

import (
	"context"
	"strings"

	rpchttp "github.com/cometbft/cometbft/rpc/client/http"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	ctypes "github.com/cometbft/cometbft/types"
)

// Client is a thin wrapper over the cometbft RPC HTTP client.
type Client struct {
	RPC *rpchttp.HTTP
}

// New connects to a CometBFT RPC endpoint (tcp:// or http:// URL).
func New(endpoint string) (*Client, error) {
	if !strings.Contains(endpoint, "://") {
		endpoint = "tcp://" + endpoint
	}
	c, err := rpchttp.New(endpoint, "/websocket")
	if err != nil {
		return nil, err
	}
	return &Client{RPC: c}, nil
}

func (c *Client) Status(ctx context.Context) (*coretypes.ResultStatus, error) {
	return c.RPC.Status(ctx)
}

func (c *Client) Health(ctx context.Context) error {
	_, err := c.RPC.Health(ctx)
	return err
}

func (c *Client) NetInfo(ctx context.Context) (*coretypes.ResultNetInfo, error) {
	return c.RPC.NetInfo(ctx)
}

func (c *Client) ABCIInfo(ctx context.Context) (*coretypes.ResultABCIInfo, error) {
	return c.RPC.ABCIInfo(ctx)
}

// Validators returns the validator set at a height (paginated).
func (c *Client) Validators(ctx context.Context, height *int64, page, perPage int) (*coretypes.ResultValidators, error) {
	return c.RPC.Validators(ctx, height, &page, &perPage)
}

// AllValidators collects the full set across pages.
func (c *Client) AllValidators(ctx context.Context, height *int64) ([]*ctypes.Validator, error) {
	var out []*ctypes.Validator
	page := 1
	for {
		res, err := c.Validators(ctx, height, page, 100)
		if err != nil {
			return nil, err
		}
		out = append(out, res.Validators...)
		if len(out) >= res.Total {
			break
		}
		page++
	}
	return out, nil
}

func (c *Client) Block(ctx context.Context, height *int64) (*coretypes.ResultBlock, error) {
	return c.RPC.Block(ctx, height)
}

func (c *Client) BlockResults(ctx context.Context, height *int64) (*coretypes.ResultBlockResults, error) {
	return c.RPC.BlockResults(ctx, height)
}

func (c *Client) DumpConsensusState(ctx context.Context) (*coretypes.ResultDumpConsensusState, error) {
	return c.RPC.DumpConsensusState(ctx)
}

// Commit returns the signed commit at a height — used to count which
// validators signed a given block (missed-block tracking).
func (c *Client) Commit(ctx context.Context, height *int64) (*coretypes.ResultCommit, error) {
	return c.RPC.Commit(ctx, height)
}
