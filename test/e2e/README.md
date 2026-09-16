# E2E tests

Run cometcli's tool layer against a live cosmos-evm node.

## Spin a local evmd testnet

```bash
git clone https://github.com/cosmos/evm && cd evm
# build the reference chain
go build -o evmd ./evmd/cmd/evmd
# single-node localnet (see evmd guide for the canonical script)
./evmd init local --chain-id cosmos_262144-1
./evmd keys add validator --keyring-backend test
./evmd genesis add-genesis-account validator 1000000000000000000000atest --keyring-backend test
./evmd genesis gentx validator 1000000000atest --keyring-backend test --chain-id cosmos_262144-1
./evmd genesis collect-gentxs
./evmd start   # serves comet :26657, grpc :9090, evm json-rpc :8545
```

## Run the suite

```bash
COMETCLI_E2E=1 go test -tags e2e ./test/e2e/... -v
```

Optional env: `COMETCLI_E2E_COMET`, `COMETCLI_E2E_GRPC`, `COMETCLI_E2E_EVM`,
`COMETCLI_E2E_CHAIN`.
