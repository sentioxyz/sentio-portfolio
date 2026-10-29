package portfolio

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

var indexLagTestOther = common.HexToAddress("0x2222222222222222222222222222222222222222")

func TestCheckIndexCheckpoint(t *testing.T) {
	const pinTime = uint64(1_700_000_000)
	live := BlockRef{ChainID: Ethereum, Number: 100, Timestamp: pinTime}
	fixed := live
	fixed.Fixed = true
	at := func(age time.Duration) uint64 { return (pinTime - uint64(age/time.Second)) * 1_000 }
	for _, test := range []struct {
		name       string
		block      BlockRef
		checkpoint uint64
		timestamp  uint64
		wantLag    time.Duration
		wantError  string
	}{
		{name: "fresh", block: live, checkpoint: 100, timestamp: at(0)},
		{name: "ahead of the pin", block: live, checkpoint: 101, timestamp: at(0), wantError: "ahead of pinned block"},
		{name: "timestamp ahead of the pin", block: live, checkpoint: 100, timestamp: at(0) + 61_000, wantError: "timestamp is ahead"},
		{name: "live within the lag", block: live, checkpoint: 90, timestamp: at(indexMaxLag)},
		{name: "live past the lag", block: live, checkpoint: 90, timestamp: at(indexMaxLag + time.Second), wantLag: indexMaxLag + time.Second},
		{name: "live a month behind", block: live, checkpoint: 90, timestamp: at(30 * 24 * time.Hour), wantLag: 30 * 24 * time.Hour},
		{name: "fixed a day behind", block: fixed, checkpoint: 90, timestamp: at(24 * time.Hour)},
		{name: "fixed at the backfill lag", block: fixed, checkpoint: 90, timestamp: at(indexBackfillMaxLag)},
		{name: "fixed past the backfill lag", block: fixed, checkpoint: 90, timestamp: at(indexBackfillMaxLag + time.Second), wantError: "stale"},
	} {
		t.Run(test.name, func(t *testing.T) {
			note, err := checkIndexCheckpoint(test.block, test.checkpoint, test.timestamp)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want one naming %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.wantLag == 0 {
				if note != nil {
					t.Fatalf("note = %v, want none", note)
				}
				return
			}
			if note == nil || note.lag != test.wantLag || note.checkpoint != test.checkpoint {
				t.Fatalf("note = %+v, want %s of lag at checkpoint %d", note, test.wantLag, test.checkpoint)
			}
		})
	}
}

func TestIndexQueryBlock(t *testing.T) {
	latest := func(processed uint64) map[ChainID]sentioChainStatus {
		return map[ChainID]sentioChainStatus{Ethereum: {State: "PROCESSING_LATEST", ProcessedBlock: processed}}
	}
	for _, test := range []struct {
		name      string
		block     BlockRef
		reads     []map[ChainID]sentioChainStatus
		wantBlock uint64
		wantReads int
		wantError string
	}{
		{
			name:  "live past the processor",
			block: BlockRef{ChainID: Ethereum, Number: 200}, reads: []map[ChainID]sentioChainStatus{latest(150)},
			wantBlock: 150, wantReads: 1,
		},
		{
			name:  "live behind the processor",
			block: BlockRef{ChainID: Ethereum, Number: 200}, reads: []map[ChainID]sentioChainStatus{latest(250)},
			wantBlock: 200, wantReads: 1,
		},
		{
			name:  "fixed pin the processor reaches on a fresh read",
			block: BlockRef{ChainID: Ethereum, Number: 200, Fixed: true},
			reads: []map[ChainID]sentioChainStatus{latest(150), latest(200)}, wantBlock: 200, wantReads: 2,
		},
		{
			name:  "fixed pin the processor has not reached",
			block: BlockRef{ChainID: Ethereum, Number: 200, Fixed: true},
			reads: []map[ChainID]sentioChainStatus{latest(150), latest(150)}, wantReads: 2,
			wantError: "short of pinned block 200",
		},
		{
			name:  "still backfilling",
			block: BlockRef{ChainID: Ethereum, Number: 200},
			reads: []map[ChainID]sentioChainStatus{{Ethereum: {State: "CATCHING_UP", ProcessedBlock: 250}}}, wantReads: 1,
			wantError: "backfill is incomplete",
		},
		{
			name:  "chain missing from the status",
			block: BlockRef{ChainID: Ethereum, Number: 200},
			reads: []map[ChainID]sentioChainStatus{{}}, wantReads: 1, wantError: "omitted chain 1",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads := 0
			_, queryBlock, err := indexQueryBlock(test.block, func(forceRefresh bool) (map[ChainID]sentioChainStatus, error) {
				if forceRefresh != (reads > 0) {
					t.Errorf("read %d forceRefresh = %t", reads, forceRefresh)
				}
				reads++
				return test.reads[reads-1], nil
			})
			if reads != test.wantReads {
				t.Fatalf("status reads = %d, want %d", reads, test.wantReads)
			}
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want one naming %q", err, test.wantError)
				}
				return
			}
			if err != nil || queryBlock != test.wantBlock {
				t.Fatalf("query block = %d, %v, want %d", queryBlock, err, test.wantBlock)
			}
		})
	}
}

