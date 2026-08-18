package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	eulerMaxVestingLocks = 1_024
)

var (
	eulerEVCABI = MustABI(`[
      {"type":"function","name":"getAccountOwner","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"address"}]}
    ]`)
	eulerVaultABI = MustABI(`[
      {"type":"function","name":"EVC","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"debtOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"balanceForwarderEnabled","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"bool"}]},
      {"type":"function","name":"name","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]},
      {"type":"function","name":"symbol","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]}
    ]`)
	eulerRewardsABI = MustABI(`[
      {"type":"function","name":"enabledRewards","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"rewarded","type":"address"}],"outputs":[{"type":"address[]"}]},
      {"type":"function","name":"earnedReward","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"rewarded","type":"address"},{"name":"reward","type":"address"},{"name":"ignoreRecentReward","type":"bool"}],"outputs":[{"type":"uint256"}]}
    ]`)
	eulerRewardTokenABI = MustABI(`[
      {"type":"function","name":"getLockedAmounts","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256[]"},{"type":"uint256[]"}]},
      {"type":"function","name":"getWithdrawAmountsByLockTimestamp","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"lockTimestamp","type":"uint256"}],"outputs":[{"type":"uint256"},{"type":"uint256"}]}
    ]`)
)

type eulerV2ChainConfig struct {
	ChainID           ChainID
	ActivationBlock   uint64
	EVC               common.Address
	TrackingRewards   common.Address
	RewardEUL         common.Address
	EUL               Token
	EVaultFactory     common.Address
	EulerEarnFactory  common.Address
	SecuritizeFactory common.Address
}

var eulerV2ChainConfigs = map[ChainID]eulerV2ChainConfig{
	Ethereum: {
		ChainID: Ethereum, ActivationBlock: 20_529_207,
		EVC:               common.HexToAddress("0x0C9a3dd6b8F28529d72d7f9cE918D493519EE383"),
		TrackingRewards:   common.HexToAddress("0x0D52d06ceB8Dcdeeb40Cfd9f17489B350dD7F8a3"),
		RewardEUL:         common.HexToAddress("0xf3e621395fc714B90dA337AA9108771597b4E696"),
		EUL:               token(Ethereum, "0xd9Fcd98c322942075A5C3860693e9f4f03AAE07b", "EUL", 18),
		EVaultFactory:     common.HexToAddress("0x29a56a1b8214D9Cf7c5561811750D5cBDb45CC8e"),
		EulerEarnFactory:  common.HexToAddress("0x59709B029B140C853FE28d277f83C3a65e308aF4"),
		SecuritizeFactory: common.HexToAddress("0x5F51D980F15fE6075aE30394dc35De57A4f76Cbb"),
	},
	BSC: {
		ChainID: BSC, ActivationBlock: 46_370_645,
		EVC:              common.HexToAddress("0xb2E5a73CeE08593d1a076a2AE7A6e02925a640ea"),
		TrackingRewards:  common.HexToAddress("0x2D13C46FE6c8B6c9ad3C5A78eD51b26733caE350"),
		RewardEUL:        common.HexToAddress("0x5e13d41913aDF18bb2acAe34228E8D21f3c2f2Eb"),
		EUL:              token(BSC, "0x2117E8b79e8E176A670c9fCf945d4348556bfFad", "EUL", 18),
		EVaultFactory:    common.HexToAddress("0x7F53E2755eB3c43824E162F7F6F087832B9C9Df6"),
		EulerEarnFactory: common.HexToAddress("0xc456d04E3F43597CC7E5a2AF284fF4C4AdDA0cb1"),
	},
	Base: {
		ChainID: Base, ActivationBlock: 22_282_353,
		EVC:              common.HexToAddress("0x5301c7dD20bD945D2013b48ed0DEE3A284ca8989"),
		TrackingRewards:  common.HexToAddress("0x029fDEe85BEdB0553D6fdc538546586641DD7438"),
		RewardEUL:        common.HexToAddress("0xE08e1f00D388E201e48842E53fA96195568e6813"),
		EUL:              token(Base, "0xa153Ad732F831a79b5575Fa02e793EC4E99181b0", "EUL", 18),
		EVaultFactory:    common.HexToAddress("0x7F321498A801A191a93C840750ed637149dDf8D0"),
		EulerEarnFactory: common.HexToAddress("0x75F49a2621b6DeC6a5baB22ce961bF3e676EFAE6"),
	},
	Arbitrum: {
		ChainID: Arbitrum, ActivationBlock: 300_690_886,
		EVC:              common.HexToAddress("0x6302ef0F34100CDDFb5489fbcB6eE1AA95CD1066"),
		TrackingRewards:  common.HexToAddress("0xbCD29c1B596d9fFAfaa6F90780956b4D3d47832f"),
		RewardEUL:        common.HexToAddress("0xFA31599a4928c2d57C0dd77DFCA5DA1E94E6D2D2"),
		EUL:              token(Arbitrum, "0x462cD9E0247b2e63831c3189aE738E5E9a5a4b64", "EUL", 18),
		EVaultFactory:    common.HexToAddress("0x78Df1CF5bf06a7f27f2ACc580B934238C1b80D50"),
		EulerEarnFactory: common.HexToAddress("0xB9B5d62B9fE9E1B505466e75817aB178A1D2ec9d"),
	},
}

