package portfolio

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestEtherfiVaultAssets(t *testing.T) {
	shares := new(big.Int)
	shares.SetString("1680330340000000000000", 10)
	rate := new(big.Int)
	rate.SetString("1203588321000000000", 10)
	want := new(big.Int)
	want.SetString("2022425972645959140000", 10)

	got, err := etherfiVaultAssets(shares, rate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("assets = %s, want %s", got, want)
	}
	if shares.String() != "1680330340000000000000" || rate.String() != "1203588321000000000" {
		t.Fatal("conversion mutated an input")
	}
}

func TestEtherfiVaultAssetsRejectsInvalidInputs(t *testing.T) {
	for _, test := range []struct {
		name     string
		shares   *big.Int
		rate     *big.Int
		decimals uint8
	}{
		{name: "nil shares", rate: big.NewInt(1)},
		{name: "negative shares", shares: big.NewInt(-1), rate: big.NewInt(1)},
		{name: "nil rate", shares: big.NewInt(1)},
		{name: "zero rate", shares: big.NewInt(1), rate: big.NewInt(0)},
		{name: "unsafe decimals", shares: big.NewInt(1), rate: big.NewInt(1), decimals: 78},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := etherfiVaultAssets(test.shares, test.rate, test.decimals); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAsterEarnAssetsAndDisplayToken(t *testing.T) {
	assets, err := asterEarnAssets(
		big.NewInt(1_000_000_000_000_000_000),
		big.NewInt(1_783_772_151),
		18,
	)
	if err != nil {
		t.Fatal(err)
	}
	want := new(big.Int)
	want.SetString("17837721510000000000", 10)
	if assets.Cmp(want) != 0 {
		t.Fatalf("Aster converted assets = %s, want %s", assets, want)
	}
	display := asterDisplayToken(Token{ChainID: BSC, Address: asterSlisBNB, Symbol: "slisBNB", Decimals: 18})
	if display.Address != asterWBNB.Address || display.Symbol != "BNB" {
		t.Fatalf("Aster display token = %+v, want %+v", display, asterWBNB)
	}
}

func TestERC1155BatchTailDecodingAndOwnerTransition(t *testing.T) {
	eventInputs := erc1155TransferABI.Events["TransferBatch"].Inputs.NonIndexed()
	data, err := eventInputs.Pack(
		[]*big.Int{big.NewInt(8_941), big.NewInt(8_942)},
		[]*big.Int{big.NewInt(1), big.NewInt(1)},
	)
	if err != nil {
		t.Fatal(err)
	}
	values, err := eventInputs.Unpack(data)
	if err != nil {
		t.Fatal(err)
	}
	ids, ok := values[0].([]*big.Int)
	if !ok || len(ids) != 2 || ids[0].Cmp(big.NewInt(8_941)) != 0 {
		t.Fatalf("decoded ERC1155 IDs = %#v", values[0])
	}

	owner := common.HexToAddress("0x1111111111111111111111111111111111111111")
	other := common.HexToAddress("0x2222222222222222222222222222222222222222")
	refs := map[string]ownerTokenRef{}
	applyOwnerTokenTransfer(refs, etherfiMembershipNFT, ids[0], other, owner, owner)
	key := ownerTokenRefKey(etherfiMembershipNFT, ids[0])
	if _, exists := refs[key]; !exists {
		t.Fatal("transfer to owner did not add the ERC1155 token ID")
	}
	applyOwnerTokenTransfer(refs, etherfiMembershipNFT, ids[0], owner, other, owner)
	if _, exists := refs[key]; exists {
		t.Fatal("transfer from owner did not remove the ERC1155 token ID")
	}
}

func TestAccountIndexesFilterQueriesByContract(t *testing.T) {
	if !strings.Contains(ownerTokenWalletQuery, "contract_in: $contracts") {
		t.Fatal("owner-token query is not contract-scoped")
	}
	if !strings.Contains(accountRequestWalletQuery, "contract_in: $contracts") {
		t.Fatal("account-request query is not contract-scoped")
	}
	first := common.HexToAddress("0x2222222222222222222222222222222222222222")
	second := common.HexToAddress("0x1111111111111111111111111111111111111111")
	addresses := sortedAllowedAddresses(map[common.Address]struct{}{first: {}, second: {}})
	if len(addresses) != 2 || addresses[0] != strings.ToLower(second.Hex()) ||
		addresses[1] != strings.ToLower(first.Hex()) {
		t.Fatalf("sorted contract filter = %v", addresses)
	}
}

func TestExpandedProtocolManifests(t *testing.T) {
	etherfi := newEtherfiAdapter(SentioIndexerConfig{}).(*EtherfiAdapter)
	if got := len(etherfi.vaults[Ethereum]); got != 15 {
		t.Fatalf("Ether.fi Ethereum vault count = %d, want 15", got)
	}
	for _, chainID := range []ChainID{Base, Arbitrum} {
		if got := len(etherfi.vaults[chainID]); got != 2 {
			t.Fatalf("Ether.fi chain %d vault count = %d, want 2", chainID, got)
		}
	}

	aster := newAsterAdapter(SentioIndexerConfig{}).(*AsterAdapter)
	if got := len(aster.receipts); got != 4 {
		t.Fatalf("Aster receipt count = %d, want 4", got)
	}

	renzo := newRenzoAdapter(SentioIndexerConfig{}).(*RenzoAdapter)
	if got := len(renzo.vaults); got != 3 {
		t.Fatalf("Renzo Eigen vault count = %d, want 3", got)
	}
	if renzo.vaults[2].Underlying.Address != common.HexToAddress("0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599") {
		t.Fatal("Renzo ezBTC underlying is not WBTC")
	}

	euler := newEulerV2Adapter(SentioIndexerConfig{}).(*EulerV2Adapter)
	if got := len(euler.Info().Chains); got != 4 || len(eulerV2ChainConfigs) != 4 {
		t.Fatalf("Euler V2 chain coverage = %d/%d, want 4/4", got, len(eulerV2ChainConfigs))
	}
	for _, chainID := range []ChainID{Ethereum, BSC, Base, Arbitrum} {
		config, exists := eulerV2ChainConfigs[chainID]
		if !exists || config.ChainID != chainID || config.EVC == (common.Address{}) ||
			config.EVaultFactory == (common.Address{}) || config.TrackingRewards == (common.Address{}) {
			t.Fatalf("Euler V2 chain %d configuration is incomplete", chainID)
		}
	}
}

func TestValidAccountRequestKey(t *testing.T) {
	valid := []string{
		"0", "42",
		"0x00000000000000000000000000000000000000000000000000000000000000ab",
	}
	for _, key := range valid {
		if !validAccountRequestKey(key) {
			t.Errorf("valid key %q was rejected", key)
		}
	}
	invalid := []string{
		"", "01", "-1", " 1", "1 ", "0X00000000000000000000000000000000000000000000000000000000000000ab",
		"0xAB00000000000000000000000000000000000000000000000000000000000000",
		"0x1234",
	}
	for _, key := range invalid {
		if validAccountRequestKey(key) {
			t.Errorf("invalid key %q was accepted", key)
		}
	}
}
