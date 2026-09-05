package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/rpc"
)

const (
	uniswapV3MaxActivePositions = 512
	uniswapV4DynamicFeeFlag     = uint32(1 << 23)
	uniswapV4MaxLPFee           = uint32(1_000_000)
)

var uniswapMaxUint128 = new(big.Int).Sub(
	new(big.Int).Lsh(big.NewInt(1), 128),
	big.NewInt(1),
)

var uniswapV3PositionManagerABI = MustABI(`[
  {"type":"function","name":"supportsInterface","stateMutability":"view","inputs":[{"name":"interfaceId","type":"bytes4"}],"outputs":[{"type":"bool"}]},
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"tokenOfOwnerByIndex","stateMutability":"view","inputs":[{"name":"owner","type":"address"},{"name":"index","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"ownerOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"owner","type":"address"}]},
  {"type":"function","name":"positions","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[
    {"name":"nonce","type":"uint96"},{"name":"operator","type":"address"},
    {"name":"token0","type":"address"},{"name":"token1","type":"address"},
    {"name":"fee","type":"uint24"},{"name":"tickLower","type":"int24"},
    {"name":"tickUpper","type":"int24"},{"name":"liquidity","type":"uint128"},
    {"name":"feeGrowthInside0LastX128","type":"uint256"},
    {"name":"feeGrowthInside1LastX128","type":"uint256"},
    {"name":"tokensOwed0","type":"uint128"},{"name":"tokensOwed1","type":"uint128"}
  ]},
  {"type":"function","name":"collect","stateMutability":"nonpayable","inputs":[{"name":"params","type":"tuple","components":[
    {"name":"tokenId","type":"uint256"},{"name":"recipient","type":"address"},
    {"name":"amount0Max","type":"uint128"},{"name":"amount1Max","type":"uint128"}
  ]}],"outputs":[{"name":"amount0","type":"uint256"},{"name":"amount1","type":"uint256"}]}
]`)

var uniswapV3FactoryABI = MustABI(`[
  {"type":"function","name":"getPool","stateMutability":"view","inputs":[
    {"name":"tokenA","type":"address"},{"name":"tokenB","type":"address"},{"name":"fee","type":"uint24"}
  ],"outputs":[{"name":"pool","type":"address"}]}
]`)

var uniswapV3PoolABI = MustABI(`[
  {"type":"function","name":"slot0","stateMutability":"view","inputs":[],"outputs":[
    {"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},
    {"name":"observationIndex","type":"uint16"},{"name":"observationCardinality","type":"uint16"},
    {"name":"observationCardinalityNext","type":"uint16"},{"name":"feeProtocol","type":"uint8"},
    {"name":"unlocked","type":"bool"}
  ]}
]`)

var uniswapV4PositionManagerABI = MustABI(`[
  {"type":"function","name":"ownerOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"owner","type":"address"}]},
  {"type":"function","name":"getPositionLiquidity","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"name":"liquidity","type":"uint128"}]},
  {"type":"function","name":"getPoolAndPositionInfo","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[
    {"name":"poolKey","type":"tuple","components":[
      {"name":"currency0","type":"address"},{"name":"currency1","type":"address"},
      {"name":"fee","type":"uint24"},{"name":"tickSpacing","type":"int24"},
      {"name":"hooks","type":"address"}
    ]},
    {"name":"positionInfo","type":"uint256"}
  ]}
]`)

var uniswapV4StateViewABI = MustABI(`[
  {"type":"function","name":"getSlot0","stateMutability":"view","inputs":[{"name":"poolId","type":"bytes32"}],"outputs":[
    {"name":"sqrtPriceX96","type":"uint160"},{"name":"tick","type":"int24"},
    {"name":"protocolFee","type":"uint24"},{"name":"lpFee","type":"uint24"}
  ]},
  {"type":"function","name":"getPositionInfo","stateMutability":"view","inputs":[
    {"name":"poolId","type":"bytes32"},{"name":"owner","type":"address"},
    {"name":"tickLower","type":"int24"},{"name":"tickUpper","type":"int24"},
    {"name":"salt","type":"bytes32"}
  ],"outputs":[
    {"name":"liquidity","type":"uint128"},{"name":"feeGrowthInside0LastX128","type":"uint256"},
    {"name":"feeGrowthInside1LastX128","type":"uint256"}
  ]},
  {"type":"function","name":"getFeeGrowthInside","stateMutability":"view","inputs":[
    {"name":"poolId","type":"bytes32"},{"name":"tickLower","type":"int24"},
    {"name":"tickUpper","type":"int24"}
  ],"outputs":[
    {"name":"feeGrowthInside0X128","type":"uint256"},{"name":"feeGrowthInside1X128","type":"uint256"}
  ]}
]`)

type uniswapV3Deployment struct {
	Factory common.Address
	Manager common.Address
	Window  deploymentWindow
}

type uniswapV4Deployment struct {
	Manager       common.Address
	StateView     common.Address
	WrappedNative common.Address
	Window        deploymentWindow
}

