package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const asterAsBNBMinterActivation = 44_582_599

const asterRateDecimals = 8

var (
	asterAsBNBMinter  = common.HexToAddress("0x2F31ab8950c50080E77999fa456372f276952fD8")
	asterAsBTCMinter  = common.HexToAddress("0x8a3C77E6c6A488d26CD44F403b95e44675f46e6A")
	asterAsUSDFMinter = common.HexToAddress("0xdB57a53C428a9faFcbFefFB6dd80d0f427543695")
	asterAsCAKEMinter = common.HexToAddress("0x1A81A28482Edd40ff1689CB3D857c3dAdF11D502")
	asterAsBTC        = token(BSC, "0x184b72289c0992BDf96751354680985a7C4825d6", "asBTC", 18)
	asterAsUSDF       = token(BSC, "0x917AF46B3C3c6e1Bb7286B9F59637Fb7C65851Fb", "asUSDF", 18)
	asterAsBNB        = token(BSC, "0x77734e70b6E88b4d82fE632a168EDf6e700912b6", "asBNB", 18)
	asterAsCAKE       = token(BSC, "0x9817F4c9f968a553fF6caEf1a2ef6cF1386F16F7", "asCAKE", 18)
	asterMinterABI    = MustABI(`[
      {"type":"function","name":"token","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"asBnb","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"assToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"convertToTokens","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"tokenMintReqQueue","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"address"},{"type":"uint256"}]}
    ]`)
	asterEarnRateABI = MustABI(`[
      {
        "type":"function","name":"supportAssToken","stateMutability":"view",
        "inputs":[{"type":"address"}],
        "outputs":[{"type":"address"},{"type":"address"},{"type":"uint8"},{"type":"uint256"},{"type":"uint256"},{"type":"bool"},{"type":"bool"},{"type":"address"},{"type":"bool"},{"type":"bool"}]
      }
    ]`)
	asterUSDFRateABI = MustABI(`[
      {"type":"function","name":"USDF","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"asUSDF","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"exchangePrice","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
    ]`)
	asterBTCWithdrawalABI = MustABI(`[
      {
        "type":"function","name":"requestWithdraws","stateMutability":"view",
        "inputs":[{"type":"uint256"}],
        "outputs":[{"type":"address"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"bool"},{"type":"address"},{"type":"bool"}]
      }
    ]`)
	asterUSDFWithdrawalABI = MustABI(`[
      {
        "type":"function","name":"requestWithdraws","stateMutability":"view",
        "inputs":[{"type":"uint256"}],
        "outputs":[{"type":"address"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"bool"},{"type":"address"},{"type":"bool"}]
      }
    ]`)
)

type asterWithdrawalContract struct {
	Address         common.Address
	Receipt         Token
	ActivationBlock uint64
	ABI             abi.ABI
	OwnerIndex      int
}

func asterEarnAssets(shares, rate *big.Int, sourceDecimals uint8) (*big.Int, error) {
	if shares == nil || shares.Sign() < 0 || rate == nil || rate.Sign() <= 0 {
		return nil, fmt.Errorf("Aster share conversion inputs are invalid")
	}
	if sourceDecimals > 77 {
		return nil, fmt.Errorf("Aster source decimals exceed the safety bound")
	}
	sourceScale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(sourceDecimals)), nil)
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(18+asterRateDecimals), nil)
	return new(big.Int).Quo(
		new(big.Int).Mul(new(big.Int).Mul(new(big.Int).Set(shares), rate), sourceScale),
		denominator,
	), nil
}

