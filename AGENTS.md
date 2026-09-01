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

## Wallet holdings

`wallet-tokens.json` is the list of assets the `wallet` adapter reads, and it is
generated rather than hand-edited. Two rules decide what belongs in it:

- **every token must be quotable by the host's price provider.** The kernel has no
  price service, so an unquotable token is not extra coverage — it reports an amount
  the response cannot value and adds a pricing failure to every scan of every
  account. Only the host can decide that, and it does: it runs the committed list
  through the price provider production uses and prunes what cannot be quoted;
- **never list a token an adapter already reads.** LSTs, vault shares, aTokens and
  LP tokens are positions, not holdings. `suppressDuplicateHoldings` enforces this
  at runtime from the `Source` each component records, so an adapter that reads a
  wallet balance must keep the contract it read in `Source.Contract` — that field is
  what stops the same balance being counted twice.

Holdings do not chase the long tail: a curated list that can be priced beats a
wider list of amounts with no value.

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
