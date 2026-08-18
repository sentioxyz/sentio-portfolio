package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

const (
	renzoMaxWithdrawalRequests = 4_096
	renzoMaxEigenWithdrawals   = 256
)

var (
	renzoRestakeManager = common.HexToAddress("0x74a09653A083691711cF8215a6ab074BB4e99ef5")
	renzoWithdrawQueue  = common.HexToAddress("0x5efc9D10E42FB517456f4ac41EB5e2eBe42C8918")
	renzoLegacyStake    = common.HexToAddress("0x1736011D3E075351B319DBC1da28Dac68Ea830A6")
	renzoNativeToken    = common.HexToAddress("0xEeeeeEeeeEeEeeEeEeEeeEEEeeeeEeeeeeeeEEeE")
	renzoETH            = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
	renzoREZ = token(
		Ethereum,
		"0x3B50805453023a91a8bf641e279401a0b23FA6F9",
		"REZ",
		18,
	)
	renzoEIGEN = token(
		Ethereum,
		"0xec53bF9167f50cDEB3Ae105f56099aaaB9061F83",
		"EIGEN",
		18,
	)
	renzoWBTC = token(
		Ethereum,
		"0x2260FAC5E5542a773Aa44fBCfeDf7C193bc2C599",
		"WBTC",
		8,
	)
	renzoWstETH = token(
		Ethereum,
		"0x7f39C581F595B53c5cb19bD0b3f8dA6c935E2Ca0",
		"wstETH",
		18,
	)
	renzoEZETH = token(
		Ethereum,
		"0xbf5495Efe5DB9ce00f80364C8B423567e58d2110",
		"ezETH",
		18,
	)
	renzoPZETH = token(
		Ethereum,
		"0x8c9532a60e0e7c6bbd2b2c1303f63ace1c3e9811",
		"pzETH",
		18,
	)
	renzoRestakeManagerABI = MustABI(`[
      {"type":"function","name":"ezETH","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"renzoOracle","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"calculateTVLs","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256[][]"},{"type":"uint256[]"},{"type":"uint256"}]}
    ]`)
	renzoOracleABI = MustABI(`[
      {"type":"function","name":"calculateRedeemAmount","stateMutability":"view","inputs":[{"type":"uint256"},{"type":"uint256"},{"type":"uint256"}],"outputs":[{"type":"uint256"}]}
    ]`)
	renzoERC4626ABI = MustABI(`[
      {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"type":"uint256"}],"outputs":[{"type":"uint256"}]}
    ]`)
	renzoSupplyABI = MustABI(`[
      {"type":"function","name":"totalSupply","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]}
    ]`)
	renzoWithdrawQueueABI = MustABI(`[
      {"type":"function","name":"getOutstandingWithdrawRequests","stateMutability":"view","inputs":[{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]},
      {
        "type":"function","name":"withdrawRequests","stateMutability":"view",
        "inputs":[{"name":"user","type":"address"},{"name":"index","type":"uint256"}],
        "outputs":[{"type":"address"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"},{"type":"uint256"}]
      }
    ]`)
	renzoEigenVaultABI = MustABI(`[
      {"type":"function","name":"underlying","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
      {"type":"function","name":"userUnderlying","stateMutability":"view","inputs":[{"name":"user","type":"address"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"getRate","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"scaleFactor","stateMutability":"view","inputs":[],"outputs":[{"type":"uint256"}]},
	  {"type":"function","name":"withdrawRequest","stateMutability":"view","inputs":[{"name":"root","type":"bytes32"}],"outputs":[{"type":"address"},{"type":"uint256"},{"type":"uint256"}]},
	  {"type":"function","name":"queuedWithdrawalInfo","stateMutability":"view","inputs":[{"name":"root","type":"bytes32"}],"outputs":[{"type":"uint256"},{"type":"uint256"}]}
    ]`)
	renzoLegacyStakeABI = MustABI(`[
      {"type":"function","name":"activeStake","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
      {"type":"function","name":"getUnstakeRequestsLength","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
		{"type":"function","name":"unstakeRequests","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"index","type":"uint256"}],"outputs":[{"type":"uint256"},{"type":"uint256"}]}
    ]`)
	renzoEigenWithdrawStartedTopic = crypto.Keccak256Hash([]byte(
		"WithdrawStarted(bytes32,address,address,address,address,uint256,uint256,address[],uint256[])",
	))
	renzoEigenWithdrawClaimedTopic = crypto.Keccak256Hash([]byte(
		"WithdrawRequestClaimed(bytes32,address,uint256,(address,address,address,uint256,uint32,address[],uint256[]))",
	))
)

