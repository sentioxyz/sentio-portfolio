# Sentio Portfolio

`sentio-portfolio` is the pure Go calculation kernel for wallet protocol
positions on Ethereum, BSC, Base, and Arbitrum. It owns protocol adapters,
latest/fixed-block RPC reads, account attribution, index-backed position reads,
and valuation aggregation.

Plain holdings — the native coin and ERC-20s an account holds outside any
protocol — are reported by the `wallet` adapter. A host-injected
`WalletBalanceProvider` is the sole source of ERC-20 candidates for both live
and fixed-block scans. There is no static token list or metadata registry.
Provider metadata is used when complete; otherwise metadata is read on-chain.

The RPC-settled block remains authoritative. Provider amounts are used directly
only when their block number and hash match that pin; otherwise every discovered
balance is re-read at the settled block, including provider rows reporting zero.
Providers may supply per-account block metadata when address batches sample
different blocks. A candidate whose successful `balanceOf` returns empty data
is omitted; reverts, RPC failures, and malformed non-empty results remain errors.

Unavailable discovery, unsupported chains, and missing account results produce
explicit coverage errors. Native balances can still be read independently over
RPC. Historical scans using a provider sample from another block report that
token discovery is incomplete: a token held only at the historical block may be
absent from current discovery. Returned quantities remain pinned, but the
response does not claim a complete historical token universe.

A token already counted by a protocol adapter is suppressed from wallet holdings
by its source contract and attributed account. Final USD valuation always comes
from the host's `PriceProvider`.

The repository deliberately does not own an HTTP or gRPC API, protobufs,
deployment configuration, authentication, or a concrete price service. A host
constructs `Engine` with chain RPC URLs and a `PriceProvider` implementation.

```go
engine := portfolio.NewEngineWithConfig(rpcURLs, priceProvider, portfolio.EngineConfig{
    WalletBalanceProvider: walletBalanceProvider,
})
result := engine.Scan(ctx, account)
```

Indexer-backed adapters receive their deployment-specific GraphQL/status
endpoints and processor versions through `EngineConfig`. Public source must not
contain project names or owner namespaces.

Uniswap V3 keeps the indexer as its fast discovery path. If that path is
unavailable, its [enumerable position manager](https://github.com/Uniswap/v3-periphery/blob/main/contracts/interfaces/INonfungiblePositionManager.sol)
can independently discover the wallet's complete NFT inventory at the settled
block. The RPC path validates every ID and owner, retains the 4,096-NFT limit,
and fails rather than returning a partial inventory. Its groups report
`discoverySource: rpc-enumeration` and `discoveryBlock`, not a fabricated
`indexerBlock`. V4 remains indexer-backed; this fallback does not apply to it.

Historical scans use a fail-closed availability registry in
`protocol_availability.go`. Every adapter must declare an explicit outer window
for every advertised chain before the engine can start; genesis support is
spelled out rather than inferred from a zero value. Adapters continue to gate
later markets, vaults, rewards, and replacement contracts with their narrower
component deployment windows.

Run the local test suites with:

```sh
go test ./...
bazel test //...
```
