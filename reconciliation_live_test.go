package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type reconciliationCorpus struct {
	ProtocolID string  `json:"protocolId"`
	ChainID    ChainID `json:"chainId"`
	Accounts   []struct {
		Address            string `json:"address"`
		AlignedBlock       uint64 `json:"alignedBlock"`
		OracleEvidencePath string `json:"oracleEvidencePath"`
	} `json:"accounts"`
}

type reconciliationEvidence struct {
	ProjectID string `json:"projectId"`
	Response  struct {
		Data []struct {
			Chain             string `json:"chain"`
			ID                string `json:"id"`
			PortfolioItemList []struct {
				AssetDict      map[string]json.RawMessage `json:"asset_dict"`
				AssetTokenList []struct {
					ID    string  `json:"id"`
					Price float64 `json:"price"`
				} `json:"asset_token_list"`
			} `json:"portfolio_item_list"`
		} `json:"data"`
	} `json:"response"`
}

var reconciliationChainNames = map[ChainID]string{
	Ethereum: "eth",
	BSC:      "bsc",
	Base:     "base",
	Arbitrum: "arb",
}

var reconciliationNativeTokens = map[ChainID]common.Address{
	Ethereum: common.HexToAddress("0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2"),
	BSC:      common.HexToAddress("0xbb4CdB9CBd36B01bD1cBaEBF2De08d9173bc095c"),
	Base:     common.HexToAddress("0x4200000000000000000000000000000000000006"),
	Arbitrum: common.HexToAddress("0x82aF49447D8a07e3bd95BD0d56f35241523fBab1"),
}

func reconciliationTokenID(chainID ChainID, value string) (string, bool) {
	if common.IsHexAddress(value) {
		return strings.ToLower(common.HexToAddress(value).Hex()), true
	}
	native := strings.ToLower(strings.TrimSpace(value))
	if native == "eth" || (chainID == BSC && (native == "bnb" || native == "bsc")) {
		return strings.ToLower(reconciliationNativeTokens[chainID].Hex()), true
	}
	return "", false
}

func reconciliationAdd(target map[string]*big.Rat, token string, amount *big.Rat) {
	token = strings.ToLower(common.HexToAddress(token).Hex())
	if current := target[token]; current != nil {
		current.Add(current, amount)
		return
	}
	target[token] = new(big.Rat).Set(amount)
}

func reconciliationExpected(
	t *testing.T,
	path string,
	chainID ChainID,
) (map[string]*big.Rat, map[string]float64) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return reconciliationExpectedPayload(t, payload, chainID)
}

func reconciliationExpectedPayload(
	t *testing.T,
	payload []byte,
	chainID ChainID,
) (map[string]*big.Rat, map[string]float64) {
	t.Helper()
	var evidence reconciliationEvidence
	if err := json.Unmarshal(payload, &evidence); err != nil {
		t.Fatal(err)
	}
	amounts := make(map[string]*big.Rat)
	prices := make(map[string]float64)
	matched := false
	for _, project := range evidence.Response.Data {
		if project.ID != evidence.ProjectID || project.Chain != reconciliationChainNames[chainID] {
			continue
		}
		matched = true
		for _, item := range project.PortfolioItemList {
			for _, token := range item.AssetTokenList {
				if tokenID, ok := reconciliationTokenID(chainID, token.ID); ok && token.Price > 0 {
					prices[tokenID] = token.Price
				}
			}
			for token, raw := range item.AssetDict {
				tokenID, ok := reconciliationTokenID(chainID, token)
				if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					continue
				}
				amount, valid := new(big.Rat).SetString(string(raw))
				if !valid {
					t.Fatalf("invalid DeBank amount %s for %s", raw, token)
				}
				reconciliationAdd(amounts, tokenID, amount)
			}
		}
	}
	if !matched {
		t.Fatalf("DeBank evidence has no %s project on chain %s", evidence.ProjectID, reconciliationChainNames[chainID])
	}
	return amounts, prices
}

func reconciliationActual(t *testing.T, groups []Group) map[string]*big.Rat {
	t.Helper()
	amounts := make(map[string]*big.Rat)
	for _, group := range groups {
		for _, component := range group.Components {
			raw, ok := new(big.Int).SetString(component.AmountRaw, 10)
			if !ok {
				t.Fatalf("invalid component amount %q", component.AmountRaw)
			}
			denominator := big.NewInt(1)
			if component.AmountDenominatorRaw != "" {
				denominator, ok = new(big.Int).SetString(component.AmountDenominatorRaw, 10)
				if !ok || denominator.Sign() <= 0 {
					t.Fatalf("invalid component denominator %q", component.AmountDenominatorRaw)
				}
			}
			scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(component.Token.Decimals)), nil)
			amount := new(big.Rat).SetInt(raw)
			amount.Quo(amount, new(big.Rat).SetInt(denominator))
			amount.Quo(amount, new(big.Rat).SetInt(scale))
			if component.Kind == "debt" {
				amount.Neg(amount)
			}
			reconciliationAdd(amounts, component.Token.Address.Hex(), amount)
		}
	}
	return amounts
}

func reconciliationDiffs(
	expected map[string]*big.Rat,
	actual map[string]*big.Rat,
	prices map[string]float64,
) []string {
	return reconciliationDiffsWithRelativeTolerance(expected, actual, prices, 1e-12)
}