type renzoEigenVault struct {
	ID              string
	Label           string
	Address         common.Address
	Underlying      Token
	ActivationBlock uint64
}

type RenzoAdapter struct {
	adapterBase
	receipts map[ChainID][]receiptPosition
	vaults   []renzoEigenVault
	indexer  *accountRequestIndexer
}

func newRenzoAdapter(config SentioIndexerConfig) Adapter {
	return &RenzoAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "renzo", Name: "Renzo", Chains: []ChainID{Ethereum, BSC, Base, Arbitrum},
		}},
		receipts: map[ChainID][]receiptPosition{
			Ethereum: {
				{
					ID: "ezeth", Label: "Liquid restaking · ezETH",
					Receipt:         renzoEZETH,
					ActivationBlock: 18_722_779,
				},
				{
					ID: "pzeth", Label: "Liquid restaking · pzETH",
					Receipt:         renzoPZETH,
					ActivationBlock: 20_175_746,
				},
			},
			BSC: {{
				ID: "ezeth", Label: "Liquid restaking · ezETH",
				Receipt:         token(BSC, "0x2416092f143378750bb29b79ed961ab195cceea5", "ezETH", 18),
				ActivationBlock: 36_596_546,
			}},
			Base: {{
				ID: "ezeth", Label: "Liquid restaking · ezETH",
				Receipt:         token(Base, "0x2416092f143378750bb29b79ed961ab195cceea5", "ezETH", 18),
				ActivationBlock: 12_682_160,
			}},
			Arbitrum: {{
				ID: "ezeth", Label: "Liquid restaking · ezETH",
				Receipt:         token(Arbitrum, "0x2416092f143378750bb29b79ed961ab195cceea5", "ezETH", 18),
				ActivationBlock: 185_410_162,
			}},
		},
		vaults: []renzoEigenVault{
			{
				ID: "ezrez", Label: "Restaked REZ · ezREZ",
				Address:    common.HexToAddress("0x77b1183e730275f6a8024ce53d54bcc12b368f60"),
				Underlying: renzoREZ, ActivationBlock: 20_805_472,
			},
			{
				ID: "ezeigen", Label: "Restaked EIGEN · ezEIGEN",
				Address:    common.HexToAddress("0xd4fcde9bb1d746Dd7e5463b01Dd819EE06aF25db"),
				Underlying: renzoEIGEN, ActivationBlock: 20_858_349,
			},
			{
				ID: "ezbtc", Label: "Restaked WBTC · ezBTC",
				Address:    common.HexToAddress("0xedd5c6f4526ea6fdea31e56d206e94e966257b70"),
				Underlying: renzoWBTC, ActivationBlock: 22_624_336,
			},
		},
		indexer: newAccountRequestIndexer(config, []ChainID{Ethereum}),
	}
}

