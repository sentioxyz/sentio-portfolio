package portfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

// DeBank reports Polymarket as an app rather than a chain protocol: one item per outcome token an
// account holds, keyed "<wallet>_<token id>", with its amount in shares and its price per share.
// The items are Polymarket's own positions API, as of the request: the same tokens, amounts and
// prices, a holding under the API's 0.1-share dust floor left out, and what is worth nothing left
// out too. An open market's price is its order book midpoint.
//
// DeBank lists only the wallets it has registered as Polymarket wallets: the legacy proxies, and
// some of the Safes and deposit wallets that hold most accounts today. A wallet it does not know
// has no Polymarket item at all, so those are reconciled against the positions API directly.
const (
	// polymarketDustShares is the positions API's dust floor: it lists a holding of more than a tenth
	// of a share, as the adapter reports one. A holding within polymarketDustMargin of the floor is
	// compared in neither direction, so the reference's rounding to four decimals cannot move it
	// across.
	polymarketDustShares = 0.1
	polymarketDustMargin = 0.0001
	// polymarketAmountTolerance is the reference's rounding of an amount to four decimals.
	polymarketAmountTolerance = 0.0001
	// polymarketReconciliationAttempts reads an account again when the order book moved between
	// the reference's snapshot and the midpoint read, or a trade landed between it and the pin.
	// polymarketReconciliationSpacing lets the positions API's mark, which can trail the book by
	// tens of seconds, catch up between attempts.
	polymarketReconciliationAttempts = 5
	polymarketReconciliationSpacing  = 20 * time.Second
	// polymarketDeBankItemCap is the most items DeBank lists for one wallet; a longer list is cut.
	polymarketDeBankItemCap = 1_000
)

// polymarketReferenceItem is one holding as DeBank or the positions API reports it.
type polymarketReferenceItem struct {
	TokenID   string
	Shares    float64
	Price     float64
	Claimable bool
}

// polymarketReference is one account's holdings as a reference reported them, and when.
type polymarketReference struct {
	Items []polymarketReferenceItem
	// At is the reference's snapshot time in unix seconds, or zero for the present.
	At uint64
}

type polymarketReferenceFetcher func(ctx context.Context, account string) (polymarketReference, error)

func polymarketGet(ctx context.Context, httpClient *http.Client, address string, header http.Header) ([]byte, error) {
	var failures []string
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return nil, err
		}
		request.Header = header.Clone()
		request.Header.Set("Accept", "application/json")
		response, err := httpClient.Do(request)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			failures = append(failures, fmt.Sprintf("status %d", response.StatusCode))
			continue
		}
		return payload, nil
	}
	return nil, fmt.Errorf("%s", strings.Join(failures, "; "))
}

// fetchDeBankPolymarket reads DeBank's Polymarket app for an account. A list DeBank cut is an
// error: it says nothing about what the account holds beyond it.
func fetchDeBankPolymarket(httpClient *http.Client, accessKey string) polymarketReferenceFetcher {
	return func(ctx context.Context, account string) (polymarketReference, error) {
		payload, err := polymarketGet(
			ctx, httpClient, "https://pro-openapi.debank.com/v1/user/complex_app_list?id="+account,
			http.Header{"AccessKey": []string{accessKey}},
		)
		if err != nil {
			return polymarketReference{}, fmt.Errorf("DeBank complex_app_list: %w", err)
		}
		var apps []struct {
			ID                string `json:"id"`
			PortfolioItemList []struct {
				PositionIndex string `json:"position_index"`
				Detail        struct {
					Amount    float64 `json:"amount"`
					Price     float64 `json:"price"`
					Claimable bool    `json:"claimable"`
				} `json:"detail"`
				UpdateAt float64 `json:"update_at"`
			} `json:"portfolio_item_list"`
		}
		if err := json.Unmarshal(payload, &apps); err != nil {
			return polymarketReference{}, fmt.Errorf("decode DeBank apps: %w", err)
		}
		for _, app := range apps {
			if app.ID != "polymarket" {
				continue
			}
			if len(app.PortfolioItemList) >= polymarketDeBankItemCap {
				return polymarketReference{}, fmt.Errorf("DeBank cut the account's list at %d items", len(app.PortfolioItemList))
			}
			var reference polymarketReference
			var latest float64
			for _, item := range app.PortfolioItemList {
				wallet, tokenID, found := strings.Cut(item.PositionIndex, "_")
				if !found || !strings.EqualFold(wallet, account) {
					return polymarketReference{}, fmt.Errorf("DeBank item %q is not keyed by the account", item.PositionIndex)
				}
				latest = math.Max(latest, item.UpdateAt)
				reference.Items = append(reference.Items, polymarketReferenceItem{
					TokenID: tokenID, Shares: item.Detail.Amount, Price: item.Detail.Price, Claimable: item.Detail.Claimable,
				})
			}
			reference.At = uint64(latest)
			return reference, nil
		}
		// DeBank drops the app from a wallet that holds nothing; a wallet it does not know has no app
		// either, and the kernel's positions then fail against the empty list.
		return polymarketReference{At: uint64(time.Now().Unix())}, nil
	}
}

