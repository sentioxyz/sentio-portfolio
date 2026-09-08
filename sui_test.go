package portfolio

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"
	"time"
)

// suiMainnetGenesisDigest is the base58 genesis checkpoint digest Sui returns as the mainnet
// chain identifier. Its first four bytes are SuiMainnetChainIdentifier.
const suiMainnetGenesisDigest = "4btiuiMPvEENsttpZC7CZ53DruC3MAgfznDbASZ7DR6S"

// suiTestDigest is a well-formed 32-byte digest that is not the mainnet genesis digest. Tests use
// it as another network's identifier and as the digest of every scripted checkpoint.
const suiTestDigest = "FP87xzMYCoratjcphxoF9HrSoRc1MXaWuSEWLGpdJ88i"

const suiLongType = "0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI"

var suiTestCheckpointTime = time.Unix(1788826290, 73_000_000).UTC()

func TestParseSuiAddressPadsShortFormsToTheLongForm(t *testing.T) {
	address, err := ParseSuiAddress("0x2")
	if err != nil {
		t.Fatal(err)
	}
	want := "0x0000000000000000000000000000000000000000000000000000000000000002"
	if address.Hex() != want {
		t.Fatalf("address = %s, want %s", address.Hex(), want)
	}
	mixed, err := ParseSuiAddress("0x3CD40490B1E5A583F8717284291C6DB97F604BD3D812B6E214591823D985B900")
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Hex() != "0x3cd40490b1e5a583f8717284291c6db97f604bd3d812b6e214591823d985b900" {
		t.Fatalf("address = %s, want lowercase", mixed.Hex())
	}
	for _, invalid := range []string{
		"", "0x", "2", "0x" + strings.Repeat("0", 65), "0xzz", "0x 2", "0x3cd4::sui::SUI",
	} {
		if _, err := ParseSuiAddress(invalid); err == nil {
			t.Errorf("ParseSuiAddress(%q) accepted an invalid address", invalid)
		}
	}
}

func TestNormalizeMoveTypeMatchesTheChainRepr(t *testing.T) {
	cases := map[string]string{
		"0x2::sui::SUI":   suiLongType,
		" 0x2::sui::SUI ": suiLongType,
		"0X02::sui::SUI":  suiLongType,
		suiLongType:       suiLongType,
		"0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<0x2::sui::SUI, 0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>": "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<" +
			suiLongType + ",0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>",
		"0x1::option::Option<vector<u8>>": "0x0000000000000000000000000000000000000000000000000000000000000001::option::Option<vector<u8>>",
		"0xa::m::S<u64,bool, address>":    "0x000000000000000000000000000000000000000000000000000000000000000a::m::S<u64,bool,address>",
	}
	for input, want := range cases {
		got, err := NormalizeMoveType(input)
		if err != nil {
			t.Errorf("NormalizeMoveType(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeMoveType(%q) = %q, want %q", input, got, want)
		}
	}
	for _, invalid := range []string{
		"", "sui::SUI", "0x2::sui", "0x2::sui::", "0x2::9sui::SUI", "0x2::sui::SUI<", "0x2::sui::SUI<u64",
		"0x2::sui::SUI<u64;bool>", "0x2::sui::SUI<notatype>", "0x2::sui::SUI extra", "0x2::sui::SUI<vector>",
		"0x" + strings.Repeat("f", 65) + "::a::B", "0x2::su-i::SUI",
	} {
		if _, err := NormalizeMoveType(invalid); err == nil {
			t.Errorf("NormalizeMoveType(%q) accepted an invalid type", invalid)
		}
	}
	nested := "0x1::a::A"
	for depth := 0; depth < moveTypeMaxDepth+1; depth++ {
		nested = "0x1::a::A<" + nested + ">"
	}
	if _, err := NormalizeMoveType(nested); err == nil {
		t.Error("NormalizeMoveType accepted a type nested past the depth bound")
	}
}