func (a *RenzoAdapter) readMainnetReceipts(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups := make([]Group, 0, 2)
	if block.Number >= 18_722_779 {
		balanceRow, err := client.Call(ctx, block, renzoEZETH.Address, erc20ABI, "balanceOf", account)
		if err != nil {
			return nil, fmt.Errorf("ezETH balance: %w", err)
		}
		balance, err := BigIntAt(balanceRow, 0)
		if err != nil {
			return nil, fmt.Errorf("ezETH balance: %w", err)
		}
		if balance.Sign() > 0 {
			rows, callErr := client.ParallelCalls(ctx, block, []ContractCall{
				{Contract: renzoRestakeManager, ABI: renzoRestakeManagerABI, Method: "ezETH"},
				{Contract: renzoRestakeManager, ABI: renzoRestakeManagerABI, Method: "renzoOracle"},
				{Contract: renzoRestakeManager, ABI: renzoRestakeManagerABI, Method: "calculateTVLs"},
				{Contract: renzoEZETH.Address, ABI: renzoSupplyABI, Method: "totalSupply"},
			})
			if callErr != nil {
				return nil, fmt.Errorf("ezETH conversion state: %w", callErr)
			}
			managerToken, decodeErr := AddressAt(rows[0], 0)
			if decodeErr != nil || managerToken != renzoEZETH.Address {
				return nil, fmt.Errorf("RestakeManager ezETH identity changed")
			}
			oracle, decodeErr := AddressAt(rows[1], 0)
			if decodeErr != nil || oracle == (common.Address{}) {
				return nil, fmt.Errorf("RestakeManager oracle is invalid")
			}
			totalTVL, decodeErr := BigIntAt(rows[2], 2)
			if decodeErr != nil {
				return nil, fmt.Errorf("RestakeManager total TVL: %w", decodeErr)
			}
			totalSupply, decodeErr := BigIntAt(rows[3], 0)
			if decodeErr != nil || totalSupply.Sign() <= 0 {
				return nil, fmt.Errorf("ezETH total supply is invalid")
			}
			amountRow, convertErr := client.Call(
				ctx, block, oracle, renzoOracleABI, "calculateRedeemAmount", balance, totalSupply, totalTVL,
			)
			if convertErr != nil {
				return nil, fmt.Errorf("ezETH conversion: %w", convertErr)
			}
			amount, decodeErr := BigIntAt(amountRow, 0)
			if decodeErr != nil {
				return nil, fmt.Errorf("ezETH conversion: %w", decodeErr)
			}
			component := NewComponent(
				"asset", renzoETH, amount,
				Source{Contract: oracle, Method: "calculateRedeemAmount(balanceOf,totalSupply,totalTVL)"},
			)
			component.Metadata = map[string]any{
				"ezETHRaw": balance.String(), "totalTVLRaw": totalTVL.String(),
				"totalSupplyRaw": totalSupply.String(),
			}
			groups = append(groups, Group{
				ID: "ezeth", Label: "Liquid restaking · ezETH", Components: []Component{component},
			})
		}
	}
	if block.Number >= 20_175_746 {
		rows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: renzoPZETH.Address, ABI: renzoERC4626ABI, Method: "balanceOf", Args: []any{account}},
			{Contract: renzoPZETH.Address, ABI: renzoERC4626ABI, Method: "asset"},
		})
		if err != nil {
			return groups, fmt.Errorf("pzETH state: %w", err)
		}
		shares, decodeErr := BigIntAt(rows[0], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("pzETH balance: %w", decodeErr)
		}
		asset, decodeErr := AddressAt(rows[1], 0)
		if decodeErr != nil || asset != renzoWstETH.Address {
			return groups, fmt.Errorf("pzETH underlying changed")
		}
		if shares.Sign() > 0 {
			converted, convertErr := client.Call(
				ctx, block, renzoPZETH.Address, renzoERC4626ABI, "convertToAssets", shares,
			)
			if convertErr != nil {
				return groups, fmt.Errorf("pzETH conversion: %w", convertErr)
			}
			amount, decodeErr := BigIntAt(converted, 0)
			if decodeErr != nil {
				return groups, fmt.Errorf("pzETH conversion: %w", decodeErr)
			}
			component := NewComponent(
				"asset", renzoWstETH, amount,
				Source{Contract: renzoPZETH.Address, Method: "convertToAssets(balanceOf)"},
			)
			component.Metadata = map[string]any{"sharesRaw": shares.String()}
			groups = append(groups, Group{
				ID: "pzeth", Label: "Liquid restaking · pzETH", Components: []Component{component},
			})
		}
	}
	return groups, nil
}