// fetchPolymarketPositions reads the positions API DeBank mirrors: every holding above its dust
// floor and worth something, as of the request.
func fetchPolymarketPositions(httpClient *http.Client) polymarketReferenceFetcher {
	return func(ctx context.Context, account string) (polymarketReference, error) {
		reference := polymarketReference{At: uint64(time.Now().Unix())}
		cursor := ""
		for page := 0; page < 50; page++ {
			query := url.Values{"user": {account}, "limit": {"1000"}}
			if cursor != "" {
				query = url.Values{"user": {account}, "cursor": {cursor}}
			}
			payload, err := polymarketGet(ctx, httpClient, "https://data-api.polymarket.com/v2/positions?"+query.Encode(), http.Header{})
			if err != nil {
				return polymarketReference{}, fmt.Errorf("positions API: %w", err)
			}
			var response struct {
				Data []struct {
					TokenID      string  `json:"token_id"`
					CurrentSize  float64 `json:"current_size"`
					CurrentPrice float64 `json:"current_price"`
					CurrentValue float64 `json:"current_value"`
					Redeemable   bool    `json:"redeemable"`
				} `json:"data"`
				Pagination struct {
					HasMore    bool   `json:"has_more"`
					NextCursor string `json:"next_cursor"`
				} `json:"pagination"`
			}
			if err := json.Unmarshal(payload, &response); err != nil {
				return polymarketReference{}, fmt.Errorf("decode positions: %w", err)
			}
			for _, position := range response.Data {
				if position.CurrentValue > 0 {
					reference.Items = append(reference.Items, polymarketReferenceItem{
						TokenID: position.TokenID, Shares: position.CurrentSize, Price: position.CurrentPrice,
						Claimable: position.Redeemable,
					})
				}
			}
			if !response.Pagination.HasMore {
				return reference, nil
			}
			cursor = response.Pagination.NextCursor
		}
		return polymarketReference{}, fmt.Errorf("positions API returned more than 50 pages")
	}
}

// fetchPolymarketMidpoints reads the order book midpoint of each token, the price the references
// report for an open market's share.
func fetchPolymarketMidpoints(ctx context.Context, httpClient *http.Client, tokenIDs []string) (map[string]float64, error) {
	midpoints := make(map[string]float64, len(tokenIDs))
	for start := 0; start < len(tokenIDs); start += 100 {
		chunk := tokenIDs[start:min(start+100, len(tokenIDs))]
		body := make([]map[string]string, len(chunk))
		for index, id := range chunk {
			body[index] = map[string]string{"token_id": id}
		}
		encoded, _ := json.Marshal(body)
		request, err := http.NewRequestWithContext(
			ctx, http.MethodPost, "https://clob.polymarket.com/midpoints", bytes.NewReader(encoded),
		)
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(request)
		if err != nil {
			return nil, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("midpoints: status %d", response.StatusCode)
		}
		var prices map[string]string
		if err := json.Unmarshal(payload, &prices); err != nil {
			return nil, fmt.Errorf("decode midpoints: %w", err)
		}
		for id, value := range prices {
			price, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return nil, fmt.Errorf("midpoint %q of %s", value, id)
			}
			midpoints[id] = price
		}
	}
	return midpoints, nil
}

