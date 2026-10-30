# Sentio Portfolio

`sentio-portfolio` is the pure Go calculation kernel for wallet protocol
positions on Ethereum, BSC, Base, Arbitrum, and Sui. It owns protocol adapters,
latest/fixed-block RPC reads, account attribution, index-backed position reads,
and valuation aggregation.

Plain holdings — the native coin and ERC-20s an account holds outside any
protocol — are reported by the `wallet` adapter. A host-injected
`WalletBalanceProvider` is the sole source of ERC-20 candidates for both live
and fixed-block scans. There is no static token list or metadata registry.
Provider metadata is used when complete; otherwise metadata is read on-chain,
once per engine, which keeps it for every later scan.

The RPC-settled block remains authoritative. Provider amounts are used directly
when their block number and hash match that pin, or on a live scan when the
provider's block time is within `LiveWalletSampleWindow` of the pin's
(`LiveWalletSample`), unless the provider marks the account `DiscoveryOnly`
because its amounts are not a block's balances; otherwise every discovered
balance is re-read at the settled block, including provider rows reporting zero.
Providers may supply per-account block metadata when address batches sample
different blocks. A candidate whose successful `balanceOf` returns empty data
is omitted, and so, at a fixed pin, is a candidate with no code at that block:
a token that did not exist yet cannot be held. Reverts of deployed tokens, RPC
failures, and malformed non-empty results remain errors.

Unavailable discovery and missing account results are `protocol` errors. A
target the provider marks unsupported, and a historical scan using a provider
sample from another block, are `coverage` notes: every returned quantity is
pinned, but a token held only at the historical block may be absent from
current discovery, so the response does not claim a complete historical token
universe. Native balances are read independently over RPC in every case. The
request tells the provider each chain's pin, so a provider that keeps samples
between scans can tell whether one is recent enough to discover from.

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

An indexer only decides which positions an adapter reads; every quantity is
still read at the pinned block. Nothing tops an index up from chain logs, so a
live scan reads it wherever its processor has got to. When the index trails the
pin by more than an hour, the scan still returns what it read, with a `coverage`
note on that protocol and chain: a position opened after the indexed block may
be missing. A scan pinned to fixed blocks needs every index past its pin and
reports a `protocol` error otherwise, so a host that keeps historical scans
retries it instead of storing a short answer.

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

Sui accounts are read by a `SuiEngine`, built with `NewSuiEngine(engine, SuiConfig{…})` from the
`Engine` whose indexer lane, observer and `SentioIndexers` it shares. It covers direct wallet
holdings, NAVI lending, NAVI Multiply, native NAVI vaults, Volo strategy vaults (including Single
Loop and Astros), Suilend lending, and Cetus/Bluefin concentrated liquidity; the protocol IDs are
`wallet`, `navi`, `volo-vaults`, `suilend`, `cetus`, and `bluefin`. `Scan` and `ScanWithOptions`
answer one request as a `SuiResponse`, shaped like `Response`; `ScanRange` answers a backfill. The
host supplies what the kernel does not own: `SuiConfig.Node` dials a node (`SuiGRPCNode`) on the
first read that needs one, `Prices` values coins through `SuiPrices`, and `WalletHistory` reads past
balances through `SuiWalletHistory`.

Each protocol reads its latest and its pinned state from one fixed source, and a source that fails
is never replaced by another, because each answers for a different checkpoint:

| Protocol | Latest | Pinned |
| --- | --- | --- |
| `wallet` | node holdings | wallet history, at the protocol samples' checkpoint |
| `navi` | node object state | processor index |
| `suilend` | processor index: the newest completed sample, at most 2h1m old | processor index |
| `volo-vaults`, `cetus`, `bluefin` | node object state | not read |

A pinned scan never reads node state: a latest-only protocol reports that it was not read rather
than answering for the head under the pin's name, and the only node request a pinned scan may make
is a coin's immutable metadata for wallet history. A request for indexed protocols alone needs no
node. Without a source, its protocols are reported unread, never empty: an unconfigured Suilend
index is an error, not a node read, because a node cannot answer for a past checkpoint and a head
answer under a past pin would be a wrong number nobody can see is wrong. NAVI and Volo discovery
reads `SuiObjectLineageReader` (`SuiGRPCClient` implements it) over the same connection.

Suilend and NAVI's history are read from version-pinned SQL processor indexes, configured as the
`Engine`'s `SentioIndexers["suilend"]` and `["navi"]`. A read executes one SQL statement, once,
against the pinned processor version: submitted to the async SQL endpoint on the LARGE engine and
polled until it finishes.

The index publishes immutable object-version rows (`PortfolioObjectState`), a
per-sample completion certificate (`PortfolioSnapshot`), coin metadata, and two
narrow read indexes ordered by object and by owner. A read selects the newest
completed sample at or before the request, resolves capabilities, obligations and
markets through those indexes, and compares the certificate's expected row count
with what is visible before accepting the sample. The returned checkpoint is the
sample's, not the current head, and group metadata reports the sample interval
the certificate itself bounds.