func renzoWithdrawalRoot(event rpcLog) (common.Hash, common.Address, error) {
	if len(event.Topics) != 1 || len(event.Data) < 64 {
		return common.Hash{}, common.Address{}, fmt.Errorf("malformed withdrawal event")
	}
	root := common.BytesToHash(event.Data[:32])
	owner := common.BytesToAddress(event.Data[44:64])
	if root == (common.Hash{}) || owner == (common.Address{}) {
		return common.Hash{}, common.Address{}, fmt.Errorf("withdrawal event contains a zero identity")
	}
	return root, owner, nil
}

func (a *RenzoAdapter) eigenWithdrawalRefs(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]accountRequestRef, error) {
	contracts := make([]common.Address, 0, len(a.vaults))
	allowed := make(map[common.Address]struct{}, len(a.vaults))
	for _, vault := range a.vaults {
		if block.Number >= vault.ActivationBlock {
			contracts = append(contracts, vault.Address)
			allowed[vault.Address] = struct{}{}
		}
	}
	snapshot, err := a.indexer.IndexedRefs(ctx, block, account, contracts)
	if err != nil {
		return nil, fmt.Errorf("Eigen vault withdrawal index: %w", err)
	}
	refs := make(map[string]accountRequestRef, len(snapshot.Refs))
	for _, ref := range snapshot.Refs {
		refs[accountRequestRefKey(ref)] = ref
	}
	if snapshot.Block < block.Number {
		logs, logsErr := client.Logs(
			ctx, snapshot.Block+1, block.Number, contracts,
			[][]common.Hash{{renzoEigenWithdrawStartedTopic, renzoEigenWithdrawClaimedTopic}},
		)
		if logsErr != nil {
			return nil, fmt.Errorf("Eigen vault withdrawal RPC tail: %w", logsErr)
		}
		sortRPCLogs(logs)
		for _, event := range logs {
			if _, exists := allowed[event.Address]; !exists {
				return nil, fmt.Errorf("Eigen withdrawal RPC tail returned unexpected vault")
			}
			root, eventOwner, decodeErr := renzoWithdrawalRoot(event)
			if decodeErr != nil {
				return nil, fmt.Errorf("Eigen withdrawal RPC tail: %w", decodeErr)
			}
			if eventOwner != account {
				continue
			}
			ref := accountRequestRef{
				Contract: event.Address, Key: strings.ToLower(root.Hex()),
			}
			switch event.Topics[0] {
			case renzoEigenWithdrawStartedTopic:
				refs[accountRequestRefKey(ref)] = ref
			case renzoEigenWithdrawClaimedTopic:
				delete(refs, accountRequestRefKey(ref))
			default:
				return nil, fmt.Errorf("Eigen withdrawal RPC tail returned unexpected event")
			}
		}
	}
	if len(refs) > renzoMaxEigenWithdrawals {
		return nil, fmt.Errorf("Eigen withdrawal count exceeds %d", renzoMaxEigenWithdrawals)
	}
	result := make([]accountRequestRef, 0, len(refs))
	for _, ref := range refs {
		result = append(result, ref)
	}
	sort.Slice(result, func(left, right int) bool {
		return accountRequestRefKey(result[left]) < accountRequestRefKey(result[right])
	})
	return result, nil
}