// fetchPolymarketTwoSided reads which tokens' books have both a bid and an ask, the books a midpoint
// is defined for. A token with no market at all — an unnamed placeholder outcome of a neg-risk
// event, which converting a No share mints alongside every listed one — has no entry.
func fetchPolymarketTwoSided(ctx context.Context, httpClient *http.Client, tokenIDs []string) (map[string]bool, error) {
	twoSided := make(map[string]bool, len(tokenIDs))
	for start := 0; start < len(tokenIDs); start += 50 {
		chunk := tokenIDs[start:min(start+50, len(tokenIDs))]
		body := make([]map[string]string, len(chunk))
		for index, id := range chunk {
			body[index] = map[string]string{"token_id": id}
		}
		encoded, _ := json.Marshal(body)
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://clob.polymarket.com/books", bytes.NewReader(encoded))
		if err != nil {
			return nil, err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(request)
		if err != nil {
			return nil, err
		}
		payload, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<20))
		response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("books: status %d", response.StatusCode)
		}
		var books []struct {
			AssetID string            `json:"asset_id"`
			Bids    []json.RawMessage `json:"bids"`
			Asks    []json.RawMessage `json:"asks"`
		}
		if err := json.Unmarshal(payload, &books); err != nil {
			return nil, fmt.Errorf("decode books: %w", err)
		}
		for _, book := range books {
			twoSided[book.AssetID] = len(book.Bids) > 0 && len(book.Asks) > 0
		}
	}
	return twoSided, nil
}

// polymarketReconciled is one outcome token the adapter reported.
type polymarketReconciled struct {
	shares   float64
	resolved bool
	payout   float64
}

func polymarketReconciledGroups(t *testing.T, groups []Group) map[string]polymarketReconciled {
	t.Helper()
	reported := make(map[string]polymarketReconciled, len(groups))
	for _, group := range groups {
		component := group.Components[0]
		amount, ok := new(big.Rat).SetString(component.AmountRaw)
		if !ok {
			t.Fatalf("group %s amount %q", group.ID, component.AmountRaw)
		}
		shares, _ := amount.Quo(amount, big.NewRat(1_000_000, 1)).Float64()
		position := polymarketReconciled{shares: shares}
		if component.PriceBasis != nil {
			ratio, ok := new(big.Rat).SetString(component.PriceBasis.RatioRaw)
			if !ok {
				t.Fatalf("group %s ratio %q", group.ID, component.PriceBasis.RatioRaw)
			}
			position.resolved = true
			position.payout, _ = ratio.Quo(ratio, new(big.Rat).SetInt(priceBasisRatioOne)).Float64()
		}
		reported[component.Token.TokenID] = position
	}
	return reported
}

// polymarketReferenceDiffs compares one account's reported positions with a reference's items. An
// open share is priced at its book's midpoint, read right after the reference's snapshot and again
// after the adapter ran, and the reference is read once more after that. A two-sided book whose two
// midpoints agree, priced the same by both of the reference's reads, held still around the snapshot
// and must match the reference exactly. Two kinds of book leave the price unconfirmed, returned
// rather than compared: one that moved within that window — an in-play market reprices within
// seconds — and one missing a side, which has no midpoint at all (the order book answers 0.5 for an
// empty one) and which the reference marks with a price of its own no public endpoint serves.
//
// A holding the reference lists and the adapter does not is returned apart, for the caller to read
// from the contract: the reference can lag a redemption the chain has already made. So is one the
// adapter reports and the reference does not, for the caller to look up in the order book: the
// reference lists only outcomes that have a market.
func polymarketReferenceDiffs(
	items []polymarketReferenceItem,
	reported map[string]polymarketReconciled,
	midpoints, after map[string]float64,
	twoSided map[string]bool,
	again map[string]float64,
) (diffs []string, moving []string, referenceOnly []polymarketReferenceItem, kernelOnly []string) {
	expected := make(map[string]polymarketReferenceItem, len(items))
	for _, item := range items {
		expected[item.TokenID] = item
	}
	nearFloor := func(shares float64) bool {
		return math.Abs(shares-polymarketDustShares) <= polymarketDustMargin
	}
	for tokenID, item := range expected {
		position, exists := reported[tokenID]
		if !exists {
			if !nearFloor(item.Shares) {
				referenceOnly = append(referenceOnly, item)
			}
			continue
		}
		if math.Abs(position.shares-item.Shares) > polymarketAmountTolerance {
			diffs = append(diffs, fmt.Sprintf("%s: shares %.6f, reference %.4f", tokenID, position.shares, item.Shares))
		}
		price := position.payout
		if !position.resolved {
			midpoint, quoted := midpoints[tokenID]
			if !quoted {
				diffs = append(diffs, fmt.Sprintf("%s: no midpoint, reference price %v", tokenID, item.Price))
				continue
			}
			if !twoSided[tokenID] {
				if math.Abs(midpoint-item.Price) > 1e-9 {
					moving = append(moving, fmt.Sprintf("%s: book lacks a side, reference %v", tokenID, item.Price))
				}
				price = item.Price
			} else if later, reread := after[tokenID]; !reread || later != midpoint || again[tokenID] != item.Price {
				if math.Abs(midpoint-item.Price) > 1e-9 {
					moving = append(moving, fmt.Sprintf(
						"%s: book moved %v -> %v, reference %v -> %v", tokenID, midpoint, later, item.Price, again[tokenID],
					))
				}
				price = item.Price
			} else {
				price = midpoint
			}
		}
		if math.Abs(price-item.Price) > 1e-9 {
			diffs = append(diffs, fmt.Sprintf("%s: price %v (resolved %v), reference %v", tokenID, price, position.resolved, item.Price))
		}
		if position.resolved != item.Claimable {
			diffs = append(diffs, fmt.Sprintf("%s: resolved %v, reference claimable %v", tokenID, position.resolved, item.Claimable))
		}
	}
	for tokenID, position := range reported {
		if _, listed := expected[tokenID]; listed || nearFloor(position.shares) {
			continue
		}
		kernelOnly = append(kernelOnly, tokenID)
	}
	sort.Strings(diffs)
	sort.Strings(moving)
	sort.Strings(kernelOnly)
	return diffs, moving, referenceOnly, kernelOnly
}

