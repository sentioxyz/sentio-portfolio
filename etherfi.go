package portfolio

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

const etherfiWithdrawalActivationBlock = 18_518_781

var (
	etherfiWithdrawalNFT = common.HexToAddress("0x7d5706f6ef3F89B3951E23e557CDFBC3239D4E2c")
	etherfiETH           = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	etherfiWithdrawalABI = MustABI(`[
      {"type":"function","name":"ownerOf","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"type":"address"}]},
      {"type":"function","name":"isFinalized","stateMutability":"view","inputs":[{"name":"requestId","type":"uint256"}],"outputs":[{"type":"bool"}]},
      {
        "type":"function","name":"getRequest","stateMutability":"view",
        "inputs":[{"name":"requestId","type":"uint256"}],
		"outputs":[{"type":"uint96"},{"type":"uint96"},{"type":"bool"},{"type":"uint256"}]
      },
      {"type":"function","name":"getClaimableAmount","stateMutability":"view","inputs":[{"name":"tokenId","type":"uint256"}],"outputs":[{"type":"uint256"}]}
    ]`)
)

type EtherfiAdapter struct {
	adapterBase
	indexer   *ownerTokenIndexer
	receipts  map[ChainID][]convertedBalancePosition
	vaults    map[ChainID][]etherfiVaultPosition
	withdraws map[ChainID][]common.Address
}