func reconciliationDiffsWithRelativeTolerance(
	expected map[string]*big.Rat,
	actual map[string]*big.Rat,
	prices map[string]float64,
	relativeTolerance float64,
) []string {
	keySet := make(map[string]struct{}, len(expected)+len(actual))
	for key := range expected {
		keySet[key] = struct{}{}
	}
	for key := range actual {
		keySet[key] = struct{}{}
	}
	keys := make([]string, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var result []string
	for _, key := range keys {
		expectedValue := expected[key]
		if expectedValue == nil {
			expectedValue = new(big.Rat)
		}
		actualValue := actual[key]
		if actualValue == nil {
			actualValue = new(big.Rat)
		}
		expectedFloat, _ := expectedValue.Float64()
		actualFloat, _ := actualValue.Float64()
		difference := math.Abs(actualFloat - expectedFloat)
		tolerance := 0.000005 + math.Abs(expectedFloat)*relativeTolerance
		if price := prices[key]; price > 0 {
			tolerance = math.Max(tolerance, 0.01/price)
		}
		if difference > tolerance {
			result = append(result, fmt.Sprintf(
				"%s expected=%.18g actual=%.18g diff=%.9g tolerance=%.9g",
				key, expectedFloat, actualFloat, difference, tolerance,
			))
		}
	}
	return result
}

func runReconciliationCorpus(
	t *testing.T,
	root string,
	rpcURL string,
	corpusPath string,
	adapter Adapter,
) {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join(root, corpusPath))
	if err != nil {
		t.Fatal(err)
	}
	var corpus reconciliationCorpus
	if err := json.Unmarshal(payload, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.ProtocolID != adapter.Info().ID {
		t.Fatalf("corpus protocol %s does not match adapter %s", corpus.ProtocolID, adapter.Info().ID)
	}
	ctx := context.Background()
	client, err := DialRPC(ctx, corpus.ChainID, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if len(corpus.Accounts) < 20 {
		t.Fatalf("corpus has %d accounts; need at least 20", len(corpus.Accounts))
	}
	for _, account := range corpus.Accounts {
		account := account
		t.Run(strings.ToLower(account.Address), func(t *testing.T) {
			block, err := client.BlockByNumber(ctx, account.AlignedBlock)
			if err != nil {
				t.Fatal(err)
			}
			groups, err := adapter.Positions(ctx, client, block, common.HexToAddress(account.Address))
			if err != nil {
				t.Fatal(err)
			}
			expected, prices := reconciliationExpected(
				t, filepath.Join(root, account.OracleEvidencePath), corpus.ChainID,
			)
			if differences := reconciliationDiffs(expected, reconciliationActual(t, groups), prices); len(differences) > 0 {
				t.Fatalf("token reconciliation failed:\n%s", strings.Join(differences, "\n"))
			}
		})
	}
}

func TestDeBankReconciliationCorpora(t *testing.T) {
	if os.Getenv("PORTFOLIO_RECONCILIATION_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_RECONCILIATION_LIVE_TEST=1 to replay DeBank corpora")
	}
	root := os.Getenv("PORTFOLIO_RECONCILIATION_ROOT")
	rpcURL := os.Getenv("PORTFOLIO_ETH_RPC_URL")
	if root == "" || rpcURL == "" {
		t.Fatal("PORTFOLIO_RECONCILIATION_ROOT and PORTFOLIO_ETH_RPC_URL are required")
	}
	t.Run("crvUSD", func(t *testing.T) {
		runReconciliationCorpus(
			t, root, rpcURL, "fixtures/accounts/crvusd.ethereum.json", newCurveCrvUSDAdapter(),
		)
	})
	t.Run("CurveLending", func(t *testing.T) {
		runReconciliationCorpus(
			t, root, rpcURL, "fixtures/accounts/curve-lending.ethereum.json", newCurveLendingAdapter(),
		)
	})
}

// debankFetcher retrieves one account's position payload for a single DeBank project.
type debankFetcher func(
	t *testing.T,
	ctx context.Context,
	httpClient *http.Client,
	accessKey string,
	account string,
	projectID string,
	chainID ChainID,
) json.RawMessage

func reconcileDeBankAPIAccount(
	t *testing.T,
	ctx context.Context,
	rpcClient *RPCClient,
	httpClient *http.Client,
	accessKey string,
	projectID string,
	adapter Adapter,
	chainID ChainID,
	account string,
	fetch debankFetcher,
) {
	t.Helper()
	payload := fetch(t, ctx, httpClient, accessKey, account, projectID, chainID)
	oracleTimestamp := requireFreshDeBankProtocol(t, payload, projectID, chainID)
	wrapped := []byte(fmt.Sprintf(
		`{"projectId":%q,"response":{"data":[%s]}}`,
		projectID,
		payload,
	))
	expected, prices := reconciliationExpectedPayload(t, wrapped, chainID)
	block, err := reconciliationBlock(ctx, rpcClient, oracleTimestamp)
	if err != nil {
		t.Fatal(err)
	}
	groups, err := adapter.Positions(ctx, rpcClient, block, common.HexToAddress(account))
	if err != nil {
		t.Fatal(err)
	}
	// DeBank refreshes asset_dict and token exchange rates on independent caches. Even after
	// aligning to update_at, its internal snapshot can straddle nearby blocks, so live comparisons
	// allow 10 ppm quantity drift. Fixed-block corpora above retain the stricter 1e-12 tolerance.
	if differences := reconciliationDiffsWithRelativeTolerance(
		expected, reconciliationActual(t, groups), prices, 1e-5,
	); len(differences) > 0 {
		t.Fatalf(
			"%s DeBank API reconciliation failed at block %d:\n%s",
			projectID, block.Number, strings.Join(differences, "\n"),
		)
	}
}

// Some RPC load balancers expose a freshly observed block through eth_getBlockByNumber a few
// seconds before every eth_call/eth_getLogs backend has caught up. Live reconciliation may opt
// into a small explicit lag; production scans remain pinned to the caller's exact block.
func reconciliationBlock(ctx context.Context, rpcClient *RPCClient, oracleTimestamp uint64) (BlockRef, error) {
	block, err := rpcClient.LatestBlock(ctx)
	if err != nil {
		return BlockRef{}, err
	}
	if oracleTimestamp > 0 && oracleTimestamp < block.Timestamp {
		// The DeBank list endpoint is cached. Compare its snapshot to the greatest canonical
		// block at or before update_at instead of disguising cache drift as a loose quantity
		// tolerance. The freshness check bounds this search to at most one hour.
		lowNumber := uint64(0)
		if block.Number > 100_000 {
			lowNumber = block.Number - 100_000
		}
		low, lowErr := rpcClient.BlockByNumber(ctx, lowNumber)
		if lowErr != nil {
			return BlockRef{}, lowErr
		}
		if low.Timestamp > oracleTimestamp {
			return BlockRef{}, fmt.Errorf(
				"DeBank timestamp %d is outside the reconciliation search window", oracleTimestamp,
			)
		}
		highNumber := block.Number
		for lowNumber+1 < highNumber {
			middleNumber := lowNumber + (highNumber-lowNumber)/2
			middle, middleErr := rpcClient.BlockByNumber(ctx, middleNumber)
			if middleErr != nil {
				return BlockRef{}, middleErr
			}
			if middle.Timestamp <= oracleTimestamp {
				lowNumber = middleNumber
				low = middle
			} else {
				highNumber = middleNumber
			}
		}
		return low, nil
	}
	lagText := os.Getenv("PORTFOLIO_RECONCILIATION_HEAD_LAG_BLOCKS")
	if lagText == "" {
		return block, nil
	}
	lag, err := strconv.ParseUint(lagText, 10, 64)
	if err != nil || lag == 0 || lag >= block.Number {
		return BlockRef{}, fmt.Errorf("invalid PORTFOLIO_RECONCILIATION_HEAD_LAG_BLOCKS %q", lagText)
	}
	return rpcClient.BlockByNumber(ctx, block.Number-lag)
}

func requireFreshDeBankProtocol(
	t *testing.T,
	payload []byte,
	projectID string,
	chainID ChainID,
) uint64 {
	t.Helper()
	var freshness struct {
		ID                string `json:"id"`
		Chain             string `json:"chain"`
		PortfolioItemList []struct {
			UpdatedAt float64 `json:"update_at"`
		} `json:"portfolio_item_list"`
	}
	if err := json.Unmarshal(payload, &freshness); err != nil {
		t.Fatal(err)
	}
	if freshness.ID != projectID || freshness.Chain != reconciliationChainNames[chainID] || len(freshness.PortfolioItemList) < 1 {
		t.Fatalf(
			"unexpected DeBank %s response: id=%s chain=%s items=%d",
			projectID, freshness.ID, freshness.Chain, len(freshness.PortfolioItemList),
		)
	}
	latestUpdate := float64(0)
	for _, item := range freshness.PortfolioItemList {
		latestUpdate = math.Max(latestUpdate, item.UpdatedAt)
	}
	// all_complex_protocol_list is DeBank's fast cached portfolio surface. Unlike the much slower
	// single-protocol endpoint it does not force a refresh, so accept one cache cycle while still
	// rejecting old fixtures or an unhealthy oracle. Quantity comparison below tolerates only 10
	// ppm, which remains the actual acceptance gate for interest-bearing positions.
	if float64(time.Now().Unix())-latestUpdate > 3600 {
		t.Fatalf("DeBank %s response is stale: update_at=%.3f", projectID, latestUpdate)
	}
	return uint64(latestUpdate)
}

func fetchDeBankProtocolRefreshed(
	t *testing.T,
	ctx context.Context,
	httpClient *http.Client,
	accessKey string,
	account string,
	projectID string,
	chainID ChainID,
) json.RawMessage {
	t.Helper()
	const attempts = 3
	var failures []error
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"https://pro-openapi.debank.com/v1/user/protocol?id="+account+"&protocol_id="+projectID,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("AccessKey", accessKey)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Accept-Encoding", "identity")
		response, requestErr := httpClient.Do(request)
		if requestErr != nil {
			failures = append(failures, fmt.Errorf("attempt %d: %w", attempt+1, requestErr))
		} else {
			payload, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil || closeErr != nil {
				failures = append(failures, fmt.Errorf(
					"attempt %d: read response: %w", attempt+1, errors.Join(readErr, closeErr),
				))
			} else if response.StatusCode != http.StatusOK {
				failures = append(failures, fmt.Errorf(
					"attempt %d: DeBank returned %d", attempt+1, response.StatusCode,
				))
			} else {
				var identity struct {
					ID    string `json:"id"`
					Chain string `json:"chain"`
				}
				if err := json.Unmarshal(payload, &identity); err != nil {
					failures = append(failures, fmt.Errorf("attempt %d: decode response: %w", attempt+1, err))
				} else if identity.ID != projectID || identity.Chain != reconciliationChainNames[chainID] {
					t.Fatalf(
						"DeBank returned %s on chain %s, expected %s on chain %s",
						identity.ID, identity.Chain, projectID, reconciliationChainNames[chainID],
					)
				} else {
					return json.RawMessage(payload)
				}
			}
		}
		if attempt+1 < attempts {
			time.Sleep(time.Duration(1<<attempt) * 500 * time.Millisecond)
		}
	}
	t.Fatalf("DeBank %s request failed after %d attempts: %v", projectID, len(failures), errors.Join(failures...))
	return nil
}