A range scan reads each index once over its instants' span: every completed sample whose
interval meets it, each owning `[Start, Until)`, and an instant is answered by the newest sample
covering it, as a single read at that instant would be. A pinned time scan is a range of one
instant. A statement covers several samples: it selects each stage's candidates once for all of
them, keeps per object only the newest version up to each sample, and counts every certificate in
one scan of their joint interval. The reader then repeats the single-sample selection for each
sample over those rows and certifies it with the same checks, so a sample whose certificate fails
carries its own error without affecting its neighbours. Statements are sized from the bytes per
sample the previous one returned, because the response is most of their cost. A statement whose
response is paged or past the body limit, or that the client or the endpoint gives up on, is asked
again for fewer samples.

The wallet is read at the state the protocols beside it describe, or an asset moving between them
would be counted twice or not at all: every protocol of an instant is answered at the oldest of the
checkpoints its samples are at — indexes sample on fixed UTC boundaries, a coarser one's a subset of
a finer one's, so each has a sample there — and wallet history is read at that checkpoint, once for
every instant of a range. Each sample is valued at its own timestamp, never at a later quote. A
range instant carries `Retry` when a source did not answer — an index read failed, wallet history
had not reached the checkpoint, the price service was unavailable — and wraps `ErrSuiRangeUnread`
when the scan ran out of time first; part of the deadline is kept for pricing what was read. An
index that answered, even with a sample it could not certify, is not a reason to retry.

Cetus and Bluefin discover directly owned `position::Position` NFTs from their
defining packages. Each position names its pool; only those pools, the two active
boundary ticks, and Cetus position accounting fields are fetched. Discovery never
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
end time. When pool state is newer than the observed head, stored growth is retained.
Coin metadata and USD valuation follow the same path as the other Sui protocols.
Each NFT remains a separate group, with principal, fees and rewards identified in
component metadata. This surface covers directly held CLMM positions; farm/strategy
wrappers, Cetus vault shares and Bluefin perpetual accounts are not enumerated.
The accounting layouts follow the [Cetus SDK](https://github.com/CetusProtocol/cetus-clmm-sui-sdk)
and [Bluefin contract interfaces](https://github.com/fireflyprotocol/bluefin-spot-contract-interface).

Suilend takes every `ObligationOwnerCap` the address held at the sample from the
index's owner index, deduplicates capabilities pointing to the same obligation,
and follows the obligation to its lending market. Market type, reserve index/coin identity,
and the dynamic object-field link back to the market's obligation table are
validated. This includes isolated lending markets without a market allowlist;
it does not enumerate the global obligation table. Strategies with nested
capabilities, liquid staking, standalone wallet cTokens and incentive rewards
are outside this lending surface.

Deposits convert cTokens through net reserve supply (available + borrowed -
unclaimed spread fees). Borrows apply the cumulative borrow index. Reserves
accrue to the sample's checkpoint timestamp using the on-chain piecewise APR
curve, per-second compounding and spread fee; utilization excludes unclaimed
spread from the available amount, which counts as zero when the spread exceeds
it (100% utilization). A reserve stamped later than the sample is a
contradiction the read rejects rather than projecting backwards. All operations use integer WAD
arithmetic and the Move operation order; raw token amounts are floored only
after conversion. Metadata comes from the chain and must agree with reserve
precision. The formulas follow
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

Latest reads report the heads observed before and after reading
(`headBeforeRead`/`headAfterRead`). A load-balanced endpoint can route these calls
to nodes at different heights. Regressing heads do not discard positions or
trigger synchronization retries; the original observations are preserved. They
do not bound the object versions read or describe an atomic portfolio at one
checkpoint. Volo, Cetus and Bluefin have no history, so a pinned scan reports them unread;
preserve this metadata when combining snapshots.

Lending projects reserve interest indices forward to the observed timestamp using
integer RAY arithmetic and NAVI's fixed nine-decimal principal precision.
If a NAVI reserve is newer than the observed head, its stored interest
is used without projecting backwards. Volo NAV and oracle timestamps describe
the objects read and need not precede the independently observed head.
Multiply retains collateral and debt. Vault amounts use stored NAV
(`valuation: stored_nav`), without simulating a rebalance or harvest. Volo
includes pending deposits and claimable principal once, and reports the oldest
contributing NAV/oracle timestamp. Incentive rewards and withdrawal fees are
excluded; amounts describe positions, not an immediate redemption quote. Coin
precision comes from on-chain metadata, and prices from the host's `SuiPrices`.

The accounting follows the official [NAVI contracts](https://github.com/naviprotocol/navi-smart-contracts)
and [NAVI SDK](https://github.com/naviprotocol/naviprotocol-monorepo), with Volo
integer redemption operations checked against its published Move bytecode.

Run the local test suites with:

```sh
go test ./...
bazel test //...
```

The Suilend index statement is also exercised against a real ClickHouse rather
than only against response fixtures, so that lifecycle ordering, dependency
selection and replay deduplication are tested by the engine that will run them:

```sh
PORTFOLIO_CLICKHOUSE_BINARY=$(command -v clickhouse) go test -run TestSuiSQLExecution .
```
