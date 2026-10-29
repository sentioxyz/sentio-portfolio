package portfolio

import (
	"strings"
	"testing"
)

// A time-travel query narrows the rows the GraphQL server reads only through id, so an
// account-keyed stream must bound id by the account's row prefix or it reads the whole table. The
// pages and the presence probe both render the stream's where clause (renderIndexWhere), so the
// prefix a stream declares bounds both.
func TestAccountKeyedStreamsBoundIDByRowPrefix(t *testing.T) {
	account := presenceTestAccount
	owner := strings.ToLower(account.Hex())
	for _, test := range []struct {
		name    string
		stream  indexStreamer
		chainID ChainID
		prefix  string
	}{
		{"morpho markets", moolahMarketStream("morphoMarketPositionRefs", account, 1), Base, "8453:" + owner + ":"},
		{"morpho vaults", morphoVaultStream(account), Base, "8453:" + owner + ":"},
		{"lista markets", moolahMarketStream("listaMarketPositionRefs", account, 1), BSC, "56:" + owner + ":"},
		{"lista vaults", listaVaultStream(account), BSC, "56:" + owner + ":"},
		{"pendle", pendleRefStream(account), Arbitrum, "42161:" + owner + ":"},
		{"euler positions", eulerPositionStream(account), Ethereum, "1:" + owner[:40] + ":"},
		{"euler rewards", eulerRewardStream(account), Ethereum, "1:" + owner[:40] + ":"},
		{"uniswap", uniswapPositionStream(uniswapIndexerDefinitions[uniswapV3], account), Base, "8453:" + owner + ":"},
		{"meth", methRequestStream(account), Ethereum, "1:" + owner + ":"},
	} {
		t.Run(test.name, func(t *testing.T) {
			where := renderIndexWhere(test.stream.spec().where(test.chainID), "")
			if !strings.Contains(where, `id_starts_with: "`+test.prefix+`"`) {
				t.Fatalf("where %q is not bounded by the row prefix %q", where, test.prefix)
			}
		})
	}
}

// The indexes that probe can probe the reads they make: every stream of each is keyed by the
// account alone. A stream that is not would silently send every chain's pages.
func TestProbingIndexesCanProveTheirReadsEmpty(t *testing.T) {
	config := SentioIndexerConfig{}
	account := presenceTestAccount
	uniswap := newUniswapIndexer(config, config)
	for _, test := range []struct {
		name    string
		index   *sentioIndex
		streams []indexStreamer
	}{
		{"morpho-blue", newMorphoIndexer(config).index, []indexStreamer{
			moolahMarketStream("morphoMarketPositionRefs", account, morphoMaxIndexedMarkets), morphoVaultStream(account),
		}},
		{"pendle", newPendleIndexer(config).index, []indexStreamer{pendleRefStream(account)}},
		{"uniswap-v3", uniswap.indexes[uniswapV3], []indexStreamer{
			uniswapPositionStream(uniswapIndexerDefinitions[uniswapV3], account),
		}},
		{"uniswap-v4", uniswap.indexes[uniswapV4], []indexStreamer{
			uniswapPositionStream(uniswapIndexerDefinitions[uniswapV4], account),
		}},
	} {
		if _, _, provable := test.index.presenceProbe(test.streams); !provable {
			t.Errorf("%s cannot probe its own read", test.name)
		}
	}
	feeRecipient := []indexStreamer{
		moolahMarketStream("morphoMarketPositionRefs", account, morphoMaxIndexedMarkets),
		morphoVaultStream(account), moolahFeeMarketStream("morphoFeeMarkets"),
	}
	if _, _, provable := newMorphoIndexer(config).index.presenceProbe(feeRecipient); provable {
		t.Error("a read of the chain's fee markets was left to the account's probe")
	}
}
