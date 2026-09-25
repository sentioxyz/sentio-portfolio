# Repository guidance

## Indexer topology

Every adapter that needs a Sentio indexer gets its own processor project. Never
share one project across protocols, even when the entity schemas would fit in a
single processor:

- one `SentioIndexerConfig` per protocol ID, supplied by the host service as a
  distinct project and processor version;
- the generic indexer clients (`ownerTokenIndexer`, `accountRequestIndexer`) are
  shared *code*, not a shared deployment — reuse the client, keep the project
  separate;
- a protocol's backfill state, checkpoint lag, and schema migrations must never
  be able to stall or break an unrelated protocol's positions;
- adding a protocol means a new project, a new processor version, and new host
  configuration — never an extra contract binding inside another protocol's
  processor.

### Indexer requests

Every indexer request passes through one `indexerLane` per engine (`sentio_lane.go`), sized
by `EngineConfig.IndexerConcurrency`: the adapters share one API key, so admission is bounded
per engine, not per protocol. Do not add a second lock or a per-adapter limiter in front of it.

`sentioAPIClient` keeps one `http.Client` that is never replaced and never locked, on a transport
restricted to HTTP/1.1 and offering only http/1.1 through ALPN (`newSentioTransport`; a clone of the
default transport still offers h2, and the edge would take it). One connection per in-flight request means a stalled
request stalls only itself, and net/http closes a connection whose request timed out rather than
returning it to the pool, so a retry never lands on it. Do not enable HTTP/2 on that transport
and do not put a mutex or a client rotation around the request: multiplexing puts every chain of a
protocol on one connection, and the lock that made rotation safe serialized them all behind one
request. Besides the lane, the only place indexer requests wait on each other is the status cache,
whose fetch runs under `statusMu` so concurrent chains of one protocol share one status read.

An adapter whose protocol spans several chains should answer "which chains hold anything for
this account" with one request rather than one per chain: `sentio_presence.go` aliases a
time-travel query per chain, at that chain's own indexed block, and the per-chain flow skips
only chains the probe proved empty. The proof must stay exact — same block, same checkpoint
validation, same where clause or a superset of it — and the RPC tail after the indexed block
must keep running for skipped chains. Refs are deleted when positions close, so never decide
presence from the latest state of the index.

The probe costs one request that the GraphQL server executes alias by alias, so it is slower
than a single-chain page. The engine therefore runs it ahead of the workers
(`presencePrefetcher`, started right after chain setup) rather than on the first chain job; an
adapter that adds a probe must implement `prefetchPresence` so its cost stays off the critical
path. A proof established at an earlier indexed block still holds — the chain job reports that
block as its indexed block so the RPC tail starts there — except for an indexer with no RPC
tail, which may only use a proof at the very block it would query.

## Wallet holdings

A host-injected `WalletBalanceProvider` is the only ERC-20 discovery source for
live and historical scans. Do not add static token lists, token-list unions,
static metadata lookups, or an ERC-20 fallback when discovery fails.

The RPC-settled block remains authoritative. Provider amounts may be used only
when the provider block number and hash exactly match that pin. Per-account
block metadata overrides the shared chain sample when accounts were queried in
separate batches. Otherwise re-read native and every discovered ERC-20 balance
at the settled block, including provider rows whose latest amount is zero.
Never label a latest provider amount with an earlier `BlockRef`.

Filter zero amounts before metadata enrichment when blocks match. When they
differ, first re-read the balance, then enrich non-zero results from provider
metadata or on-chain `symbol` and `decimals` (including bytes32 symbols). Never
invent either field or obtain it from a static token registry.

An empty successful `balanceOf` return disqualifies a discovery candidate for
that scan. Reverts, RPC failures, and malformed non-empty results remain errors.
Do not implement an address blacklist or silently turn failed reads into zero.

Provider failure, unsupported chains, and missing account results must surface
explicit coverage errors; native balances can still be read independently.
Historical scans with discovery sampled at another block must report incomplete
token discovery: assets held only at the requested block may be absent. Pinned
quantities do not establish a complete historical token universe.

The provider is not the price source. `PriceProvider` alone supplies valuation.

