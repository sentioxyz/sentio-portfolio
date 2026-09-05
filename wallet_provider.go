package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ethereum/go-ethereum/common"
)

// WalletBalanceProvider discovers the native and ERC-20 balances held by a set of accounts.
// It is deliberately transport-neutral: the host owns the concrete service, credentials,
// pagination, retries and rate limits.
//
// The engine's RPC-settled block remains canonical. Implementations must return the exact block
// number and hash shared by every page and account for one chain: the kernel uses amounts directly
// only when that block matches its settled pin. Otherwise the result is token discovery and every
// discovered amount is re-read through RPC at the settled block.
type WalletBalanceProvider interface {
	WalletBalances(context.Context, WalletBalanceRequest) (WalletBalanceResult, error)
}

// WalletBalanceTarget identifies one account on one chain. Targets are unique and sorted by the
// kernel so providers can safely batch or cache an otherwise equivalent request.
type WalletBalanceTarget struct {
	ChainID ChainID
	Account common.Address
}

type WalletBalanceRequest struct {
	// RootAccount is the address passed to Engine.Scan. A provider with a smaller address batch
	// limit than Targets must always keep this account and may omit attributed accounts.
	RootAccount common.Address
	Targets     []WalletBalanceTarget
}

// WalletBalanceResult may contain verified account results together with failures and explicit
// clean fallbacks. A missing account result is not an empty wallet: WalletBalanceAccount with an
// empty Balances slice is the successful representation of an account with no holdings.
type WalletBalanceResult struct {
	Chains          []WalletBalanceChain
	Failures        []WalletBalanceFailure
	FallbackTargets []WalletBalanceFallback
}

// WalletBalanceFallbackReason explains why the provider deliberately left a requested target to
// the kernel's committed-manifest RPC path. Values are closed so a typo cannot silently suppress
// a missing-result error.
type WalletBalanceFallbackReason string

const (
	WalletBalanceFallbackUnsupportedChain WalletBalanceFallbackReason = "unsupported_chain"
	WalletBalanceFallbackAddressLimit     WalletBalanceFallbackReason = "address_limit"
)

// WalletBalanceFallback is an explicit, successful provider hand-off for exactly one requested
// account. Unsupported chains may include the root account. Address-limit fallback is reserved
// for attributed accounts: providers must always retain WalletBalanceRequest.RootAccount on a
// chain they support.
type WalletBalanceFallback struct {
	ChainID ChainID
	Account common.Address
	Reason  WalletBalanceFallbackReason
}

// WalletBalanceChain groups every account result that was observed at one provider block.
type WalletBalanceChain struct {
	Block    BlockRef
	Accounts []WalletBalanceAccount
}

type WalletBalanceAccount struct {
	Account  common.Address
	Balances []WalletBalance
}

// WalletBalance is a non-negative integer amount in the token's smallest unit. For Native
// balances the kernel preserves its existing wrapped-token pricing identity, so Token.Address
// and provider metadata are ignored while Token.ChainID must still match the enclosing chain.
//
// MetadataComplete distinguishes a legitimate zero-decimal ERC-20 from a provider response
// whose decimals were absent. When metadata is incomplete, the kernel first consults its
// committed manifest and otherwise reads the token at the verified canonical block.
type WalletBalance struct {
	Token            Token
	AmountRaw        string
	Native           bool
	MetadataComplete bool
}

// WalletBalanceFailure reports a partial provider failure. Account may be zero for a chain-wide
// failure and Asset may be nil when no single token caused it. Implementations must return only
// public, credential-free messages.
type WalletBalanceFailure struct {
	ChainID ChainID
	Account common.Address
	Asset   *AssetID
	Message string
}

type walletProviderAccount struct {
	balances   []WalletBalance
	exactBlock bool
}