func newEtherfiAdapter(config SentioIndexerConfig) Adapter {
	return &EtherfiAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "etherfi", Name: "Ether.fi", Chains: []ChainID{Ethereum, BSC, Base, Arbitrum},
		}},
		indexer: newOwnerTokenIndexer(config, []ChainID{Ethereum}),
		receipts: map[ChainID][]convertedBalancePosition{
			Ethereum: {
				{
					ID: "eeth", Label: "Liquid staking · eETH",
					BalanceContract: common.HexToAddress("0x35fA164735182de50811E8e2E824cFb9B6118ac2"),
					Token:           etherfiETH,
					ActivationBlock: 17_664_324,
				},
				{
					ID: "weeth", Label: "Liquid staking · weETH",
					BalanceContract: common.HexToAddress("0xCd5fE23C85820F7B72D0926FC9b05b43E359b7ee"),
					Converter:       common.HexToAddress("0xCd5fE23C85820F7B72D0926FC9b05b43E359b7ee"),
					Method:          "getEETHByWeETH",
					Token:           etherfiETH,
					ActivationBlock: 17_664_336,
				},
			},
			BSC: {{
				ID: "weeth", Label: "Liquid staking · weETH",
				BalanceContract: common.HexToAddress("0x04C0599Ae5A44757c0af6F9eC3b93da8976c150A"),
				Token:           token(BSC, "0x04C0599Ae5A44757c0af6F9eC3b93da8976c150A", "weETH", 18),
				ActivationBlock: 38_098_558,
			}},
			Base: {{
				ID: "weeth", Label: "Liquid staking · weETH",
				BalanceContract: common.HexToAddress("0x04C0599Ae5A44757c0af6F9eC3b93da8976c150A"),
				Token:           token(Base, "0x04C0599Ae5A44757c0af6F9eC3b93da8976c150A", "weETH", 18),
				ActivationBlock: 13_524_685,
			}},
			Arbitrum: {{
				ID: "weeth", Label: "Liquid staking · weETH",
				BalanceContract: common.HexToAddress("0x35751007a407ca6FEFfE80b3cB397736D2cf4dbe"),
				Token:           token(Arbitrum, "0x35751007a407ca6FEFfE80b3cB397736D2cf4dbe", "weETH", 18),
				ActivationBlock: 156_547_814,
			}},
		},
		vaults: map[ChainID][]etherfiVaultPosition{
			Ethereum: {
				etherfiVault("sethfi", "ETHFI staking", "0x86B5780b606940Eb59A062aA85a07959518c0161", "0x05A1552c5e18F5A0BB9571b5F2D6a4765ebdA32b", 20_265_589),
				etherfiVault("ebtc", "eBTC", "0x657e8C867D8B37dCC18fA4Caead9C45EB088C642", "0x1b293DC39F94157fA0D1D36d7e0090C8B8B8c13F", 20_523_455),
				etherfiVault("eusd", "eUSD", "0x939778D83b46B456224A33Fb59630B11DEC56663", "0xEB440B36f61Bf62E0C54C622944545f159C3B790", 20_693_643),
				etherfiVault("weeths", "Super Symbiotic LRT", "0x917ceE801a67f933F2e6b33fC0cD1ED2d5909D88", "0xbe16605B22a7faCEf247363312121670DFe5afBE", 20_072_943),
				etherfiVault("weethk", "King Karak LRT", "0x7223442cad8e9ca474fc40109ab981608f8c4273", "0x126af21dc55C300B7D0bBfC4F3898F558aE8156b", 20_220_505),
				etherfiVault("liquid-eth", "Liquid ETH vault", "0xf0bb20865277aBd641a307eCe5Ee04E79073416C", "0x0d05D94a5F1E76C18fbeB7A13d17C8a314088198", 20_014_552),
				etherfiVault("liquid-usd", "Liquid USD vault", "0x08c6F91e2B681FaF5e17227F2a44C307b3C1364C", "0xc315D6e14DDCDC7407784e2Caf815d131Bc1D3E7", 19_672_707),
				etherfiVault("liquid-btc", "Liquid BTC vault", "0x5f46d540b6eD704C3c8789105F30E075AA900726", "0xEa23aC6D7D11f6b181d6B98174D334478ADAe6b0", 21_189_184),
				etherfiVault("liquid-elixir", "Liquid Elixir vault", "0x352180974C71f84a934953Cf49C4E538a6F9c997", "0xBae19b38Bf727Be64AF0B578c34985c3D612e2Ba", 20_637_837),
				etherfiVault("liquid-usual", "Liquid Usual vault", "0xeDa663610638E6557c27e2f4e973D3393e844E70", "0x1D4F0F05e50312d3E7B65659Ef7d06aa74651e0C", 20_614_334),
				etherfiVault("liquid-ultrayield", "Liquid UltraYield vault", "0xbc0f3B23930fff9f4894914bD745ABAbA9588265", "0x95fE19b324bE69250138FE8EE50356e9f6d17Cfe", 21_340_787),
				etherfiVault("liquid-bera-btc", "Liquid Bera BTC vault", "0xC673ef7791724f0dcca38adB47Fbb3AEF3DB6C80", "0xF44BD12956a0a87c2C20113DdFe1537A442526B5", 21_514_201),
				etherfiVault("liquid-bera-eth", "Liquid Bera ETH vault", "0x83599937c2C9bEA0E0E8ac096c6f32e86486b410", "0x04B8136820598A4e50bEe21b8b6a23fE25Df9Bd8", 21_514_284),
				etherfiVault("liquid-move-eth", "Liquid Move ETH vault", "0xca8711dAF13D852ED2121E4bE3894Dae366039E4", "0xb53244f7716dC83811C8fB1a91971dC188C1C5aA", 21_636_059),
				etherfiVault("liquid-katana-eth", "Liquid Katana ETH vault", "0x69d210d3b60E939BFA6E87cCcC4fAb7e8F44C16B", "0xFCb9a6bF02C43f9E38Bb102fd960Cc1e738e787d", 22_646_002),
			},
			Base: {
				etherfiVault("sethfi", "ETHFI staking", "0x86B5780b606940Eb59A062aA85a07959518c0161", "0x05A1552c5e18F5A0BB9571b5F2D6a4765ebdA32b", 19_686_015),
				etherfiVault("ebtc", "eBTC", "0x657e8C867D8B37dCC18fA4Caead9C45EB088C642", "0x1b293DC39F94157fA0D1D36d7e0090C8B8B8c13F", 22_113_991),
			},
			Arbitrum: {
				etherfiVault("sethfi", "ETHFI staking", "0x86B5780b606940Eb59A062aA85a07959518c0161", "0x05A1552c5e18F5A0BB9571b5F2D6a4765ebdA32b", 230_459_108),
				etherfiVault("ebtc", "eBTC", "0x657e8C867D8B37dCC18fA4Caead9C45EB088C642", "0x1b293DC39F94157fA0D1D36d7e0090C8B8B8c13F", 282_047_547),
			},
		},
		withdraws: map[ChainID][]common.Address{Ethereum: {etherfiWithdrawalNFT}},
	}
}

