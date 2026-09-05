package portfolio

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
)

func TestEtherfiVaultAssetRatio(t *testing.T) {
	shares := new(big.Int)
	shares.SetString("1680330340000000000000", 10)
	rate := new(big.Int)
	rate.SetString("1203588321000000000", 10)
	wantNumerator := new(big.Int)
	wantNumerator.SetString("2022425972645959140000000000000000000000", 10)
	wantDenominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

	numerator, denominator, err := etherfiVaultAssetRatio(shares, rate, 18)
	if err != nil {
		t.Fatal(err)
	}
	if numerator.Cmp(wantNumerator) != 0 || denominator.Cmp(wantDenominator) != 0 {
		t.Fatalf(
			"asset ratio = %s/%s, want %s/%s",
			numerator,
			denominator,
			wantNumerator,
			wantDenominator,
		)
	}
	if shares.String() != "1680330340000000000000" || rate.String() != "1203588321000000000" {
		t.Fatal("conversion mutated an input")
	}
}

func TestEtherfiVaultComponentPreservesPinnedOptimismSharesAndRates(t *testing.T) {
	positions := newEtherfiAdapter(SentioIndexerConfig{}).(*EtherfiAdapter).vaults[Optimism]
	byID := make(map[string]etherfiVaultPosition, len(positions))
	for _, position := range positions {
		byID[position.ID] = position
	}
	for _, test := range []struct {
		id           string
		token        Token
		shares       string
		rate         string
		rateDecimals uint8
		numerator    string
		denominator  string
	}{
		{
			id: "sethfi",
			token: Token{
				ChainID: Optimism, Address: common.HexToAddress("0xe0080d2F853ecDdbd81A643dC10DA075Df26fD3f"),
				Symbol: "ETHFI", Decimals: 18,
			},
			shares: "5167382909661579358", rate: "1203588321333271298", rateDecimals: 18,
			numerator: "6219401721925815387269409211370666684", denominator: "1000000000000000000",
		},
		{
			id: "ebtc",
			token: Token{
				ChainID: Optimism, Address: common.HexToAddress("0x68f180fcCe6836688e9084f035309E29Bf0A2095"),
				Symbol: "WBTC", Decimals: 8,
			},
			shares: "135436", rate: "100388040", rateDecimals: 8,
			numerator: "13596154585440", denominator: "100000000",
		},
	} {
		t.Run(test.id, func(t *testing.T) {
			position, exists := byID[test.id]
			if !exists {
				t.Fatalf("missing Ether.fi Optimism vault %q", test.id)
			}
			shares, sharesOK := new(big.Int).SetString(test.shares, 10)
			rate, rateOK := new(big.Int).SetString(test.rate, 10)
			if !sharesOK || !rateOK {
				t.Fatal("invalid integer fixture")
			}
			component, err := etherfiVaultComponent(
				position,
				test.token,
				shares,
				rate,
				test.rateDecimals,
				"getRateSafe",
			)
			if err != nil {
				t.Fatal(err)
			}
			if component.Token != test.token || component.AmountRaw != test.numerator ||
				component.AmountDenominatorRaw != test.denominator {
				t.Fatalf("component = %+v", component)
			}
			if component.Source.Contract != position.Accountant || component.Source.Method != "getRateSafe" {
				t.Errorf("source = %+v", component.Source)
			}
			if component.Metadata["sharesRaw"] != test.shares || component.Metadata["rateRaw"] != test.rate {
				t.Errorf("metadata = %+v", component.Metadata)
			}
		})
	}
}