// configureLiveWalletBalances installs provider results as either exact balances or discovery.
// The RPC-settled block remains authoritative: only a provider result with the same number and
// hash may supply amounts directly. Results from a newer block still discover token addresses,
// whose balances the wallet adapter re-reads at the settled block.
func configureLiveWalletBalances(
	ctx context.Context,
	provider WalletBalanceProvider,
	root common.Address,
	chains map[ChainID]*chainScan,
) {
	if provider == nil {
		return
	}

	targets := make([]WalletBalanceTarget, 0)
	requested := make(map[ChainID]map[common.Address]attributedAccount)
	for _, chainID := range SupportedChainIDs {
		chain := chains[chainID]
		if chain == nil || chain.block.Fixed {
			continue
		}
		requested[chainID] = make(map[common.Address]attributedAccount, len(chain.accounts))
		for _, account := range chain.accounts {
			requested[chainID][account.Address] = account
			targets = append(targets, WalletBalanceTarget{ChainID: chainID, Account: account.Address})
		}
	}
	if len(targets) == 0 {
		return
	}
	sort.Slice(targets, func(left, right int) bool {
		if targets[left].ChainID != targets[right].ChainID {
			return targets[left].ChainID < targets[right].ChainID
		}
		return strings.ToLower(targets[left].Account.Hex()) <
			strings.ToLower(targets[right].Account.Hex())
	})

	result, providerErr := provider.WalletBalances(ctx, WalletBalanceRequest{
		RootAccount: root,
		Targets:     targets,
	})
	if providerErr != nil {
		for chainID := range requested {
			chains[chainID].walletProviderErrors = append(
				chains[chainID].walletProviderErrors,
				fmt.Errorf("wallet balance provider: %w", redactEndpoints(providerErr)),
			)
		}
		// A request-level error cannot prove which parts of a returned payload are complete.
		// Keep the existing manifest path for every target rather than trusting partial data.
		return
	}

	providerContractError := func(chainID ChainID, message string) {
		err := errors.New("wallet balance provider contract: " + message)
		if requested[chainID] != nil {
			chains[chainID].walletProviderErrors = append(chains[chainID].walletProviderErrors, err)
			return
		}
		// An unknown chain cannot be attributed to one scan. Report it on every requested chain
		// instead of silently accepting an incomplete provider response.
		for requestedChainID := range requested {
			chains[requestedChainID].walletProviderErrors = append(
				chains[requestedChainID].walletProviderErrors,
				err,
			)
		}
	}
	requestedTarget := func(target WalletBalanceTarget) bool {
		accounts := requested[target.ChainID]
		if accounts == nil {
			return false
		}
		_, exists := accounts[target.Account]
		return exists
	}
	fallbackReasons := make(map[WalletBalanceTarget]WalletBalanceFallbackReason)
	invalidTargets := make(map[WalletBalanceTarget]struct{})
	seenFallbacks := make(map[WalletBalanceTarget]struct{})
	for _, fallback := range result.FallbackTargets {
		target := WalletBalanceTarget{ChainID: fallback.ChainID, Account: fallback.Account}
		if !requestedTarget(target) {
			providerContractError(
				fallback.ChainID,
				fmt.Sprintf("fallback target %d/%s was not requested", fallback.ChainID, fallback.Account.Hex()),
			)
			continue
		}
		if _, duplicate := seenFallbacks[target]; duplicate {
			delete(fallbackReasons, target)
			invalidTargets[target] = struct{}{}
			providerContractError(
				fallback.ChainID,
				fmt.Sprintf("duplicate fallback target %d/%s", fallback.ChainID, fallback.Account.Hex()),
			)
			continue
		}
		seenFallbacks[target] = struct{}{}
		switch fallback.Reason {
		case WalletBalanceFallbackUnsupportedChain:
			fallbackReasons[target] = fallback.Reason
		case WalletBalanceFallbackAddressLimit:
			if fallback.Account == root {
				invalidTargets[target] = struct{}{}
				providerContractError(
					fallback.ChainID,
					fmt.Sprintf("root account %s cannot use address-limit fallback", root.Hex()),
				)
				continue
			}
			fallbackReasons[target] = fallback.Reason
		default:
			invalidTargets[target] = struct{}{}
			providerContractError(
				fallback.ChainID,
				fmt.Sprintf("fallback target %d/%s has an unknown reason", fallback.ChainID, fallback.Account.Hex()),
			)
		}
	}

	failedChains := make(map[ChainID]struct{})
	failedAccounts := make(map[ChainID]map[common.Address]struct{})
	for _, failure := range result.Failures {
		chain := chains[failure.ChainID]
		if chain == nil || chain.block.Fixed || requested[failure.ChainID] == nil {
			continue
		}
		chain.walletProviderErrors = append(
			chain.walletProviderErrors,
			walletBalanceFailureError(failure),
		)
		if failure.Account == (common.Address{}) {
			failedChains[failure.ChainID] = struct{}{}
			for target := range fallbackReasons {
				if target.ChainID != failure.ChainID {
					continue
				}
				delete(fallbackReasons, target)
				invalidTargets[target] = struct{}{}
				providerContractError(
					failure.ChainID,
					fmt.Sprintf("fallback target %d/%s overlaps a chain failure", target.ChainID, target.Account.Hex()),
				)
			}
			continue
		}
		target := WalletBalanceTarget{ChainID: failure.ChainID, Account: failure.Account}
		if _, overlaps := fallbackReasons[target]; overlaps {
			delete(fallbackReasons, target)
			invalidTargets[target] = struct{}{}
			providerContractError(
				failure.ChainID,
				fmt.Sprintf("fallback target %d/%s overlaps a failure", target.ChainID, target.Account.Hex()),
			)
		}
		if failedAccounts[failure.ChainID] == nil {
			failedAccounts[failure.ChainID] = make(map[common.Address]struct{})
		}
		failedAccounts[failure.ChainID][failure.Account] = struct{}{}
	}

	byChain := make(map[ChainID]WalletBalanceChain, len(result.Chains))
	duplicateChains := make(map[ChainID]struct{})
	for _, providerChain := range result.Chains {
		chainID := providerChain.Block.ChainID
		if requested[chainID] == nil {
			continue
		}
		if _, exists := byChain[chainID]; exists {
			duplicateChains[chainID] = struct{}{}
			continue
		}
		byChain[chainID] = providerChain
	}
	targetHandledWithoutResult := func(chainID ChainID, account common.Address) bool {
		target := WalletBalanceTarget{ChainID: chainID, Account: account}
		if _, fallback := fallbackReasons[target]; fallback {
			return true
		}
		if _, invalid := invalidTargets[target]; invalid {
			return true
		}
		_, failed := failedAccounts[chainID][account]
		return failed
	}

	for chainID, targetAccounts := range requested {
		chain := chains[chainID]
		if _, failed := failedChains[chainID]; failed {
			continue
		}
		providerChain, exists := byChain[chainID]
		if !exists {
			for account := range targetAccounts {
				if targetHandledWithoutResult(chainID, account) {
					continue
				}
				chain.walletProviderErrors = append(
					chain.walletProviderErrors,
					errors.New("wallet balance provider returned no chain result"),
				)
				break
			}
			continue
		}
		if _, duplicate := duplicateChains[chainID]; duplicate {
			chain.walletProviderErrors = append(
				chain.walletProviderErrors,
				errors.New("wallet balance provider returned duplicate chain results"),
			)
			continue
		}
		resultAccounts := make(map[common.Address]WalletBalanceAccount, len(providerChain.Accounts))
		duplicateAccounts := make(map[common.Address]struct{})
		for _, resultAccount := range providerChain.Accounts {
			if _, wanted := targetAccounts[resultAccount.Account]; !wanted {
				chain.walletProviderErrors = append(
					chain.walletProviderErrors,
					fmt.Errorf(
						"wallet balance provider returned unexpected account %s",
						resultAccount.Account.Hex(),
					),
				)
				continue
			}
			target := WalletBalanceTarget{ChainID: chainID, Account: resultAccount.Account}
			if _, fallback := fallbackReasons[target]; fallback {
				delete(fallbackReasons, target)
				invalidTargets[target] = struct{}{}
				providerContractError(
					chainID,
					fmt.Sprintf("fallback target %d/%s also has an account result", chainID, resultAccount.Account.Hex()),
				)
				continue
			}
			if _, invalid := invalidTargets[target]; invalid {
				continue
			}
			if _, failed := failedAccounts[chainID][resultAccount.Account]; failed {
				continue
			}
			if _, duplicate := duplicateAccounts[resultAccount.Account]; duplicate {
				continue
			}
			if _, duplicate := resultAccounts[resultAccount.Account]; duplicate {
				chain.walletProviderErrors = append(
					chain.walletProviderErrors,
					fmt.Errorf(
						"wallet balance provider returned duplicate account %s",
						resultAccount.Account.Hex(),
					),
				)
				delete(resultAccounts, resultAccount.Account)
				duplicateAccounts[resultAccount.Account] = struct{}{}
				continue
			}
			resultAccounts[resultAccount.Account] = resultAccount
		}
		if len(resultAccounts) == 0 {
			for account := range targetAccounts {
				if targetHandledWithoutResult(chainID, account) {
					continue
				}
				if _, duplicate := duplicateAccounts[account]; duplicate {
					continue
				}
				chain.walletProviderErrors = append(
					chain.walletProviderErrors,
					errors.New("wallet balance provider returned no account results"),
				)
				break
			}
			continue
		}

		blockMetadataComplete := providerChain.Block.Number != 0 &&
			providerChain.Block.Hash != (common.Hash{})
		if !blockMetadataComplete {
			chain.walletProviderErrors = append(
				chain.walletProviderErrors,
				errors.New("wallet balance provider returned incomplete block metadata"),
			)
		}
		exactBlock := blockMetadataComplete &&
			providerChain.Block.Number == chain.block.Number &&
			providerChain.Block.Hash == chain.block.Hash
		// A different height is the expected live case. The same height with another hash is a
		// reorg or inconsistent response and deserves an explicit partial error.
		if blockMetadataComplete && providerChain.Block.Number == chain.block.Number &&
			providerChain.Block.Hash != chain.block.Hash {
			chain.walletProviderErrors = append(
				chain.walletProviderErrors,
				fmt.Errorf(
					"wallet balance block hash mismatch at block %d",
					providerChain.Block.Number,
				),
			)
		}
		if exactBlock && providerChain.Block.Timestamp != 0 &&
			providerChain.Block.Timestamp != chain.block.Timestamp {
			chain.walletProviderErrors = append(
				chain.walletProviderErrors,
				fmt.Errorf(
					"wallet balance block timestamp mismatch at block %d",
					providerChain.Block.Number,
				),
			)
			exactBlock = false
		}

		providerAccounts := make(map[common.Address]walletProviderAccount, len(resultAccounts))
		for account, resultAccount := range resultAccounts {
			providerAccounts[account] = walletProviderAccount{
				balances:   resultAccount.Balances,
				exactBlock: exactBlock,
			}
		}
		chain.walletProviderAccounts = providerAccounts
		for _, account := range chain.accounts {
			if _, exists := providerAccounts[account.Address]; !exists {
				if targetHandledWithoutResult(chainID, account.Address) {
					continue
				}
				if _, alreadyReported := duplicateAccounts[account.Address]; alreadyReported {
					continue
				}
				chain.walletProviderErrors = append(
					chain.walletProviderErrors,
					fmt.Errorf(
						"wallet balance provider returned no result for account %s",
						account.Address.Hex(),
					),
				)
			}
		}
	}
}

