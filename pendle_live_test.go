package portfolio

import (
	"os"
	"testing"
)

// Ethereum accounts whose DeBank `pendle2` items are only `pendle_deposited` (wallet PT) and
// `pendle_liquidity` (market LP) — the two this adapter reports, on the same basis DeBank uses:
// PT as the token itself, LP decomposed into the holder's share of the market's SY and PT
// reserves plus accrued rewards. 16 of the 26 carry at least one LP position.
//
// DeBank's other Pendle adapters are out of this adapter's scope and would fail an item-for-item
// comparison, so accounts carrying them are excluded: `pendle2_sy_token`, `pendle_locked`
// (vePENDLE), `pendle_staked` (staked PENDLE), `pendle_ve_reward`, `pendle_leveraged_farming` and
// `pendle_withdraw_locked`. DeBank has no YT adapter at all, so YT coverage is a superset with
// nothing to reconcile against.
var pendleDeBankAccounts = []string{
	"0x0b129e1a16bcb6ec8fab24d2182ad59cc8fae4eb",
	"0x0c8eb038c58e0a9d8d66bf5805a6ec0dfdae6c4c",
	"0x109e8d93943e519e1ef75335a1757e05b71a2e9a",
	"0x1b648ade1ef219c87987cd60eba069a7faf1621f",
	"0x2274f2c030607329bb4952fd98e96cb78fd47cb3",
	"0x29ff004b5e78dd11c1021368f0eb0a9d9dde1322",
	"0x44f64a628c70aab95ed0b7f8a1624a82397860c6",
	"0x65859153491f44e2226ca6ae37347c855158a4fb",
	"0x78cb7537e0b9a6bde79654cef4a6a2f4a7f3b514",
	"0x78fca58f1e54a00e44f7d6c40b771a8ed97c2296",
	"0x7f9e4e2ef9e4da30524405d894fe12997eda81e4",
	"0x9e92798ca8d3112599f0cd9dd5d7a5708e20c65c",
	"0xa69bfa73f231b999c47bf6e822524313334ce324",
	"0xaca2ef22f720ae3f622b9ce3065848c4333687ae",
	"0xaf73ef59d4afe0cc959d6eceed6378119f0cbc4d",
	"0xbbbbbbbbbb9cc5e90e3b3af64bdaf62c37eeffcb",
	"0xc02dd10b401e01e0fb3bf497e46e6d6b51664ad7",
	"0xc0c6f83080702793cc3c27981c6c1289b4430100",
	"0xce343b1752fb05310f03103e979a4723eaf4a61b",
	"0xd86ef14bc11893446ef438a4c02571983218cb33",
	"0xdbb2d91c92c9332def8c671dc511730482e3a724",
	"0xdc67d0d0fb5a7e697c51b2b84126136691b1ba6c",
	"0xe29f5ad2a33c76ccaca2d2e3226ff80085ce4573",
	"0xe3292ce7cbbb9b4b81119d638abaa2519e7a02ac",
	"0xe96be8875bd0a076ca0e0c741110368901598bf0",
	"0xebb870a0aaaa7bc55ead925d03a4af4a8db79a03",
}

func TestPendleDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	indexer := newPendleIndexer(liveSentioIndexerConfig(t, "PORTFOLIO_PENDLE_INDEXER"))
	indexer.requiredChains = []ChainID{Ethereum}
	runDeBankAPIReconciliation(
		t, "pendle2", newPendleAdapterWithIndexer(indexer), Ethereum, pendleDeBankAccounts,
	)
}
