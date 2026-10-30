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

### Indexed reads

An adapter reads a Sentio index only through the indexed read module (`sentio_index.go`). A
`sentioIndex` is one deployed index — a protocol's project at its processor version — declared
once with the chains it indexes, whether it probes, and the start block that stands in for an
unwritten checkpoint. A read declares **entity streams** (`indexStream`): one GraphQL entity each,
paged by `id` under its own cursor, with its where clause (`indexWhere`), its row bound, and a
`Decode` that checks what only the protocol knows of a row. The module owns everything else, in
this order: the processor status (`indexQueryBlock`), the presence proof, the pages — every stream
of the read in one request per page, rows in `id` order within the stream's prefix — and the
checkpoint that certifies them (`checkIndexCheckpoint`). A read returns an `indexAt`, where it
answered; a lookup that depends on what the read found (Pendle's token and market lookups) reads
at that `indexAt`, at the same block and held to the same checkpoint. Do not send an indexer
request beside the module, and do not move a rule below into a client: each lives in the module
once.

### Indexer requests

Every GraphQL query and SQL statement passes through one `indexerLane` per engine
(`sentio_lane.go`), sized by `EngineConfig.IndexerConcurrency`: the adapters share one API key,
so admission is bounded per engine, not per protocol. Do not add a second lock or a per-adapter
limiter in front of it. A GraphQL request — a page, a lookup or a presence probe — holds its slot
for one attempt's round trip and gives it back before a retry backs off
(`sentioAPIClient.graphql`). A SQL statement holds its slot from submission until it finishes,
polls included, since the statement is what occupies the key's server-side queue. Nothing else
holds a slot: not a status read, not a wait for a presence proof, not a stand-in's block read.
Never hold one across anything but a request: a read may wait for a probe that needs a slot to
send, and with a lane of one a waiter holding the slot would deadlock the scan. Processor status
reads take no slot: they ask the processor status API, which runs none of the key's queries, and
normally once per indexer per `sentioStatusCacheTTL`; every chain job of a processor waits on the
same read, so never put it behind the lane.

`sentioAPIClient` keeps one `http.Client` that is never replaced and never locked, on a transport
restricted to HTTP/1.1 and offering only http/1.1 through ALPN (`newSentioTransport`; a clone of the
default transport still offers h2, and the edge would take it). One connection per in-flight request means a stalled
request stalls only itself, and net/http closes a connection whose request timed out rather than
returning it to the pool, so a retry never lands on it. Do not enable HTTP/2 on that transport
and do not put a mutex or a client rotation around the request: multiplexing puts every chain of a
protocol on one connection, and the lock that made rotation safe serialized them all behind one
request. Besides the lane, the only place indexer requests wait on each other is the status cache.
A stale entry is refreshed by one read per `StatusURL@version` at a time, shared by every caller
that needs it while it runs, failure included; a failure is not cached. The read runs on a context
no caller owns, bounded by `sentioStatusRefreshTimeout`, and each caller waits for it or for its own
context, whichever ends first, so the caller that happened to start it can leave without failing
the others. The mutex in front of the cache is held only to read or swap an entry. Keep both
properties: a scan's collection deadline is only a bound if every wait under it ends with its
context — so never send the read under that mutex, nor on a caller's context. mETH reads its status through the same client.

A scan with `ScanOptions.CollectDeadline` stops reading at that time and reports each chain or
deployment it did not finish as an `incomplete` error rather than a failure; pricing then runs on
its own context, bounded by `PricingDeadline`, so what was read is still valued. Pricing cut short
by its context adds one `incomplete` error about no chain, kept apart from per-token `pricing`
errors: a token the provider refuses is unquotable, a token it never reached is not. Any new wait
on the scan path must select on its context for that to hold.

An indexer query keyed by an account also bounds `id` by the account's row prefix
(`id_starts_with`), on its pages and in its presence probe alike: a stream declares it as
`indexWhere.Prefix`, and the module renders the pages and the probe from that one clause. A time-travel query narrows the
rows the GraphQL server reads only through `id`: filtered on `account` or `ownerPrefix` alone,
every query reads the whole table — seconds of CPU per query on tables of tens of millions of
rows — to return a handful of them, and an `id_gt` cursor alone still reads everything that sorts
after it. The refs read by account have IDs that begin with `<chain>:<account>:`
(`<chain>:<ownerPrefix>:` for Euler), and a row without that prefix is already rejected, so the
bound drops nothing. An index whose IDs do not carry the account, such as owner-token refs keyed
by contract and token, cannot be bounded this way.

An adapter whose protocol spans several chains should answer "which chains hold anything for
this account" with one request rather than one per chain: `sentio_presence.go` aliases a
time-travel query per chain, at that chain's own indexed block, and the per-chain flow skips
only chains the probe proved empty. The proof must stay exact — same block, same checkpoint
validation, the same where clause, rendered from the read's own streams — and a chain whose page
would carry a lag note is not proven, so its page runs and the scan reports the lag. Only an index
declared to probe, read through streams each keyed by the account alone (`indexStream.Account`)
under a prefix, can be proven empty; a read that includes any other stream, such as a chain's
Morpho fee markets, sends its pages. Refs are deleted when
positions close, so never decide presence from the latest state of the index.

The probe costs one request that the GraphQL server executes alias by alias, so it is slower
than a single-chain page. The engine therefore runs it ahead of the workers
(`presencePrefetcher`, started right after chain setup) rather than on the first chain job; an
adapter that adds a probe must implement `prefetchPresence`, passing `sentioIndex.prefetchPresence`
the streams its reads use, so its cost stays off the critical path. A proof established at an earlier indexed block still holds, and the chain job reports
that block as its indexed block; at a fixed pin the proof must be at the pin itself.

A probe asks only about the chains the scan reads its account on, so an account attributed
through Instadapp, read on Ethereum alone, is never probed, ahead of the workers or inline. The
engine ends a protocol's prefetched probes once no deployment of the protocol in the scan is left
to use them — each finished, skipped as inactive at its pin, or never dispatched — instead of
leaving a probe nobody waits for to hold its slots and run its queries to the end. A read checks
the proof before it sends anything, so a chain the probe proved empty takes no slot at all; the
probe takes one only to send.

### Indexer checkpoints

An index decides which positions an adapter reads, never what they are worth: every quantity is
read at the pin. Nothing tops an index read up from the chain — do not add an RPC tail of
`eth_getLogs` after the indexed block — so a read answers as of the block its processor had
reached. `indexQueryBlock` (`sentio_checkpoint.go`) applies the status rules: the chain must be
at `PROCESSING_LATEST`; a live read queries `min(pin, processed)` wherever the processor is; a
read at a fixed pin needs the processor at or past the pin and otherwise fails as a `protocol`
error, so a host that keeps historical scans retries it rather than storing a short answer. Every
page and every lookup of a read asks at that one block, Uniswap's included.

An index read is certified by the index's `IndexerCheckpoint` row for the chain, carried by every
page of the read at the query block and validated once by `checkIndexCheckpoint` against the pin:
never past the query block; at a fixed pin no older than the backfill lag; on a live read any age,
but past `indexMaxLag` (an hour) the read returns what it read with an `indexLagNote`. Every later
page, and every lookup at the read's `indexAt`, must carry the same row, or the read fails. A read
that succeeds records its note in the deployment's coverage sink (`withCoverage`, opened by the
engine per deployment), however the adapter's own read then ends, and the engine reports it as a
`coverage` note on the protocol and chain rather than a failure; adapters never handle it. Keep
every lag rule in that one function and its constants; do not reintroduce per-indexer lag
constants.