func (a *RenzoAdapter) readEigenWithdrawals(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	refs, err := a.eigenWithdrawalRefs(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	vaultByAddress := make(map[common.Address]renzoEigenVault, len(a.vaults))
	for _, vault := range a.vaults {
		vaultByAddress[vault.Address] = vault
	}
	groups := make([]Group, 0, len(refs))
	for _, ref := range refs {
		vault, exists := vaultByAddress[ref.Contract]
		if !exists {
			return nil, fmt.Errorf("Eigen withdrawal references an unknown vault")
		}
		root := common.HexToHash(ref.Key)
		rows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: vault.Address, ABI: renzoEigenVaultABI, Method: "withdrawRequest", Args: []any{root}},
			{Contract: vault.Address, ABI: renzoEigenVaultABI, Method: "scaleFactor"},
			{Contract: vault.Address, ABI: renzoEigenVaultABI, Method: "queuedWithdrawalInfo", Args: []any{root}},
		})
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s: %w", vault.Label, root, err)
		}
		withdrawer, err := AddressAt(rows[0], 0)
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s owner: %w", vault.Label, root, err)
		}
		locked, err := BigIntAt(rows[0], 1)
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s locked amount: %w", vault.Label, root, err)
		}
		createdAt, err := BigIntAt(rows[0], 2)
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s creation block: %w", vault.Label, root, err)
		}
		scale, err := BigIntAt(rows[1], 0)
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s scale: %w", vault.Label, root, err)
		}
		slashedShares, err := BigIntAt(rows[2], 0)
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s slashed shares: %w", vault.Label, root, err)
		}
		initialShares, err := BigIntAt(rows[2], 1)
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s initial shares: %w", vault.Label, root, err)
		}
		if withdrawer != account || locked.Sign() <= 0 || !createdAt.IsUint64() ||
			createdAt.Uint64() > block.Number || createdAt.Uint64() < vault.ActivationBlock || scale.Sign() <= 0 ||
			slashedShares.Sign() < 0 || initialShares.Sign() < 0 || slashedShares.Cmp(initialShares) > 0 {
			return nil, fmt.Errorf("%s withdrawal %s does not match the pinned index", vault.Label, root)
		}
		creationBlock := BlockRef{ChainID: Ethereum, Number: createdAt.Uint64(), Fixed: true}
		rateRow, err := client.Call(ctx, creationBlock, vault.Address, renzoEigenVaultABI, "getRate")
		if err != nil {
			return nil, fmt.Errorf("%s withdrawal %s creation rate: %w", vault.Label, root, err)
		}
		rate, err := BigIntAt(rateRow, 0)
		if err != nil || rate.Sign() <= 0 {
			return nil, fmt.Errorf("%s withdrawal %s creation rate is invalid", vault.Label, root)
		}
		amount := new(big.Int).Mul(locked, rate)
		amount.Quo(amount, scale)
		if initialShares.Sign() > 0 && slashedShares.Sign() > 0 {
			remainingShares := new(big.Int).Sub(initialShares, slashedShares)
			amount.Mul(amount, remainingShares)
			amount.Quo(amount, initialShares)
		}
		if amount.Sign() <= 0 {
			continue
		}
		component := NewComponent(
			"asset", vault.Underlying, amount,
			Source{Contract: vault.Address, Method: "withdrawRequest/getRate@creationBlock"},
		)
		component.Metadata = map[string]any{
			"withdrawalRoot": strings.ToLower(root.Hex()),
			"lockedShares":   locked.String(), "createdAtBlock": createdAt.String(),
			"creationRate": rate.String(), "scaleFactor": scale.String(),
			"initialWithdrawableShares": initialShares.String(),
			"slashedShares":             slashedShares.String(),
		}
		groups = append(groups, Group{
			ID:    vault.ID + "-withdrawal:" + strings.ToLower(root.Hex()),
			Label: vault.Label + " withdrawal", Components: []Component{component},
			Metadata: map[string]any{"vault": vault.Address, "withdrawalRoot": strings.ToLower(root.Hex())},
		})
	}
	return groups, nil
}