func (a *AsterAdapter) readYieldPositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != BSC {
		return nil, nil
	}
	type balanceState struct {
		position receiptPosition
		shares   *big.Int
	}
	active := make([]receiptPosition, 0, len(a.receipts))
	calls := make([]ContractCall, 0, len(a.receipts))
	for _, position := range a.receipts {
		if block.Number < position.ActivationBlock {
			continue
		}
		active = append(active, position)
		calls = append(calls, ContractCall{
			Contract: position.Receipt.Address, ABI: erc20ABI, Method: "balanceOf", Args: []any{account},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("yield receipt balances: %w", err)
	}
	nonZero := make([]balanceState, 0, len(rows))
	for index, row := range rows {
		shares, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s balance: %w", active[index].Receipt.Symbol, decodeErr)
		}
		if shares.Sign() > 0 {
			nonZero = append(nonZero, balanceState{position: active[index], shares: shares})
		}
	}
	groups := make([]Group, 0, len(nonZero))
	for _, state := range nonZero {
		var underlyingAddress common.Address
		var amount *big.Int
		var source Source
		switch state.position.Receipt.Address {
		case asterAsBNB.Address:
			header, callErr := client.ParallelCalls(ctx, block, []ContractCall{
				{Contract: asterAsBNBMinter, ABI: asterMinterABI, Method: "token"},
				{Contract: asterAsBNBMinter, ABI: asterMinterABI, Method: "asBnb"},
				{Contract: asterAsBNBMinter, ABI: asterMinterABI, Method: "convertToTokens", Args: []any{state.shares}},
			})
			if callErr != nil {
				return groups, fmt.Errorf("asBNB conversion: %w", callErr)
			}
			underlyingAddress, err = AddressAt(header[0], 0)
			receiptAddress, receiptErr := AddressAt(header[1], 0)
			if err != nil || receiptErr != nil || receiptAddress != asterAsBNB.Address {
				return groups, fmt.Errorf("asBNB minter identity changed")
			}
			amount, err = BigIntAt(header[2], 0)
			source = Source{Contract: asterAsBNBMinter, Method: "convertToTokens(balanceOf)"}
		case asterAsCAKE.Address:
			header, callErr := client.ParallelCalls(ctx, block, []ContractCall{
				{Contract: asterAsCAKEMinter, ABI: asterMinterABI, Method: "token"},
				{Contract: asterAsCAKEMinter, ABI: asterMinterABI, Method: "assToken"},
				{Contract: asterAsCAKEMinter, ABI: asterMinterABI, Method: "convertToTokens", Args: []any{state.shares}},
			})
			if callErr != nil {
				return groups, fmt.Errorf("asCAKE conversion: %w", callErr)
			}
			underlyingAddress, err = AddressAt(header[0], 0)
			receiptAddress, receiptErr := AddressAt(header[1], 0)
			if err != nil || receiptErr != nil || receiptAddress != asterAsCAKE.Address {
				return groups, fmt.Errorf("asCAKE minter identity changed")
			}
			amount, err = BigIntAt(header[2], 0)
			source = Source{Contract: asterAsCAKEMinter, Method: "convertToTokens(balanceOf)"}
		case asterAsBTC.Address:
			header, callErr := client.Call(
				ctx, block, asterAsBTCMinter, asterEarnRateABI, "supportAssToken", asterAsBTC.Address,
			)
			if callErr != nil {
				return groups, fmt.Errorf("asBTC conversion: %w", callErr)
			}
			receiptAddress, receiptErr := AddressAt(header, 0)
			underlyingAddress, err = AddressAt(header, 1)
			decimals, decimalsErr := Uint8At(header, 2)
			rate, rateErr := BigIntAt(header, 3)
			if receiptErr != nil || err != nil || decimalsErr != nil || rateErr != nil ||
				receiptAddress != asterAsBTC.Address {
				return groups, fmt.Errorf("asBTC rate state is invalid")
			}
			amount, err = asterEarnAssets(state.shares, rate, decimals)
			source = Source{Contract: asterAsBTCMinter, Method: "supportAssToken(balanceOf)"}
		case asterAsUSDF.Address:
			header, callErr := client.ParallelCalls(ctx, block, []ContractCall{
				{Contract: asterAsUSDFMinter, ABI: asterUSDFRateABI, Method: "USDF"},
				{Contract: asterAsUSDFMinter, ABI: asterUSDFRateABI, Method: "asUSDF"},
				{Contract: asterAsUSDFMinter, ABI: asterUSDFRateABI, Method: "exchangePrice"},
			})
			if callErr != nil {
				return groups, fmt.Errorf("asUSDF conversion: %w", callErr)
			}
			underlyingAddress, err = AddressAt(header[0], 0)
			receiptAddress, receiptErr := AddressAt(header[1], 0)
			price, priceErr := BigIntAt(header[2], 0)
			if err != nil || receiptErr != nil || priceErr != nil || receiptAddress != asterAsUSDF.Address {
				return groups, fmt.Errorf("asUSDF rate state is invalid")
			}
			amount = new(big.Int).Quo(
				new(big.Int).Mul(new(big.Int).Set(state.shares), price),
				new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil),
			)
			source = Source{Contract: asterAsUSDFMinter, Method: "exchangePrice(balanceOf)"}
		default:
			return groups, fmt.Errorf("unsupported Aster receipt %s", state.position.Receipt.Address)
		}
		if err != nil || amount == nil || amount.Sign() <= 0 || underlyingAddress == (common.Address{}) {
			return groups, fmt.Errorf("%s conversion returned invalid state", state.position.Receipt.Symbol)
		}
		// Every branch names its minter, but what it converted is the receipt balance read above.
		source.Holds = []common.Address{state.position.Receipt.Address}
		// Report the minter's own underlying, whatever it is. asBNB converts through slisBNB,
		// so the amount above is slisBNB-denominated; naming it BNB priced a slisBNB quantity
		// at the BNB price and understated the position by the slisBNB/BNB premium.
		underlying, tokenErr := readERC20Token(ctx, client, block, underlyingAddress)
		if tokenErr != nil {
			return groups, fmt.Errorf("%s underlying token: %w", state.position.Receipt.Symbol, tokenErr)
		}
		component := NewComponent("asset", underlying, amount, source)
		component.Metadata = map[string]any{"receiptRaw": state.shares.String()}
		groups = append(groups, Group{
			ID: state.position.ID, Label: state.position.Label, Components: []Component{component},
		})
	}
	return groups, nil
}