// laggingTestAdapter reads its group from an index that trails the pin, and may fail after it.
type laggingTestAdapter struct {
	*deadlineTestAdapter
	failure error
}

func (a *laggingTestAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	groups, _ := a.deadlineTestAdapter.Positions(ctx, client, block, account)
	noteCoverage(ctx, &indexLagNote{checkpoint: 900, lag: 2*time.Hour + 30*time.Second})
	return groups, a.failure
}

// A lagging index keeps what was read and says what may be missing, as a coverage note, apart from
// any failure of the same deployment: the note reaches the scan however the adapter's read ends.
func TestEngineReportsIndexLagAsCoverage(t *testing.T) {
	engine := newDeadlineTestEngine(t, availabilityRPCServer{t: t, block: 1_000}, &deadlineTestPrices{}, nil,
		&laggingTestAdapter{deadlineTestAdapter: newDeadlineTestAdapter("lagging", false)},
		&laggingTestAdapter{
			deadlineTestAdapter: newDeadlineTestAdapter("failing", false), failure: errors.New("vault read failed"),
		},
	)
	startedAt := time.Now()
	response := engine.ScanWithOptions(
		context.Background(),
		common.HexToAddress("0x1"),
		deadlineTestOptions(time.Time{}, startedAt.Add(time.Minute)),
	)
	read := make(map[string]bool)
	for _, snapshot := range response.Snapshots {
		read[snapshot.ProtocolID] = len(snapshot.Groups) == 1
	}
	if !read["lagging"] || !read["failing"] {
		t.Fatalf("snapshots = %+v, want both groups kept", response.Snapshots)
	}
	want := "indexer trails the pinned block by 2h0m0s: positions opened after block 900 may be missing"
	coverage := scanErrorsIn(response, "coverage")
	if len(coverage) != 2 {
		t.Fatalf("coverage = %+v, want one note per adapter", coverage)
	}
	for _, note := range coverage {
		if note.ChainID != BSC || note.Message != want {
			t.Fatalf("coverage note = %+v, want %q on BSC", note, want)
		}
	}
	failures := scanErrorsIn(response, "protocol")
	if len(failures) != 1 || failures[0].ProtocolID != "failing" || failures[0].Message != "vault read failed" {
		t.Fatalf("protocol errors = %+v, want the failing adapter's failure alone", failures)
	}
}

// Without an RPC tail, an index read lists what the account held as of its checkpoint. A request
// settled since, or an NFT burned or transferred since, must read as nothing at the pin rather
// than fail the protocol as a disagreement with the index.

func TestAsterSkipsWithdrawalsClaimedAfterTheIndex(t *testing.T) {
	adapter := newAsterAdapter(SentioIndexerConfig{}).(*AsterAdapter)
	withdrawal := adapter.withdrawals[0]
	stub := newContractStub(t, BSC, presenceTestAccount)
	stub.answer(withdrawal.Address, withdrawal.ABI, "requestWithdraws", []any{big.NewInt(7)},
		common.Address{}, new(big.Int), new(big.Int), new(big.Int), false, common.Address{}, false)
	groups, err := adapter.withdrawalGroups(
		context.Background(), stub.dial(), BlockRef{ChainID: BSC, Number: withdrawal.ActivationBlock + 1},
		presenceTestAccount, map[common.Address]asterWithdrawalContract{withdrawal.Address: withdrawal},
		[]accountRequestRef{{Contract: withdrawal.Address, Key: "7"}},
	)
	if err != nil || len(groups) != 0 {
		t.Fatalf("groups = %d err = %v, want the claimed request skipped", len(groups), err)
	}
}

