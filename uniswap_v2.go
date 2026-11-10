package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

// uniswapV2MaxIndexedPairs bounds the LP balances of one account a scan reads on one chain, across
// every fork the index covers there.
const uniswapV2MaxIndexedPairs = 1_024

// uniswapV2PairABI is the surface every fork's pair shares. Aerodrome's getReserves returns three
// uint256 where Uniswap's returns (uint112, uint112, uint32); each word decodes as a uint256, so
// one ABI reads both.
var uniswapV2PairABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"totalSupply","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"getReserves","stateMutability":"view","inputs":[],"outputs":[{"name":"reserve0","type":"uint256"},{"name":"reserve1","type":"uint256"},{"name":"blockTimestampLast","type":"uint256"}]},
  {"type":"function","name":"token0","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"token1","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

// uniswapV2Fork is Uniswap V2, or a fork of it, on one chain: a factory whose pairs the V2 index
// follows. The processor labels the fork's rows and its checkpoint row with ID.
type uniswapV2Fork struct {
	ID      string
	Name    string
	Factory common.Address
	// Window opens at the factory's creation block, where the processor binds it. The kernel never
	// calls the factory: it reads the pairs the index lists, which exist wherever they are listed.
	Window deploymentWindow
}

// uniswapV2Forks lists the forks the V2 index covers on each chain, earliest first. The first is
// the one whose checkpoint certifies a read on the chain: the processor writes one checkpoint row
// per fork, every fork's at the same block, and the earliest fork's has been written longest. Its
// creation block is also the adapter's activation on the chain.
var uniswapV2Forks = map[ChainID][]uniswapV2Fork{
	Ethereum: {
		{ID: "uniswap-v2", Name: "Uniswap V2", Factory: common.HexToAddress("0x5C69bEe701ef814a2B6a3EDD4B1652CB9cc5aA6f"), Window: deploymentWindow{ActivationBlock: 10_000_835}},
		{ID: "sushiswap-v2", Name: "SushiSwap V2", Factory: common.HexToAddress("0xC0AEe478e3658e2610c5F7A4A2E1777cE9e4f2Ac"), Window: deploymentWindow{ActivationBlock: 10_794_229}},
		{ID: "pancakeswap-v2", Name: "PancakeSwap V2", Factory: common.HexToAddress("0x1097053Fd2ea711dad45caCcc45EfF7548fCB362"), Window: deploymentWindow{ActivationBlock: 15_614_590}},
	},
	BSC: {
		{ID: "sushiswap-v2", Name: "SushiSwap V2", Factory: common.HexToAddress("0xc35DADB65012eC5796536bD9864eD8773aBc74C4"), Window: deploymentWindow{ActivationBlock: 5_205_069}},
		{ID: "pancakeswap-v2", Name: "PancakeSwap V2", Factory: common.HexToAddress("0xcA143Ce32Fe78f1f7019d7d551a6402fC5350c73"), Window: deploymentWindow{ActivationBlock: 6_809_737}},
		{ID: "uniswap-v2", Name: "Uniswap V2", Factory: common.HexToAddress("0x8909Dc15e40173Ff4699343b6eB8132c65e18eC6"), Window: deploymentWindow{ActivationBlock: 33_496_018}},
	},
	Base: {
		{ID: "sushiswap-v2", Name: "SushiSwap V2", Factory: common.HexToAddress("0x71524B4f93c58fcbF659783284E38825f0622859"), Window: deploymentWindow{ActivationBlock: 2_631_214}},
		{ID: "pancakeswap-v2", Name: "PancakeSwap V2", Factory: common.HexToAddress("0x02a84c1b3BBD7401a5f7fa98a384EBC70bB5749E"), Window: deploymentWindow{ActivationBlock: 2_910_387}},
		{ID: "aerodrome-v2", Name: "Aerodrome", Factory: common.HexToAddress("0x420DD381b31aEf6683db6B902084cB0FFECe40Da"), Window: deploymentWindow{ActivationBlock: 3_200_559}},
		{ID: "uniswap-v2", Name: "Uniswap V2", Factory: common.HexToAddress("0x8909Dc15e40173Ff4699343b6eB8132c65e18eC6"), Window: deploymentWindow{ActivationBlock: 6_601_915}},
	},
	Arbitrum: {
		{ID: "sushiswap-v2", Name: "SushiSwap V2", Factory: common.HexToAddress("0xc35DADB65012eC5796536bD9864eD8773aBc74C4"), Window: deploymentWindow{ActivationBlock: 70}},
		{ID: "pancakeswap-v2", Name: "PancakeSwap V2", Factory: common.HexToAddress("0x02a84c1b3BBD7401a5f7fa98a384EBC70bB5749E"), Window: deploymentWindow{ActivationBlock: 101_022_992}},
		{ID: "uniswap-v2", Name: "Uniswap V2", Factory: common.HexToAddress("0xf1D7CC64Fb4452F05c498126312eBE29f30Fbcf9"), Window: deploymentWindow{ActivationBlock: 150_442_611}},
	},
}

// uniswapV2CatchingUp names the chains the V2 index is read on while its processor is still
// CATCHING_UP (sentioIndex.catchingUp). BSC has not finished its first backfill, and failing every
// BSC read until it does would hide positions the index already lists; a live read there answers
// as of the processor's block, with a lag note. Remove the chain once it reaches PROCESSING_LATEST.
var uniswapV2CatchingUp = map[ChainID]struct{}{BSC: {}}

// uniswapV2ForkOf returns the fork the index labels protocol on chainID.
func uniswapV2ForkOf(chainID ChainID, protocol string) (uniswapV2Fork, bool) {
	for _, fork := range uniswapV2Forks[chainID] {
		if fork.ID == protocol {
			return fork, true
		}
	}
	return uniswapV2Fork{}, false
}

// UniswapV2Adapter values the LP tokens an account holds in Uniswap V2 pairs and in those of its
// forks, read through one index whose processor follows every fork's factory.
type UniswapV2Adapter struct {
	adapterBase
	index *sentioIndex
}

func newUniswapV2Adapter(config SentioIndexerConfig) Adapter {
	chains := deploymentChains(uniswapV2Forks)
	return &UniswapV2Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "uniswap-v2", Name: "Uniswap V2", Chains: chains}},
		index: &sentioIndex{
			name: "uniswap-v2", api: newSentioAPIClient(), config: config, chains: chains, probe: true,
			start: func(chainID ChainID) uint64 {
				if forks := uniswapV2Forks[chainID]; len(forks) > 0 {
					return forks[0].Window.ActivationBlock
				}
				return 0
			},
			checkpoint: func(chainID ChainID) string {
				if forks := uniswapV2Forks[chainID]; len(forks) > 0 {
					return fmt.Sprintf("%d:%s", chainID, forks[0].ID)
				}
				return ""
			},
			catchingUp: func(chainID ChainID) bool {
				_, catchingUp := uniswapV2CatchingUp[chainID]
				return catchingUp
			},
		},
	}
}