var uniswapV3Deployments = map[ChainID]uniswapV3Deployment{
	Ethereum: {
		Factory: common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
		Manager: common.HexToAddress("0xC36442b4a4522E871399CD717aBDD847Ab11FE88"),
		Window:  deploymentWindow{ActivationBlock: 12_369_651},
	},
	BSC: {
		Factory: common.HexToAddress("0xdB1d10011AD0Ff90774D0C6Bb92e5C5c8b4461F7"),
		Manager: common.HexToAddress("0x7b8A01B39D58278b5DE7e48c8449c9f4F5170613"),
		Window:  deploymentWindow{ActivationBlock: 26_324_045},
	},
	Base: {
		Factory: common.HexToAddress("0x33128a8fC17869897dcE68Ed026d694621f6FDfD"),
		Manager: common.HexToAddress("0x03a520b32C04BF3bEEf7BEb72E919cf822Ed34f1"),
		Window:  deploymentWindow{ActivationBlock: 1_371_714},
	},
	Arbitrum: {
		Factory: common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
		Manager: common.HexToAddress("0xC36442b4a4522E871399CD717aBDD847Ab11FE88"),
		Window:  deploymentWindow{ActivationBlock: 173},
	},
	Polygon: {
		Factory: common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
		Manager: common.HexToAddress("0xC36442b4a4522E871399CD717aBDD847Ab11FE88"),
		Window:  deploymentWindow{ActivationBlock: 22_760_586},
	},
	Monad: {
		Factory: common.HexToAddress("0x204faca1764b154221e35c0d20abb3c525710498"),
		Manager: common.HexToAddress("0x7197e214c0b767cfb76fb734ab638e2c192f4e53"),
		Window:  deploymentWindow{ActivationBlock: 29_255_879},
	},
	Plasma: {
		Factory: common.HexToAddress("0xcb2436774C3e191c85056d248EF4260ce5f27A9D"),
		Manager: common.HexToAddress("0x743E03cceB4af2efA3CC76838f6E8B50B63F184c"),
		Window:  deploymentWindow{ActivationBlock: 430_178},
	},
	Avalanche: {
		Factory: common.HexToAddress("0x740b1c1de25031C31FF4fC9A62f554A55cdC1baD"),
		Manager: common.HexToAddress("0x655C406EBFa14EE2006250925e54ec43AD184f8B"),
		Window:  deploymentWindow{ActivationBlock: 27_833_025},
	},
	Optimism: {
		Factory: common.HexToAddress("0x1F98431c8aD98523631AE4a59f267346ea31F984"),
		Manager: common.HexToAddress("0xC36442b4a4522E871399CD717aBDD847Ab11FE88"),
		Window:  deploymentWindow{ActivationBlock: 0},
	},
}

var uniswapV4Deployments = map[ChainID]uniswapV4Deployment{
	Ethereum: {
		Manager:       common.HexToAddress("0xbd216513d74c8cf14cf4747e6aaa6420ff64ee9e"),
		StateView:     common.HexToAddress("0x7ffe42c4a5deea5b0fec41c94c136cf115597227"),
		WrappedNative: common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
		Window:        deploymentWindow{ActivationBlock: 21_689_089},
	},
	BSC: {
		Manager:       common.HexToAddress("0x7a4a5c919ae2541aed11041a1aeee68f1287f95b"),
		StateView:     common.HexToAddress("0xd13dd3d6e93f276fafc9db9e6bb47c1180aee0c4"),
		WrappedNative: common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c"),
		Window:        deploymentWindow{ActivationBlock: 45_970_613},
	},
	Base: {
		Manager:       common.HexToAddress("0x7c5f5a4bbd8fd63184577525326123b519429bdc"),
		StateView:     common.HexToAddress("0xa3c0c9b65bad0b08107aa264b0f3db444b867a71"),
		WrappedNative: common.HexToAddress("0x4200000000000000000000000000000000000006"),
		Window:        deploymentWindow{ActivationBlock: 25_350_993},
	},
	Arbitrum: {
		Manager:       common.HexToAddress("0xd88f38f930b7952f2db2432cb002e7abbf3dd869"),
		StateView:     common.HexToAddress("0x76fd297e2d437cd7f76d50f01afe6160f86e9990"),
		WrappedNative: common.HexToAddress("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1"),
		Window:        deploymentWindow{ActivationBlock: 297_842_893},
	},
	Polygon: {
		Manager:       common.HexToAddress("0x1ec2ebf4f37e7363fdfe3551602425af0b3ceef9"),
		StateView:     common.HexToAddress("0x5ea1bd7974c8a611cbab0bdcafcb1d9cc9b3ba5a"),
		WrappedNative: common.HexToAddress("0x0d500B1d8E8eF31E21C99d1Db9A6444d3ADf1270"),
		Window:        deploymentWindow{ActivationBlock: 66_980_399},
	},
	Monad: {
		Manager:       common.HexToAddress("0x5b7ec4a94ff9bedb700fb82ab09d5846972f4016"),
		StateView:     common.HexToAddress("0x77395f3b2e73ae90843717371294fa97cc419d64"),
		WrappedNative: common.HexToAddress("0x3bd359C1119dA7Da1D913D1C4D2B7c461115433A"),
		Window:        deploymentWindow{ActivationBlock: 29_255_924},
	},
	Avalanche: {
		Manager:       common.HexToAddress("0xB74b1F14d2754AcfcbBe1a221023a5cf50Ab8ACD"),
		StateView:     common.HexToAddress("0xc3c9e198c735a4b97e3e683f391ccbdd60b69286"),
		WrappedNative: common.HexToAddress("0xB31f66AA3C1e785363F0875A1B74E27b85FD66c7"),
		Window:        deploymentWindow{ActivationBlock: 56_195_389},
	},
	Optimism: {
		Manager:       common.HexToAddress("0x3c3ea4b57a46241e54610e5f022e5c45859a1017"),
		StateView:     common.HexToAddress("0xc18a3169788f4f75a170290584eca6395c75ecdb"),
		WrappedNative: common.HexToAddress("0x4200000000000000000000000000000000000006"),
		Window:        deploymentWindow{ActivationBlock: 130_947_685},
	},
}