func TestAsterSkipsMintRequestsSettledAfterTheIndex(t *testing.T) {
	adapter := newAsterAdapter(SentioIndexerConfig{}).(*AsterAdapter)
	underlying := common.HexToAddress("0x00000000000000000000000000000000000000a1")
	stub := newContractStub(t, BSC, presenceTestAccount)
	stub.answer(asterAsBNBMinter, asterMinterABI, "token", nil, underlying)
	stub.answer(asterAsBNBMinter, asterMinterABI, "asBnb", nil, asterAsBNB.Address)
	stub.answer(underlying, erc20ABI, "symbol", nil, "slisBNB")
	stub.answer(underlying, erc20ABI, "decimals", nil, uint8(18))
	stub.answer(asterAsBNBMinter, asterMinterABI, "tokenMintReqQueue", []any{big.NewInt(3)}, common.Address{}, new(big.Int))
	groups, err := adapter.mintGroups(
		context.Background(), stub.dial(), BlockRef{ChainID: BSC, Number: asterAsBNBMinterActivation + 1},
		presenceTestAccount, []accountRequestRef{{Contract: asterAsBNBMinter, Key: "3"}},
	)
	if err != nil || len(groups) != 0 {
		t.Fatalf("groups = %d err = %v, want the processed request skipped", len(groups), err)
	}
}

func TestRenzoSkipsWithdrawalsClaimedAfterTheIndex(t *testing.T) {
	adapter := newRenzoAdapter(SentioIndexerConfig{}).(*RenzoAdapter)
	vault := adapter.vaults[0]
	claimed, pending := common.HexToHash("0x01"), common.HexToHash("0x02")
	stub := newContractStub(t, Ethereum, presenceTestAccount)
	stub.answer(vault.Address, renzoEigenVaultABI, "withdrawRequest", []any{claimed}, common.Address{}, new(big.Int), new(big.Int))
	stub.answer(vault.Address, renzoEigenVaultABI, "withdrawRequest", []any{pending},
		presenceTestAccount, big.NewInt(100), new(big.Int).SetUint64(vault.ActivationBlock+1))
	for _, root := range []common.Hash{claimed, pending} {
		stub.answer(vault.Address, renzoEigenVaultABI, "queuedWithdrawalInfo", []any{root}, new(big.Int), new(big.Int))
	}
	stub.answer(vault.Address, renzoEigenVaultABI, "scaleFactor", nil, big.NewInt(1))
	stub.answer(vault.Address, renzoEigenVaultABI, "getRate", nil, big.NewInt(1))
	groups, err := adapter.eigenWithdrawalGroups(
		context.Background(), stub.dial(), BlockRef{ChainID: Ethereum, Number: vault.ActivationBlock + 2},
		presenceTestAccount, []accountRequestRef{
			{Contract: vault.Address, Key: strings.ToLower(claimed.Hex())},
			{Contract: vault.Address, Key: strings.ToLower(pending.Hex())},
		},
	)
	if err != nil || len(groups) != 1 || groups[0].Components[0].AmountRaw != "100" {
		t.Fatalf("groups = %+v err = %v, want only the pending withdrawal", groups, err)
	}
}

func TestEtherfiSkipsWithdrawalNFTsGoneAfterTheIndex(t *testing.T) {
	adapter := newEtherfiAdapter(SentioIndexerConfig{}).(*EtherfiAdapter)
	burned, transferred, held := big.NewInt(11), big.NewInt(12), big.NewInt(13)
	stub := newContractStub(t, Ethereum, presenceTestAccount)
	stub.revert(etherfiWithdrawalNFT, etherfiWithdrawalABI, "ownerOf", burned)
	stub.answer(etherfiWithdrawalNFT, etherfiWithdrawalABI, "ownerOf", []any{transferred}, indexLagTestOther)
	for _, tokenID := range []*big.Int{burned, transferred} {
		stub.revert(etherfiWithdrawalNFT, etherfiWithdrawalABI, "getRequest", tokenID)
		stub.revert(etherfiWithdrawalNFT, etherfiWithdrawalABI, "isFinalized", tokenID)
	}
	stub.answer(etherfiWithdrawalNFT, etherfiWithdrawalABI, "ownerOf", []any{held}, presenceTestAccount)
	stub.answer(etherfiWithdrawalNFT, etherfiWithdrawalABI, "getRequest", []any{held},
		big.NewInt(500), big.NewInt(400), true, new(big.Int))
	stub.answer(etherfiWithdrawalNFT, etherfiWithdrawalABI, "isFinalized", []any{held}, false)
	groups, err := adapter.withdrawalGroups(
		context.Background(), stub.dial(), BlockRef{ChainID: Ethereum, Number: etherfiWithdrawalActivationBlock + 1},
		presenceTestAccount, []ownerTokenRef{
			{Contract: etherfiWithdrawalNFT, TokenID: burned}, {Contract: etherfiWithdrawalNFT, TokenID: transferred},
			{Contract: etherfiWithdrawalNFT, TokenID: held},
		},
	)
	if err != nil || len(groups) != 1 || groups[0].ID != "withdrawal:13" {
		t.Fatalf("groups = %+v err = %v, want only the request still held", groups, err)
	}
}