// polymarketChainHoldings reads the account's balance of each token from the contract at block, in
// shares.
func polymarketChainHoldings(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	items []polymarketReferenceItem,
) (map[string]float64, error) {
	ids := make([]*big.Int, 0, len(items))
	owners := make([]common.Address, 0, len(items))
	for _, item := range items {
		id, ok := polymarketParseTokenID(item.TokenID)
		if !ok {
			return nil, fmt.Errorf("reference token id %q", item.TokenID)
		}
		ids = append(ids, id)
		owners = append(owners, account)
	}
	held := make(map[string]float64, len(items))
	if len(ids) == 0 {
		return held, nil
	}
	values, err := client.Call(ctx, block, polymarketCTF, polymarketCTFABI, "balanceOfBatch", owners, ids)
	if err != nil {
		return nil, err
	}
	balances, ok := values[0].([]*big.Int)
	if !ok || len(balances) != len(ids) {
		return nil, fmt.Errorf("balanceOfBatch returned %d balances for %d tokens", len(balances), len(ids))
	}
	for index, balance := range balances {
		held[ids[index].String()], _ = new(big.Rat).SetFrac(balance, big.NewInt(1_000_000)).Float64()
	}
	return held, nil
}

// reconcilePolymarketAccounts compares the adapter with a reference account by account: the
// outcome tokens held above the dust floor, their amounts, and their prices — a resolved share's
// payout, an open share's order book midpoint, which the host's price provider is expected to
// supply.
func reconcilePolymarketAccounts(t *testing.T, accounts []string, fetch func(*http.Client) polymarketReferenceFetcher) {
	t.Helper()
	rpcURL := os.Getenv("PORTFOLIO_POLYGON_RPC_URL")
	if rpcURL == "" {
		t.Fatal("PORTFOLIO_POLYGON_RPC_URL is required")
	}
	// A comma-separated list reconciles those accounts instead of the corpus.
	if only := os.Getenv("PORTFOLIO_POLYMARKET_RECONCILIATION_ACCOUNTS"); only != "" {
		accounts = strings.Split(strings.ToLower(only), ",")
	}
	adapter := newPolymarketAdapter(liveSentioIndexerConfig(t, "PORTFOLIO_POLYMARKET"))
	ctx := context.Background()
	rpcClient, err := DialRPC(ctx, Polygon, rpcURL)
	if err != nil {
		t.Fatal(err)
	}
	defer rpcClient.Close()
	httpClient := &http.Client{Timeout: 60 * time.Second}
	reference := fetch(httpClient)
	for _, account := range accounts {
		t.Run(account, func(t *testing.T) {
			var diffs []string
			for attempt := 0; attempt < polymarketReconciliationAttempts; attempt++ {
				if attempt > 0 {
					time.Sleep(polymarketReconciliationSpacing)
				}
				snapshot, err := reference(ctx, account)
				if err != nil {
					t.Fatal(err)
				}
				tokenIDs := make([]string, 0, len(snapshot.Items))
				for _, item := range snapshot.Items {
					tokenIDs = append(tokenIDs, item.TokenID)
				}
				// The books the reference priced are read right after its snapshot, before the adapter runs.
				midpoints, err := fetchPolymarketMidpoints(ctx, httpClient, tokenIDs)
				if err != nil {
					t.Fatal(err)
				}
				twoSided, err := fetchPolymarketTwoSided(ctx, httpClient, tokenIDs)
				if err != nil {
					t.Fatal(err)
				}
				block, err := reconciliationBlock(ctx, rpcClient, snapshot.At)
				if err != nil {
					t.Fatal(err)
				}
				groups, err := adapter.Positions(ctx, rpcClient, block, common.HexToAddress(account))
				if err != nil {
					t.Fatal(err)
				}
				reported := polymarketReconciledGroups(t, groups)
				after, err := fetchPolymarketMidpoints(ctx, httpClient, tokenIDs)
				if err != nil {
					t.Fatal(err)
				}
				// The reference read once more closes the window the midpoints were read in.
				closing, err := reference(ctx, account)
				if err != nil {
					t.Fatal(err)
				}
				again := make(map[string]float64, len(closing.Items))
				for _, item := range closing.Items {
					again[item.TokenID] = item.Price
				}
				var moving, kernelOnly []string
				var referenceOnly []polymarketReferenceItem
				diffs, moving, referenceOnly, kernelOnly = polymarketReferenceDiffs(
					snapshot.Items, reported, midpoints, after, twoSided, again,
				)
				markets, err := fetchPolymarketTwoSided(ctx, httpClient, kernelOnly)
				if err != nil {
					t.Fatal(err)
				}
				var unlisted []string
				for _, tokenID := range kernelOnly {
					position := reported[tokenID]
					if _, listed := markets[tokenID]; listed {
						diffs = append(diffs, fmt.Sprintf(
							"%s: kernel holds %.6f (resolved %v), reference nothing", tokenID, position.shares, position.resolved,
						))
					} else {
						unlisted = append(unlisted, fmt.Sprintf("%s: %.6f shares", tokenID, position.shares))
					}
				}
				if len(unlisted) > 0 {
					t.Logf("kernel holds outcomes no market lists, which the reference leaves out:\n%s", strings.Join(unlisted, "\n"))
				}
				chain, err := polymarketChainHoldings(ctx, rpcClient, block, common.HexToAddress(account), referenceOnly)
				if err != nil {
					t.Fatal(err)
				}
				var stale []string
				for _, item := range referenceOnly {
					if shares := chain[item.TokenID]; shares > polymarketDustShares {
						diffs = append(diffs, fmt.Sprintf(
							"%s: reference holds %.4f at %v, chain %.6f, kernel nothing", item.TokenID, item.Shares, item.Price, shares,
						))
					} else {
						stale = append(stale, fmt.Sprintf("%s: reference holds %.4f, chain %.6f", item.TokenID, item.Shares, shares))
					}
				}
				if len(stale) > 0 {
					t.Logf("reference lists holdings the chain does not have at block %d:\n%s", block.Number, strings.Join(stale, "\n"))
				}
				if len(diffs) == 0 {
					t.Logf("block %d: %d reference items, %d kernel positions, %d prices unconfirmed on moving or one-sided books",
						block.Number, len(snapshot.Items), len(reported), len(moving))
					if len(moving) > 0 {
						t.Logf("unconfirmed:\n%s", strings.Join(moving, "\n"))
					}
					return
				}
				t.Logf("attempt %d at block %d:\n%s", attempt+1, block.Number, strings.Join(diffs, "\n"))
			}
			t.Fatalf("Polymarket reconciliation failed:\n%s", strings.Join(diffs, "\n"))
		})
	}
}

func TestPolymarketDeBankAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_DEBANK_API_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_DEBANK_API_LIVE_TEST=1 to compare with DeBank Pro API")
	}
	accessKey := os.Getenv("DEBANK_ACCESS_KEY")
	if accessKey == "" {
		t.Fatal("DEBANK_ACCESS_KEY is required")
	}
	if len(polymarketDeBankAccounts) < 20 {
		t.Fatalf("Polymarket DeBank corpus has %d accounts; need at least 20", len(polymarketDeBankAccounts))
	}
	reconcilePolymarketAccounts(t, polymarketDeBankAccounts, func(httpClient *http.Client) polymarketReferenceFetcher {
		return fetchDeBankPolymarket(httpClient, accessKey)
	})
}

// TestPolymarketPositionsAPIReconciliation covers the wallets DeBank does not list against the
// positions API DeBank mirrors.
func TestPolymarketPositionsAPIReconciliation(t *testing.T) {
	if os.Getenv("PORTFOLIO_POLYMARKET_POSITIONS_LIVE_TEST") != "1" {
		t.Skip("set PORTFOLIO_POLYMARKET_POSITIONS_LIVE_TEST=1 to compare with Polymarket's positions API")
	}
	reconcilePolymarketAccounts(t, polymarketPositionsAPIAccounts, fetchPolymarketPositions)
}