func walletBalanceFailureError(failure WalletBalanceFailure) error {
	message := failure.Message
	if message == "" {
		message = "unspecified failure"
	}
	prefix := "wallet balance provider"
	if failure.Account != (common.Address{}) {
		prefix += " account " + failure.Account.Hex()
	}
	if failure.Asset != nil {
		prefix += " token " + failure.Asset.Address.Hex()
	}
	return fmt.Errorf("%s: %w", prefix, redactEndpoints(errors.New(message)))
}

func providerWalletGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	chainID ChainID,
	accountAddress common.Address,
	account walletProviderAccount,
) ([]Group, error) {
	if account.exactBlock {
		return directProviderWalletGroups(ctx, client, block, chainID, account)
	}
	return discoveredProviderWalletGroups(
		ctx,
		client,
		block,
		chainID,
		accountAddress,
		account,
	)
}

func directProviderWalletGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	chainID ChainID,
	account walletProviderAccount,
) ([]Group, error) {
	groups := make([]Group, 0, len(account.balances))
	failures := make([]error, 0)
	seenTokens := make(map[common.Address]struct{})
	nativeSeen := false
	for _, balance := range account.balances {
		amount, err := parseWalletBalanceAmount(balance.AmountRaw)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		// Provider pages commonly include zero rows. Drop them before metadata validation or
		// enrichment so an empty holding never causes extra RPC work or a spurious error.
		if amount.Sign() == 0 {
			continue
		}
		if balance.Token.ChainID != chainID {
			failures = append(failures, fmt.Errorf(
				"wallet balance token chain %d does not match chain %d",
				balance.Token.ChainID,
				chainID,
			))
			continue
		}
		if balance.Native {
			if nativeSeen {
				failures = append(failures, errors.New("wallet balance provider returned duplicate native balance"))
				continue
			}
			nativeSeen = true
			coin, exists := walletNativeCoin[chainID]
			if !exists {
				failures = append(failures, fmt.Errorf("chain %d has no native coin identity", chainID))
				continue
			}
			component := NewComponent(
				"asset",
				Token{
					ChainID: chainID, Address: coin.Wrapped, Symbol: coin.Symbol, Decimals: 18,
				},
				amount,
				Source{Method: "eth_getBalance"},
			)
			component.Metadata = map[string]any{"native": true}
			groups = append(groups, Group{
				ID:         walletNativeGroupID,
				Label:      coin.Symbol,
				Components: []Component{component},
				Metadata:   map[string]any{"holding": "native"},
			})
			continue
		}

		if balance.Token.Address == (common.Address{}) {
			failures = append(failures, fmt.Errorf(
				"wallet balance provider returned an empty token address",
			))
			continue
		}
		if _, duplicate := seenTokens[balance.Token.Address]; duplicate {
			failures = append(failures, fmt.Errorf(
				"wallet balance provider returned duplicate token %s",
				balance.Token.Address.Hex(),
			))
			continue
		}
		seenTokens[balance.Token.Address] = struct{}{}
		token, metadataErr := providerWalletToken(
			ctx,
			client,
			block,
			balance.Token,
			balance.MetadataComplete,
		)
		if metadataErr != nil {
			failures = append(failures, fmt.Errorf(
				"wallet token %s metadata: %w",
				balance.Token.Address.Hex(),
				metadataErr,
			))
			continue
		}
		groups = append(groups, Group{
			ID:    walletTokenGroupID(token.Address),
			Label: token.Symbol,
			Components: []Component{NewComponent(
				"asset",
				token,
				amount,
				Source{Contract: token.Address, Method: "balanceOf"},
			)},
			Metadata: map[string]any{"holding": "token"},
		})
	}
	sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
	return groups, errors.Join(failures...)
}