`suppressDuplicateHoldings` drops a holding when a protocol component on the
same chain and attributed account declares that token in `Source.Holds`. An
adapter that consumes an ERC-20 balance of the account — an LST it converts,
the aToken or debt token behind a data provider's amount, vault or gauge
shares, an LP token it decomposes — must declare the token there, whatever
`Source.Contract` names: the converter, oracle or data provider an amount came
from is provenance, not a holding. Declare only balances the component
consumed; a declared token disappears from the wallet, so naming an underlying
the account also holds outright hides a real balance. A component that consumed
no ERC-20 balance — a staking contract's internal balance, say — declares an
empty `Holds`. Only a nil `Holds` falls back to treating `Source.Contract` as
held when `Source.Method` mentions `balanceOf`, an inference that misses every
other shape, so new code declares explicitly.

## Sui reads

Sui is read through `SuiReader` (`sui.go`), implemented by `SuiGRPCClient` (`sui_grpc.go`) over
the `sui.rpc.v2` services a fullnode, or a proxy in front of one, serves. The rules:

- The services answer for the head only. A holdings read records two head observations
  (`GetServiceInfo` before, `GetCheckpoint` after): `Checkpoint` is the
  head seen after the read and `HeadBeforeRead` the one seen before. Pooled backends may return
  regressing heads or objects newer than either head; preserve these observations without
  rejecting positions, clamping metadata, or retrying just to order the heads. They are not
  bounds on the object versions returned. A consumer must surface a
  head read as such; the rule that a latest amount is never labelled with an earlier `BlockRef`
  applies here too.
- Direct wallet history is unavailable. A pinned read (`Holdings` with a non-nil checkpoint)
  returns no balances with `HistoryUnsupported` set and makes no round trip; it never reads the
  head under the pin's name. Present that result as not read, never as nothing held. Reading
  specific retained object versions through gRPC does not establish complete wallet holdings
  at a historical checkpoint; there is no wallet-history fallback.
- NAVI, Volo, Cetus and Bluefin *latest* reads use `SuiProtocolReader.ReadLatest` with direct object state.
  Root objects are discovered through `SuiObjectLineageReader` over the same gRPC connection:
  NAVI market-counter versions and their producing transactions, and Volo package publication.
  Cache root IDs, refresh NAVI discovery when the counter version changes, and never scan
  checkpoint ranges. Missing retained lineage is a coverage error. No dedicated protocol
  processor is required. Check market inventory completeness against chain state,
  attribute capabilities/receipts to their current owners, and report the latest read window.
  Historical protocol requests are unsupported on this path and must not read latest state under
  a past pin; NAVI's history is served by its index instead, see below.
- Suilend is read from a version-pinned SQL processor index (`sui_history.go`, `sui_sql.go`,
  `sui_sql_query.go`), for latest and historical positions alike. Each address read is one SQL
  statement executed once, with no automatic HTTP retry, no Sui node call and no status or
  GraphQL follow-up; an unconfigured index is an error, never a node read. Hosts supply
  `SentioIndexerConfig.SQLURL` and an explicit processor version. Volo, Cetus and Bluefin
  still read head state from a node and still have no history.
- NAVI is read from the same index machinery, but only for history. Its
  `ReadLatest` stays on the node, which answers for the head rather than for the
  newest completed sample; `NaviHistoryReader.ReadAtTime` / `ReadAtCheckpoint`
  answer what a node cannot. Both paths run the same `naviLending` and
  `suiVaults` over a `suiProtocolState`, so only where that state is read
  differs — the test that matters builds one holding both ways and compares the
  groups. Do not make the indexed path a fallback for a failed node read: the
  two answer for different checkpoints.
- NAVI's dependency levels are not a chain. Account capabilities and vault
  receipts are owned outright, a receipt names its vault, and reserves, markets
  and storage are one protocol-wide topology every account shares and every read
  fetches whole — the topology is what names the reserve tables a principal must
  sit under, so it cannot be derived from the account's own rows. Principals and
  receipt states are dynamic fields underneath, so they are object-owned by
  their table rather than by the account: reach principals through the owner
  index row the processor writes for the account the field's name names, never
  by scanning the kind. The principal kind is an order of magnitude larger than
  any other in the index, so that scan is the shape that cost Suilend seconds a
  read. The key and parent predicates stay on the state rows and the calculator
  repeats them: the index says which objects an account has ever had a position
  in, those checks decide whether a row is a live position in a reserve of the
  observed topology.
