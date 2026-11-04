package portfolio

import (
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

// walletOnlyChains are served for plain holdings alone: no protocol adapter has a
// verified deployment on them yet.
var walletOnlyChains = []ChainID{HyperEVM, Robinhood}

func TestWalletOnlyChainCanonicalIdentity(t *testing.T) {
	want := map[ChainID]struct {
		priceName string
		symbol    string
		wrapped   common.Address
	}{
		HyperEVM:  {"hyperevm", "HYPE", common.HexToAddress("0x5555555555555555555555555555555555555555")},
		Robinhood: {"robinhood", "ETH", common.HexToAddress("0x0Bd7D308f8E1639FAb988df18A8011f41EAcAD73")},
	}
	for _, chainID := range walletOnlyChains {
		expected := want[chainID]
		if !supportsChain(SupportedChainIDs, chainID) {
			t.Errorf("chain %d is not in SupportedChainIDs", chainID)
		}
		if got := chainPriceNames[chainID]; got != expected.priceName {
			t.Errorf("chain %d price name = %q, want %q", chainID, got, expected.priceName)
		}
		coin, exists := walletNativeCoin[chainID]
		if !exists {
			t.Errorf("chain %d native coin is missing", chainID)
			continue
		}
		if coin.Symbol != expected.symbol || coin.Wrapped != expected.wrapped {
			t.Errorf("chain %d native coin = %+v, want %s/%s", chainID, coin, expected.symbol, expected.wrapped)
		}
	}
}

// TestWalletOnlyChainsAdvertiseOnlyTheWallet keeps a protocol from claiming a
// wallet-only chain without its own verified deployment and availability window.
func TestWalletOnlyChainsAdvertiseOnlyTheWallet(t *testing.T) {
	for _, protocol := range NewEngine(nil, nil).Protocols() {
		for _, chainID := range walletOnlyChains {
			advertised := supportsChain(protocol.Chains, chainID)
			if protocol.ID == walletProtocolID && !advertised {
				t.Errorf("wallet does not advertise chain %d", chainID)
			}
			if protocol.ID != walletProtocolID && advertised {
				t.Errorf("protocol %q advertises wallet-only chain %d", protocol.ID, chainID)
			}
		}
	}
}