Since the index lags the pin, the refs it lists may have been settled after the indexed block.
An adapter must read a ref the account no longer holds at the pin as nothing, never as a
disagreement with the index: a request the protocol deleted on claim reads empty, an NFT
burned on redemption reverts `ownerOf`, an NFT transferred away has another owner. Keep
failing on what cannot come from lag, such as a live request with another owner.

Processors write the checkpoint row on a time interval, every ten minutes once live and a week
long while they backfill, and the driver fires the interval on weekly boundaries rather than at
the processor's start block, so the first row lands up to seven days after the index starts. A
query block before it finds no checkpoint, although the chain status proves the processor has
passed it. An index declared with a `start` (`sentio_index.go`) then stands the start block in as
the checkpoint, with that block's time, read from the chain with no lane slot held, and the usual
validation decides: at a fixed pin the
stand-in passes only within the backfill lag of the start, and a checkpoint still missing after
that fails as a stale one.

The start block an index declares must be the block the processor's checkpoint binding starts
at on that chain, which is also the adapter's own activation for the index. An index that started
later than the adapter's window would read as empty there for up to the backfill lag instead of
failing, so keep the two equal when adding a chain. The presence probe never uses the stand-in: a
chain without a checkpoint is not proven empty and runs its own pages. The Lista, Morpho, Pendle
and Euler indexes declare a start; the owner-token, account-request, mETH and Uniswap indexes
require a written checkpoint.