func TestEtherfiVaultAssetRatioRejectsInvalidInputs(t *testing.T) {
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
			if _, _, err := etherfiVaultAssetRatio(test.shares, test.rate, test.decimals); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestAsterEarnAssets(t *testing.T) {
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
	wantOptimismVaults := map[string]struct {
		vault      common.Address
		accountant common.Address
		activation uint64
	}{
		"sethfi": {
			vault:      common.HexToAddress("0x86B5780b606940Eb59A062aA85a07959518c0161"),
			accountant: common.HexToAddress("0x05A1552c5e18F5A0BB9571b5F2D6a4765ebdA32b"),
			activation: 149_699_007,
		},
		"ebtc": {
			vault:      common.HexToAddress("0x657e8C867D8B37dCC18fA4Caead9C45EB088C642"),
			accountant: common.HexToAddress("0x1b293DC39F94157fA0D1D36d7e0090C8B8B8c13F"),
			activation: 149_699_299,
		},
		"eusd": {
			vault:      common.HexToAddress("0x939778D83b46B456224A33Fb59630B11DEC56663"),
			accountant: common.HexToAddress("0xEB440B36f61Bf62E0C54C622944545f159C3B790"),
			activation: 149_822_646,
		},
		"liquid-eth": {
			vault:      common.HexToAddress("0xf0bb20865277aBd641a307eCe5Ee04E79073416C"),
			accountant: common.HexToAddress("0x0d05D94a5F1E76C18fbeB7A13d17C8a314088198"),
			activation: 123_081_511,
		},
		"liquid-usd": {
			vault:      common.HexToAddress("0x08c6F91e2B681FaF5e17227F2a44C307b3C1364C"),
			accountant: common.HexToAddress("0xc315D6e14DDCDC7407784e2Caf815d131Bc1D3E7"),
			activation: 149_698_252,
		},
		"liquid-btc": {
			vault:      common.HexToAddress("0x5f46d540b6eD704C3c8789105F30E075AA900726"),
			accountant: common.HexToAddress("0xEa23aC6D7D11f6b181d6B98174D334478ADAe6b0"),
			activation: 149_698_606,
		},
	}
	optimismVaults := etherfi.vaults[Optimism]
	if got, want := len(optimismVaults), len(wantOptimismVaults); got != want {
		t.Fatalf("Ether.fi Optimism vault count = %d, want %d", got, want)
	}
	seenOptimismVaults := make(map[string]struct{}, len(optimismVaults))
	for _, position := range optimismVaults {
		want, exists := wantOptimismVaults[position.ID]
		if !exists {
			t.Fatalf("Ether.fi Optimism has unexpected vault %q", position.ID)
		}
		if _, duplicate := seenOptimismVaults[position.ID]; duplicate {
			t.Fatalf("Ether.fi Optimism has duplicate vault %q", position.ID)
		}
		seenOptimismVaults[position.ID] = struct{}{}
		if position.Vault != want.vault || position.Accountant != want.accountant ||
			position.ActivationBlock != want.activation {
			t.Errorf("Ether.fi Optimism vault %q = %+v, want %+v", position.ID, position, want)
		}
	}
	for id := range wantOptimismVaults {
		if _, exists := seenOptimismVaults[id]; !exists {
			t.Errorf("Ether.fi Optimism vault %q is missing", id)
		}
	}
	for _, absentID := range []string{
		"weeths", "weethk", "liquid-elixir", "liquid-usual", "liquid-ultrayield",
		"liquid-bera-btc", "liquid-bera-eth", "liquid-move-eth", "liquid-katana-eth",
	} {
		for _, position := range optimismVaults {
			if position.ID == absentID {
				t.Errorf("Ether.fi Optimism config includes undeployed vault %q", absentID)
			}
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
	if got := len(euler.Info().Chains); got != 8 || len(eulerV2ChainConfigs) != 8 {
		t.Fatalf("Euler V2 chain coverage = %d/%d, want 8/8", got, len(eulerV2ChainConfigs))
	}
	for _, chainID := range []ChainID{Ethereum, BSC, Base, Arbitrum, Polygon, Monad, Plasma, Avalanche} {
		config, exists := eulerV2ChainConfigs[chainID]
		if !exists || config.ChainID != chainID || config.EVC == (common.Address{}) ||
			config.EVaultFactory == (common.Address{}) || config.TrackingRewards == (common.Address{}) {
			t.Fatalf("Euler V2 chain %d configuration is incomplete", chainID)
		}
	}
}

func TestEtherfiOptimismReceiptSuppressesWalletDuplicate(t *testing.T) {
	adapter := newEtherfiAdapter(SentioIndexerConfig{}).(*EtherfiAdapter)
	if !supportsChain(adapter.Info().Chains, Optimism) {
		t.Fatal("Ether.fi does not advertise Optimism")
	}
	receipts := adapter.receipts[Optimism]
	if len(receipts) != 1 || receipts[0].ActivationBlock != 120_917_167 {
		t.Fatalf("Ether.fi Optimism receipts = %+v", receipts)
	}
	receipt := receipts[0]
	protocolGroup := Group{
		ID: "weeth",
		Components: []Component{NewComponent(
			"asset",
			receipt.Token,
			big.NewInt(7),
			Source{Contract: receipt.BalanceContract, Method: "balanceOf"},
		)},
	}
	walletGroup := Group{
		ID: walletTokenGroupID(receipt.Token.Address),
		Components: []Component{NewComponent(
			"asset",
			receipt.Token,
			big.NewInt(7),
			Source{Contract: receipt.Token.Address, Method: "balanceOf"},
		)},
		Metadata: map[string]any{"holding": "token"},
	}
	snapshots := suppressDuplicateHoldings([]Snapshot{
		{ProtocolID: "etherfi", ChainID: Optimism, Groups: []Group{protocolGroup}},
		{ProtocolID: walletProtocolID, ChainID: Optimism, Groups: []Group{walletGroup}},
	})
	if len(snapshots) != 1 || snapshots[0].ProtocolID != "etherfi" {
		t.Fatalf("duplicate weETH wallet holding survived: %+v", snapshots)
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
