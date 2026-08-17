# Sentio Portfolio

`sentio-portfolio` is the pure Go calculation kernel for wallet protocol
positions on Ethereum, BSC, Base, and Arbitrum. It owns protocol adapters,
latest/fixed-block RPC reads, account attribution, index-backed position reads,
and valuation aggregation.

The repository deliberately does not own an HTTP or gRPC API, protobufs,
deployment configuration, authentication, or a concrete price service. A host
constructs `Engine` with chain RPC URLs and a `PriceProvider` implementation.

```go
engine := portfolio.NewEngine(rpcURLs, priceProvider)
result := engine.Scan(ctx, account)
```

Indexer-backed adapters receive their deployment-specific GraphQL/status
endpoints and processor versions through `EngineConfig`. Public source must not
contain project names or owner namespaces.

Run the local test suites with:

```sh
go test ./...
bazel test //...
```