func (a *RenzoAdapter) readEigenVaults(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups := make([]Group, 0, len(a.vaults))
	for _, vault := range a.vaults {
		if block.Number < vault.ActivationBlock {
			continue
		}
		rows, err := client.ParallelCalls(ctx, block, []ContractCall{
			{Contract: vault.Address, ABI: renzoEigenVaultABI, Method: "underlying"},
			{Contract: vault.Address, ABI: renzoEigenVaultABI, Method: "userUnderlying", Args: []any{account}},
		})
		if err != nil {
			return nil, fmt.Errorf("%s position: %w", vault.Label, err)
		}
		underlying, err := AddressAt(rows[0], 0)
		if err != nil {
			return nil, fmt.Errorf("%s underlying: %w", vault.Label, err)
		}
		if underlying != vault.Underlying.Address {
			return nil, fmt.Errorf("%s underlying changed", vault.Label)
		}
		amount, err := BigIntAt(rows[1], 0)
		if err != nil {
			return nil, fmt.Errorf("%s underlying balance: %w", vault.Label, err)
		}
		if amount.Sign() == 0 {
			continue
		}
		groups = append(groups, Group{
			ID: vault.ID, Label: vault.Label,
			Components: []Component{NewComponent(
				"asset", vault.Underlying, amount,
				Source{Contract: vault.Address, Method: "userUnderlying"},
			)},
			Metadata: map[string]any{"vault": vault.Address},
		})
	}
	return groups, nil
}