## Wallet holdings

A host-injected `WalletBalanceProvider` is the only ERC-20 discovery source for
live and historical scans. Do not add static token lists, token-list unions,
static metadata lookups, or an ERC-20 fallback when discovery fails.

The RPC-settled block remains authoritative. Provider amounts may be used
directly when the provider block number and hash exactly match that pin, or, on
a live scan only, when `LiveWalletSample` accepts the sample: both block times
known and no more than `LiveWalletSampleWindow` apart. A live answer is as of
now, so the wallet is then read as of the provider's block, a few blocks from
the protocols read at the pin. Keep that tolerance to live pins; a fixed pin
takes provider amounts only at its own block. Per-account block metadata
overrides the shared chain sample when accounts were queried in separate
batches. Otherwise re-read native and every discovered ERC-20 balance at the
settled block, including provider rows whose latest amount is zero. Never label
a latest provider amount with an earlier fixed `BlockRef`.

Filter zero amounts before metadata enrichment when amounts are used directly.
Otherwise first re-read the balance, then enrich non-zero results from provider
metadata or on-chain `symbol` and `decimals` (including bytes32 symbols). Never
invent either field or obtain it from a static token registry. What the wallet
reads on-chain the engine keeps for its lifetime (`walletTokenCache`, by chain
and address, with no expiry): a token's decimals and symbol do not change, and a
provider that reports amounts without metadata would otherwise cost every scan
those reads for every token it holds.

An empty successful `balanceOf` return disqualifies a discovery candidate for
that scan. At a fixed pin, a candidate with no code at the pinned block is
skipped before it is read: the token did not exist then, so the account could
not hold it, and its `balanceOf` may revert rather than return empty data (a
precompile-backed token does until it is activated). Live pins sit next to the
provider's sample and skip the check. Reverts of deployed tokens, RPC
failures, and malformed non-empty results remain errors. Do not implement an
address blacklist or silently turn failed reads into zero.

Provider failures and missing account results are errors (`protocol` scope):
the wallet was not read as asked. A target the provider marks `Unsupported` (a
chain it does not index) and a historical scan whose discovery was sampled at
another block are `coverage` notes instead: every balance reported was read at
the pin, but assets held only at the requested block may be absent. Keep the
two apart — a host that stores only scans without chain or protocol errors
retries the first and keeps the second. Pinned quantities do not establish a
complete historical token universe. Native balances are read independently in
every case.

`WalletBalanceRequest.Pins` tells the provider the block each chain is pinned
to. A provider may answer at the pin, or decide from it whether a sample it
keeps between scans is recent enough to discover from; it changes nothing the
kernel verifies.

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

Discovery runs beside the scan workers, not ahead of them: only the wallet's own
deployments read its answer. Once it is in they all run at once, each on its own
chain, and never on a worker, so they do not queue behind a slow protocol, and a
scan stopped at its deadline loses index-backed protocols before it loses the
wallet. No other deployment may read what discovery fills in, since it runs while
discovery is still writing; whatever reconciles holdings with positions, such as
`suppressDuplicateHoldings`, runs once both are done.

## Sui reads

A host reads Sui only through the Sui engine (`sui_engine*.go`): `NewSuiEngine` builds it from an
`Engine`, whose indexer lane, observer and `SentioIndexers` it shares, and `Scan`,
`ScanWithOptions` and `ScanRange` are its only reads. The rules it owns, each once:

