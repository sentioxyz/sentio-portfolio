package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

type uniswapGeneration string

const (
	uniswapV3 uniswapGeneration = "v3"
	uniswapV4 uniswapGeneration = "v4"
)

type uniswapIndexerDefinition struct {
	version        uniswapGeneration
	maxIndexedNFTs int
}

var uniswapIndexerDefinitions = map[uniswapGeneration]uniswapIndexerDefinition{
	uniswapV3: {
		version:        uniswapV3,
		maxIndexedNFTs: 4_096,
	},
	uniswapV4: {
		version:        uniswapV4,
		maxIndexedNFTs: 512,
	},
}

type uniswapIndexedNFT struct {
	TokenID *big.Int
	Manager common.Address
}

type uniswapIndexedNFTs struct {
	// IndexerBlock is the block the index was read at.
	IndexerBlock uint64
	NFTs         []uniswapIndexedNFT
}

// uniswapIndexer reads the live position NFTs of each Uniswap generation, a project of its own.
type uniswapIndexer struct {
	indexes map[uniswapGeneration]*sentioIndex
}

func newUniswapIndexer(
	v3Config SentioIndexerConfig,
	v4Config SentioIndexerConfig,
) *uniswapIndexer {
	api := newSentioAPIClient()
	indexes := make(map[uniswapGeneration]*sentioIndex, 2)
	for version, config := range map[uniswapGeneration]SentioIndexerConfig{uniswapV3: v3Config, uniswapV4: v4Config} {
		chains, _ := uniswapIndexerChains(version)
		indexes[version] = &sentioIndex{
			name: "uniswap-" + string(version), api: api, config: config, chains: chains, probe: true,
		}
	}
	return &uniswapIndexer{indexes: indexes}
}

func uniswapIndexerChains(version uniswapGeneration) ([]ChainID, error) {
	switch version {
	case uniswapV3:
		return deploymentChains(uniswapV3Deployments), nil
	case uniswapV4:
		return deploymentChains(uniswapV4Deployments), nil
	default:
		return nil, fmt.Errorf("unsupported Uniswap generation %q", version)
	}
}

type uniswapPositionRow struct {
	ChainID int    `json:"chainId"`
	Owner   string `json:"owner"`
	TokenID string `json:"tokenId"`
	Manager string `json:"manager"`
	Balance string `json:"balance"`
}

// uniswapPositionStream reads the position NFTs account holds of one generation. The index keeps
// a row per NFT the account ever held, so only those with a balance are live.
func uniswapPositionStream(definition uniswapIndexerDefinition, account common.Address) *indexStream[uniswapPositionRow, uniswapIndexedNFT] {
	owner := strings.ToLower(account.Hex())
	return &indexStream[uniswapPositionRow, uniswapIndexedNFT]{
		Entity: "positions", Account: account, MaxRows: definition.maxIndexedNFTs,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account), Terms: []indexTerm{{"balance_not", 0}}}
		},
		Decode: func(chainID ChainID, id string, row uniswapPositionRow) (uniswapIndexedNFT, error) {
			expectedManager, exists := uniswapExpectedManager(definition.version, chainID)
			if !exists {
				return uniswapIndexedNFT{}, fmt.Errorf("Uniswap %s has no deployment on chain %d", definition.version, chainID)
			}
			if nft, valid := uniswapLivePosition(chainID, account, owner, expectedManager, id, row); valid {
				return nft, nil
			}
			return uniswapIndexedNFT{}, fmt.Errorf("GraphQL returned malformed position row %q", id)
		},
	}
}

// uniswapLivePosition reads a position row as the live NFT it must be: owner's, of the deployment's
// manager, held with a balance of one, under the id its fields rebuild.
func uniswapLivePosition(
	chainID ChainID,
	account common.Address,
	owner string,
	expectedManager common.Address,
	id string,
	row uniswapPositionRow,
) (uniswapIndexedNFT, bool) {
	if row.ChainID != int(chainID) || row.Owner != owner || !common.IsHexAddress(row.Manager) {
		return uniswapIndexedNFT{}, false
	}
	tokenID, ok := new(big.Int).SetString(row.TokenID, 10)
	if !ok || tokenID.Sign() < 0 || tokenID.BitLen() > 256 || row.TokenID != tokenID.String() {
		return uniswapIndexedNFT{}, false
	}
	manager := common.HexToAddress(row.Manager)
	if row.Manager != strings.ToLower(manager.Hex()) || manager != expectedManager {
		return uniswapIndexedNFT{}, false
	}
	balance, ok := new(big.Int).SetString(row.Balance, 10)
	if !ok || row.Balance != balance.String() || balance.Cmp(big.NewInt(1)) != 0 {
		return uniswapIndexedNFT{}, false
	}
	if id != uniswapPositionRowID(chainID, account, manager, tokenID) {
		return uniswapIndexedNFT{}, false
	}
	return uniswapIndexedNFT{TokenID: tokenID, Manager: manager}, true
}

func uniswapPositionRowID(
	chainID ChainID,
	owner common.Address,
	manager common.Address,
	tokenID *big.Int,
) string {
	return fmt.Sprintf(
		"%d:%s:%s:%s",
		chainID,
		strings.ToLower(owner.Hex()),
		strings.ToLower(manager.Hex()),
		tokenID.String(),
	)
}

func uniswapExpectedManager(
	version uniswapGeneration,
	chainID ChainID,
) (common.Address, bool) {
	switch version {
	case uniswapV3:
		deployment, exists := uniswapV3Deployments[chainID]
		return deployment.Manager, exists
	case uniswapV4:
		deployment, exists := uniswapV4Deployments[chainID]
		return deployment.Manager, exists
	default:
		return common.Address{}, false
	}
}

// prefetchPresence asks every chain of one Uniswap generation at once whether the account holds
// any live position NFT, so a scan queries only the chains where it does.
func (i *uniswapIndexer) prefetchPresence(ctx context.Context, version uniswapGeneration, account common.Address) {
	definition, exists := uniswapIndexerDefinitions[version]
	if !exists {
		return
	}
	i.indexes[version].prefetchPresence(ctx, uniswapPositionStream(definition, account))
}

func (i *uniswapIndexer) indexedNFTs(
	ctx context.Context,
	version uniswapGeneration,
	block BlockRef,
	account common.Address,
) (uniswapIndexedNFTs, error) {
	definition, exists := uniswapIndexerDefinitions[version]
	if !exists {
		return uniswapIndexedNFTs{}, fmt.Errorf("unknown Uniswap generation %q", version)
	}
	positions := uniswapPositionStream(definition, account)
	at, err := i.indexes[version].read(ctx, nil, block, positions)
	if err != nil {
		return uniswapIndexedNFTs{}, fmt.Errorf("Uniswap %s index query: %w", version, err)
	}
	return uniswapIndexedNFTs{IndexerBlock: at.QueryBlock, NFTs: positions.Rows()}, nil
}