func (a *RenzoAdapter) readEZETHWithdrawals(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.Number < 20_047_248 {
		return nil, nil
	}
	row, err := client.Call(
		ctx, block, renzoWithdrawQueue, renzoWithdrawQueueABI,
		"getOutstandingWithdrawRequests", account,
	)
	if err != nil {
		return nil, fmt.Errorf("withdrawal queue length: %w", err)
	}
	length, err := BigIntAt(row, 0)
	if err != nil {
		return nil, fmt.Errorf("withdrawal queue length: %w", err)
	}
	if !length.IsUint64() || length.Uint64() > renzoMaxWithdrawalRequests {
		return nil, fmt.Errorf("withdrawal queue length exceeds %d", renzoMaxWithdrawalRequests)
	}
	calls := make([]ContractCall, 0, length.Uint64())
	for index := uint64(0); index < length.Uint64(); index++ {
		calls = append(calls, ContractCall{
			Contract: renzoWithdrawQueue, ABI: renzoWithdrawQueueABI,
			Method: "withdrawRequests", Args: []any{account, new(big.Int).SetUint64(index)},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("withdrawal queue entries: %w", err)
	}
	groups := make([]Group, 0, len(rows))
	for index, entry := range rows {
		collateral, decodeErr := AddressAt(entry, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("withdrawal %d collateral: %w", index, decodeErr)
		}
		requestID, decodeErr := BigIntAt(entry, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("withdrawal %d id: %w", index, decodeErr)
		}
		amount, decodeErr := BigIntAt(entry, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("withdrawal %d amount: %w", index, decodeErr)
		}
		ezETHLocked, decodeErr := BigIntAt(entry, 3)
		if decodeErr != nil {
			return nil, fmt.Errorf("withdrawal %d ezETH amount: %w", index, decodeErr)
		}
		createdAt, decodeErr := BigIntAt(entry, 4)
		if decodeErr != nil {
			return nil, fmt.Errorf("withdrawal %d creation time: %w", index, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		asset := renzoETH
		if collateral != renzoNativeToken {
			asset, decodeErr = readERC20Token(ctx, client, block, collateral)
			if decodeErr != nil {
				return nil, fmt.Errorf("withdrawal %d collateral metadata: %w", index, decodeErr)
			}
		}
		component := NewComponent(
			"asset", asset, amount,
			Source{Contract: renzoWithdrawQueue, Method: "withdrawRequests.amountToRedeem"},
		)
		component.Metadata = map[string]any{
			"requestId": requestID.String(), "ezETHLocked": ezETHLocked.String(),
			"createdAt": createdAt.String(),
		}
		groups = append(groups, Group{
			ID: "ezeth-withdrawal:" + requestID.String(), Label: "ezETH withdrawal",
			Components: []Component{component}, Metadata: map[string]any{"queue": renzoWithdrawQueue},
		})
	}
	return groups, nil
}

func (a *RenzoAdapter) readLegacyStake(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.Number < 19_868_460 {
		return nil, nil
	}
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: renzoLegacyStake, ABI: renzoLegacyStakeABI, Method: "activeStake", Args: []any{account}},
		{Contract: renzoLegacyStake, ABI: renzoLegacyStakeABI, Method: "getUnstakeRequestsLength", Args: []any{account}},
	})
	if err != nil {
		return nil, fmt.Errorf("REZ staking header: %w", err)
	}
	active, err := BigIntAt(rows[0], 0)
	if err != nil {
		return nil, fmt.Errorf("REZ active stake: %w", err)
	}
	length, err := BigIntAt(rows[1], 0)
	if err != nil {
		return nil, fmt.Errorf("REZ unstake queue length: %w", err)
	}
	if !length.IsUint64() || length.Uint64() > renzoMaxWithdrawalRequests {
		return nil, fmt.Errorf("REZ unstake queue length exceeds %d", renzoMaxWithdrawalRequests)
	}
	groups := make([]Group, 0, 1+length.Uint64())
	if active.Sign() > 0 {
		groups = append(groups, Group{
			ID: "rez-stake", Label: "Staked REZ",
			Components: []Component{NewComponent(
				"asset", renzoREZ, active,
				Source{Contract: renzoLegacyStake, Method: "activeStake"},
			)},
		})
	}
	calls := make([]ContractCall, 0, length.Uint64())
	for index := uint64(0); index < length.Uint64(); index++ {
		calls = append(calls, ContractCall{
			Contract: renzoLegacyStake, ABI: renzoLegacyStakeABI,
			Method: "unstakeRequests", Args: []any{account, new(big.Int).SetUint64(index)},
		})
	}
	requests, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("REZ unstake requests: %w", err)
	}
	for index, request := range requests {
		timestamp, decodeErr := BigIntAt(request, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("REZ unstake request %d timestamp: %w", index, decodeErr)
		}
		amount, decodeErr := BigIntAt(request, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("REZ unstake request %d amount: %w", index, decodeErr)
		}
		if amount.Sign() == 0 {
			continue
		}
		component := NewComponent(
			"asset", renzoREZ, amount,
			Source{Contract: renzoLegacyStake, Method: "unstakeRequests.amount"},
		)
		component.Metadata = map[string]any{"unstakeTimestamp": timestamp.String()}
		groups = append(groups, Group{
			ID: fmt.Sprintf("rez-unstake:%d", index), Label: "REZ unstake",
			Components: []Component{component},
		})
	}
	return groups, nil
}

func (a *RenzoAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	var groups []Group
	var err error
	if block.ChainID == Ethereum {
		groups, err = a.readMainnetReceipts(ctx, client, block, account)
	} else {
		groups, err = readReceiptPositions(ctx, client, block, account, a.receipts[block.ChainID])
	}
	if err != nil {
		return nil, err
	}
	if block.ChainID != Ethereum {
		return groups, nil
	}
	for _, reader := range []func(context.Context, *RPCClient, BlockRef, common.Address) ([]Group, error){
		a.readEigenVaults,
		a.readEigenWithdrawals,
		a.readEZETHWithdrawals,
		a.readLegacyStake,
	} {
		positions, readErr := reader(ctx, client, block, account)
		if readErr != nil {
			return groups, readErr
		}
		groups = append(groups, positions...)
	}
	return groups, nil
}