- NAVI also certifies `PortfolioValue` rows, which Suilend does not: e-mode is a
  running per-account setting only its events report, so the latest row at or
  before P is the one in force and the range has no lower bound. `valueCount` is
  compared against an observed count the same way `objectCount` is — a protocol
  that declares no value kinds still fails closed on a nonzero `valueCount`, and
  the observed count is only read where the schema has that table.
- The Suilend and NAVI schema is version 2: `PortfolioSnapshot`, `PortfolioObjectState` and
  `PortfolioTokenMetadata`, plus the narrow `PortfolioObjectIndex` and `PortfolioOwnerIndex`
  read indexes. Shared checkpoint, version, timestamp and count fields are native signed 64-bit
  integers. Object-state IDs are `<kind>:<20-digit source checkpoint>:<immutable object-version
  ID>`; the source checkpoint, never the later materialization checkpoint, decides replay
  identity. A certified `valueCount` is a contract this reader does not implement, so it fails
  closed rather than being ignored.
- The statement selects a sample P with materialization H and bounds all state by source P and
  visibility H. Its snapshot branch returns the expected and observed changed-object counts;
  compute those counts only in that branch, never in the snapshot CTE the dependency bounds
  reuse. Go compares them before accepting P, without another request. Fewer visible rows than
  certified is incomplete; more is not, because the processor counts its rows with a store list
  that a concurrent commit can outrun. Return P, not H, as the quantity timestamp. A requested
  checkpoint or time belongs to `[P,next)`, which may exceed the sampling interval during a
  chain halt, and `sampleIntervalSeconds` is derived from the certificate's own bound rather
  than assumed.
- Resolve every dependency level through the narrow read indexes: root candidates from one
  `PortfolioOwnerIndex` prefix range per root kind, each stage's latest state IDs from
  `PortfolioObjectIndex`, then the state rows by exact ID. Scanning a kind's ID range instead
  cost seconds per stage at six million rows. Keep each stage's IDs in a scalar array alias: a
  CTE is expanded again at every reference, so a chain of them rescans the table once per
  dependency level and per output branch. Build a dynamic ID bound entirely inside one scalar
  subquery — key analysis takes a scalar as a constant but will not evaluate `concat` over one,
  and the range degrades to a scan to the end of the table.
- Discover object IDs using historical owner/parent/key predicates, select the latest lifecycle
  for those IDs, and only then filter the current owner and live relation. Immutable version IDs
  sort a terminal row after a live row of the same version, so filtering terminals out before
  latest selection resurrects a closed position. Deduplicate by object identity to absorb
  deterministic replay writes. An obligation's membership in a market is checked by deriving its
  dynamic-object-field parent, not by trusting the row's owner.
- Suilend interest accrues to P's timestamp; a reserve newer than P fails closed. Match Move WAD
  rounding. Parse each lending market once per read — the main pool's JSON is hundreds of
  kilobytes and every account of a snapshot would otherwise re-parse it.
- NAVI and Volo NAV/oracle timestamps are object metadata, not assertions against a head.
- Volo's stored USD NAV must use the base-coin oracle version from the same valuation
  transaction, never a newer global quote. Follow the nonzero NAV timestamp fields to that
  transaction and read its quote version over gRPC. Keep a separate quote per vault, including
  vaults with the same base coin. Missing retained versions or mixed nonzero NAV periods are
  coverage errors. Report `valuation=settled_nav` and the NAV/quote timestamps: current receipt
  ownership does not make a stored valuation a live strategy valuation. Zero asset rows do not
  determine the valuation period. Ordinary portfolio history remains unsupported.
- A checkpoint is the pin: sequence number, 32-byte digest and timestamp fill `BlockRef` as a
  block does. `GetCheckpoint` is asked with a read mask for those three fields only. The dialer
  verifies the endpoint's chain identifier (`GetServiceInfo.chain_id`, the base58 genesis digest
  whose first four bytes are the JSON-RPC spelling) the way `DialRPC` verifies `eth_chainId`.
- The endpoint's scheme selects transport security: `grpc://` and `http://` are plaintext,
  `grpcs://`, `https://` and a bare `host:port` are TLS. Anything beyond `host:port` is refused
  rather than silently ignored.
- The chain enumerates holdings itself, so Sui needs no `WalletBalanceProvider`. `ListBalances`
  is paginated to completion and fails past its page bound rather than truncating.
- Coin types are normalized with `NormalizeMoveType` (zero-padded lowercase addresses, generics
  joined with a bare comma), the form the chain's long spelling and the host's price service
  both use.