type UniswapV3Adapter struct {
	adapterBase
	indexer *uniswapIndexer
}

type UniswapV4Adapter struct {
	adapterBase
	indexer *uniswapIndexer
}

func newUniswapAdapters(
	v3Config SentioIndexerConfig,
	v4Config SentioIndexerConfig,
) []Adapter {
	indexer := newUniswapIndexer(v3Config, v4Config)
	return []Adapter{
		&UniswapV3Adapter{
			adapterBase: adapterBase{info: ProtocolInfo{
				ID: "uniswap-v3", Name: "Uniswap V3", Chains: deploymentChains(uniswapV3Deployments),
			}},
			indexer: indexer,
		},
		&UniswapV4Adapter{
			adapterBase: adapterBase{info: ProtocolInfo{
				ID: "uniswap-v4", Name: "Uniswap V4", Chains: deploymentChains(uniswapV4Deployments),
			}},
			indexer: indexer,
		},
	}
}

// applyUniswapV3Details folds the interleaved slot0/collect batch into the owned
// positions, classifying per-position failures: a defunct pool (slot0 reverts) drops
// only its own position, and a reverting collect simulation zeroes the unreadable fees
// while keeping the principal — one frozen or malicious pool asset must not drop the
// account's whole Uniswap surface. Transport-level errors still fail the scan.
// Extracted so the unit rules are testable without a live RPC client.
func applyUniswapV3Details(
	owned []uniswapV3Position,
	details []ContractCallResult,
) ([]uniswapV3Position, error) {
	if len(details) != len(owned)*2 {
		return nil, fmt.Errorf(
			"position details: got %d results for %d positions", len(details), len(owned),
		)
	}
	detailed := make([]uniswapV3Position, 0, len(owned))
	for index := range owned {
		slotRow, collectRow := details[index*2], details[index*2+1]
		if slotRow.Error != nil {
			if executionReverted(slotRow.Error) {
				continue
			}
			return nil, fmt.Errorf("slot0 %s: %w", owned[index].Pool, slotRow.Error)
		}
		var err error
		owned[index].SqrtPrice, err = BigIntAt(slotRow.Values, 0)
		if err != nil {
			return nil, err
		}
		if collectRow.Error != nil {
			if !executionReverted(collectRow.Error) {
				return nil, fmt.Errorf(
					"collect %s: %w", owned[index].NFT.TokenID, collectRow.Error,
				)
			}
			owned[index].Collectible0 = new(big.Int)
			owned[index].Collectible1 = new(big.Int)
		} else {
			owned[index].Collectible0, err = BigIntAt(collectRow.Values, 0)
			if err != nil {
				return nil, err
			}
			owned[index].Collectible1, err = BigIntAt(collectRow.Values, 1)
			if err != nil {
				return nil, err
			}
		}
		detailed = append(detailed, owned[index])
	}
	return detailed, nil
}

func executionReverted(err error) bool {
	if err == nil {
		return false
	}
	var rpcError rpc.Error
	if errors.As(err, &rpcError) && rpcError.ErrorCode() == 3 {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "execution reverted") ||
		strings.Contains(message, "nonexistent token") ||
		strings.Contains(message, "invalid token id")
}

func uniswapInt32At(values []any, index int) (int32, error) {
	value, err := BigIntAt(values, index)
	if err != nil {
		return 0, err
	}
	if !value.IsInt64() || value.Int64() < -1<<23 || value.Int64() >= 1<<23 {
		return 0, fmt.Errorf("result %d is outside int24", index)
	}
	return int32(value.Int64()), nil
}

func uniswapUint32At(values []any, index int) (uint32, error) {
	value, err := BigIntAt(values, index)
	if err != nil {
		return 0, err
	}
	if !value.IsUint64() || value.Uint64() >= 1<<24 {
		return 0, fmt.Errorf("result %d is outside uint24", index)
	}
	return uint32(value.Uint64()), nil
}

func uniswapFeeLabel(fee uint32) string {
	return strconv.FormatFloat(float64(fee)/10_000, 'f', -1, 64) + "%"
}