type AsterAdapter struct {
	adapterBase
	receipts    []receiptPosition
	withdrawals []asterWithdrawalContract
	indexer     *accountRequestIndexer
}

func newAsterAdapter(config SentioIndexerConfig) Adapter {
	return &AsterAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "aster", Name: "Aster", Chains: []ChainID{BSC},
		}},
		receipts: []receiptPosition{
			{
				ID: "asbtc", Label: "Yield · asBTC",
				Receipt:         asterAsBTC,
				ActivationBlock: 43_713_424,
			},
			{
				ID: "asbnb", Label: "Liquid staking · asBNB",
				Receipt:         asterAsBNB,
				ActivationBlock: 44_582_599,
			},
			{
				ID: "asusdf", Label: "Yield · asUSDF",
				Receipt:         asterAsUSDF,
				ActivationBlock: 44_197_809,
			},
			{
				ID: "ascake", Label: "Yield · asCAKE",
				Receipt:         asterAsCAKE,
				ActivationBlock: 44_948_158,
			},
		},
		withdrawals: []asterWithdrawalContract{
			{
				Address: asterAsBTCMinter, Receipt: asterAsBTC,
				ActivationBlock: 43_713_424, ABI: asterBTCWithdrawalABI, OwnerIndex: 5,
			},
			{
				Address: asterAsUSDFMinter, Receipt: asterAsUSDF,
				ActivationBlock: 44_197_809, ABI: asterUSDFWithdrawalABI, OwnerIndex: 6,
			},
		},
		indexer: newAccountRequestIndexer(config, []ChainID{BSC}),
	}
}

func (a *AsterAdapter) readWithdrawalRequests(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	active := make([]asterWithdrawalContract, 0, len(a.withdrawals))
	contracts := make([]common.Address, 0, len(a.withdrawals))
	byAddress := make(map[common.Address]asterWithdrawalContract, len(a.withdrawals))
	for _, withdrawal := range a.withdrawals {
		if block.Number < withdrawal.ActivationBlock {
			continue
		}
		active = append(active, withdrawal)
		contracts = append(contracts, withdrawal.Address)
		byAddress[withdrawal.Address] = withdrawal
	}
	if len(active) == 0 {
		return nil, nil
	}
	snapshot, err := a.indexer.IndexedRefs(ctx, block, account, contracts)
	if err != nil {
		return nil, fmt.Errorf("withdrawal request index: %w", err)
	}
	return a.withdrawalGroups(ctx, client, block, account, byAddress, snapshot.Refs)
}