- Where a protocol is read lives in `suiRoutes` alone: one source for its latest state and one for
  its pinned state. A read never falls back from one source to another — a NAVI node failure is not
  answered from NAVI's index, an unconfigured Suilend index is not answered from the node —
  because each answers for a different checkpoint.
- A pinned scan never reads node state. A protocol with no pinned source reports that it was not
  read; the only node request a pinned scan makes is wallet coin metadata, which is immutable. A
  request whose protocols are all indexed never dials the node, and a node that cannot be dialled
  costs only the protocols that need it.
- A pinned time scan is a range scan of one instant, so `ScanRange([t])[0]` and
  `ScanWithOptions(SuiAtTime(t))` answer alike; keep them on one path.
- An indexed latest read must name a whole sample (nonzero sequence, digest and timestamp) no
  older than `suiIndexedLatestMaxAge` and not in the future; a pinned one must not postdate its
  pin. Anything else is a protocol error, not a position.
- The wallet's past comes from the host's `SuiWalletHistory`, read at the one checkpoint every
  protocol of the instant is aligned to (`alignSamples`). Without that adapter, or without a
  protocol sample to pin to, the wallet is reported unread, never empty. The adapter must fail
  rather than answer for a checkpoint it has not ingested through; only `ErrSuiWalletNotCovered`
  is an answer, every other failure is retried.
- Value head state at the latest quote and a sample at its own timestamp, through `SuiPrices`, and
  never substitute one for the other. An unquoted protocol coin is a reported gap; an unquoted
  wallet coin is left for the host to hide, as on EVM chains. A `SuiPriceFailure` marked
  `Unavailable` makes a range instant retry; a missing quote does not.
- Every scan's reads, latest or ranged, stop short of its deadline (`suiPriceReserve`, at most a
  quarter of what is left), so what they read is still priced: a protocol that runs out of time
  costs no other protocol its valuation.
- A range instant retries only when a source did not answer. An index that answered — even with a
  sample it could not certify — is final. What a range scan's reads did not reach wraps
  `ErrSuiRangeUnread`.
- Test these rules through the engine (`sui_engine_test.go`): the index, node, price and wallet
  sources are fakes behind their seams, and a test asserts on the `SuiResponse` and on what each
  source was asked.

The transports and indexes underneath:

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
- The node has no wallet history. A pinned read (`Holdings` with a non-nil checkpoint) returns no
  balances with `HistoryUnsupported` set and makes no round trip; it never reads the head under
  the pin's name. Present that result as not read, never as nothing held. Reading specific
  retained object versions through gRPC does not establish complete wallet holdings at a
  historical checkpoint; the wallet's past comes from the host's `SuiWalletHistory` alone.
- NAVI, Volo, Cetus and Bluefin *latest* reads use direct object state (`suiNodeProtocols`).
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
  statement, executed once, with no Sui node call and no status or GraphQL follow-up; an
  unconfigured index is an error, never a node read. Hosts supply it as the engine's
  `SentioIndexers["suilend"]`: a `SentioIndexerConfig.SQLURL` (the project's `…/sql/execute`)
  and an explicit processor version. Volo, Cetus and Bluefin
  still read head state from a node and still have no history.
- Every SQL statement goes through `sentio_sql.go`: submitted to the async execute endpoint on the
  LARGE engine and polled at `…/sql/query_result/{id}` until it finishes. A poll that fails
  transiently is asked again; the submission never is, because asking again executes the
  statement again. A statement that outlives `sentioSQLStatementTimeout` or its caller is
  cancelled, and one that ran out of time or hit a ClickHouse execution limit is reported as too
  costly, so a range read asks for fewer samples. A failed submission never is, whatever its
  status: its answer may have been lost after the service accepted it, and a smaller statement
  would run beside one nobody can cancel. A server error keeps none of its message, which may
  name the deployment.
- NAVI is read from the same index machinery, but only for history. Its
  latest read stays on the node, which answers for the head rather than for the
  newest completed sample; its index (`SentioIndexers["navi"]`) answers only pinned
  reads, what a node cannot. Both paths run the same `naviLending` and
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
The Sui engine follows the same rule through `SuiPrices`: a sample is valued at its own
timestamp, and an index answering a latest request is still valued at its sample's.

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