- Coin metadata comes from `GetCoinInfo` or not at all: a coin whose metadata is absent
  (`NotFound`, or no metadata object) or unusable is a reported gap, never a guessed symbol or
  precision. There is no batch form, so reads run a few at a time.
- Statuses about the request (`NotFound`, `InvalidArgument`, `Unauthenticated`, ...) are final;
  transient ones (`Unavailable`, `ResourceExhausted`, `Aborted`, `DeadlineExceeded`, `Unknown`,
  `Internal`) are retried with backoff. Errors keep the status code, reachable through
  `status.Code`, and never the endpoint: transport-level statuses drop their message because grpc
  spells the dial target out in it.

## Historical valuation

A scan pinned to fixed blocks holds what the account had then, so the engine
values it through `PriceProvider.USDPricesAt` at each pinned block's timestamp;
only live scans use `USDPrices`. Never fall back from a historical quote to the
latest one: an unpriced component is a gap the response reports, while a current
price on a past balance is a wrong number nobody can see is wrong. A provider
that cannot serve a historical quote must return a `PriceFailure` for the token.

## Pricing a token nothing quotes

An adapter that reads a token no price provider knows has two honest options, and
which one applies depends on whether the account holds that token.

- **A position that decomposes** reports the tokens it decomposes into. A Pendle
  liquidity position is the holder's share of the market's reserves, so it reports
  those reserves; the account does not hold an LP token's worth of anything else.
- **A token held outright** keeps its own identity and sets `Component.PriceBasis`:
  the quoted token to value it through, plus the price ratio between the two in 1e18
  fixed point. Converting the amount instead would report an asset the account does
  not hold, and would key an external comparison on a different token than the source
  uses — the DeBank harness keys on `Component.Token.Address`.

`PriceBasis` redirects valuation only. `PriceUSD` still ends up being the price of the
component's own token, so consumers never need to know a basis was involved. The
response's `prices` map stays what the provider actually quoted, which is why a
consumer must read a component's own `priceUsd` rather than looking its token up
there.

A basis is not licence to invent a number. Every input must be read, not assumed: if
the ratio cannot be established the component keeps its unquoted token and no basis,
because an unpriced component is a gap the response reports whereas a guessed one is a
wrong number nobody can see is wrong.

That includes solvency. A wrapper's claim on its underlying is only whole while the
underlying has held its value, and the impaired case is exactly the one where an
overstatement matters, so the factor is read rather than defaulted to one. Pendle's
PT/YT ratios carry `syIndex / pyIndexStored` for this reason, and a pair whose index
cannot be read stays unpriced.

## Deployment windows

Every contract address the kernel reads — hardcoded anchors, manifest entries, and
registries alike — must be gated by the block that created it:

- an `eth_call` against an address that has no code yet returns empty data, which
  fails strict batch decoding and drops the protocol's whole surface for every
  fixed-block scan inside the gap (an Aave v4 hub deployed late broke historical
  scans across a 600k-block interval exactly this way);
- carry a `deploymentWindow{ActivationBlock: …}` next to every static address and
  skip the contract whenever the window is not active at the pinned block;
- addresses enumerated on-chain at the pinned block (registry and factory getters)
  are self-gating — a registry cannot return a contract that does not exist yet —
  but the registry contract itself still needs its own window;
- establish a creation block with an `eth_getCode` binary search rather than
  trusting documentation or a first event, and treat it as closed history;
- cover each new window with a boundary regression test.

## Sensitive data

This is a public repository. Never commit secrets or environment-specific
credentials, including:

- RPC URLs, API keys, access keys, bearer tokens, passwords, or private keys;
- authenticated service endpoints or URLs containing tokens or credentials;
- Sentio project names or slugs, owner namespaces, processor identifiers, or
  endpoint paths that embed any of those values;
- `.env` files, local override files, shell history, captured production
  responses, or test fixtures copied from private systems;
- internal hostnames, private network addresses, or deployment credentials.

Use obvious placeholders in examples and tests. Supply required endpoints and
credentials, including Sentio indexer endpoints and processor versions, only at
runtime through the host service's secret/configuration system. Error messages
must redact endpoint URLs.

Before committing or pushing, inspect every new and modified file and scan the
entire repository for credentials and environment-specific endpoints. If there
is any doubt whether a value is sensitive, do not commit it.