func TestDecodeBase58RecoversTheMainnetChainIdentifier(t *testing.T) {
	digest, err := decodeBase58(suiMainnetGenesisDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 32 || hex.EncodeToString(digest[:4]) != SuiMainnetChainIdentifier {
		t.Fatalf("digest = %x", digest)
	}
	leading, err := decodeBase58("11a")
	if err != nil || len(leading) != 3 || leading[0] != 0 || leading[1] != 0 || leading[2] != 33 {
		t.Fatalf("decodeBase58(11a) = %x, %v", leading, err)
	}
	if _, err := decodeBase58("0OIl"); err == nil {
		t.Error("decodeBase58 accepted characters outside the alphabet")
	}
	for _, spelling := range []string{suiMainnetGenesisDigest, "35834A8A"} {
		identifier, err := suiChainIdentifierHex(spelling)
		if err != nil || identifier != SuiMainnetChainIdentifier {
			t.Errorf("suiChainIdentifierHex(%q) = %q, %v", spelling, identifier, err)
		}
	}
	if err := checkSuiChainIdentifier(suiTestDigest, SuiMainnetChainIdentifier); err == nil {
		t.Error("checkSuiChainIdentifier accepted another network's digest")
	}
}

func TestNewSuiCheckpointValidatesDigestAndTimestamp(t *testing.T) {
	checkpoint, err := newSuiCheckpoint(320_000_000, suiTestDigest, time.Unix(1788826290, 73_000_000))
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := decodeBase58(suiTestDigest)
	if checkpoint.Sequence != 320_000_000 || string(checkpoint.Digest[:]) != string(wantDigest) {
		t.Fatalf("checkpoint = %+v", checkpoint)
	}
	if !checkpoint.Timestamp.Equal(suiTestCheckpointTime) || checkpoint.Timestamp.Location() != time.UTC {
		t.Fatalf("timestamp = %s, want %s in UTC", checkpoint.Timestamp, suiTestCheckpointTime)
	}
	if _, err := newSuiCheckpoint(1, "11", time.Now()); err == nil {
		t.Error("newSuiCheckpoint accepted a digest shorter than 32 bytes")
	}
	if _, err := newSuiCheckpoint(1, suiTestDigest, time.Time{}); err == nil {
		t.Error("newSuiCheckpoint accepted a checkpoint without a timestamp")
	}
	if _, err := newSuiCheckpoint(1, "0OIl", time.Now()); err == nil {
		t.Error("newSuiCheckpoint accepted a digest outside the base58 alphabet")
	}
}

func TestSuiBalancesFromNormalizesAndRejectsBadRows(t *testing.T) {
	lpSpaced := "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<0x2::sui::SUI, 0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>"
	lpLong := "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<" + suiLongType +
		",0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>"
	balances, err := suiBalancesFrom([]suiBalanceRow{
		{coinType: lpSpaced, amount: big.NewInt(1000)},
		{coinType: "0xdead::spent::SPENT", amount: big.NewInt(0)},
		{coinType: "0x2::sui::SUI", amount: big.NewInt(96383083128)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(balances) != 2 {
		t.Fatalf("balances = %+v, want SUI and the LP with the zero row dropped", balances)
	}
	if balances[0].CoinType != suiLongType || balances[0].Amount.String() != "96383083128" {
		t.Fatalf("first balance = %+v, want the short SUI type normalized and sorted first", balances[0])
	}
	if balances[1].CoinType != lpLong || balances[1].Amount.String() != "1000" {
		t.Fatalf("second balance = %+v, want the spaced generic normalized to %s", balances[1], lpLong)
	}

	for name, rows := range map[string][]suiBalanceRow{
		"duplicate spellings of one coin": {
			{coinType: "0x2::sui::SUI", amount: big.NewInt(1)},
			{coinType: suiLongType, amount: big.NewInt(2)},
		},
		"malformed coin type": {{coinType: "0x2::sui", amount: big.NewInt(1)}},
		"missing amount":      {{coinType: "0x2::sui::SUI"}},
		"negative amount":     {{coinType: "0x2::sui::SUI", amount: big.NewInt(-1)}},
	} {
		if _, err := suiBalancesFrom(rows); err == nil {
			t.Errorf("suiBalancesFrom accepted %s", name)
		}
	}
}

func TestSuiCoinMetadataFromNeverGuesses(t *testing.T) {
	nine, thirtySeven := 9, 37
	symbol, control, blank := "SUI", "S\x00UI", "  "
	for name, invalid := range map[string]struct {
		decimals *int
		symbol   *string
	}{
		"no decimals":           {nil, &symbol},
		"no symbol":             {&nine, nil},
		"decimals out of range": {&thirtySeven, &symbol},
		"control character":     {&nine, &control},
		"blank symbol":          {&nine, &blank},
	} {
		if _, err := suiCoinMetadataFrom(suiLongType, invalid.decimals, invalid.symbol, "Sui"); err == nil {
			t.Errorf("suiCoinMetadataFrom accepted %s", name)
		}
	}
	metadata, err := suiCoinMetadataFrom(suiLongType, &nine, &symbol, "Sui")
	if err != nil {
		t.Fatal(err)
	}
	if metadata != (SuiCoinMetadata{CoinType: suiLongType, Symbol: "SUI", Name: "Sui", Decimals: 9}) {
		t.Fatalf("metadata = %+v", metadata)
	}
}