func fetchDeBankProtocol(
	t *testing.T,
	ctx context.Context,
	httpClient *http.Client,
	accessKey string,
	account string,
	projectID string,
	chainID ChainID,
) json.RawMessage {
	t.Helper()
	const attempts = 3
	var failures []error
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"https://pro-openapi.debank.com/v1/user/all_complex_protocol_list?id="+account,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("AccessKey", accessKey)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Accept-Encoding", "identity")
		response, requestErr := httpClient.Do(request)
		if requestErr != nil {
			failures = append(failures, fmt.Errorf("attempt %d: %w", attempt+1, requestErr))
		} else {
			var projects []json.RawMessage
			decodeErr := json.NewDecoder(response.Body).Decode(&projects)
			closeErr := response.Body.Close()
			if decodeErr != nil || closeErr != nil {
				failures = append(failures, fmt.Errorf(
					"attempt %d: decode response: %w", attempt+1, errors.Join(decodeErr, closeErr),
				))
			} else if response.StatusCode == http.StatusOK {
				var selected json.RawMessage
				for _, project := range projects {
					var identity struct {
						ID    string `json:"id"`
						Chain string `json:"chain"`
					}
					if err := json.Unmarshal(project, &identity); err != nil {
						t.Fatalf("decode DeBank project identity: %v", err)
					}
					if identity.ID == projectID && identity.Chain == reconciliationChainNames[chainID] {
						if selected != nil {
							t.Fatalf("DeBank returned duplicate %s projects on chain %d", projectID, chainID)
						}
						selected = append(json.RawMessage(nil), project...)
					}
				}
				if selected == nil {
					failures = append(failures, fmt.Errorf(
						"attempt %d: DeBank response omitted %s on chain %d", attempt+1, projectID, chainID,
					))
				} else {
					return selected
				}
			} else {
				failures = append(failures, fmt.Errorf(
					"attempt %d: HTTP %d", attempt+1, response.StatusCode,
				))
				if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < http.StatusInternalServerError {
					break
				}
			}
		}
		if attempt+1 < attempts {
			time.Sleep(time.Duration(1<<attempt) * 500 * time.Millisecond)
		}
	}
	t.Fatalf("DeBank %s request failed after %d attempts: %v", projectID, len(failures), errors.Join(failures...))
	return nil
}