func (a *UniswapV2Adapter) prefetchPresence(ctx context.Context, account common.Address) {
	a.index.prefetchPresence(ctx, uniswapV2BalanceStream(account))
}

type uniswapV2BalanceRow struct {
	ChainID  int    `json:"chainId"`
	Owner    string `json:"owner"`
	Protocol string `json:"protocol"`
	Factory  string `json:"factory"`
	Pair     string `json:"pair"`
	Token0   string `json:"token0"`
	Token1   string `json:"token1"`
	Balance  string `json:"balance"`
}

// uniswapV2IndexedPair is a pair the index lists the account holding LP tokens of.
type uniswapV2IndexedPair struct {
	Fork   uniswapV2Fork
	Pair   common.Address
	Token0 common.Address
	Token1 common.Address
}

// uniswapV2BalanceStream reads the pairs account holds LP tokens of, on every fork at once: each
// row's id starts with the chain and the account, whatever the fork. The index keeps a row per pair
// the account ever held, so only those with a balance are read.
func uniswapV2BalanceStream(account common.Address) *indexStream[uniswapV2BalanceRow, uniswapV2IndexedPair] {
	owner := strings.ToLower(account.Hex())
	return &indexStream[uniswapV2BalanceRow, uniswapV2IndexedPair]{
		Entity: "lpBalances", Account: account, MaxRows: uniswapV2MaxIndexedPairs,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account), Terms: []indexTerm{{"balance_gt", 0}}}
		},
		Decode: func(chainID ChainID, id string, row uniswapV2BalanceRow) (uniswapV2IndexedPair, error) {
			if pair, valid := uniswapV2HeldPair(chainID, owner, id, row); valid {
				return pair, nil
			}
			return uniswapV2IndexedPair{}, fmt.Errorf("GraphQL returned malformed LP balance row %q", id)
		},
	}
}