type EulerV2Adapter struct {
	adapterBase
	indexer eulerPositionIndexer
}

type eulerPositionIndexer interface {
	PositionRefs(
		context.Context,
		*RPCClient,
		BlockRef,
		common.Address,
	) ([]eulerPositionRef, error)
}

func newEulerV2Adapter(config SentioIndexerConfig) Adapter {
	return newEulerV2AdapterWithIndexer(newEulerIndexer(config))
}

func newEulerV2AdapterWithIndexer(indexer eulerPositionIndexer) *EulerV2Adapter {
	return &EulerV2Adapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "euler-v2", Name: "Euler V2", Chains: []ChainID{Ethereum, BSC, Base, Arbitrum},
		}},
		indexer: indexer,
	}
}

type eulerPositionState struct {
	ref                     eulerPositionRef
	asset                   common.Address
	shares                  *big.Int
	assets                  *big.Int
	debt                    *big.Int
	balanceForwarderEnabled bool
	name                    string
	symbol                  string
}

func (a *EulerV2Adapter) ownedRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	refs []eulerPositionRef,
	chain eulerV2ChainConfig,
) ([]eulerPositionRef, error) {
	accountsByAddress := make(map[common.Address]struct{}, len(refs))
	for _, ref := range refs {
		accountsByAddress[ref.Account] = struct{}{}
	}
	accounts := make([]common.Address, 0, len(accountsByAddress))
	for account := range accountsByAddress {
		accounts = append(accounts, account)
	}
	sort.Slice(accounts, func(left, right int) bool {
		return strings.ToLower(accounts[left].Hex()) < strings.ToLower(accounts[right].Hex())
	})
	calls := make([]ContractCall, 0, len(accounts))
	for _, account := range accounts {
		calls = append(calls, ContractCall{
			Contract: chain.EVC, ABI: eulerEVCABI, Method: "getAccountOwner", Args: []any{account},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("account owners: %w", err)
	}
	owned := make(map[common.Address]struct{}, len(accounts))
	for index, row := range rows {
		actual, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("account %s owner: %w", accounts[index], decodeErr)
		}
		// EVC stores address(0) for an ordinary primary account; in that state
		// the account owns itself. Registered subaccounts return their owner.
		if actual == owner || (actual == (common.Address{}) && accounts[index] == owner) {
			owned[accounts[index]] = struct{}{}
		} else if accounts[index] == owner {
			return nil, fmt.Errorf("root account ownership changed at pinned block")
		}
	}
	result := make([]eulerPositionRef, 0, len(refs))
	for _, ref := range refs {
		if _, exists := owned[ref.Account]; exists {
			result = append(result, ref)
		}
	}
	return result, nil
}

func (a *EulerV2Adapter) readStates(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	refs []eulerPositionRef,
	chain eulerV2ChainConfig,
) ([]eulerPositionState, error) {
	states := make([]eulerPositionState, 0, len(refs))
	for _, ref := range refs {
		calls := []ContractCall{
			{Contract: ref.Vault, ABI: eulerVaultABI, Method: "EVC"},
			{Contract: ref.Vault, ABI: eulerVaultABI, Method: "asset"},
			{Contract: ref.Vault, ABI: eulerVaultABI, Method: "balanceOf", Args: []any{ref.Account}},
		}
		if ref.Kind == eulerEVault {
			calls = append(calls,
				ContractCall{Contract: ref.Vault, ABI: eulerVaultABI, Method: "debtOf", Args: []any{ref.Account}},
				ContractCall{Contract: ref.Vault, ABI: eulerVaultABI, Method: "balanceForwarderEnabled", Args: []any{ref.Account}},
			)
		} else {
			calls = append(calls,
				ContractCall{Contract: ref.Vault, ABI: eulerVaultABI, Method: "name"},
				ContractCall{Contract: ref.Vault, ABI: eulerVaultABI, Method: "symbol"},
			)
		}
		rows, err := client.ParallelCalls(ctx, block, calls)
		if err != nil {
			return nil, fmt.Errorf("vault %s position: %w", ref.Vault, err)
		}
		evc, err := AddressAt(rows[0], 0)
		if err != nil {
			return nil, fmt.Errorf("vault %s EVC: %w", ref.Vault, err)
		}
		if evc != chain.EVC {
			return nil, fmt.Errorf("vault %s is wired to an unexpected EVC", ref.Vault)
		}
		asset, err := AddressAt(rows[1], 0)
		if err != nil {
			return nil, fmt.Errorf("vault %s asset: %w", ref.Vault, err)
		}
		shares, err := BigIntAt(rows[2], 0)
		if err != nil {
			return nil, fmt.Errorf("vault %s shares: %w", ref.Vault, err)
		}
		state := eulerPositionState{
			ref: ref, asset: asset, shares: shares,
			assets: new(big.Int), debt: new(big.Int),
		}
		if ref.Kind == eulerEVault {
			state.debt, err = BigIntAt(rows[3], 0)
			if err != nil {
				return nil, fmt.Errorf("vault %s debt: %w", ref.Vault, err)
			}
			state.balanceForwarderEnabled, err = BoolAt(rows[4], 0)
			if err != nil {
				return nil, fmt.Errorf("vault %s reward forwarding: %w", ref.Vault, err)
			}
		} else {
			state.name, err = StringAt(rows[3], 0)
			if err != nil {
				return nil, fmt.Errorf("vault %s name: %w", ref.Vault, err)
			}
			state.symbol, err = StringAt(rows[4], 0)
			if err != nil {
				return nil, fmt.Errorf("vault %s symbol: %w", ref.Vault, err)
			}
		}
		if shares.Sign() > 0 && ref.Kind != eulerSecuritize {
			converted, convertErr := client.Call(
				ctx, block, ref.Vault, eulerVaultABI, "convertToAssets", shares,
			)
			if convertErr != nil {
				return nil, fmt.Errorf("vault %s share conversion: %w", ref.Vault, convertErr)
			}
			state.assets, err = BigIntAt(converted, 0)
			if err != nil {
				return nil, fmt.Errorf("vault %s assets: %w", ref.Vault, err)
			}
		}
		states = append(states, state)
	}
	return states, nil
}

type eulerRewardState struct {
	Account common.Address
	Vault   common.Address
	Token   common.Address
	Amount  *big.Int
}

func uniqueEulerAddresses(addresses []common.Address) []common.Address {
	seen := make(map[common.Address]struct{}, len(addresses))
	for _, address := range addresses {
		if address != (common.Address{}) {
			seen[address] = struct{}{}
		}
	}
	result := make([]common.Address, 0, len(seen))
	for address := range seen {
		result = append(result, address)
	}
	sort.Slice(result, func(left, right int) bool {
		return strings.ToLower(result[left].Hex()) < strings.ToLower(result[right].Hex())
	})
	return result
}

func (a *EulerV2Adapter) readRewards(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	states []eulerPositionState,
	chain eulerV2ChainConfig,
) ([]eulerRewardState, error) {
	eVaults := make([]eulerPositionState, 0)
	for _, state := range states {
		if state.ref.Kind == eulerEVault {
			eVaults = append(eVaults, state)
		}
	}
	enabledCalls := make([]ContractCall, 0, len(eVaults))
	for _, state := range eVaults {
		enabledCalls = append(enabledCalls, ContractCall{
			Contract: chain.TrackingRewards, ABI: eulerRewardsABI, Method: "enabledRewards",
			Args: []any{state.ref.Account, state.ref.Vault},
		})
	}
	enabledRows, err := client.ParallelCalls(ctx, block, enabledCalls)
	if err != nil {
		return nil, fmt.Errorf("enabled rewards: %w", err)
	}
	type candidate struct {
		account, vault, token common.Address
	}
	candidates := make([]candidate, 0)
	for index, state := range eVaults {
		enabled, decodeErr := AddressSliceAt(enabledRows[index], 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("vault %s enabled rewards: %w", state.ref.Vault, decodeErr)
		}
		for _, reward := range uniqueEulerAddresses(append(state.ref.RewardTokens, enabled...)) {
			candidates = append(candidates, candidate{state.ref.Account, state.ref.Vault, reward})
		}
	}
	calls := make([]ContractCall, 0, len(candidates))
	for _, candidate := range candidates {
		calls = append(calls, ContractCall{
			Contract: chain.TrackingRewards, ABI: eulerRewardsABI, Method: "earnedReward",
			Args: []any{candidate.account, candidate.vault, candidate.token, false},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("earned rewards: %w", err)
	}
	result := make([]eulerRewardState, 0, len(candidates))
	for index, row := range rows {
		amount, decodeErr := BigIntAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("earned reward: %w", decodeErr)
		}
		if amount.Sign() > 0 {
			result = append(result, eulerRewardState{
				Account: candidates[index].account, Vault: candidates[index].vault,
				Token: candidates[index].token, Amount: amount,
			})
		}
	}
	return result, nil
}

type eulerVestingState struct {
	Timestamp *big.Int
	Amount    *big.Int
	Claimable *big.Int
	Remainder *big.Int
}

func (a *EulerV2Adapter) readVestings(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	chain eulerV2ChainConfig,
) ([]eulerVestingState, error) {
	row, err := client.Call(ctx, block, chain.RewardEUL, eulerRewardTokenABI, "getLockedAmounts", owner)
	if err != nil {
		return nil, fmt.Errorf("rEUL locks: %w", err)
	}
	timestamps, err := BigIntSliceAt(row, 0)
	if err != nil {
		return nil, fmt.Errorf("rEUL lock timestamps: %w", err)
	}
	amounts, err := BigIntSliceAt(row, 1)
	if err != nil {
		return nil, fmt.Errorf("rEUL locked amounts: %w", err)
	}
	if len(timestamps) != len(amounts) || len(timestamps) > eulerMaxVestingLocks {
		return nil, fmt.Errorf("rEUL lock enumeration is inconsistent")
	}
	calls := make([]ContractCall, 0, len(timestamps))
	for _, timestamp := range timestamps {
		calls = append(calls, ContractCall{
			Contract: chain.RewardEUL, ABI: eulerRewardTokenABI,
			Method: "getWithdrawAmountsByLockTimestamp", Args: []any{owner, timestamp},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("rEUL withdraw amounts: %w", err)
	}
	result := make([]eulerVestingState, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for index, values := range rows {
		if _, duplicate := seen[timestamps[index].String()]; duplicate {
			return nil, fmt.Errorf("rEUL returned duplicate lock timestamps")
		}
		seen[timestamps[index].String()] = struct{}{}
		claimable, decodeErr := BigIntAt(values, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("rEUL claimable amount: %w", decodeErr)
		}
		remainder, decodeErr := BigIntAt(values, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("rEUL remainder amount: %w", decodeErr)
		}
		conserved := new(big.Int).Add(claimable, remainder)
		if conserved.Cmp(amounts[index]) != 0 {
			return nil, fmt.Errorf("rEUL withdraw amounts do not conserve value")
		}
		if amounts[index].Sign() > 0 {
			result = append(result, eulerVestingState{
				Timestamp: timestamps[index], Amount: amounts[index],
				Claimable: claimable, Remainder: remainder,
			})
		}
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].Timestamp.Cmp(result[right].Timestamp) < 0
	})
	return result, nil
}

func eulerPositionNumber(owner, account common.Address) int {
	return int(owner.Bytes()[19] ^ account.Bytes()[19])
}

func (a *EulerV2Adapter) buildGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
	states []eulerPositionState,
	rewards []eulerRewardState,
	vestings []eulerVestingState,
	chain eulerV2ChainConfig,
) ([]Group, error) {
	groupsByAccount := make(map[common.Address]*Group)
	groups := make([]Group, 0)
	metadataAddresses := make([]common.Address, 0)
	lendingGroup := func(account common.Address) *Group {
		if group := groupsByAccount[account]; group != nil {
			return group
		}
		number := eulerPositionNumber(owner, account)
		label := "Lending"
		if number != 0 {
			label = fmt.Sprintf("Lending · Position %d", number)
		}
		group := &Group{
			ID: "euler-v2:lending:" + strings.ToLower(account.Hex()), Label: label,
			NetValuePolicy: "floor-zero",
			Metadata:       map[string]any{"account": account, "positionNumber": number},
		}
		groupsByAccount[account] = group
		return group
	}
	for _, state := range states {
		marketID := string(state.ref.Kind) + ":" + strings.ToLower(state.ref.Vault.Hex())
		switch state.ref.Kind {
		case eulerEVault:
			group := lendingGroup(state.ref.Account)
			if state.assets.Sign() > 0 {
				component := NewComponent(
					"asset", Token{ChainID: block.ChainID, Address: state.asset}, state.assets,
					Source{Contract: state.ref.Vault, Method: "convertToAssets(balanceOf)"},
				)
				component.Metadata = map[string]any{
					"account": state.ref.Account, "vault": state.ref.Vault, "marketId": marketID,
					"shares":                  state.shares.String(),
					"balanceForwarderEnabled": state.balanceForwarderEnabled,
				}
				group.Components = append(group.Components, component)
				metadataAddresses = append(metadataAddresses, state.asset)
			}
			if state.debt.Sign() > 0 {
				component := NewComponent(
					"debt", Token{ChainID: block.ChainID, Address: state.asset}, state.debt,
					Source{Contract: state.ref.Vault, Method: "debtOf"},
				)
				component.Metadata = map[string]any{
					"account": state.ref.Account, "vault": state.ref.Vault, "marketId": marketID,
				}
				group.Components = append(group.Components, component)
				metadataAddresses = append(metadataAddresses, state.asset)
			}
		case eulerSecuritize:
			if state.shares.Sign() > 0 {
				component := NewComponent(
					"asset", Token{ChainID: block.ChainID, Address: state.ref.Vault}, state.shares,
					Source{Contract: state.ref.Vault, Method: "balanceOf"},
				)
				component.Metadata = map[string]any{
					"account": state.ref.Account, "vault": state.ref.Vault, "marketId": marketID,
					"vaultKind": string(state.ref.Kind), "shareSymbol": state.symbol,
				}
				lendingGroup(state.ref.Account).Components = append(
					lendingGroup(state.ref.Account).Components,
					component,
				)
				metadataAddresses = append(metadataAddresses, state.ref.Vault)
			}
		case eulerEarn:
			if state.assets.Sign() > 0 {
				component := NewComponent(
					"asset", Token{ChainID: block.ChainID, Address: state.asset}, state.assets,
					Source{Contract: state.ref.Vault, Method: "convertToAssets(balanceOf)"},
				)
				component.Metadata = map[string]any{
					"account": state.ref.Account, "vault": state.ref.Vault, "marketId": marketID,
					"shares": state.shares.String(), "vaultKind": string(state.ref.Kind),
					"shareSymbol": state.symbol,
				}
				identity := strings.TrimSpace(state.name)
				if identity == "" {
					identity = strings.TrimSpace(state.symbol)
				}
				groups = append(groups, Group{
					ID:    "euler-v2:euler-earn:" + strings.ToLower(state.ref.Vault.Hex()) + ":" + strings.ToLower(state.ref.Account.Hex()),
					Label: "Yield · " + identity, Components: []Component{component},
					Metadata: map[string]any{
						"account": state.ref.Account, "vault": state.ref.Vault,
						"vaultKind": string(state.ref.Kind),
					},
				})
				metadataAddresses = append(metadataAddresses, state.asset)
			}
		}
	}
	for _, reward := range rewards {
		component := NewComponent(
			"reward", Token{ChainID: block.ChainID, Address: reward.Token}, reward.Amount,
			Source{Contract: chain.TrackingRewards, Method: "earnedReward"},
		)
		component.Metadata = map[string]any{
			"account": reward.Account, "vault": reward.Vault,
			"marketId": "evault:" + strings.ToLower(reward.Vault.Hex()),
		}
		lendingGroup(reward.Account).Components = append(lendingGroup(reward.Account).Components, component)
		metadataAddresses = append(metadataAddresses, reward.Token)
	}
	for _, vesting := range vestings {
		component := NewComponent(
			"asset", chain.EUL, vesting.Amount,
			Source{Contract: chain.RewardEUL, Method: "getLockedAmounts/getWithdrawAmountsByLockTimestamp"},
		)
		component.Metadata = map[string]any{
			"account": owner, "lockTimestamp": vesting.Timestamp.String(),
			"claimableAmountRaw": vesting.Claimable.String(),
			"remainderAmountRaw": vesting.Remainder.String(),
		}
		groups = append(groups, Group{
			ID:    "euler-v2:vesting:" + strings.ToLower(owner.Hex()) + ":" + vesting.Timestamp.String(),
			Label: "Vesting", Components: []Component{component},
			Metadata: map[string]any{"account": owner, "lockTimestamp": vesting.Timestamp.String()},
		})
	}
	for _, group := range groupsByAccount {
		if len(group.Components) > 0 {
			groups = append(groups, *group)
		}
	}
	tokens, err := readERC20Tokens(ctx, client, block, metadataAddresses)
	if err != nil {
		return nil, fmt.Errorf("token metadata: %w", err)
	}
	for groupIndex := range groups {
		for componentIndex := range groups[groupIndex].Components {
			component := &groups[groupIndex].Components[componentIndex]
			if component.Token.Address == chain.EUL.Address {
				component.Token = chain.EUL
				continue
			}
			token, exists := tokens[component.Token.Address]
			if !exists {
				return nil, fmt.Errorf("token metadata is absent")
			}
			component.Token = token
		}
	}
	sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
	return groups, nil
}

func (a *EulerV2Adapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	owner common.Address,
) ([]Group, error) {
	chain, supported := eulerV2ChainConfigs[block.ChainID]
	if !supported || block.Number < chain.ActivationBlock {
		return nil, nil
	}
	refs, err := a.indexer.PositionRefs(ctx, client, block, owner)
	if err != nil {
		return nil, fmt.Errorf("position enumeration: %w", err)
	}
	refs, err = a.ownedRefs(ctx, client, block, owner, refs, chain)
	if err != nil {
		return nil, err
	}
	states, err := a.readStates(ctx, client, block, refs, chain)
	if err != nil {
		return nil, err
	}
	rewards, err := a.readRewards(ctx, client, block, states, chain)
	if err != nil {
		return nil, err
	}
	vestings, err := a.readVestings(ctx, client, block, owner, chain)
	if err != nil {
		return nil, err
	}
	return a.buildGroups(ctx, client, block, owner, states, rewards, vestings, chain)
}