func fetchDeBankTokenPrice(
	t *testing.T,
	ctx context.Context,
	httpClient *http.Client,
	accessKey string,
	chain string,
	tokenID string,
) float64 {
	t.Helper()
	const attempts = 3
	var failures []error
	for attempt := 0; attempt < attempts; attempt++ {
		request, err := http.NewRequestWithContext(
			ctx,
			http.MethodGet,
			"https://pro-openapi.debank.com/v1/token?chain_id="+chain+"&id="+tokenID,
			nil,
		)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("AccessKey", accessKey)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Accept-Encoding", "identity")
		response, requestErr := httpClient.Do(request)
		if requestErr != nil {
			failures = append(failures, fmt.Errorf("attempt %d: %w", attempt+1, requestErr))
		} else {
			var payload struct {
				Price float64 `json:"price"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&payload)
			closeErr := response.Body.Close()
			if decodeErr != nil || closeErr != nil {
				failures = append(failures, fmt.Errorf(
					"attempt %d: decode response: %w", attempt+1, errors.Join(decodeErr, closeErr),
				))
			} else if response.StatusCode == http.StatusOK && payload.Price > 0 {
				return payload.Price
			} else {
				failures = append(failures, fmt.Errorf(
					"attempt %d: HTTP %d or non-positive price", attempt+1, response.StatusCode,
				))
				if response.StatusCode != http.StatusTooManyRequests && response.StatusCode < http.StatusInternalServerError {
					break
				}
			}
		}
		if attempt+1 < attempts {
			time.Sleep(time.Duration(1<<attempt) * 500 * time.Millisecond)
		}
	}
	t.Fatalf("DeBank token request failed after %d attempts: %v", len(failures), errors.Join(failures...))
	return 0
}

func runStaderBSCDeBankAPIReconciliation(t *testing.T, accounts []string) {
	t.Helper()
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	rpcURL := os.Getenv("PORTFOLIO_BSC_RPC_URL")
	accessKey := os.Getenv("DEBANK_ACCESS_KEY")
	if rpcURL == "" || accessKey == "" {
		t.Fatal("PORTFOLIO_BSC_RPC_URL and DEBANK_ACCESS_KEY are required")
	}
	if len(accounts) < 20 {
		t.Fatalf("bsc_stader has %d accounts; need at least 20", len(accounts))
	}
	ctx := context.Background()
	rpcClient, err := DialRPC(ctx, BSC, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rpcClient.Close()
	httpClient := &http.Client{Timeout: 60 * time.Second}
	bnbxPrice := fetchDeBankTokenPrice(
		t,
		ctx,
		httpClient,
		accessKey,
		reconciliationChainNames[BSC],
		staderBSCDeployment.liquidToken.Address.Hex(),
	)
	bnbxKey := strings.ToLower(staderBSCDeployment.liquidToken.Address.Hex())
	nativeKey := strings.ToLower(staderBSCDeployment.nativeToken.Address.Hex())
	for _, account := range accounts {
		account := account
		t.Run(strings.ToLower(account), func(t *testing.T) {
			payload := fetchDeBankProtocol(t, ctx, httpClient, accessKey, account, "bsc_stader", BSC)
			requireFreshDeBankProtocol(t, payload, "bsc_stader", BSC)
			wrapped := []byte(fmt.Sprintf(
				`{"projectId":"bsc_stader","response":{"data":[%s]}}`, payload,
			))
			expected, prices := reconciliationExpectedPayload(t, wrapped, BSC)
			expectedNative := expected[nativeKey]
			nativePrice := prices[nativeKey]
			if expectedNative == nil || expectedNative.Sign() <= 0 || nativePrice <= 0 {
				t.Fatal("DeBank Stader response has no positive normalized BNB position")
			}
			block, blockErr := rpcClient.LatestBlock(ctx)
			if blockErr != nil {
				t.Fatal(blockErr)
			}
			groups, positionErr := newStaderAdapter().Positions(
				ctx, rpcClient, block, common.HexToAddress(account),
			)
			if positionErr != nil {
				t.Fatal(positionErr)
			}
			actual := reconciliationActual(t, groups)
			actualBNBx := actual[bnbxKey]
			if actualBNBx == nil || actualBNBx.Sign() <= 0 || len(actual) != 1 {
				t.Fatalf("Stader corpus account is not a direct BNBx-only position: %v", actual)
			}
			// DeBank replaces BNBx with a price-equivalent BNB amount in asset_dict:
			// BNBx amount * BNBx price == displayed BNB amount * BNB price. Reverse
			// only that presentation transform; never treat it as an on-chain rate.
			// asset_dict, the embedded BNB price, and /v1/token's BNBx price refresh
			// on independent caches. A live response has exhibited 5 bps of internal
			// drift, so this economic-value boundary intentionally allows 10 bps.
			expectedBNBx := new(big.Rat).Mul(expectedNative, new(big.Rat).SetFloat64(nativePrice))
			expectedBNBx.Quo(expectedBNBx, new(big.Rat).SetFloat64(bnbxPrice))
			if differences := reconciliationDiffsWithRelativeTolerance(
				map[string]*big.Rat{bnbxKey: expectedBNBx},
				map[string]*big.Rat{bnbxKey: actualBNBx},
				map[string]float64{bnbxKey: bnbxPrice},
				1e-3,
			); len(differences) > 0 {
				t.Fatalf(
					"bsc_stader DeBank economic reconciliation failed at block %d:\n%s",
					block.Number, strings.Join(differences, "\n"),
				)
			}
		})
	}
}

func runDeBankAPIReconciliation(
	t *testing.T,
	projectID string,
	adapter Adapter,
	chainID ChainID,
	accounts []string,
) {
	t.Helper()
	runDeBankAPIReconciliationWith(t, projectID, adapter, chainID, accounts, fetchDeBankProtocol)
}

// runDeBankAPIReconciliationRefreshed compares against DeBank's single-protocol endpoint, which
// forces a refresh before answering. Corpora drawn from an index rather than from DeBank's own
// popular-account set are usually absent from its cache for months, so the cached surface would
// fail the freshness gate rather than disagree on a number.
func runDeBankAPIReconciliationRefreshed(
	t *testing.T,
	projectID string,
	adapter Adapter,
	chainID ChainID,
	accounts []string,
) {
	t.Helper()
	runDeBankAPIReconciliationWith(t, projectID, adapter, chainID, accounts, fetchDeBankProtocolRefreshed)
}

func runDeBankAPIReconciliationWith(
	t *testing.T,
	projectID string,
	adapter Adapter,
	chainID ChainID,
	accounts []string,
	fetch debankFetcher,
) {
	t.Helper()
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	rpcEnvironment := map[ChainID]string{
		Ethereum: "PORTFOLIO_ETH_RPC_URL",
		BSC:      "PORTFOLIO_BSC_RPC_URL",
		Base:     "PORTFOLIO_BASE_RPC_URL",
		Arbitrum: "PORTFOLIO_ARB_RPC_URL",
	}[chainID]
	rpcURL := os.Getenv(rpcEnvironment)
	accessKey := os.Getenv("DEBANK_ACCESS_KEY")
	if rpcURL == "" || accessKey == "" {
		t.Fatalf("%s and DEBANK_ACCESS_KEY are required", rpcEnvironment)
	}
	if len(accounts) < 20 {
		t.Fatalf("%s has %d accounts; need at least 20", projectID, len(accounts))
	}
	ctx := context.Background()
	rpcClient, err := DialRPC(ctx, chainID, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rpcClient.Close()
	httpClient := &http.Client{Timeout: 60 * time.Second}
	for _, account := range accounts {
		account := account
		t.Run(strings.ToLower(account), func(t *testing.T) {
			reconcileDeBankAPIAccount(
				t, ctx, rpcClient, httpClient, accessKey, projectID, adapter, chainID, account, fetch,
			)
		})
	}
}

func TestVesperDeBankAPIReconciliation(t *testing.T) {
	accounts := []string{
		"0x1cc10d67622a73f2995cfa20fae8d8ac1cda30b8",
		"0x3132d5bae89d248deda77aba04a43255ef188264",
		"0x3691ef68ba22a854c36bc92f6b5f30473ef5fb0a",
		"0x3c76a3fee9831309c8d2f66ac723eaab740b1ee0",
		"0x45ff0e3bd649a1d4b78982c8eeae0839aaa7f84f",
		"0x4a9de7de834723723b041d230d9046e952f3ed35",
		"0x4fda54c85e7db9890a7e95b5fac4ff634ba608c8",
		"0x5386a02d131566ff5a315defa84f73d5003ea32a",
		"0x640f7c690cbed1f692728dce99ffb9d59bbcba51",
		"0x666c80feca6fcd371b0535a9846e2d223cbf1d10",
		"0x7beae16328d9e269f594b90fc0471e34fd96039e",
		"0x82ed3fc9d93112124b04b6c7b35394a5aba8af39",
		"0x87e22ef377a0bbc3a2555e832301da2b11f98159",
		"0x89de12b45b4aac8c2841fa29c03ee3bfab1de462",
		"0x99bfef73a7935492a19b63526d983e21eb37b12e",
		"0xbbbbbbbbbb9cc5e90e3b3af64bdaf62c37eeffcb",
		"0xbdb7194a6f048f57e0bab51aaae0fb623615805b",
		"0xd1bcb2b1534584e5783000d2d8f60692ab1cfa4d",
		"0xd44a3e93a256c445f17a12f35a0ffef975ec6817",
		"0xe0e7ac2b0884ba8a05190fb6ceaffadc7c3aedf1",
	}
	runDeBankAPIReconciliation(t, "vesper", newVesperAdapter(), Ethereum, accounts)
}

func TestMorphoDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	indexer := newMorphoIndexer(liveSentioIndexerConfig(t, "PORTFOLIO_MORPHO_INDEXER"))
	indexer.requiredChains = []ChainID{Ethereum}
	runDeBankAPIReconciliation(t, "morphoblue", newMorphoAdapterWithIndexer(indexer), Ethereum, []string{
		"0x0000005bb4df4109bf356a585c8b8ea70fcbaac0",
		"0x01dde420d4f0b76257e4bfc9afdd5f5ffd7ec906",
		"0x0393aefcdfbcedc0e01ef09f1e8b92d4b6f38e22",
		"0x03ba37d4bad5d3097fbd22a2f4a46c44e7620846",
		"0x0472bac34c66a58d946c6fac025ce27322ab8d85",
		"0x0854c79ec9600fd1d02caa14ef0527f93bb5e4cc",
		"0x08c6f91e2b681faf5e17227f2a44c307b3c1364c",
		"0x0922d95226dd20cbb0e4e4aec8e672387a376991",
		"0x0963232eb842baf53e8e517691f81745c1f228a0",
		"0x0acc3952f10a10d96ff4000d040f215dba70737a",
		"0x0c0f0a1e9ab93e23d8b94a9fec7ec5a792b6ea2c",
		"0x0c95996826d7b779b575f97aa818923ec9bb8c9e",
		"0x0c9cad91cdf2673d10d636a8c2f37d67aca0f1d2",
		"0x0cd7e1660d63a6aebda782a2eb7f2875716ac3e8",
		"0x0daf220d630ce7f8e6068322aacf716410395936",
		"0x0e0c281ff05d34729cd764dcfc4fa999b720407d",
		"0x0ec94bd28ec6d4a6246a4614e6b0df0e0502d086",
		"0x0f7be9c006ae131eccaf1daadd544f7b66fb767d",
		"0x11a35fb98b265ff72b6c723ba776c3eb7ef9d160",
		"0x132aef8ea9c3b640248a024ed8d0da9f7415f46a",
	})
}

func TestFluidDeBankAPIReconciliation(t *testing.T) {
	runDeBankAPIReconciliation(t, "fluid", newFluidAdapter(), Ethereum, []string{
		"0x022f8876029fd6e0c1b1bb85c98463cd71c0a54e",
		"0x04940c3800275fe84f615ab07bc723eaa9a1a00f",
		"0x0a3106793a57ead402b1235a3adca7e383a15bf0",
		"0x0c944ef7ba8f75da6ae4499c5d1754fbc8bd5133",
		"0x1929d726442305465f7d0996be32c71ba2b5f45a",
		"0x196a5888d5603a4363cfaf9d75abf1bc961cd37d",
		"0x1acfbd88adc974b0b7a36005947335ecf3965c60",
		"0x1e703e7858a56c64ad07be504599d5d03a1aae11",
		"0x20a26c1bfa57572cf716b2b4b3e10661d5e062e8",
		"0x2156f29d64f81701b58877129006e8b1964149b6",
		"0x25a0db7f677cf466271d488942b22365f31209c9",
		"0x295db3cbe299edc2205c046b395fc2377c3b974d",
		"0x29ff57f9730fe51cf107fa3e6348250596100662",
		"0x2d13f31e0382b33fcba80dca79b5e4efa72d95d4",
		"0x2e3cc8cd22812eaa229cbe85f3de7c9a39a8f4f7",
		"0x3da6ce12e4a9b0e4605c9750521b538cb72c5084",
		"0x4a991fb2eb27aafb4c7f38b8ccf80ac1d51f57ed",
		"0x54b1e10fe030542d8ec917104316fc31d26ac132",
		"0x60860e4ea4a08f8e90d51237c56c00092d2ef966",
		"0x658dd744c08ca1d31107bb7c7ef19430924f1476",
		"0x83f51629e1533f372e0ebff4e65ee99ad509b91c",
	})
}

func TestCrvUSDDeBankAPIReconciliation(t *testing.T) {
	accounts := []string{
		"0xD8EEA67D9b461Be287cd9C4dc7d57b7f501BA40a",
		"0x254D70AD4dd894F59F66d09890d06B682E06d36D",
		"0x559458Aac63528fB18893d797FF223dF4D5fa3C9",
		"0x98289E90d6fC92a8769bC892D006A2Baa7705aFE",
		"0xEB2Cafb6Ea9DEF8084842558Ad1242bBF7Fcac2B",
		"0xdA5e1FE535f4e35C639B778f252eC928E45907F3",
		"0x2E5941Cc1be5d93466Bb8878da510fd484A34a69",
		"0x510b2D8E30e8c79247C51E04D5Be8bF7262F9938",
		"0x25b6F5F1525F0074D53570Ea2Fb4Cd9Ee545B296",
		"0xd46c81245A0582891c2249daf2d68925977cF2a7",
		"0xf992376908c4b3f6E84dFd1DD44f3590b87C3d75",
		"0x36dcC39103F5056b85d6310dAd7172dfbcee731f",
		"0x617eb09D7869de4Ea8a9768C5c657ff7C2D80F97",
		"0x297Bf4cd5CeB8A8e355B71D8a32380bd13607A2a",
		"0xD91432eA17a0B4FF2C28Af958D4B175353C7c730",
		"0x854F1269b659A727a2268AB86FF77CFB30BfB358",
		"0xF20317D989D2577bfb7c3487B47D5f93590B76DC",
		"0xb0e83C2D71A991017e0116d58c5765Abc57384af",
		"0xA8e9fdC7c5959f2ADcb5E49F6eA9bB3002bD8Ad2",
		"0xbc2Dbe6370b7de6aB1B3A68d7b6f12A9C72f0f6F",
	}
	runDeBankAPIReconciliation(t, "crvusd", newCurveCrvUSDAdapter(), Ethereum, accounts)
}

func TestCurveLendingDeBankAPIReconciliation(t *testing.T) {
	accounts := []string{
		"0x1653fF6cd12628AE47fcED55F65082FE4390e605",
		"0xeE820192D9BE0547A7BC55177757e8b28B95B682",
		"0xD872C034300cDd9591a8702Ace1eB09D198DBc9A",
		"0xD4f9FE0039Da59e6DDb21bbb6E84e0C9e83D73eD",
		"0xc8ec15B1354684e06a110FFD5eb06858F538c343",
		"0x5f1D6C57614C1adEe21a20a3681A582C08fA47ed",
		"0x9286F05C159Dd9fd989BD1BbB5fd7D5f38551012",
		"0x1Da3dD644DcEAB11e08E0B31B2eeaFEA59cdDaA4",
		"0x181ae03A7F3F320eC1255c913C9cb63fcE12F77A",
		"0x63b48A331667AEaf305514953e55a5a1c865EDfd",
		"0x2a50a100B1d16950cc6DC637661C41Fc60Ad8554",
		"0x62dFA6eBA6a34E55c454894dd9b3E688F88CB09b",
		"0x81f0D1cc2afbaA0EF876Aca3a2A491D7C0B68617",
		"0x8E8745Acd6fbE3e70Afc96042c7F3eC84B1f0e9f",
		"0xF4BD7B061f379ff54Ab54a5A5097A18a93CA8819",
		"0x8eC85901F390dc55A848561ED1BbB062f64Ebf78",
		"0xF791da446D04282f921f38FBF954aD5cAee899a3",
		"0xACf19E0Cb91017E74c5B788110E0b203736235fE",
		"0xe9c0DF9BD4607850d410C957FeC11eC209De5Ef6",
		"0x3fE2461f7CB328629F2924993c6218748c740C83",
	}
	runDeBankAPIReconciliation(t, "lendcurve", newCurveLendingAdapter(), Ethereum, accounts)
}

func TestUnitasDeBankAPIReconciliation(t *testing.T) {
	accounts := []string{
		"0x055862e6e62de7a0eace524ba59642b1d96b019a",
		"0x05cc251eaf764949b5f16a62178d4c23c4133e15",
		"0x07e04fb70219f11fa6e3822ec13808155b973022",
		"0x1223d56c982432d7dff4d6149bfee033f11021e4",
		"0x17a9ae6e930459dc771330885986fbee2ee0940f",
		"0x19752ea9e690134fbbb6e44d3858bdffb4ff6ae6",
		"0x1bd1c1788d6a95470df0cb425d8c9baf8adf6c62",
		"0x25abb30ee0010f78f4469bb01358717db84f8cfa",
		"0x28e2ea090877bf75740558f6bfb36a5ffee9e9df",
		"0x3173f5cd4f8f8c10d0571055f92753e0095ce735",
		"0x33a7434875978688f13cf43b9d5be9d86ca6a3f4",
		"0x378c284907dc3080e565326e0a9e55e7bf353e4d",
		"0x38a23bac57c7664f71b29e675e403b4cd3131e60",
		"0x3d8d7b3fa5a95f6254b4db557fb1a9a1365da2e4",
		"0x3f9dbbc650065b07259cb05b3d02046a51e3f663",
		"0x408d5e0f4c62b956d7cad7408016fbec5b177aee",
		"0x424afe9de9b6e38c9659932a6da1e8af463ec6ec",
		"0x44eedbe82c49f182efaca48d9c89955fd55fc10f",
		"0x4cf145831395732a147f23dbd1ecdac0572662fe",
		"0x4ecf2874bad5d24cbc4a5bec5fb230b33019f0d7",
	}
	var adapter Adapter
	for _, candidate := range erc4626Adapters() {
		if candidate.Info().ID == "unitas" {
			adapter = candidate
			break
		}
	}
	if adapter == nil {
		t.Fatal("Unitas adapter is not registered")
	}
	runDeBankAPIReconciliation(t, "bsc_unitas", adapter, BSC, accounts)
}

func TestMissingChainDeBankAPIReconciliation(t *testing.T) {
	t.Run("SparkBase", func(t *testing.T) {
		runDeBankAPIReconciliation(t, "base_spark", newAaveAdapter("spark", "Spark", nil, sparkSavingsVaults), Base, []string{
			"0x93442c29746ed5a8de6a781f55eec0266d289ad4",
			"0x832b4ff5471aefc4fd53eeae4f0a1f8ee81bdb01",
			"0x2155a71668989c516b2819d9362c48f311308d3e",
			"0x4059e170d325163e2ec96cf8ca489c40b6927a8c",
			"0x9eb118c9384aedb72ad16a25d42fcff1666cf76a",
			"0xe799961b76d65a32365d34289d5aea6c2242fc98",
			"0xf3bdbe3b76b1d4370389b1059b8bc83aa1c0f42c",
			"0xa8bb1f1a7591e1942e6f177a7011f01552ed0bc2",
			"0x92090a4da3b7a109bd8d3f0f7a46559796c38c03",
			"0x0d8fa4887b0be4f7f64bc6e4501ba1f5815780a8",
			"0x2eb8241ef7abe43debe14303ad20f7cbe8b9b498",
			"0xa920f0fdf5840cadcbf920112e0fe89d8f2ed527",
			"0xb2dd2ceb4a12508140b1e748a1944ace78158eec",
			"0x31fda758fcedffd331feab585b0d209c00a99188",
			"0x5fd95d0f45fc83326fc415385c2aa35070ab6445",
			"0x4be74aca4a0ab2da013d655390b492d816b2b335",
			"0x19cf52f9f4722efae1be4c32396ba818b2c3021d",
			"0xb2f6aa71170b2d1553d7779b61b064b1eab55888",
			"0xf7a05fb577910db6541695d6c62df48a856e85bd",
			"0xfff467cd6b5c546576a8f0cca608e96e894ea780",
		})
	})
	t.Run("SparkArbitrum", func(t *testing.T) {
		runDeBankAPIReconciliation(t, "arb_spark", newAaveAdapter("spark", "Spark", nil, sparkSavingsVaults), Arbitrum, []string{
			"0x11bdf98925a04f9338989c4dd065b2e1b20dc03d",
			"0x9312fb559586ecffe6586c229489dffc636688d2",
			"0xad664976299b26f11e44c7b98a395b66b2795568",
			"0x0cf6a27662fcbc63944ee2c27f8a0bb2685a7e20",
			"0x4a20b9496610941053858bd0b7e92493f44c3c26",
			"0x801c5c1896136f5ae19d47e6b6f29f43941f7c69",
			"0x2a0636dcdeb676a0ef50fba0956fc016fa5ecc21",
			"0xb97c11f7a586fc5baa58daa9d92384dfe760a092",
			"0x8d2ae677001f5a2961072d2c72e802b454264474",
			"0xac20cd734c65baf48a1476447af7d3e3165dc739",
			"0x4442ce2fc5e2f2a2d2732075fdcdc277a521a637",
			"0xd33e7f580ea3c21dfb891ed1d064ca29bcb05c06",
			"0x7684a6dcb6e0183fa6f4faa58a59f697bd8d7b7e",
			"0xd4181fb25e7747bced15f2c383a13b8614ddd5cf",
			"0x16498accbd75b0e55c581480c535e69c4a30ea84",
			"0x627d83dfec9a60f66a4bb0b851a39c263c20be9c",
			"0xdaec334d5174b87082567d929b55674caf52e576",
			"0xd564b3ae673caa49d054bf185bd72a6853763ee7",
			"0x9c89f595f5515609ad61f6fda94beff85ae6600e",
			"0x5513fd8d55133ecc0f5ff0bf768c100528132630",
		})
	})
	t.Run("StaderBSC", func(t *testing.T) {
		runStaderBSCDeBankAPIReconciliation(t, []string{
			"0x76a820fc831b859a20b511b5a17f8a73bc0874ea",
			"0x79e91a7f5c9e51ffb02d86650a94fb2ed22e42d3",
			"0x8da5c98e7e283395257099c6e1936f42908c6870",
			"0x55c2b7d3e0fdc9b105572f9c138b927e983d40a1",
			"0x1fc34df45170d042538a32c80f401e4199160bba",
			"0x9808813e2b4243003f9b33460431462aeff01c7a",
			"0xcc1778c738d80d63175ec691afa502e2957cf924",
			"0x64434a6588a926e7477a98ee1909af766b214957",
			"0xc0e90d43e7ac43c9140f87fcd31aea61579fe8a6",
			"0x0c3a534afaa2282c0b0232bfb0524b9d8294a300",
			"0xf87f6e304d66f15358043d6aa670c8d7c075b883",
			"0xaa0b4b27bb889d09ebf563039891a39059b66fb0",
			"0xb6646d689cc6f2d1f804a3edb6737b6b84b52e59",
			"0xa2cc07d34a82a1b375c0142f7672abecaeef2e04",
			"0x88b28f1c58d3b2da02be42496e8a75282a2fb5b3",
			"0x63623c1a35a73964427b88afec2bd289e779b4e5",
			"0xa09e729b79872b29f5b532c48131ff80750efd35",
			"0x31945cb19c2d95af690203bc5884a36890e9345d",
			"0x212a24944d751d9edcd7676fa3348dc18221bd92",
			"0xd827d9d713e739dddc91562d26bc8ab19b62a4fa",
		})
	})
	t.Run("YearnBase", func(t *testing.T) {
		runDeBankAPIReconciliation(t, "base_yearn3", newYearnV3Adapter(), Base, []string{
			"0xc3bd0a2193c8f027b82dde3611d18589ef3f62a9",
			"0xb13cf163d916917d9cd6e836905ca5f12a1def4b",
			"0x26c60b38fe7e55d699c8102c18cc5d7152e0762e",
			"0x76d55a64b403fdbc1303f485dc8757f6de7566b6",
			"0x1793be9e963b83769bc2b84a64d16785d0f72a84",
			"0xf115c134c23c7a05fbd489a8be3116ebf54b0d9f",
			"0x0000b9ceefe45e8156b9b2bd7c5c6b5ce2b80000",
			"0x229d6dcc4d2a97d3943ee1d6071ca494ff311275",
			"0x5892c394f772b573357df879f47387f971fcd2c5",
			"0x72ff7fb03e1a198c9b876f6b0cc689577935e164",
			"0x9f46fb5bb7dbff410456feb227149f9ba0798f4a",
			"0x4c737146591cadc9c215ba8885047e9bde534e35",
			"0x179e1803a2b162b568f0c3be4551823a6cf7a4a7",
			"0x382e051ce9978419925192da2cd5d308a419b4b3",
			"0x5d1fcc1547da844f39490c5f30370b9e90c364dd",
			"0x99c2e4708493b19baa116e26dfa0056f5a69a783",
			"0xbb700f96f3388af6f46a247702bc40a213ca76c3",
			"0xcb44ccfbc39bd7e3095641d2a710501a0ed0c960",
			"0x03508bb71268bba25ecacc8f620e01866650532c",
			"0x0dcb575fd705f1352b0c0f8fa6a64a0561ce8c3c",
		})
	})
	t.Run("YearnArbitrum", func(t *testing.T) {
		runDeBankAPIReconciliation(t, "arb_yearn3", newYearnV3Adapter(), Arbitrum, []string{
			"0x8ee796309494a10b4170f8912613ee78c75a3430",
			"0x8cf845948bbe3a6e034f0bb35ef826d2c9d745c8",
			"0x13b27de837e531f8f5c7033b391f5679000af1f2",
			"0x17904e2266bfa3aeae1b3f8fdfd8559a954e13e8",
			"0x547f5fd4910f5bb5b637cbfc83cb1bf69d8a0cae",
			"0xb3671166430ec9b5343b2f3f12d0a5dc335d32f2",
			"0x17673237f7a2c6285bff40a1b4200b94b94f37a6",
			"0xc01595286d42ed75ed23adc8c13ba300eb3afd6c",
			"0xb6bc033d34733329971b938fef32fad7e98e56ad",
			"0xa8ee4e54b5fe1f97ae3d42af77f178ecb22a30a1",
			"0xb0612167d2c749131a07c07c254119b9e613c287",
			"0xf59677afe12adae783db6f680a8d9b2a288aa17e",
			"0xdd53b4a4677c73ea47f13f8df1a114986144f8d9",
			"0xc61232f593954b77a518022a49f642d324e47fc6",
			"0xd7392bcc3d3611adf1793fddaaab4770772ac35a",
			"0xe3fbfc0ec13d5cbac2ddb222017abbfa4ba785d0",
			"0xea3053e144e720eba39d9e66eca55a7623efcd81",
			"0xe6dbfb035b44e94d07f7b3e4f6bfbf1c6e68e3d0",
			"0x4ca3a16da87f2aea40d93504f397889400863511",
			"0xf4a3adf696838d8f8035cdb4fcfa9403e3a16863",
		})
	})
	t.Run("CurveLendingArbitrum", func(t *testing.T) {
		runDeBankAPIReconciliation(t, "arb_lendcurve", newCurveLendingAdapter(), Arbitrum, []string{
			"0xfd632fa4fe5c2e2aef32bd973ce1a68a517de461",
			"0xe8b191f29f17d3ced5118a16192eeace66d3d00f",
			"0x32af1f29923b49bf8471ebdb167ef2d6e9fa80be",
			"0x050cb33e85764851eac3b4b4fef46cf7a870ea8d",
			"0xd0e08d653c3ead747865c3674ffa93ecdefdd045",
			"0x16668854dd35b25d3b219db372cb256eb9898b98",
			"0xe75c6d867f658e534450e0595dd0eb4fcc7a4060",
			"0x9be9cd9c9b2dc0a7b47478e4ba14b08b1f640cc7",
			"0x360e68faccca8ca495c1b759fd9eee466db9fb32",
			"0x3c8999e6b8ae7eec906e0bf5de1e26ba4b03b0d8",
			"0x58cb28d6c550f911758a3a6d1da9f120837aaf4e",
			"0x9c6a52c6f736ba3cd9c21bd13eebe08760f931e5",
			"0xcea077172675bf31e879bba71fb46c3188591070",
			"0x090e1fdc0cb866317751f0621884a203a8d797aa",
			"0xbe543fc11b6eb4ae1a80cb4e06828f06dc3791da",
			"0x4f8db1e75bf70c2b3b078811c2b1c2219238197e",
			"0xca1edb2f2bae3363919ba1f114b0cb86ca3c7b76",
			"0x5d62291edc1a684e2920b9c347c845cbd2a1ccdc",
			"0xd628b30bf0db121a21615a516790293285d6a4af",
			"0x50f617990b328de3cce1e2921db91ac294f4eb1a",
		})
	})
}
