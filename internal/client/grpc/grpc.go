// Package grpc provides a single gRPC connection to a cosmos-evm node plus
// typed query helpers built on the generated cosmossdk.io/api clients.
package grpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	authv1beta1 "cosmossdk.io/api/cosmos/auth/v1beta1"
	bankv1beta1 "cosmossdk.io/api/cosmos/bank/v1beta1"
	distv1beta1 "cosmossdk.io/api/cosmos/distribution/v1beta1"
	govv1 "cosmossdk.io/api/cosmos/gov/v1"
	govv1beta1 "cosmossdk.io/api/cosmos/gov/v1beta1"
	mintv1beta1 "cosmossdk.io/api/cosmos/mint/v1beta1"
	slashingv1beta1 "cosmossdk.io/api/cosmos/slashing/v1beta1"
	stakingv1beta1 "cosmossdk.io/api/cosmos/staking/v1beta1"
	txv1beta1 "cosmossdk.io/api/cosmos/tx/v1beta1"
	upgradev1beta1 "cosmossdk.io/api/cosmos/upgrade/v1beta1"
)

// Conn bundles the connection and all module query clients.
type Conn struct {
	cc *grpc.ClientConn

	Auth     authv1beta1.QueryClient
	Bank     bankv1beta1.QueryClient
	Dist     distv1beta1.QueryClient
	GovV1    govv1.QueryClient
	GovBeta  govv1beta1.QueryClient
	Mint     mintv1beta1.QueryClient
	Slashing slashingv1beta1.QueryClient
	Staking  stakingv1beta1.QueryClient
	Tx       txv1beta1.ServiceClient
	Upgrade  upgradev1beta1.QueryClient
}

// Dial connects. Endpoint may be "host:port" or "host:port;tls".
func Dial(ctx context.Context, endpoint string) (*Conn, error) {
	useTLS := false
	if strings.HasSuffix(endpoint, ";tls") {
		useTLS = true
		endpoint = strings.TrimSuffix(endpoint, ";tls")
	}
	var creds credentials.TransportCredentials
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	} else {
		creds = insecure.NewCredentials()
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cc, err := grpc.DialContext(dctx, endpoint,
		grpc.WithTransportCredentials(creds),
		grpc.WithBlock(), // fail fast when the endpoint is dead
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(32 << 20)),
	)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", endpoint, err)
	}
	return &Conn{
		cc:       cc,
		Auth:     authv1beta1.NewQueryClient(cc),
		Bank:     bankv1beta1.NewQueryClient(cc),
		Dist:     distv1beta1.NewQueryClient(cc),
		GovV1:    govv1.NewQueryClient(cc),
		GovBeta:  govv1beta1.NewQueryClient(cc),
		Mint:     mintv1beta1.NewQueryClient(cc),
		Slashing: slashingv1beta1.NewQueryClient(cc),
		Staking:  stakingv1beta1.NewQueryClient(cc),
		Tx:       txv1beta1.NewServiceClient(cc),
		Upgrade:  upgradev1beta1.NewQueryClient(cc),
	}, nil
}

// RawConn exposes the underlying connection for services not wrapped here.
func (c *Conn) RawConn() *grpc.ClientConn { return c.cc }

func (c *Conn) Close() error { return c.cc.Close() }

// Account fetches account_number/sequence for an address. Cosmos-EVM chains
// may return either BaseAccount or EthAccount; both are handled.
func (c *Conn) Account(ctx context.Context, bech32Addr string) (num, seq uint64, err error) {
	res, err := c.Auth.Account(ctx, &authv1beta1.QueryAccountRequest{Address: bech32Addr})
	if err != nil {
		return 0, 0, err
	}
	if res.Account == nil {
		return 0, 0, fmt.Errorf("account %s not found", bech32Addr)
	}
	return DecodeAccount(res.Account.TypeUrl, res.Account.Value)
}