func tokenMetadataAt(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	addresses []common.Address,
) (map[common.Address]Token, error) {
	unique := make([]common.Address, 0, len(addresses))
	seen := make(map[common.Address]struct{})
	for _, address := range addresses {
		if _, exists := seen[address]; exists {
			continue
		}
		seen[address] = struct{}{}
		unique = append(unique, address)
	}
	calls := make([]ContractCall, 0, len(unique)*2)
	for _, address := range unique {
		calls = append(calls,
			ContractCall{Contract: address, ABI: erc20ABI, Method: "symbol"},
			ContractCall{Contract: address, ABI: erc20ABI, Method: "decimals"},
		)
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	tokens := make(map[common.Address]Token, len(unique))
	fallbackAddresses := make([]common.Address, 0)
	for index, address := range unique {
		decimalsRow := rows[index*2+1]
		if decimalsRow.Error != nil {
			return nil, fmt.Errorf("%s decimals: %w", address, decimalsRow.Error)
		}
		decimals, decodeErr := Uint8At(decimalsRow.Values, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s decimals: %w", address, decodeErr)
		}
		symbol := ""
		if rows[index*2].Error == nil {
			symbol, _ = StringAt(rows[index*2].Values, 0)
		}
		tokens[address] = Token{
			ChainID: block.ChainID, Address: address, Symbol: symbol, Decimals: decimals,
		}
		if symbol == "" {
			fallbackAddresses = append(fallbackAddresses, address)
		}
	}
	if len(fallbackAddresses) == 0 {
		return tokens, nil
	}
	fallbackCalls := make([]ContractCall, len(fallbackAddresses))
	for index, address := range fallbackAddresses {
		fallbackCalls[index] = ContractCall{
			Contract: address, ABI: erc20Bytes32SymbolABI, Method: "symbol",
		}
	}
	fallbackRows, err := client.ParallelCalls(ctx, block, fallbackCalls)
	if err != nil {
		return nil, fmt.Errorf("token symbol fallback: %w", err)
	}
	for index, address := range fallbackAddresses {
		symbol, decodeErr := Bytes32StringAt(fallbackRows[index], 0)
		if decodeErr != nil || symbol == "" {
			return nil, fmt.Errorf("%s bytes32 symbol is invalid", address)
		}
		token := tokens[address]
		token.Symbol = symbol
		tokens[address] = token
	}
	return tokens, nil
}

type uniswapV3Position struct {
	NFT          uniswapIndexedNFT
	Token0       common.Address
	Token1       common.Address
	Fee          uint32
	TickLower    int32
	TickUpper    int32
	Liquidity    *big.Int
	TokensOwed0  *big.Int
	TokensOwed1  *big.Int
	Pool         common.Address
	SqrtPrice    *big.Int
	Collectible0 *big.Int
	Collectible1 *big.Int
}

func decodeUniswapV3Position(nft uniswapIndexedNFT, values []any) (uniswapV3Position, error) {
	token0, err := AddressAt(values, 2)
	if err != nil {
		return uniswapV3Position{}, err
	}
	token1, err := AddressAt(values, 3)
	if err != nil {
		return uniswapV3Position{}, err
	}
	fee, err := uniswapUint32At(values, 4)
	if err != nil {
		return uniswapV3Position{}, err
	}
	tickLower, err := uniswapInt32At(values, 5)
	if err != nil {
		return uniswapV3Position{}, err
	}
	tickUpper, err := uniswapInt32At(values, 6)
	if err != nil {
		return uniswapV3Position{}, err
	}
	liquidity, err := BigIntAt(values, 7)
	if err != nil {
		return uniswapV3Position{}, err
	}
	owed0, err := BigIntAt(values, 10)
	if err != nil {
		return uniswapV3Position{}, err
	}
	owed1, err := BigIntAt(values, 11)
	if err != nil {
		return uniswapV3Position{}, err
	}
	return uniswapV3Position{
		NFT: nft, Token0: token0, Token1: token1, Fee: fee,
		TickLower: tickLower, TickUpper: tickUpper, Liquidity: liquidity,
		TokensOwed0: owed0, TokensOwed1: owed1,
	}, nil
}

// enumerateUniswapV3NFTs is an independent, complete discovery path when the
// indexer is unavailable. NFPM implements IERC721Enumerable. Every read uses
// the settled block; no latest-state quantities or indexer rows are mixed in.
// Do not apply this to V4, whose position manager is not enumerable.
func enumerateUniswapV3NFTs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	manager common.Address,
) ([]uniswapIndexedNFT, error) {
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: manager, ABI: uniswapV3PositionManagerABI, Method: "supportsInterface", Args: []any{[4]byte{0x78, 0x0e, 0x9d, 0x63}}},
		{Contract: manager, ABI: uniswapV3PositionManagerABI, Method: "balanceOf", Args: []any{account}},
	})
	if err != nil {
		return nil, fmt.Errorf("NFT enumeration capability/balance: %w", err)
	}
	enumerable, err := BoolAt(rows[0], 0)
	if err != nil || !enumerable {
		return nil, fmt.Errorf("Uniswap v3 manager does not support ERC721Enumerable")
	}
	count, err := BigIntAt(rows[1], 0)
	if err != nil {
		return nil, fmt.Errorf("NFT balance: %w", err)
	}
	maximum := uniswapIndexerDefinitions[uniswapV3].maxIndexedNFTs
	if !count.IsUint64() || count.Uint64() > uint64(maximum) {
		return nil, fmt.Errorf("account has %s Uniswap v3 NFTs, maximum is %d", count, maximum)
	}
	calls := make([]ContractCall, int(count.Uint64()))
	for index := range calls {
		calls[index] = ContractCall{
			Contract: manager, ABI: uniswapV3PositionManagerABI, Method: "tokenOfOwnerByIndex",
			Args: []any{account, new(big.Int).SetUint64(uint64(index))},
		}
	}
	rows, err = client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("NFT enumeration: %w", err)
	}
	nfts := make([]uniswapIndexedNFT, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for index, row := range rows {
		id, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil || id.Sign() <= 0 {
			return nil, fmt.Errorf("NFT index %d returned an invalid ID", index)
		}
		if _, duplicate := seen[id.String()]; duplicate {
			return nil, fmt.Errorf("NFT enumeration returned duplicate ID %s", id)
		}
		seen[id.String()] = struct{}{}
		nfts[index] = uniswapIndexedNFT{TokenID: id, Manager: manager}
		calls[index] = ContractCall{
			Contract: manager, ABI: uniswapV3PositionManagerABI, Method: "ownerOf", Args: []any{id},
		}
	}
	// Validate all IDs, including closed positions, before treating the inventory
	// as complete. A bad/missing result must never become a partial success.
	rows, err = client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("enumerated NFT owners: %w", err)
	}
	for index, row := range rows {
		owner, decodeErr := AddressAt(row, 0)
		if decodeErr != nil || owner != account {
			return nil, fmt.Errorf("enumerated NFT %s has an invalid owner", nfts[index].TokenID)
		}
	}
	return nfts, nil
}

