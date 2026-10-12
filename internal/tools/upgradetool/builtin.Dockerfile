# cometcli's built-in Dockerfile for official cosmos/evm releases — the one
# primium node-setup builds with (Dockerfile.primium). Used when a host has none.
#
# Primium build — uses Go 1.25.9 (satisfies go.mod >= 1.25.9), RocksDB compiled in, goleveldb used at runtime
#
# IMG_TAG must track the Alpine version underlying golang:1.25.9-alpine below
# (currently 3.23), not float on "latest": the evmd binary is CGO-linked
# against the builder stage's rocksdb-dev at build time, and the runtime
# stage's rocksdb *shared library* must be the same version or the binary
# fails at startup with "Error relocating /evmd/evmd: rocksdb_...: symbol
# not found" — alpine:latest and golang:*-alpine drift to different Alpine
# releases independently, which pull different (ABI-incompatible) rocksdb
# major versions from each release's package repo.
ARG IMG_TAG=3.23

# ──────────────────────────────────────────────────────────────
# Stage 1: Compile the evmd binary
# ──────────────────────────────────────────────────────────────
FROM golang:1.25.9-alpine AS evmd-builder
WORKDIR /work

ENV PACKAGES="curl build-base git bash file linux-headers eudev-dev \
    rocksdb-dev snappy-dev lz4-dev zstd-dev bzip2-dev zlib-dev"
RUN apk add --no-cache $PACKAGES

COPY go.mod go.sum* ./
RUN go mod download

COPY . .
RUN LEDGER_ENABLED=false COSMOS_BUILD_OPTIONS="rocksdb" make build
RUN echo "Checking binary linkage..." && file /work/build/evmd

# ──────────────────────────────────────────────────────────────
# Stage 2: Runtime image
# ──────────────────────────────────────────────────────────────
FROM alpine:$IMG_TAG AS run
# rocksdb + its runtime deps required for the CGO-linked binary
RUN apk add --no-cache jq bash curl rocksdb snappy lz4-libs zstd-libs bzip2 zlib
RUN addgroup -g 1025 nonroot
RUN adduser -D nonroot -u 1025 -G nonroot

# Set up the runtime environment
EXPOSE 26656 26657 1317 9090 26660 8545 8100
STOPSIGNAL SIGTERM
VOLUME /evmd
WORKDIR /evmd

# Copy the wrapper script and binary to expected locations
COPY contrib/images/evmd-env/wrapper.sh /usr/bin/wrapper.sh
COPY --from=evmd-builder /work/build/evmd /evmd/
COPY --from=evmd-builder /work/build/evmd /usr/local/bin/

# Set proper ownership and permissions before switching to nonroot user
RUN chown nonroot:nonroot /usr/bin/wrapper.sh && chmod +x /usr/bin/wrapper.sh
RUN chown -R nonroot:nonroot /evmd

USER nonroot

ENTRYPOINT ["/usr/bin/wrapper.sh"]
CMD ["start", "--log_format", "plain", "--minimum-gas-prices", "0.0001atest", "--json-rpc.enable", "true", "--json-rpc.api", "eth,txpool,personal,net,debug,web3", "--chain-id", "local-4221"]
