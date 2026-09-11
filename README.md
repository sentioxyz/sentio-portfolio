# Sentio Portfolio

`sentio-portfolio` is the pure Go calculation kernel for wallet protocol
positions on Ethereum, BSC, Base, Arbitrum, and Sui. It owns protocol adapters,
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
from the host's `PriceProvider`: live scans through `USDPrices`, scans pinned to
fixed blocks through `USDPricesAt` at each pinned block's timestamp, so a
historical balance is never valued at today's price.

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

Sui uses a separate `SuiReader` for direct wallet holdings and
`NewSuiProtocolReader()` for latest NAVI lending, NAVI Multiply,
native NAVI vaults, Volo strategy vaults (including Single Loop and Astros),
and Suilend lending. The protocol IDs are `navi`, `volo-vaults`, and `suilend`.
`ReadLatest` takes a `SuiObjectReader`; NAVI and Volo discovery also requires
`SuiObjectLineageReader` (`SuiGRPCClient` implements both). All discovery and
state reads use that gRPC connection; these protocols require no dedicated
processor or separate object-directory service.

Suilend discovers every directly owned `ObligationOwnerCap` from its defining
package, deduplicates capabilities pointing to the same obligation, and follows
the obligation to its lending market. Market type, reserve index/coin identity,
and the dynamic object-field link back to the market's obligation table are
validated. This includes isolated lending markets without a market allowlist;
it does not enumerate the global obligation table. Strategies with nested
capabilities, liquid staking, standalone wallet cTokens and incentive rewards
are outside this lending surface.

Deposits convert cTokens through net reserve supply (available + borrowed -
unclaimed spread fees). Borrows apply the cumulative borrow index. Reserves
accrue to the observed checkpoint timestamp using the on-chain piecewise APR
curve, per-second compounding and spread fee. All operations use integer WAD
arithmetic and the Move operation order; raw token amounts are floored only
after conversion. Metadata comes from the chain and must agree with reserve
precision. USD prices remain the host's responsibility. The formulas follow
[Suilend's Move contracts](https://github.com/suilend/suilend/tree/devel/contracts/suilend/sources).

NAVI discovery follows the main Storage's market-counter object through its
producing transactions and previous object versions. Created Storage objects
are checked against current chain state and the counter's change. This visits
market-creation transactions, without scanning checkpoint ranges. Root IDs are
cached by counter version; new versions extend the inventory. The endpoint must
retain those specific transactions and object versions; missing lineage is an
explicit coverage error. MarketInfo and reserve tables establish relationships,
and `last_market_id` checks inventory completeness. New markets and
reserves need no address-list update. Capability ownership identifies Multiply
accounts, including child capabilities that share one logical account. User
principals and e-mode entries are batched point reads of derived dynamic field
IDs, without enumerating the protocol's user tables.

Wallet-owned receipts discover every native and Volo vault the wallet uses,
including strategy variants. NAVI receipts use `vault_address`; Volo receipts
use `vault_id`. The Volo oracle is discovered once from its defining package's
publication transaction. Vault shares, NAV tables and oracle prices come from current
chain state. Fresh or emptied receipts with no state entry contribute no
position; RPC failures remain coverage errors.

Reads report a latest observation window (`headBeforeRead`/`headAfterRead`),
not an atomic portfolio at one checkpoint. History is not supported. Hosts must
reject historical requests before reading current state. They must preserve the
window metadata when combining wallet and protocol snapshots.

Lending projects reserve interest indices to the observed timestamp using
integer RAY arithmetic and NAVI's fixed nine-decimal principal precision.
Multiply retains collateral and debt. Vault amounts use stored NAV
(`valuation: stored_nav`), without simulating a rebalance or harvest. Volo
includes pending deposits and claimable principal once, and reports the oldest
contributing NAV/oracle timestamp. Incentive rewards and withdrawal fees are
excluded; amounts describe positions, not an immediate redemption quote. Coin
precision comes from on-chain metadata, and the host supplies current prices.

The accounting follows the official [NAVI contracts](https://github.com/naviprotocol/navi-smart-contracts)
and [NAVI SDK](https://github.com/naviprotocol/naviprotocol-monorepo), with Volo
integer redemption operations checked against its published Move bytecode.

Run the local test suites with:

```sh
go test ./...
bazel test //...
```