// uniswapV2HeldPair reads an LP balance row as the holding it must be: owner's, on a pair of a fork
// the index covers on the chain, created by that fork's factory, with a positive balance, under
// the id its fields rebuild.
func uniswapV2HeldPair(chainID ChainID, owner string, id string, row uniswapV2BalanceRow) (uniswapV2IndexedPair, bool) {
	if row.ChainID != int(chainID) || row.Owner != owner {
		return uniswapV2IndexedPair{}, false
	}
	fork, exists := uniswapV2ForkOf(chainID, row.Protocol)
	if !exists || row.Factory != strings.ToLower(fork.Factory.Hex()) {
		return uniswapV2IndexedPair{}, false
	}
	pair, pairValid := lowercaseAddress(row.Pair)
	token0, token0Valid := lowercaseAddress(row.Token0)
	token1, token1Valid := lowercaseAddress(row.Token1)
	if !pairValid || !token0Valid || !token1Valid {
		return uniswapV2IndexedPair{}, false
	}
	balance, ok := new(big.Int).SetString(row.Balance, 10)
	if !ok || row.Balance != balance.String() || balance.Sign() <= 0 {
		return uniswapV2IndexedPair{}, false
	}
	if id != fmt.Sprintf("%d:%s:%s:%s", chainID, owner, row.Protocol, row.Pair) {
		return uniswapV2IndexedPair{}, false
	}
	return uniswapV2IndexedPair{Fork: fork, Pair: pair, Token0: token0, Token1: token1}, true
}

// lowercaseAddress parses value as an address spelled the way processors write one: lowercase hex.
func lowercaseAddress(value string) (common.Address, bool) {
	if !common.IsHexAddress(value) {
		return common.Address{}, false
	}
	address := common.HexToAddress(value)
	return address, value == strings.ToLower(address.Hex())
}

// uniswapV2Position is an LP balance as read at the pin.
type uniswapV2Position struct {
	pair        uniswapV2IndexedPair
	balance     *big.Int
	totalSupply *big.Int
	reserve0    *big.Int
	reserve1    *big.Int
}

