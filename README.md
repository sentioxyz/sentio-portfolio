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

Sui uses a separate `SuiReader` for direct wallet coin holdings. Protocol
positions for `navi`, `volo-vaults`, `suilend`, `cetus` and `bluefin` come entirely
from five dedicated, version-pinned processor indexes. Go never calls a Sui node
for these protocols, including latest requests.

Construct `NewNaviHistoryReader`, `NewVoloHistoryReader`, `NewSuilendHistoryReader`,
`NewCetusHistoryReader` or `NewBluefinHistoryReader` with a
`SentioIndexerConfig{SQLURL: ..., ProcessorVersion: ...}`. Each exposes:

- `ReadLatest(ctx, owner)`: the newest completed hourly sample.
- `ReadAtCheckpoint(ctx, owner, sequence)` and `ReadAtTime(ctx, owner, at)`:
  completed hourly history, with the actual selected checkpoint in the result.

Every address/protocol request executes one SQL statement. The statement returns
its completed snapshot, address-owned positions, dependency objects, coin metadata
and optional protocol values. Go then performs local integer arithmetic. The SQL
uses the common `PortfolioSnapshot`, `PortfolioObjectState`,
`PortfolioTokenMetadata` and `PortfolioValue` schema, version 1. Processor
publication coverage starts at NAVI 7,877,880, Volo 172,857,371, Suilend 28,510,257,
Cetus 1,579,561 and Bluefin 71,783,891.

At hourly callback H, the processor materializes the final changed objects for
sample P. The snapshot carries a certificate of the object and value row counts
for that interval. One SQL returns the expected counts and the observed counts,
computed once in the snapshot output. Go compares them before accepting P, without
any additional request.
Object sources are bounded by P and materialization by H. The raw object history
view avoids a global entity aggregation; the query selects each object’s latest
lifecycle and deduplicates replayed quote IDs itself. Ownership and dynamic
field candidates are resolved to their latest lifecycle before filtering the
current relation, so transfers, wrapping and deletions cannot resurrect old state.
The requested time/checkpoint must belong to `[P,next)`. A chain halt can make this
interval longer than an hour; it does not invalidate complete history.

The matching `SuiProtocolReader.With*History` methods configure indexed latest
routing. Missing or incomplete indexes, missing required contents and truncated
SQL responses fail explicitly, without any node fallback or automatic SQL retry.
Empty wallets still require the completed snapshot sentinel. Hosts enforce latest freshness; hourly
materialization can leave the newest completed sample nearly two hours old.

Volo reads each nonzero NAV asset's settlement timestamp and transaction from
the index, then selects the base-coin quote written in that transaction. Vaults
with the same base coin retain independent settlement quotes. A newer global
quote is never substituted. Zero NAV entries do not determine the settlement
period. Pending deposits and claimable principal retain their own amounts.

Use `WithEngine(engine)` to share the host's indexer concurrency lane. Historical
valuation must quote the returned sample timestamp and report missing prices;
current prices are not a fallback. Direct wallet coin history remains unsupported.

Cetus and Bluefin discover directly owned `position::Position` NFTs from their
defining packages. Each position names its pool; only those pools, the two active
boundary ticks, and Cetus position accounting fields are selected by the SQL. Discovery never
enumerates a global position or tick table. Pool types, coin types, ownership,
field keys, and NFT/accounting identities must agree. Cetus calculations use the
pool's `PositionInfo` liquidity; the NFT's display liquidity can remain stale after
a liquidity cut. Accounting is read before choosing active boundary ticks, and its
object ID/version are included in group metadata. Missing state is a coverage
error. A zero-liquidity position still contributes unpaid fees and rewards without
reading boundary ticks.

Principal uses the pool and boundary square-root prices with Q64 integer withdrawal
rounding. Fees include stored amounts plus uncollected inside growth, with wrapping
u128 counters. Rewards advance from stored growth to the observed checkpoint time
using pool liquidity and emission rates; Bluefin emissions stop at their configured
end time. Stored growth is never extrapolated backwards.
Coin metadata and USD valuation follow the same path as the other Sui protocols.
Each NFT remains a separate group, with principal, fees and rewards identified in
component metadata. This surface covers directly held CLMM positions; farm/strategy
wrappers, Cetus vault shares and Bluefin perpetual accounts are not enumerated.
The accounting layouts follow the [Cetus SDK](https://github.com/CetusProtocol/cetus-clmm-sui-sdk)
and [Bluefin contract interfaces](https://github.com/fireflyprotocol/bluefin-spot-contract-interface).

Suilend discovers directly owned `ObligationOwnerCap` objects from the index,
deduplicates capabilities pointing to the same obligation, and follows each
obligation to its indexed lending market. Market type and reserve index/coin
identity must agree. The obligation owner must equal the dynamic-object-field
ID derived from the market's obligation table, the obligation ID, and the exact
`Wrapper<object::ID>` type tag. This verifies the parent link without a node read.
Only markets referenced by the address's held capabilities are needed.
This includes isolated lending markets without a market allowlist; it does not
enumerate the global obligation table. Strategies with nested
capabilities, liquid staking, standalone wallet cTokens and incentive rewards
are outside this lending surface.

Deposits convert cTokens through net reserve supply (available + borrowed -
unclaimed spread fees). Borrows apply the cumulative borrow index. Reserves
accrue to the selected hourly sample timestamp using the on-chain piecewise APR
curve, per-second compounding and spread fee. All operations use integer WAD
arithmetic and the Move operation order; raw token amounts are floored only
after conversion. Reserve timestamps newer than the sample fail closed. Metadata
comes from the index at H and must agree with reserve precision. USD prices
remain the host's responsibility. The formulas follow
[Suilend's Move contracts](https://github.com/suilend/suilend/tree/devel/contracts/suilend/sources).

NAVI's indexed main Storage, MarketInfo and reserve objects establish the market
inventory. The processor validates market and reserve inventory completeness. New markets
and reserves need no address-list update. Capability ownership identifies
Multiply accounts, including capabilities sharing one logical account.
Principals use derived dynamic field IDs; readers do not enumerate user tables.

Receipt ownership discovers native NAVI and Volo vaults. NAVI receipts use
`vault_address`; Volo receipts use `vault_id`. Fresh or emptied receipts may have
no state field. Required vault/NAV/quote contents and lifecycle records remain
mandatory. Volo's oracle root is discovered from its processor catalog.

NAVI lending accrues indexed reserve interest to the selected sample timestamp
using integer RAY arithmetic and fixed nine-decimal principal precision. Native
vaults report stored NAV; Volo reports `valuation: settled_nav` and the NAV/quote
timestamps. These calculations do not simulate a harvest or rebalance. Incentive
rewards and withdrawal fees are excluded; amounts describe positions, not an
immediate redemption quote. Coin precision comes from indexed metadata, and the
host supplies prices for the requested valuation time.

Formulas follow the [NAVI SDK](https://github.com/naviprotocol/naviprotocol-monorepo), with Volo
integer redemption operations checked against its published Move bytecode.

Run the local test suites with:

```sh
go test ./...
bazel test //...
```