func (a *UniswapV3Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := uniswapV3Deployments[block.ChainID]
	if !exists || !deployment.Window.ActiveAt(block.Number) {
		return nil, nil
	}
	indexed, err := a.indexer.indexedNFTs(ctx, uniswapV3, block, account)
	enumerated := false
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		nfts, enumerationErr := enumerateUniswapV3NFTs(ctx, client, block, account, deployment.Manager)
		if enumerationErr != nil {
			return nil, fmt.Errorf("Uniswap v3 indexer: %w; RPC discovery: %w", err, enumerationErr)
		}
		indexed = uniswapIndexedNFTs{NFTs: nfts}
		enumerated = true
	}
	positionCalls := make([]ContractCall, len(indexed.NFTs))
	for index, nft := range indexed.NFTs {
		if nft.Manager != deployment.Manager {
			return nil, fmt.Errorf("indexer returned foreign manager %s", nft.Manager)
		}
		positionCalls[index] = ContractCall{
			Contract: deployment.Manager, ABI: uniswapV3PositionManagerABI,
			Method: "positions", Args: []any{nft.TokenID},
		}
	}
	positionRows, err := client.ParallelCallsAllowFailure(ctx, block, positionCalls)
	if err != nil {
		return nil, fmt.Errorf("positions: %w", err)
	}
	positions := make([]uniswapV3Position, 0)
	for index, row := range positionRows {
		if row.Error != nil {
			if !enumerated && executionReverted(row.Error) {
				continue
			}
			return nil, fmt.Errorf("position %s: %w", indexed.NFTs[index].TokenID, row.Error)
		}
		position, decodeErr := decodeUniswapV3Position(indexed.NFTs[index], row.Values)
		if decodeErr != nil {
			return nil, fmt.Errorf("position %s: %w", indexed.NFTs[index].TokenID, decodeErr)
		}
		if position.Liquidity.Sign() == 0 &&
			position.TokensOwed0.Sign() == 0 && position.TokensOwed1.Sign() == 0 {
			continue
		}
		positions = append(positions, position)
	}
	if len(positions) > uniswapV3MaxActivePositions {
		return nil, fmt.Errorf(
			"account has more than %d economically active Uniswap v3 positions",
			uniswapV3MaxActivePositions,
		)
	}
	ownerCalls := make([]ContractCall, len(positions))
	for index, position := range positions {
		ownerCalls[index] = ContractCall{
			Contract: deployment.Manager, ABI: uniswapV3PositionManagerABI,
			Method: "ownerOf", Args: []any{position.NFT.TokenID},
		}
	}
	ownerRows, err := client.ParallelCallsAllowFailure(ctx, block, ownerCalls)
	if err != nil {
		return nil, fmt.Errorf("owners: %w", err)
	}
	owned := make([]uniswapV3Position, 0, len(positions))
	for index, row := range ownerRows {
		if row.Error != nil {
			if !enumerated && executionReverted(row.Error) {
				continue
			}
			return nil, fmt.Errorf("ownerOf %s: %w", positions[index].NFT.TokenID, row.Error)
		}
		owner, decodeErr := AddressAt(row.Values, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if owner == account {
			owned = append(owned, positions[index])
		} else if enumerated {
			return nil, fmt.Errorf("enumerated NFT %s changed owner at the pinned block", positions[index].NFT.TokenID)
		}
	}
	if len(owned) == 0 {
		return nil, nil
	}

	poolCalls := make([]ContractCall, len(owned))
	for index, position := range owned {
		poolCalls[index] = ContractCall{
			Contract: deployment.Factory, ABI: uniswapV3FactoryABI, Method: "getPool",
			Args: []any{position.Token0, position.Token1, new(big.Int).SetUint64(uint64(position.Fee))},
		}
	}
	poolRows, err := client.ParallelCalls(ctx, block, poolCalls)
	if err != nil {
		return nil, fmt.Errorf("pools: %w", err)
	}
	detailCalls := make([]ContractCall, 0, len(owned)*2)
	for index := range owned {
		pool, decodeErr := AddressAt(poolRows[index], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if pool == (common.Address{}) {
			return nil, fmt.Errorf("position %s resolves to no pool", owned[index].NFT.TokenID)
		}
		owned[index].Pool = pool
		detailCalls = append(detailCalls,
			ContractCall{Contract: pool, ABI: uniswapV3PoolABI, Method: "slot0"},
			ContractCall{
				Contract: deployment.Manager, From: account, ABI: uniswapV3PositionManagerABI,
				Method: "collect", Args: []any{struct {
					TokenId    *big.Int
					Recipient  common.Address
					Amount0Max *big.Int
					Amount1Max *big.Int
				}{
					TokenId: owned[index].NFT.TokenID, Recipient: account,
					Amount0Max: uniswapMaxUint128, Amount1Max: uniswapMaxUint128,
				}},
			},
		)
	}
	details, err := client.ParallelCallsAllowFailure(ctx, block, detailCalls)
	if err != nil {
		return nil, fmt.Errorf("position details: %w", err)
	}
	detailed, err := applyUniswapV3Details(owned, details)
	if err != nil {
		return nil, err
	}
	tokenAddresses := make([]common.Address, 0, len(detailed)*2)
	for _, position := range detailed {
		tokenAddresses = append(tokenAddresses, position.Token0, position.Token1)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, tokenAddresses)
	if err != nil {
		return nil, fmt.Errorf("token metadata: %w", err)
	}
	groups := make([]Group, 0, len(detailed))
	for _, position := range detailed {
		principal0, principal1, mathErr := uniswapAmountsForLiquidity(
			position.SqrtPrice,
			position.TickLower,
			position.TickUpper,
			position.Liquidity,
		)
		if mathErr != nil {
			return nil, mathErr
		}
		if principal0.Sign() == 0 && principal1.Sign() == 0 &&
			position.Collectible0.Sign() == 0 && position.Collectible1.Sign() == 0 {
			continue
		}
		token0 := tokens[position.Token0]
		token1 := tokens[position.Token1]
		components := make([]Component, 0, 4)
		if principal0.Sign() > 0 {
			component := NewComponent("asset", token0, principal0, Source{
				Contract: deployment.Manager, Method: "positions/slot0",
			})
			component.Metadata = map[string]any{"role": "principal"}
			components = append(components, component)
		}
		if principal1.Sign() > 0 {
			component := NewComponent("asset", token1, principal1, Source{
				Contract: deployment.Manager, Method: "positions/slot0",
			})
			component.Metadata = map[string]any{"role": "principal"}
			components = append(components, component)
		}
		if position.Collectible0.Sign() > 0 {
			component := NewComponent("reward", token0, position.Collectible0, Source{
				Contract: deployment.Manager, Method: "collect(eth_call)",
			})
			component.Metadata = map[string]any{"role": "uncollected-fee"}
			components = append(components, component)
		}
		if position.Collectible1.Sign() > 0 {
			component := NewComponent("reward", token1, position.Collectible1, Source{
				Contract: deployment.Manager, Method: "collect(eth_call)",
			})
			component.Metadata = map[string]any{"role": "uncollected-fee"}
			components = append(components, component)
		}
		groups = append(groups, Group{
			ID:         "nft:" + position.NFT.TokenID.String(),
			Label:      fmt.Sprintf("%s / %s · %s", token0.Symbol, token1.Symbol, uniswapFeeLabel(position.Fee)),
			Components: components,
			Metadata: map[string]any{
				"tokenId": position.NFT.TokenID.String(), "pool": position.Pool.Hex(),
				"fee": position.Fee, "tickLower": position.TickLower,
				"tickUpper": position.TickUpper, "liquidity": position.Liquidity.String(),
				"indexerBlock": indexed.CheckpointBlock,
			},
		})
		if enumerated {
			metadata := groups[len(groups)-1].Metadata
			delete(metadata, "indexerBlock")
			metadata["discoverySource"] = "rpc-enumeration"
			metadata["discoveryBlock"] = block.Number
		}
	}
	return groups, nil
}

type uniswapV4PoolKey struct {
	Currency0   common.Address
	Currency1   common.Address
	Fee         *big.Int
	TickSpacing *big.Int
	Hooks       common.Address
}

type uniswapV4Position struct {
	NFT              uniswapIndexedNFT
	PoolKey          uniswapV4PoolKey
	PoolID           common.Hash
	TickLower        int32
	TickUpper        int32
	ManagerLiquidity *big.Int
	SqrtPrice        *big.Int
	LPFee            uint32
	DynamicFee       bool
	Liquidity        *big.Int
	LastGrowth0      *big.Int
	LastGrowth1      *big.Int
	CurrentGrowth0   *big.Int
	CurrentGrowth1   *big.Int
}

var uniswapV4PoolKeyArguments = func() abi.Arguments {
	tuple, err := abi.NewType("tuple", "", []abi.ArgumentMarshaling{
		{Name: "currency0", Type: "address"},
		{Name: "currency1", Type: "address"},
		{Name: "fee", Type: "uint24"},
		{Name: "tickSpacing", Type: "int24"},
		{Name: "hooks", Type: "address"},
	})
	if err != nil {
		panic(err)
	}
	return abi.Arguments{{Type: tuple}}
}()

func decodeUniswapV4PoolKey(value any) (uniswapV4PoolKey, error) {
	converted := abi.ConvertType(value, new(uniswapV4PoolKey))
	poolKey, ok := converted.(*uniswapV4PoolKey)
	if !ok || poolKey == nil || poolKey.Fee == nil || poolKey.TickSpacing == nil {
		return uniswapV4PoolKey{}, fmt.Errorf("unexpected pool key type %T", value)
	}
	return *poolKey, nil
}

func uniswapV4PoolID(poolKey uniswapV4PoolKey) (common.Hash, error) {
	encoded, err := uniswapV4PoolKeyArguments.Pack(poolKey)
	if err != nil {
		return common.Hash{}, err
	}
	return crypto.Keccak256Hash(encoded), nil
}

func uniswapV4IsDynamicFee(value *big.Int) (bool, error) {
	if value == nil || !value.IsUint64() || value.Uint64() >= 1<<24 {
		return false, fmt.Errorf("pool key fee is outside uint24")
	}
	return uint32(value.Uint64()) == uniswapV4DynamicFeeFlag, nil
}

func uniswapV4LPFeeAt(values []any) (uint32, error) {
	fee, err := uniswapUint32At(values, 3)
	if err != nil {
		return 0, fmt.Errorf("slot0 lp fee: %w", err)
	}
	if fee > uniswapV4MaxLPFee {
		return 0, fmt.Errorf("slot0 lp fee %d exceeds maximum %d", fee, uniswapV4MaxLPFee)
	}
	return fee, nil
}

func (a *UniswapV4Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	deployment, exists := uniswapV4Deployments[block.ChainID]
	if !exists || !deployment.Window.ActiveAt(block.Number) {
		return nil, nil
	}
	indexed, err := a.indexer.indexedNFTs(ctx, uniswapV4, block, account)
	if err != nil {
		return nil, err
	}
	ownerCalls := make([]ContractCall, len(indexed.NFTs))
	for index, nft := range indexed.NFTs {
		if nft.Manager != deployment.Manager {
			return nil, fmt.Errorf("indexer returned foreign manager %s", nft.Manager)
		}
		ownerCalls[index] = ContractCall{
			Contract: deployment.Manager, ABI: uniswapV4PositionManagerABI,
			Method: "ownerOf", Args: []any{nft.TokenID},
		}
	}
	ownerRows, err := client.ParallelCallsAllowFailure(ctx, block, ownerCalls)
	if err != nil {
		return nil, fmt.Errorf("owners: %w", err)
	}
	positions := make([]uniswapV4Position, 0)
	for index, row := range ownerRows {
		if row.Error != nil {
			if executionReverted(row.Error) {
				continue
			}
			return nil, fmt.Errorf("ownerOf %s: %w", indexed.NFTs[index].TokenID, row.Error)
		}
		owner, decodeErr := AddressAt(row.Values, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if owner == account {
			positions = append(positions, uniswapV4Position{NFT: indexed.NFTs[index]})
		}
	}
	if len(positions) == 0 {
		return nil, nil
	}
	managerCalls := make([]ContractCall, 0, len(positions)*2)
	for _, position := range positions {
		managerCalls = append(managerCalls,
			ContractCall{
				Contract: deployment.Manager, ABI: uniswapV4PositionManagerABI,
				Method: "getPoolAndPositionInfo", Args: []any{position.NFT.TokenID},
			},
			ContractCall{
				Contract: deployment.Manager, ABI: uniswapV4PositionManagerABI,
				Method: "getPositionLiquidity", Args: []any{position.NFT.TokenID},
			},
		)
	}
	managerRows, err := client.ParallelCalls(ctx, block, managerCalls)
	if err != nil {
		return nil, fmt.Errorf("position manager: %w", err)
	}
	stateCalls := make([]ContractCall, 0, len(positions)*3)
	for index := range positions {
		poolKey, decodeErr := decodeUniswapV4PoolKey(managerRows[index*2][0])
		if decodeErr != nil {
			return nil, decodeErr
		}
		packedInfo, decodeErr := BigIntAt(managerRows[index*2], 1)
		if decodeErr != nil {
			return nil, decodeErr
		}
		managerLiquidity, decodeErr := BigIntAt(managerRows[index*2+1], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		poolID, encodeErr := uniswapV4PoolID(poolKey)
		if encodeErr != nil {
			return nil, fmt.Errorf("pool ID: %w", encodeErr)
		}
		positions[index].PoolKey = poolKey
		positions[index].PoolID = poolID
		positions[index].DynamicFee, decodeErr = uniswapV4IsDynamicFee(poolKey.Fee)
		if decodeErr != nil {
			return nil, decodeErr
		}
		positions[index].TickLower = uniswapDecodePackedInt24(packedInfo, 8)
		positions[index].TickUpper = uniswapDecodePackedInt24(packedInfo, 32)
		positions[index].ManagerLiquidity = managerLiquidity
		poolIDArg := [32]byte(poolID)
		salt := [32]byte(common.BigToHash(positions[index].NFT.TokenID))
		lower := big.NewInt(int64(positions[index].TickLower))
		upper := big.NewInt(int64(positions[index].TickUpper))
		stateCalls = append(stateCalls,
			ContractCall{
				Contract: deployment.StateView, ABI: uniswapV4StateViewABI,
				Method: "getSlot0", Args: []any{poolIDArg},
			},
			ContractCall{
				Contract: deployment.StateView, ABI: uniswapV4StateViewABI,
				Method: "getPositionInfo",
				Args:   []any{poolIDArg, deployment.Manager, lower, upper, salt},
			},
			ContractCall{
				Contract: deployment.StateView, ABI: uniswapV4StateViewABI,
				Method: "getFeeGrowthInside", Args: []any{poolIDArg, lower, upper},
			},
		)
	}
	stateRows, err := client.ParallelCalls(ctx, block, stateCalls)
	if err != nil {
		return nil, fmt.Errorf("state view: %w", err)
	}
	tokenAddresses := make([]common.Address, 0, len(positions)*2)
	for index := range positions {
		positions[index].SqrtPrice, err = BigIntAt(stateRows[index*3], 0)
		if err != nil {
			return nil, err
		}
		positions[index].LPFee, err = uniswapV4LPFeeAt(stateRows[index*3])
		if err != nil {
			return nil, err
		}
		positions[index].Liquidity, err = BigIntAt(stateRows[index*3+1], 0)
		if err != nil {
			return nil, err
		}
		positions[index].LastGrowth0, err = BigIntAt(stateRows[index*3+1], 1)
		if err != nil {
			return nil, err
		}
		positions[index].LastGrowth1, err = BigIntAt(stateRows[index*3+1], 2)
		if err != nil {
			return nil, err
		}
		positions[index].CurrentGrowth0, err = BigIntAt(stateRows[index*3+2], 0)
		if err != nil {
			return nil, err
		}
		positions[index].CurrentGrowth1, err = BigIntAt(stateRows[index*3+2], 1)
		if err != nil {
			return nil, err
		}
		if positions[index].Liquidity.Cmp(positions[index].ManagerLiquidity) != 0 {
			return nil, fmt.Errorf(
				"position %s manager and StateView liquidity disagree",
				positions[index].NFT.TokenID,
			)
		}
		currency0 := positions[index].PoolKey.Currency0
		currency1 := positions[index].PoolKey.Currency1
		if currency0 == (common.Address{}) {
			currency0 = deployment.WrappedNative
		}
		if currency1 == (common.Address{}) {
			currency1 = deployment.WrappedNative
		}
		tokenAddresses = append(tokenAddresses, currency0, currency1)
	}
	tokens, err := tokenMetadataAt(ctx, client, block, tokenAddresses)
	if err != nil {
		return nil, fmt.Errorf("token metadata: %w", err)
	}
	groups := make([]Group, 0, len(positions))
	for _, position := range positions {
		principal0, principal1, mathErr := uniswapAmountsForLiquidity(
			position.SqrtPrice,
			position.TickLower,
			position.TickUpper,
			position.Liquidity,
		)
		if mathErr != nil {
			return nil, mathErr
		}
		fee0 := uniswapFeesFromGrowth(position.Liquidity, position.CurrentGrowth0, position.LastGrowth0)
		fee1 := uniswapFeesFromGrowth(position.Liquidity, position.CurrentGrowth1, position.LastGrowth1)
		if principal0.Sign() == 0 && principal1.Sign() == 0 && fee0.Sign() == 0 && fee1.Sign() == 0 {
			continue
		}
		currency0 := position.PoolKey.Currency0
		currency1 := position.PoolKey.Currency1
		token0Address := currency0
		token1Address := currency1
		if token0Address == (common.Address{}) {
			token0Address = deployment.WrappedNative
		}
		if token1Address == (common.Address{}) {
			token1Address = deployment.WrappedNative
		}
		token0 := tokens[token0Address]
		token1 := tokens[token1Address]
		components := make([]Component, 0, 4)
		appendComponent := func(kind string, token Token, amount *big.Int, role string, native bool, method string) {
			if amount.Sign() == 0 {
				return
			}
			component := NewComponent(kind, token, amount, Source{
				Contract: deployment.StateView, Method: method,
			})
			component.Metadata = map[string]any{"role": role, "nativeCurrency": native}
			components = append(components, component)
		}
		appendComponent("asset", token0, principal0, "principal", currency0 == (common.Address{}), "getSlot0/getPositionInfo")
		appendComponent("asset", token1, principal1, "principal", currency1 == (common.Address{}), "getSlot0/getPositionInfo")
		appendComponent("reward", token0, fee0, "uncollected-fee", currency0 == (common.Address{}), "getFeeGrowthInside/getPositionInfo")
		appendComponent("reward", token1, fee1, "uncollected-fee", currency1 == (common.Address{}), "getFeeGrowthInside/getPositionInfo")
		groups = append(groups, Group{
			ID:         "nft:" + position.NFT.TokenID.String(),
			Label:      fmt.Sprintf("%s / %s · %s", token0.Symbol, token1.Symbol, uniswapFeeLabel(position.LPFee)),
			Components: components,
			Metadata: map[string]any{
				"tokenId": position.NFT.TokenID.String(), "poolId": position.PoolID.Hex(),
				"fee": position.LPFee, "dynamicFee": position.DynamicFee,
				"tickSpacing": position.PoolKey.TickSpacing.String(),
				"tickLower":   position.TickLower, "tickUpper": position.TickUpper,
				"hooks": position.PoolKey.Hooks.Hex(), "liquidity": position.Liquidity.String(),
				"indexerBlock": indexed.CheckpointBlock,
			},
		})
	}
	return groups, nil
}