// discoveredProviderWalletGroups treats provider rows as token discovery only and re-reads
// every amount at the engine's settled block. Zero provider rows remain candidates: the balance
// may be non-zero at the earlier settled block even when it is zero at the provider's latest.
// The manifest is unioned into discovery so this path can never cover fewer known assets than the
// pre-provider implementation.
func discoveredProviderWalletGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	chainID ChainID,
	accountAddress common.Address,
	account walletProviderAccount,
) ([]Group, error) {
	groups := make([]Group, 0, len(account.balances))
	failures := make([]error, 0)

	coin, nativeSupported := walletNativeCoin[chainID]
	if nativeSupported {
		amount, err := client.NativeBalance(ctx, block, accountAddress)
		if err != nil {
			failures = append(failures, fmt.Errorf("%s balance: %w", coin.Symbol, err))
		} else if amount.Sign() != 0 {
			component := NewComponent(
				"asset",
				Token{
					ChainID: chainID, Address: coin.Wrapped, Symbol: coin.Symbol, Decimals: 18,
				},
				amount,
				Source{Method: "eth_getBalance"},
			)
			component.Metadata = map[string]any{"native": true}
			groups = append(groups, Group{
				ID:         walletNativeGroupID,
				Label:      coin.Symbol,
				Components: []Component{component},
				Metadata:   map[string]any{"holding": "native"},
			})
		}
	}

	candidates := make([]WalletBalance, 0, len(account.balances))
	seen := make(map[common.Address]struct{})
	for _, balance := range account.balances {
		// A mismatched provider block means this amount is not used, but malformed data still
		// signals a degraded response. Keep a valid address as a discovery candidate so the
		// settled-block RPC read can recover the actual balance.
		if _, err := parseWalletBalanceAmount(balance.AmountRaw); err != nil {
			failures = append(failures, err)
		}
		if balance.Native {
			continue
		}
		if balance.Token.ChainID != chainID {
			failures = append(failures, fmt.Errorf(
				"wallet balance token chain %d does not match chain %d",
				balance.Token.ChainID,
				chainID,
			))
			continue
		}
		if balance.Token.Address == (common.Address{}) {
			failures = append(failures, errors.New(
				"wallet balance provider returned an empty token address",
			))
			continue
		}
		if _, duplicate := seen[balance.Token.Address]; duplicate {
			failures = append(failures, fmt.Errorf(
				"wallet balance provider returned duplicate token %s",
				balance.Token.Address.Hex(),
			))
			continue
		}
		seen[balance.Token.Address] = struct{}{}
		candidates = append(candidates, balance)
	}
	for _, entry := range walletTokens.Tokens {
		if entry.ChainID != chainID {
			continue
		}
		if _, exists := seen[entry.Address]; exists {
			continue
		}
		seen[entry.Address] = struct{}{}
		candidates = append(candidates, WalletBalance{
			Token:            entry.token(),
			AmountRaw:        "0",
			MetadataComplete: true,
		})
	}
	if len(candidates) == 0 {
		sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
		return groups, errors.Join(failures...)
	}

	calls := make([]ContractCall, len(candidates))
	for index, candidate := range candidates {
		calls[index] = ContractCall{
			Contract: candidate.Token.Address,
			ABI:      erc20ABI,
			Method:   "balanceOf",
			Args:     []any{accountAddress},
		}
	}
	rows, err := client.ParallelCallsAllowFailure(ctx, block, calls)
	if err != nil {
		return groups, errors.Join(
			errors.Join(failures...),
			fmt.Errorf("wallet balances at settled block: %w", err),
		)
	}
	for index, row := range rows {
		candidate := candidates[index]
		if row.Error != nil {
			failures = append(failures, fmt.Errorf(
				"%s balance: %w",
				candidate.Token.Address.Hex(),
				row.Error,
			))
			continue
		}
		amount, decodeErr := BigIntAt(row.Values, 0)
		if decodeErr != nil {
			failures = append(failures, fmt.Errorf(
				"%s balance: %w",
				candidate.Token.Address.Hex(),
				decodeErr,
			))
			continue
		}
		if amount.Sign() == 0 {
			continue
		}
		token, metadataErr := providerWalletToken(
			ctx,
			client,
			block,
			candidate.Token,
			candidate.MetadataComplete,
		)
		if metadataErr != nil {
			failures = append(failures, fmt.Errorf(
				"wallet token %s metadata: %w",
				candidate.Token.Address.Hex(),
				metadataErr,
			))
			continue
		}
		groups = append(groups, Group{
			ID:    walletTokenGroupID(token.Address),
			Label: token.Symbol,
			Components: []Component{NewComponent(
				"asset",
				token,
				amount,
				Source{Contract: token.Address, Method: "balanceOf"},
			)},
			Metadata: map[string]any{"holding": "token"},
		})
	}
	sort.Slice(groups, func(left, right int) bool { return groups[left].ID < groups[right].ID })
	return groups, errors.Join(failures...)
}

