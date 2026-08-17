package portfolio

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/crypto"
)

func TestMorphoDeploymentsCoverSupportedChains(t *testing.T) {
	for _, chainID := range SupportedChainIDs {
		deployment, exists := morphoDeployments[chainID]
		if !exists {
			t.Fatalf("Morpho deployment is absent for chain %d", chainID)
		}
		if deployment.Morpho == (common.Address{}) || deployment.Window.ActivationBlock == 0 {
			t.Errorf("chain %d has an incomplete Morpho core deployment", chainID)
		}
		if len(deployment.VaultV1Factories) == 0 {
			t.Errorf("chain %d has no Morpho V1 factory", chainID)
		}
		for _, factory := range append(deployment.VaultV1Factories, deployment.VaultV2Factories...) {
			if factory.Address == (common.Address{}) || factory.Window.ActivationBlock < deployment.Window.ActivationBlock {
				t.Errorf("chain %d has an invalid Morpho factory: %+v", chainID, factory)
			}
		}
	}
}

func TestMorphoCanonicalMarketID(t *testing.T) {
	marketID, err := morphoMarketID(
		common.HexToAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48"),
		common.HexToAddress("0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599"),
		common.HexToAddress("0xDddd770BADd886dF3864029e4B377B5F6a2B6b83"),
		common.HexToAddress("0x870aC11D48B15DB9a138Cf899d20F13F79Ba00BC"),
		big.NewInt(860_000_000_000_000_000),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := common.HexToHash("0x3a85e619751152991742810df6ec69ce473daef99e28a64ab2340d7b7ccfee49")
	if marketID != want {
		t.Fatalf("market ID = %s, want %s", marketID, want)
	}
}

func TestMorphoStoredShareFractionMatchesDeBankBoundary(t *testing.T) {
	shares, ok := new(big.Int).SetString("1660150005780932", 10)
	if !ok {
		t.Fatal("invalid shares fixture")
	}
	totalAssets, ok := new(big.Int).SetString("106321198846013", 10)
	if !ok {
		t.Fatal("invalid total assets fixture")
	}
	totalShares, ok := new(big.Int).SetString("91830935606491379162", 10)
	if !ok {
		t.Fatal("invalid total shares fixture")
	}
	numerator, denominator := morphoStoredShareFraction(
		shares,
		totalAssets,
		totalShares,
	)
	wantNumerator := new(big.Int).Mul(
		shares,
		new(big.Int).Add(totalAssets, big.NewInt(1)),
	)
	wantDenominator := new(big.Int).Add(totalShares, big.NewInt(1_000_000))
	if numerator.Cmp(wantNumerator) != 0 || denominator.Cmp(wantDenominator) != 0 {
		t.Fatalf("fraction = %s/%s, want %s/%s", numerator, denominator, wantNumerator, wantDenominator)
	}
}

func TestMergeMorphoRefsRejectsGenerationDrift(t *testing.T) {
	vault := common.HexToAddress("0x1111111111111111111111111111111111111111")
	_, err := mergeMorphoRefs(
		morphoPositionRefs{Vaults: []morphoVaultRef{{Address: vault, Version: morphoVaultV1}}},
		nil,
		[]morphoVaultRef{{Address: vault, Version: morphoVaultV2}},
	)
	if err == nil {
		t.Fatal("expected generation mismatch")
	}
}

func TestMorphoTailFiltersMatchIndexedAccountTopics(t *testing.T) {
	wantTopic2 := map[common.Hash]struct{}{
		crypto.Keccak256Hash([]byte("Withdraw(bytes32,address,address,address,uint256,uint256)")):   {},
		crypto.Keccak256Hash([]byte("Borrow(bytes32,address,address,address,uint256,uint256)")):     {},
		crypto.Keccak256Hash([]byte("WithdrawCollateral(bytes32,address,address,address,uint256)")): {},
	}
	wantTopic3 := map[common.Hash]struct{}{
		crypto.Keccak256Hash([]byte("Supply(bytes32,address,address,uint256,uint256)")):                            {},
		crypto.Keccak256Hash([]byte("Repay(bytes32,address,address,uint256,uint256)")):                             {},
		crypto.Keccak256Hash([]byte("SupplyCollateral(bytes32,address,address,uint256)")):                          {},
		crypto.Keccak256Hash([]byte("Liquidate(bytes32,address,address,uint256,uint256,uint256,uint256,uint256)")): {},
	}
	assertTopics := func(name string, actual []common.Hash, expected map[common.Hash]struct{}) {
		t.Helper()
		if len(actual) != len(expected) {
			t.Fatalf("%s filters = %d, want %d", name, len(actual), len(expected))
		}
		for _, topic := range actual {
			if _, exists := expected[topic]; !exists {
				t.Fatalf("%s contains unexpected topic %s", name, topic)
			}
		}
	}
	assertTopics("topic2", morphoTopic2AccountEvents, wantTopic2)
	assertTopics("topic3", morphoTopic3AccountEvents, wantTopic3)
}

func TestMorphoFeeRecipientTailReplaysEventOrder(t *testing.T) {
	alice := common.HexToAddress("0x1111111111111111111111111111111111111111")
	bob := common.HexToAddress("0x2222222222222222222222222222222222222222")
	marketBeforeChange := common.HexToHash("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	marketForBob := common.HexToHash("0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	marketWithoutFee := common.HexToHash("0xcccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	marketAfterRestore := common.HexToHash("0xdddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd")
	accrue := func(block uint64, index uint64, market common.Hash, feeShares int64) rpcLog {
		data := make([]byte, 96)
		copy(data[64:], common.LeftPadBytes(big.NewInt(feeShares).Bytes(), 32))
		return rpcLog{
			Topics: []common.Hash{morphoAccrueInterestTopic, market}, Data: hexutil.Bytes(data),
			BlockNumber: hexutil.Uint64(block), LogIndex: hexutil.Uint64(index),
		}
	}
	setRecipient := func(block uint64, index uint64, recipient common.Address) rpcLog {
		return rpcLog{
			Topics:      []common.Hash{morphoSetFeeRecipientTopic, common.BytesToHash(recipient.Bytes())},
			BlockNumber: hexutil.Uint64(block), LogIndex: hexutil.Uint64(index),
		}
	}
	markets, err := morphoFeeMarketIDsFromLogs(alice, alice, []rpcLog{
		accrue(12, 2, marketAfterRestore, 2),
		setRecipient(11, 0, bob),
		accrue(10, 0, marketBeforeChange, 1),
		accrue(11, 1, marketForBob, 1),
		setRecipient(12, 0, alice),
		accrue(12, 1, marketWithoutFee, 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []common.Hash{marketBeforeChange, marketAfterRestore}
	if len(markets) != len(want) || markets[0] != want[0] || markets[1] != want[1] {
		t.Fatalf("fee markets = %v, want %v", markets, want)
	}
}