func TestEtherfiSkipsMembershipsGoneAfterTheIndex(t *testing.T) {
	adapter := newEtherfiAdapter(SentioIndexerConfig{}).(*EtherfiAdapter)
	unwrapped, held := big.NewInt(6958), big.NewInt(6959)
	stub := newContractStub(t, Ethereum, presenceTestAccount)
	stub.answer(etherfiMembershipNFT, etherfiMembershipABI, "membershipManager", nil, etherfiMembershipManager)
	stub.answer(etherfiMembershipNFT, etherfiMembershipABI, "balanceOf", []any{presenceTestAccount, unwrapped}, new(big.Int))
	stub.revert(etherfiMembershipNFT, etherfiMembershipABI, "valueOf", unwrapped)
	stub.answer(etherfiMembershipNFT, etherfiMembershipABI, "balanceOf", []any{presenceTestAccount, held}, big.NewInt(1))
	stub.answer(etherfiMembershipNFT, etherfiMembershipABI, "valueOf", []any{held}, big.NewInt(5))
	groups, err := adapter.membershipGroups(
		context.Background(), stub.dial(), BlockRef{ChainID: Ethereum, Number: etherfiMembershipActivationBlock + 1},
		presenceTestAccount, []ownerTokenRef{
			{Contract: etherfiMembershipNFT, TokenID: unwrapped}, {Contract: etherfiMembershipNFT, TokenID: held},
		},
	)
	if err != nil || len(groups) != 1 || groups[0].ID != "membership:6959" {
		t.Fatalf("groups = %+v err = %v, want only the membership still held", groups, err)
	}
}

func TestFraxSkipsTicketsGoneAfterTheIndex(t *testing.T) {
	queue := fraxEtherQueueConfig{Address: fraxEtherV2Queue, ActivationBlock: 21_404_228, Version: 2}
	burned, transferred, held := big.NewInt(21), big.NewInt(22), big.NewInt(23)
	stub := newContractStub(t, Ethereum, presenceTestAccount)
	stub.revert(queue.Address, fraxEtherQueueV2ABI, "ownerOf", burned)
	stub.answer(queue.Address, fraxEtherQueueV2ABI, "ownerOf", []any{transferred}, indexLagTestOther)
	for _, tokenID := range []*big.Int{burned, transferred} {
		stub.revert(queue.Address, fraxEtherQueueV2ABI, "nftInformation", tokenID)
	}
	stub.answer(queue.Address, fraxEtherQueueV2ABI, "ownerOf", []any{held}, presenceTestAccount)
	stub.answer(queue.Address, fraxEtherQueueV2ABI, "nftInformation", []any{held},
		false, big.NewInt(1), big.NewInt(1_000), new(big.Int), new(big.Int))
	groups, err := ticketGroups(
		context.Background(), stub.dial(), BlockRef{ChainID: Ethereum, Number: queue.ActivationBlock + 1},
		presenceTestAccount, map[common.Address]fraxEtherQueueConfig{queue.Address: queue},
		[]ownerTokenRef{
			{Contract: queue.Address, TokenID: burned}, {Contract: queue.Address, TokenID: transferred},
			{Contract: queue.Address, TokenID: held},
		},
	)
	if err != nil || len(groups) != 1 || groups[0].Components[0].AmountRaw != "1000" {
		t.Fatalf("groups = %+v err = %v, want only the ticket still held", groups, err)
	}
}