func (a *EtherfiAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups, err := readConvertedBalancePositions(ctx, client, block, account, a.receipts[block.ChainID])
	if err != nil {
		return nil, err
	}
	vaultGroups, vaultErr := readEtherfiVaultPositions(ctx, client, block, account, a.vaults[block.ChainID])
	groups = append(groups, vaultGroups...)
	if vaultErr != nil {
		return groups, fmt.Errorf("vault positions: %w", vaultErr)
	}
	memberships, membershipErr := a.readMemberships(ctx, client, block, account)
	groups = append(groups, memberships...)
	if membershipErr != nil {
		return groups, membershipErr
	}
	if block.ChainID != Ethereum || block.Number < etherfiWithdrawalActivationBlock {
		return groups, nil
	}
	refs, err := a.indexer.PositionRefs(ctx, client, block, account, a.withdraws[block.ChainID])
	if err != nil {
		return groups, fmt.Errorf("withdrawal enumeration: %w", err)
	}
	if len(refs) == 0 {
		return groups, nil
	}
	headerCalls := make([]ContractCall, 0, len(refs)*3)
	for _, ref := range refs {
		headerCalls = append(headerCalls,
			ContractCall{Contract: ref.Contract, ABI: etherfiWithdrawalABI, Method: "ownerOf", Args: []any{ref.TokenID}},
			ContractCall{Contract: ref.Contract, ABI: etherfiWithdrawalABI, Method: "getRequest", Args: []any{ref.TokenID}},
			ContractCall{Contract: ref.Contract, ABI: etherfiWithdrawalABI, Method: "isFinalized", Args: []any{ref.TokenID}},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, headerCalls)
	if err != nil {
		return groups, fmt.Errorf("withdrawal state: %w", err)
	}
	finalizedRefs := make([]ownerTokenRef, 0, len(refs))
	finalizedIndex := make(map[string]int)
	type withdrawalState struct {
		ref       ownerTokenRef
		amount    *big.Int
		shares    *big.Int
		valid     bool
		finalized bool
		feeGwei   *big.Int
	}
	states := make([]withdrawalState, 0, len(refs))
	for index, ref := range refs {
		owner, decodeErr := AddressAt(rows[index*3], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("withdrawal %s owner: %w", ref.TokenID, decodeErr)
		}
		if owner != account {
			return groups, fmt.Errorf("withdrawal %s ownership changed at pinned block", ref.TokenID)
		}
		amount, decodeErr := BigIntAt(rows[index*3+1], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("withdrawal %s amount: %w", ref.TokenID, decodeErr)
		}
		shares, decodeErr := BigIntAt(rows[index*3+1], 1)
		if decodeErr != nil {
			return groups, fmt.Errorf("withdrawal %s shares: %w", ref.TokenID, decodeErr)
		}
		valid, decodeErr := BoolAt(rows[index*3+1], 2)
		if decodeErr != nil {
			return groups, fmt.Errorf("withdrawal %s validity: %w", ref.TokenID, decodeErr)
		}
		feeGwei, decodeErr := BigIntAt(rows[index*3+1], 3)
		if decodeErr != nil {
			return groups, fmt.Errorf("withdrawal %s fee: %w", ref.TokenID, decodeErr)
		}
		finalized, decodeErr := BoolAt(rows[index*3+2], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("withdrawal %s finalization: %w", ref.TokenID, decodeErr)
		}
		if finalized {
			finalizedIndex[ownerTokenRefKey(ref.Contract, ref.TokenID)] = len(finalizedRefs)
			finalizedRefs = append(finalizedRefs, ref)
		}
		states = append(states, withdrawalState{
			ref: ref, amount: amount, shares: shares, valid: valid, finalized: finalized, feeGwei: feeGwei,
		})
	}
	claimCalls := make([]ContractCall, 0, len(finalizedRefs))
	for _, ref := range finalizedRefs {
		claimCalls = append(claimCalls, ContractCall{
			Contract: ref.Contract, ABI: etherfiWithdrawalABI,
			Method: "getClaimableAmount", Args: []any{ref.TokenID},
		})
	}
	claimRows, err := client.ParallelCalls(ctx, block, claimCalls)
	if err != nil {
		return groups, fmt.Errorf("finalized withdrawal amounts: %w", err)
	}
	for _, state := range states {
		amount := state.amount
		sourceMethod := "getRequest.amountOfEEth"
		if state.finalized {
			rowIndex := finalizedIndex[ownerTokenRefKey(state.ref.Contract, state.ref.TokenID)]
			amount, err = BigIntAt(claimRows[rowIndex], 0)
			if err != nil {
				return groups, fmt.Errorf("withdrawal %s claimable amount: %w", state.ref.TokenID, err)
			}
			sourceMethod = "getClaimableAmount"
		}
		if amount.Sign() == 0 {
			continue
		}
		component := NewComponent(
			"asset",
			etherfiETH,
			amount,
			Source{Contract: state.ref.Contract, Method: sourceMethod},
		)
		component.Metadata = map[string]any{
			"tokenId": state.ref.TokenID.String(), "shares": state.shares.String(),
			"finalized": state.finalized, "valid": state.valid, "feeGwei": state.feeGwei.String(),
		}
		groups = append(groups, Group{
			ID: "withdrawal:" + state.ref.TokenID.String(), Label: "Withdrawal request",
			Components: []Component{component},
			Metadata:   map[string]any{"requestNFT": state.ref.Contract},
		})
	}
	return groups, nil
}