func (a *AsterAdapter) withdrawalGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	byAddress map[common.Address]asterWithdrawalContract,
	refs []accountRequestRef,
) ([]Group, error) {
	groups := make([]Group, 0, len(refs))
	for _, ref := range refs {
		withdrawal, exists := byAddress[ref.Contract]
		if !exists {
			return nil, fmt.Errorf("withdrawal request references an unknown contract")
		}
		requestID, valid := new(big.Int).SetString(ref.Key, 10)
		if !valid || requestID.Sign() < 0 {
			return nil, fmt.Errorf("withdrawal request has invalid index")
		}
		row, callErr := client.Call(
			ctx, block, withdrawal.Address, withdrawal.ABI, "requestWithdraws", requestID,
		)
		if callErr != nil {
			return nil, fmt.Errorf("%s withdrawal %s: %w", withdrawal.Receipt.Symbol, requestID, callErr)
		}
		receiptAddress, decodeErr := AddressAt(row, 0)
		// A request claimed after the indexed block is deleted, and reads empty at the pin.
		if decodeErr == nil && receiptAddress == (common.Address{}) {
			continue
		}
		if decodeErr != nil || receiptAddress != withdrawal.Receipt.Address {
			return nil, fmt.Errorf("%s withdrawal %s returned an unexpected receipt token", withdrawal.Receipt.Symbol, requestID)
		}
		amount, decodeErr := BigIntAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s withdrawal %s amount: %w", withdrawal.Receipt.Symbol, requestID, decodeErr)
		}
		if amount.Sign() <= 0 {
			return nil, fmt.Errorf("%s withdrawal %s does not match the pinned index", withdrawal.Receipt.Symbol, requestID)
		}
		requestOwner, decodeErr := AddressAt(row, withdrawal.OwnerIndex)
		if decodeErr != nil || requestOwner != account {
			return nil, fmt.Errorf("%s withdrawal %s owner does not match the pinned index", withdrawal.Receipt.Symbol, requestID)
		}
		sourceAmount, decodeErr := BigIntAt(row, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("%s withdrawal %s source amount: %w", withdrawal.Receipt.Symbol, requestID, decodeErr)
		}
		var underlyingAddress common.Address
		if withdrawal.Receipt.Address == asterAsBTC.Address {
			rateRow, rateErr := client.Call(
				ctx, block, asterAsBTCMinter, asterEarnRateABI, "supportAssToken", asterAsBTC.Address,
			)
			if rateErr != nil {
				return nil, fmt.Errorf("asBTC withdrawal %s rate: %w", requestID, rateErr)
			}
			underlyingAddress, decodeErr = AddressAt(rateRow, 1)
			if decodeErr != nil {
				return nil, fmt.Errorf("asBTC withdrawal %s underlying: %w", requestID, decodeErr)
			}
			if sourceAmount.Sign() == 0 {
				decimals, decimalsErr := Uint8At(rateRow, 2)
				rate, valueErr := BigIntAt(rateRow, 3)
				if decimalsErr != nil || valueErr != nil {
					return nil, fmt.Errorf("asBTC withdrawal %s rate state is invalid", requestID)
				}
				sourceAmount, decodeErr = asterEarnAssets(amount, rate, decimals)
				if decodeErr != nil {
					return nil, fmt.Errorf("asBTC withdrawal %s conversion: %w", requestID, decodeErr)
				}
			}
		} else {
			assetRow, assetErr := client.Call(ctx, block, asterAsUSDFMinter, asterUSDFRateABI, "USDF")
			if assetErr != nil {
				return nil, fmt.Errorf("asUSDF withdrawal %s underlying: %w", requestID, assetErr)
			}
			underlyingAddress, decodeErr = AddressAt(assetRow, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("asUSDF withdrawal %s underlying: %w", requestID, decodeErr)
			}
		}
		if sourceAmount.Sign() <= 0 {
			return nil, fmt.Errorf("%s withdrawal %s has no source amount", withdrawal.Receipt.Symbol, requestID)
		}
		underlying, tokenErr := readERC20Token(ctx, client, block, underlyingAddress)
		if tokenErr != nil {
			return nil, fmt.Errorf("%s withdrawal %s token: %w", withdrawal.Receipt.Symbol, requestID, tokenErr)
		}
		component := NewComponent(
			"asset", underlying, sourceAmount,
			Source{Contract: withdrawal.Address, Method: "requestWithdraws"},
		)
		component.Metadata = map[string]any{
			"requestIndex": requestID.String(), "owner": account,
			"receiptRaw": amount.String(), "sourceAmountRaw": sourceAmount.String(),
		}
		groups = append(groups, Group{
			ID:         strings.ToLower(withdrawal.Receipt.Symbol) + "-withdrawal:" + requestID.String(),
			Label:      withdrawal.Receipt.Symbol + " withdrawal pending",
			Components: []Component{component},
			Metadata:   map[string]any{"requestIndex": requestID.String(), "contract": withdrawal.Address},
		})
	}
	return groups, nil
}