// polymarketDeBankAccounts are wallets DeBank lists as Polymarket wallets: from a single share to a
// million dollars, with open markets and with resolved winnings not yet redeemed.
var polymarketDeBankAccounts = []string{
	"0x04ae122e3575769c5b26ba9d9903d0e18e770f2d",
	"0x36b8a2f9b9e85f623fccffedeb24ee551d554ee0",
	"0x373204dbfde29b208fbc407fa8f4e78d5aef4eca",
	"0x401ee31e9ebf9ab9f6315cd95faca5f950436fc9",
	"0x41583f2efc720b8e2682750fffb67f2806fece9f",
	"0x424eb20fcd25113e3b98f42522a54580350b263b",
	"0x48de48b1ee7db2de50d5a4eefe267c0d486295ba",
	"0x4e1322e22e12f14d9a239f1909afe8042080b20b",
	"0x4e2de27450ab8222b3cad8353731779c6f304936",
	"0x53adff278ec68011f2c1e1b07a90379a85d99deb",
	"0x56acab44cfca2e88bb9b3406890aea7bfa0cd77e",
	"0x60062ecbce1b143ed4955129377d2ff3632ac7e4",
	"0x6c8b45da3d2de8aff87a458ab44c4396b4b8ec48",
	"0x84cfffc3f16dcc353094de30d4a45226eccd2f63",
	"0x8d44cb1571ecd675788fb7f3b72787513eb67ba3",
	"0xa3eed2b845ea04772d055cf11503fd128a03e096",
	"0xa43e8d963111c38bccfdcec8c82bd15cb7066936",
	"0xb6c6271ca430f29bfd70ccb70b803cff122a043d",
	"0xbca08c1bc204a34f2fddbe47b438b9bd42ac9705",
	"0xbfd770b7ff07fb2ece41b0e7c593db65a84cf0bf",
	"0xcaf59289145789242a4af6b975997344229841ab",
	"0xe29042f5d913dcc4015aab3455c13c58514ca33f",
	"0xe439b8e52eeeeda4eff19a10a4307706a85d61b4",
	"0xf0318c32136c2db7fec88b84869aee6a1106c80c",
}

// polymarketPositionsAPIAccounts are deposit wallets and Safes DeBank does not list.
var polymarketPositionsAPIAccounts = []string{
	"0x0063b23cdeb43166d6c0246c05baaf9b9bd72dd2",
	"0x051adea0d16c85df8a8a9c7e400e0655905d1db9",
	"0x0a0e1501b1c161039e91f112f119b13a71410856",
	"0x0dc607c5b65025dcb443c10fdcbaed4256bb9889",
	"0x14309783cfc9be9bc7498c5ce478120b0587a753",
	"0x1f4ebb8ef69d381cd7122da3154dddf7b8699d5c",
	"0x40ca38c18ef099ed9e05ae65ea8ca401e8c3a96d",
	"0x4fbfee91cb92887382dfc18387b87eba7954f319",
	"0x646404ea0d3bd89a4c96af6f155a20b2545186bc",
	"0x71f8f8ff82aba9fdb47d210a8727766baaa87d15",
	"0x7545a98994a81aa6a33dbf293945048f9718fb6b",
	"0x8ee758db003028735fbedf89be7f48a3dac28291",
	"0x924379a79c64b77ad5816ad362122a5f6228658e",
	"0x9672535851457ba79c01ab8ab416c143a458fb03",
}