// Positions reads the account's share of each pair's reserves at the pin. The index decides which
// pairs to read, as of its checkpoint; balance, supply and reserves are all read at the pin, so a
// pair the account has left since reads as nothing.
func (a *UniswapV2Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	forks, exists := uniswapV2Forks[block.ChainID]
	if !exists || !forks[0].Window.ActiveAt(block.Number) {
		return nil, nil
	}
	balances := uniswapV2BalanceStream(account)
	at, err := a.index.read(ctx, client, block, balances)
	if err != nil {
		return nil, fmt.Errorf("Uniswap V2 index query: %w", err)
	}
	pairs := balances.Rows()
	if len(pairs) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(pairs)*5)
	for _, pair := range pairs {
		calls = append(calls,
			ContractCall{Contract: pair.Pair, ABI: uniswapV2PairABI, Method: "balanceOf", Args: []any{account}},
			ContractCall{Contract: pair.Pair, ABI: uniswapV2PairABI, Method: "totalSupply"},
			ContractCall{Contract: pair.Pair, ABI: uniswapV2PairABI, Method: "getReserves"},
			ContractCall{Contract: pair.Pair, ABI: uniswapV2PairABI, Method: "token0"},
			ContractCall{Contract: pair.Pair, ABI: uniswapV2PairABI, Method: "token1"},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("pair state: %w", err)
	}
	positions := make([]uniswapV2Position, 0, len(pairs))
	tokenAddresses := make([]common.Address, 0, len(pairs)*2)
	for index, pair := range pairs {
		position, held, err := decodeUniswapV2Position(pair, rows[index*5:index*5+5])
		if err != nil {
			return nil, fmt.Errorf("pair %s: %w", pair.Pair, err)
		}
		if !held {
			continue
		}
		positions = append(positions, position)
		tokenAddresses = append(tokenAddresses, pair.Token0, pair.Token1)
	}
	if len(positions) == 0 {
		return nil, nil
	}
	tokens, err := tokenMetadataAt(ctx, client, block, tokenAddresses)
	if err != nil {
		return nil, fmt.Errorf("token metadata: %w", err)
	}
	groups := make([]Group, 0, len(positions))
	for _, position := range positions {
		if group, valued := uniswapV2Group(position, tokens, at.QueryBlock); valued {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

// decodeUniswapV2Position reads one pair's balanceOf, totalSupply, getReserves, token0 and token1
// rows. A zero balance is a pair the account no longer holds, which lag explains; tokens that
// disagree with the index, or a balance with no supply behind it, cannot come from lag.
func decodeUniswapV2Position(pair uniswapV2IndexedPair, rows [][]any) (uniswapV2Position, bool, error) {
	balance, err := BigIntAt(rows[0], 0)
	if err != nil {
		return uniswapV2Position{}, false, fmt.Errorf("balanceOf: %w", err)
	}
	if balance.Sign() == 0 {
		return uniswapV2Position{}, false, nil
	}
	totalSupply, err := BigIntAt(rows[1], 0)
	if err != nil {
		return uniswapV2Position{}, false, fmt.Errorf("totalSupply: %w", err)
	}
	if totalSupply.Cmp(balance) < 0 {
		return uniswapV2Position{}, false, fmt.Errorf("LP balance %s exceeds total supply %s", balance, totalSupply)
	}
	reserve0, err := BigIntAt(rows[2], 0)
	if err != nil {
		return uniswapV2Position{}, false, fmt.Errorf("reserve0: %w", err)
	}
	reserve1, err := BigIntAt(rows[2], 1)
	if err != nil {
		return uniswapV2Position{}, false, fmt.Errorf("reserve1: %w", err)
	}
	token0, err := AddressAt(rows[3], 0)
	if err != nil {
		return uniswapV2Position{}, false, fmt.Errorf("token0: %w", err)
	}
	token1, err := AddressAt(rows[4], 0)
	if err != nil {
		return uniswapV2Position{}, false, fmt.Errorf("token1: %w", err)
	}
	if token0 != pair.Token0 || token1 != pair.Token1 {
		return uniswapV2Position{}, false, fmt.Errorf("indexed tokens disagree with the pair at the pinned block")
	}
	return uniswapV2Position{
		pair: pair, balance: balance, totalSupply: totalSupply, reserve0: reserve0, reserve1: reserve1,
	}, true, nil
}

// uniswapV2Group reports a position as the account's share of each reserve, balance × reserve /
// totalSupply, kept exact as a numerator over the supply. The LP token is what the account holds,
// so the wallet drops it.
func uniswapV2Group(position uniswapV2Position, tokens map[common.Address]Token, indexerBlock uint64) (Group, bool) {
	pair := position.pair
	source := Source{
		Contract: pair.Pair, Method: "balanceOf/getReserves/totalSupply", Holds: []common.Address{pair.Pair},
	}
	token0, token1 := tokens[pair.Token0], tokens[pair.Token1]
	components := make([]Component, 0, 2)
	for _, share := range []struct {
		token   Token
		reserve *big.Int
	}{{token0, position.reserve0}, {token1, position.reserve1}} {
		amount := new(big.Int).Mul(position.balance, share.reserve)
		if amount.Sign() == 0 {
			continue
		}
		component := NewComponent("asset", share.token, amount, source)
		component.AmountDenominatorRaw = position.totalSupply.String()
		components = append(components, component)
	}
	if len(components) == 0 {
		return Group{}, false
	}
	return Group{
		ID:         "pair:" + strings.ToLower(pair.Pair.Hex()),
		Label:      fmt.Sprintf("%s / %s · %s", token0.Symbol, token1.Symbol, pair.Fork.Name),
		Components: components,
		Metadata: map[string]any{
			"dex": pair.Fork.ID, "pair": pair.Pair.Hex(), "lpBalance": position.balance.String(),
			"totalSupply": position.totalSupply.String(), "indexerBlock": indexerBlock,
		},
	}, true
}
