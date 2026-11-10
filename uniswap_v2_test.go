package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var (
	uniswapV2TestAccount = common.HexToAddress("0x8e0a14666a68b7aeb604ee81c6a32fe6deb376c1")
	uniswapV2TestPairA   = common.HexToAddress("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	uniswapV2TestPairB   = common.HexToAddress("0x4444444444444444444444444444444444444444")
	uniswapV2TestToken0  = common.HexToAddress("0x2222222222222222222222222222222222222222")
	uniswapV2TestToken1  = common.HexToAddress("0x3333333333333333333333333333333333333333")
	uniswapV2TestToken2  = common.HexToAddress("0x5555555555555555555555555555555555555555")
)

// uniswapV2TestBlock is a pin every chain of the index has activated by.
const uniswapV2TestBlock = uint64(200_000_000)

func uniswapV2TestPin(chainID ChainID) BlockRef {
	return BlockRef{ChainID: chainID, Number: uniswapV2TestBlock, Timestamp: indexTestTimestamp}
}

// newUniswapV2TestAdapter reads its index from fake, declared as the adapter declares it.
func newUniswapV2TestAdapter(t *testing.T, fake *fakeIndex) *UniswapV2Adapter {
	t.Helper()
	served := fake.serve("uniswap-v2")
	adapter := newUniswapV2Adapter(served.config).(*UniswapV2Adapter)
	adapter.index.api = served.api
	return adapter
}

func uniswapV2TestRow(chainID ChainID, owner common.Address, fork uniswapV2Fork, pair, token0, token1 common.Address, balance string) map[string]any {
	lower := func(address common.Address) string { return strings.ToLower(address.Hex()) }
	return map[string]any{
		"id":       fmt.Sprintf("%d:%s:%s:%s", chainID, lower(owner), fork.ID, lower(pair)),
		"chainId":  int(chainID),
		"owner":    lower(owner),
		"protocol": fork.ID,
		"factory":  lower(fork.Factory),
		"pair":     lower(pair),
		"token0":   lower(token0),
		"token1":   lower(token1),
		"balance":  balance,
	}
}

func uniswapV2TestFork(t *testing.T, chainID ChainID, id string) uniswapV2Fork {
	t.Helper()
	fork, exists := uniswapV2ForkOf(chainID, id)
	if !exists {
		t.Fatalf("no %s fork on chain %d", id, chainID)
	}
	return fork
}

// stubUniswapV2Pair answers a pair's reads at the pin.
func stubUniswapV2Pair(stub *contractStub, pair, token0, token1 common.Address, balance, supply, reserve0, reserve1 *big.Int) {
	stub.balance(pair, balance)
	stub.answer(pair, uniswapV2PairABI, "totalSupply", nil, supply)
	stub.answer(pair, uniswapV2PairABI, "getReserves", nil, reserve0, reserve1, big.NewInt(1))
	stub.answer(pair, uniswapV2PairABI, "token0", nil, token0)
	stub.answer(pair, uniswapV2PairABI, "token1", nil, token1)
}

func stubUniswapV2Token(stub *contractStub, token common.Address, symbol string) {
	stub.answer(token, erc20ABI, "symbol", nil, symbol)
	stub.answer(token, erc20ABI, "decimals", nil, uint8(18))
}

// One read lists the account's LP balances on every fork of the chain, certified by the earliest
// fork's checkpoint row. Each pair is valued at the pin as the account's share of its reserves,
// labelled with its fork, and a pair the account has left since the index reads as nothing.
func TestUniswapV2ReadsEveryForkOfTheChainInOneStream(t *testing.T) {
	fake := newFakeIndex(t, uniswapV2TestBlock)
	uniswap, aerodrome := uniswapV2TestFork(t, Base, "uniswap-v2"), uniswapV2TestFork(t, Base, "aerodrome-v2")
	fake.rows["lpBalances"] = []map[string]any{
		uniswapV2TestRow(Base, uniswapV2TestAccount, uniswap, uniswapV2TestPairA, uniswapV2TestToken0, uniswapV2TestToken1, "100"),
		uniswapV2TestRow(Base, uniswapV2TestAccount, aerodrome, uniswapV2TestPairB, uniswapV2TestToken0, uniswapV2TestToken2, "5"),
		uniswapV2TestRow(Base, indexLagTestOther, uniswap, uniswapV2TestPairA, uniswapV2TestToken0, uniswapV2TestToken1, "9"),
	}
	adapter := newUniswapV2TestAdapter(t, fake)

	stub := newContractStub(t, Base, uniswapV2TestAccount)
	// Aerodrome's reserves are uint256, wider than Uniswap's uint112.
	reserve0 := new(big.Int).Lsh(big.NewInt(1), 120)
	stubUniswapV2Pair(stub, uniswapV2TestPairA, uniswapV2TestToken0, uniswapV2TestToken1,
		big.NewInt(100), big.NewInt(1_000), reserve0, big.NewInt(7_000))
	stubUniswapV2Pair(stub, uniswapV2TestPairB, uniswapV2TestToken0, uniswapV2TestToken2,
		new(big.Int), big.NewInt(1_000), big.NewInt(1), big.NewInt(1))
	stubUniswapV2Token(stub, uniswapV2TestToken0, "TK0")
	stubUniswapV2Token(stub, uniswapV2TestToken1, "TK1")

	groups, err := adapter.Positions(context.Background(), stub.dial(), uniswapV2TestPin(Base), uniswapV2TestAccount)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 {
		t.Fatalf("groups = %+v, want the Uniswap pair alone", groups)
	}
	group := groups[0]
	if group.ID != "pair:"+strings.ToLower(uniswapV2TestPairA.Hex()) || group.Label != "TK0 / TK1 · Uniswap V2" ||
		group.Metadata["dex"] != "uniswap-v2" || group.Metadata["indexerBlock"] != uniswapV2TestBlock {
		t.Fatalf("group = %+v", group)
	}
	wantAmount0 := new(big.Int).Mul(big.NewInt(100), reserve0).String()
	if len(group.Components) != 2 ||
		group.Components[0].AmountRaw != wantAmount0 || group.Components[0].AmountDenominatorRaw != "1000" ||
		group.Components[1].AmountRaw != "700000" || group.Components[1].AmountDenominatorRaw != "1000" {
		t.Fatalf("components = %+v", group.Components)
	}
	stub.assertHolds(groups, uniswapV2TestPairA)

	requests := fake.requests()
	if len(requests) != 1 {
		t.Fatalf("sent %d requests, want one page", len(requests))
	}
	for _, want := range []string{
		`where: { id: "8453:sushiswap-v2" }`,
		`balance_gt: 0, id_starts_with: "8453:` + strings.ToLower(uniswapV2TestAccount.Hex()) + `:"`,
	} {
		if !strings.Contains(requests[0], want) {
			t.Fatalf("page does not ask %s:\n%s", want, requests[0])
		}
	}
}

// BSC's processor has not finished its backfill. A live read there answers as of the processor's
// block and says how far behind it is; a fixed pin it has not reached still fails, and so does
// any other chain that is catching up.
func TestUniswapV2ReadsBSCWhileItsProcessorCatchesUp(t *testing.T) {
	const processed = uniswapV2TestBlock - 1_000
	behind := func(_ int, queryBlock uint64) []map[string]any {
		return []map[string]any{fakeCheckpointRow(queryBlock, indexTestTimestamp-uint64((48*time.Hour).Seconds()))}
	}
	newAdapter := func(t *testing.T, chainID ChainID) (*UniswapV2Adapter, *fakeIndex) {
		fake := newFakeIndex(t, processed)
		fake.states = map[ChainID]string{chainID: "CATCHING_UP"}
		fake.checkpoint = behind
		return newUniswapV2TestAdapter(t, fake), fake
	}

	t.Run("live on BSC", func(t *testing.T) {
		adapter, fake := newAdapter(t, BSC)
		ctx, coverage := withCoverage(context.Background())
		groups, err := adapter.Positions(ctx, nil, uniswapV2TestPin(BSC), uniswapV2TestAccount)
		if err != nil || len(groups) != 0 {
			t.Fatalf("groups = %+v err = %v, want an empty answer", groups, err)
		}
		requests := fake.requests()
		if len(requests) != 1 || !strings.Contains(requests[0], fmt.Sprintf("block: { number: %d }", processed)) ||
			!strings.Contains(requests[0], `where: { id: "56:sushiswap-v2" }`) {
			t.Fatalf("requests = %v, want one page at the processed block", requests)
		}
		notes := coverage.take()
		var lag *indexLagNote
		if len(notes) != 1 || !errors.As(notes[0], &lag) || lag.checkpoint != processed {
			t.Fatalf("notes = %v, want one lag note at block %d", notes, processed)
		}
	})
	t.Run("fixed pin on BSC past the processor", func(t *testing.T) {
		adapter, _ := newAdapter(t, BSC)
		block := uniswapV2TestPin(BSC)
		block.Fixed = true
		_, err := adapter.Positions(context.Background(), nil, block, uniswapV2TestAccount)
		if err == nil || !strings.Contains(err.Error(), "short of pinned block") {
			t.Fatalf("error = %v, want the pin the processor has not reached", err)
		}
	})
	t.Run("live on Ethereum", func(t *testing.T) {
		adapter, _ := newAdapter(t, Ethereum)
		_, err := adapter.Positions(context.Background(), nil, uniswapV2TestPin(Ethereum), uniswapV2TestAccount)
		if err == nil || !strings.Contains(err.Error(), "backfill is incomplete") {
			t.Fatalf("error = %v, want Ethereum held to PROCESSING_LATEST", err)
		}
	})
}

// A row is read only as the holding its fields rebuild: the account's, on a fork the index covers
// on the chain, from that fork's factory, with a positive balance.
func TestUniswapV2RejectsRowsItCannotAttribute(t *testing.T) {
	owner := strings.ToLower(uniswapV2TestAccount.Hex())
	pancake := uniswapV2TestFork(t, BSC, "pancakeswap-v2")
	valid := func() map[string]any {
		return uniswapV2TestRow(BSC, uniswapV2TestAccount, pancake, uniswapV2TestPairA, uniswapV2TestToken0, uniswapV2TestToken1, "7")
	}
	decode := func(fields map[string]any) (uniswapV2IndexedPair, bool) {
		row := uniswapV2BalanceRow{
			ChainID: fields["chainId"].(int), Owner: fields["owner"].(string), Protocol: fields["protocol"].(string),
			Factory: fields["factory"].(string), Pair: fields["pair"].(string), Token0: fields["token0"].(string),
			Token1: fields["token1"].(string), Balance: fields["balance"].(string),
		}
		return uniswapV2HeldPair(BSC, owner, fields["id"].(string), row)
	}
	if pair, ok := decode(valid()); !ok || pair.Fork.ID != "pancakeswap-v2" || pair.Pair != uniswapV2TestPairA ||
		pair.Token0 != uniswapV2TestToken0 || pair.Token1 != uniswapV2TestToken1 {
		t.Fatalf("valid row = %+v, %t", pair, ok)
	}
	for name, change := range map[string]func(map[string]any){
		"another chain":           func(row map[string]any) { row["chainId"] = int(Base) },
		"another owner":           func(row map[string]any) { row["owner"] = strings.ToLower(indexLagTestOther.Hex()) },
		"a fork not on the chain": func(row map[string]any) { row["protocol"] = "aerodrome-v2" },
		"another fork's factory": func(row map[string]any) {
			row["factory"] = strings.ToLower(uniswapV2TestFork(t, BSC, "sushiswap-v2").Factory.Hex())
		},
		"a checksummed pair": func(row map[string]any) {
			// The id spells the pair the same way, so only the spelling can reject the row.
			row["pair"] = uniswapV2TestPairA.Hex()
			row["id"] = strings.TrimSuffix(row["id"].(string), strings.ToLower(uniswapV2TestPairA.Hex())) + uniswapV2TestPairA.Hex()
		},
		"an invalid token": func(row map[string]any) { row["token1"] = "0x33" },
		"no balance":       func(row map[string]any) { row["balance"] = "0" },
		"a padded balance": func(row map[string]any) { row["balance"] = "07" },
		"an id of another row": func(row map[string]any) {
			row["id"] = strings.Replace(row["id"].(string), "pancakeswap", "sushiswap", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			row := valid()
			change(row)
			if pair, ok := decode(row); ok {
				t.Fatalf("decoded %+v, want the row rejected", pair)
			}
		})
	}
}

// What the pair reports at the pin decides the position. A balance gone since the index is
// nothing; tokens the index misreports, or a balance no supply backs, cannot come from lag.
func TestUniswapV2DecodesPairStateAtThePin(t *testing.T) {
	pair := uniswapV2IndexedPair{Pair: uniswapV2TestPairA, Token0: uniswapV2TestToken0, Token1: uniswapV2TestToken1}
	rows := func(balance, supply int64, token0 common.Address) [][]any {
		return [][]any{
			{big.NewInt(balance)}, {big.NewInt(supply)}, {big.NewInt(10), big.NewInt(20), big.NewInt(1)},
			{token0}, {uniswapV2TestToken1},
		}
	}
	if _, held, err := decodeUniswapV2Position(pair, rows(0, 1_000, uniswapV2TestToken0)); err != nil || held {
		t.Fatalf("held = %t err = %v, want a pair left since the index read as nothing", held, err)
	}
	position, held, err := decodeUniswapV2Position(pair, rows(5, 1_000, uniswapV2TestToken0))
	if err != nil || !held || position.reserve1.Int64() != 20 {
		t.Fatalf("position = %+v held = %t err = %v", position, held, err)
	}
	if _, _, err := decodeUniswapV2Position(pair, rows(5, 1_000, uniswapV2TestToken2)); err == nil {
		t.Fatal("tokens that disagree with the index were accepted")
	}
	if _, _, err := decodeUniswapV2Position(pair, rows(5, 4, uniswapV2TestToken0)); err == nil {
		t.Fatal("a balance past the total supply was accepted")
	}
}

// Each chain lists its forks earliest first, so the first fork's checkpoint and creation block
// stand for the index on the chain, and the protocol's availability opens there.
func TestUniswapV2ForksStartTheIndexAtTheEarliestFactory(t *testing.T) {
	adapter := newUniswapV2Adapter(testSentioIndexerConfig("uniswap-v2")).(*UniswapV2Adapter)
	if got, want := adapter.Info().Chains, []ChainID{Ethereum, BSC, Base, Arbitrum}; !slices.Equal(got, want) {
		t.Fatalf("chains = %v, want %v", got, want)
	}
	for chainID, forks := range uniswapV2Forks {
		protocols, factories := make(map[string]struct{}), make(map[common.Address]struct{})
		for index, fork := range forks {
			if index > 0 && fork.Window.ActivationBlock < forks[index-1].Window.ActivationBlock {
				t.Errorf("chain %d lists %s before an earlier fork", chainID, fork.ID)
			}
			if _, twice := protocols[fork.ID]; twice {
				t.Errorf("chain %d lists %s twice", chainID, fork.ID)
			}
			if _, twice := factories[fork.Factory]; twice {
				t.Errorf("chain %d lists factory %s twice", chainID, fork.Factory)
			}
			protocols[fork.ID], factories[fork.Factory] = struct{}{}, struct{}{}
		}
		if got, want := adapter.index.checkpointID(chainID), fmt.Sprintf("%d:%s", chainID, forks[0].ID); got != want {
			t.Errorf("chain %d checkpoint = %q, want %q", chainID, got, want)
		}
		if got := adapter.index.start(chainID); got != forks[0].Window.ActivationBlock {
			t.Errorf("chain %d start = %d, want %d", chainID, got, forks[0].Window.ActivationBlock)
		}
		windows := protocolAvailabilityByID["uniswap-v2"][chainID]
		if len(windows) != 1 || windows[0].deploymentWindow.ActivationBlock != forks[0].Window.ActivationBlock {
			t.Errorf("chain %d availability = %+v, want it from block %d", chainID, windows, forks[0].Window.ActivationBlock)
		}
	}
}

// The presence probe reads the same checkpoint row on each chain as that chain's pages do.
func TestUniswapV2PresenceProbeReadsEachChainsForkCheckpoint(t *testing.T) {
	adapter := newUniswapV2Adapter(testSentioIndexerConfig("uniswap-v2")).(*UniswapV2Adapter)
	probe, _, provable := adapter.index.presenceProbe([]indexStreamer{uniswapV2BalanceStream(uniswapV2TestAccount)})
	if !provable {
		t.Fatal("the LP balance stream cannot be proven empty")
	}
	targets := make(map[ChainID]presenceTarget)
	for _, chainID := range adapter.Info().Chains {
		targets[chainID] = presenceTarget{block: uniswapV2TestPin(chainID), queryBlock: uniswapV2TestBlock}
	}
	query := presenceQuery(probe, targets)
	for _, chainID := range adapter.Info().Chains {
		want := fmt.Sprintf(`where: { id: "%d:%s" }`, chainID, uniswapV2Forks[chainID][0].ID)
		if !strings.Contains(query, want) {
			t.Errorf("probe does not ask %s:\n%s", want, query)
		}
	}
}