var walletManifestTokens = func() map[AssetID]Token {
	tokens := make(map[AssetID]Token, len(walletTokens.Tokens))
	for _, entry := range walletTokens.Tokens {
		token := entry.token()
		tokens[AssetForToken(token)] = token
	}
	return tokens
}()

const maxWalletTokenSymbolBytes = 64

func providerWalletToken(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	provided Token,
	metadataComplete bool,
) (Token, error) {
	if token, exists := walletManifestTokens[AssetForToken(provided)]; exists {
		return token, nil
	}
	if metadataComplete && provided.Decimals <= 36 {
		symbol, err := validatedWalletTokenSymbol(provided.Symbol)
		if err == nil {
			provided.Symbol = symbol
			return provided, nil
		}
	}
	if client == nil {
		return Token{}, errors.New("RPC client is not configured for metadata enrichment")
	}
	token, err := readToken(ctx, client, block, provided.Address)
	if err != nil {
		return Token{}, err
	}
	if token.Decimals > 36 {
		return Token{}, errors.New("on-chain token decimals are invalid")
	}
	token.Symbol, err = validatedWalletTokenSymbol(token.Symbol)
	if err != nil {
		return Token{}, fmt.Errorf("on-chain token symbol: %w", err)
	}
	return token, nil
}

func validatedWalletTokenSymbol(symbol string) (string, error) {
	if !utf8.ValidString(symbol) {
		return "", errors.New("token symbol is not valid UTF-8")
	}
	for _, character := range symbol {
		if unicode.IsControl(character) || unicode.In(character, unicode.Cf) {
			return "", errors.New("token symbol contains a control or format character")
		}
	}
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return "", errors.New("token symbol is empty")
	}
	if len(symbol) > maxWalletTokenSymbolBytes {
		return "", fmt.Errorf("token symbol exceeds %d bytes", maxWalletTokenSymbolBytes)
	}
	return symbol, nil
}

func parseWalletBalanceAmount(raw string) (*big.Int, error) {
	if raw == "" {
		return nil, errors.New("wallet balance provider returned an empty raw amount")
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return nil, fmt.Errorf("wallet balance provider returned invalid raw amount %q", raw)
		}
	}
	amount, exists := new(big.Int).SetString(raw, 10)
	if !exists {
		return nil, fmt.Errorf("wallet balance provider returned invalid raw amount %q", raw)
	}
	return amount, nil
}