func (a *AsterAdapter) readMintRequests(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != BSC || block.Number < asterAsBNBMinterActivation {
		return nil, nil
	}
	snapshot, err := a.indexer.IndexedRefs(ctx, block, account, []common.Address{asterAsBNBMinter})
	if err != nil {
		return nil, fmt.Errorf("mint request index: %w", err)
	}
	return a.mintGroups(ctx, client, block, account, snapshot.Refs)
}

func (a *AsterAdapter) mintGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	refs []accountRequestRef,
) ([]Group, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	header, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: asterAsBNBMinter, ABI: asterMinterABI, Method: "token"},
		{Contract: asterAsBNBMinter, ABI: asterMinterABI, Method: "asBnb"},
	})
	if err != nil {
		return nil, fmt.Errorf("asBNB minter assets: %w", err)
	}
	underlyingAddress, err := AddressAt(header[0], 0)
	if err != nil {
		return nil, fmt.Errorf("asBNB minter underlying: %w", err)
	}
	receiptAddress, err := AddressAt(header[1], 0)
	if err != nil {
		return nil, fmt.Errorf("asBNB minter receipt: %w", err)
	}
	if receiptAddress != asterAsBNB.Address {
		return nil, fmt.Errorf("asBNB minter receipt token changed")
	}
	// The queue holds the token the minter takes in, which is slisBNB rather than BNB. Report it
	// as-is so the amount and the price agree.
	underlying, err := readERC20Token(ctx, client, block, underlyingAddress)
	if err != nil {
		return nil, fmt.Errorf("asBNB mint request token metadata: %w", err)
	}
	ids := make([]*big.Int, 0, len(refs))
	for _, ref := range refs {
		id, valid := new(big.Int).SetString(ref.Key, 10)
		if !valid || id.Sign() < 0 {
			return nil, fmt.Errorf("asBNB mint request has invalid index")
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left].Cmp(ids[right]) < 0 })
	calls := make([]ContractCall, 0, len(ids))
	for _, id := range ids {
		calls = append(calls, ContractCall{
			Contract: asterAsBNBMinter, ABI: asterMinterABI,
			Method: "tokenMintReqQueue", Args: []any{id},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("asBNB mint requests: %w", err)
	}
	groups := make([]Group, 0, len(rows))
	for index, row := range rows {
		requestOwner, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("asBNB mint request %s owner: %w", ids[index], decodeErr)
		}
		amount, decodeErr := BigIntAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("asBNB mint request %s amount: %w", ids[index], decodeErr)
		}
		// A request processed or cancelled after the indexed block is deleted, and reads empty.
		if requestOwner == (common.Address{}) && amount.Sign() == 0 {
			continue
		}
		if requestOwner != account || amount.Sign() <= 0 {
			return nil, fmt.Errorf("asBNB mint request %s does not match the pinned index", ids[index])
		}
		component := NewComponent(
			"asset", underlying, amount,
			Source{Contract: asterAsBNBMinter, Method: "tokenMintReqQueue.amountIn"},
		)
		component.Metadata = map[string]any{"requestIndex": ids[index].String()}
		groups = append(groups, Group{
			ID: "asbnb-mint:" + ids[index].String(), Label: "asBNB mint pending",
			Components: []Component{component},
			Metadata:   map[string]any{"requestIndex": ids[index].String()},
		})
	}
	return groups, nil
}

func (a *AsterAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups, err := a.readYieldPositions(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	requests, err := a.readMintRequests(ctx, client, block, account)
	if err != nil {
		return groups, err
	}
	groups = append(groups, requests...)
	withdrawals, err := a.readWithdrawalRequests(ctx, client, block, account)
	if err != nil {
		return groups, err
	}
	return append(groups, withdrawals...), nil
}
