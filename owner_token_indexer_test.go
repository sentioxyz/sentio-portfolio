package portfolio

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
)

var (
	erc1155TestOwner = common.HexToAddress("0x1111111111111111111111111111111111111111")
	erc1155TestOther = common.HexToAddress("0x2222222222222222222222222222222222222222")
)

func addressTopic(address common.Address) common.Hash {
	return common.BytesToHash(address.Bytes())
}

func transferSingleLog(from, to common.Address, tokenID, value int64) rpcLog {
	data := make([]byte, 64)
	big.NewInt(tokenID).FillBytes(data[:32])
	big.NewInt(value).FillBytes(data[32:])
	return rpcLog{
		Address: etherfiMembershipNFT,
		Topics: []common.Hash{
			erc1155TransferSingleTopic,
			addressTopic(erc1155TestOther),
			addressTopic(from),
			addressTopic(to),
		},
		Data: data,
	}
}

func transferBatchLog(t *testing.T, from, to common.Address, ids, amounts []int64) rpcLog {
	t.Helper()
	tokenIDs := make([]*big.Int, len(ids))
	for index, id := range ids {
		tokenIDs[index] = big.NewInt(id)
	}
	units := make([]*big.Int, len(amounts))
	for index, amount := range amounts {
		units[index] = big.NewInt(amount)
	}
	arguments := abi.Arguments(erc1155TransferABI.Events["TransferBatch"].Inputs.NonIndexed())
	data, err := arguments.Pack(tokenIDs, units)
	if err != nil {
		t.Fatal(err)
	}
	return rpcLog{
		Address: etherfiMembershipNFT,
		Topics: []common.Hash{
			erc1155TransferBatchTopic,
			addressTopic(erc1155TestOther),
			addressTopic(from),
			addressTopic(to),
		},
		Data: hexutil.Bytes(data),
	}
}

func refKeys(refs map[string]ownerTokenRef) []string {
	keys := make([]string, 0, len(refs))
	for key := range refs {
		keys = append(keys, key)
	}
	return keys
}

// A zero-unit ERC-1155 transfer is legal and moves no ownership. Ether.fi's membership NFT
// emits one at block 19,771,724; treating it as fatal failed every Ether.fi scan whose RPC
// tail covered that block, rather than skipping an event that says nothing.
func TestERC1155TailSkipsZeroUnitTransferSingle(t *testing.T) {
	refs := map[string]ownerTokenRef{}
	if err := applyERC1155TailLog(refs, transferSingleLog(common.Address{}, erc1155TestOwner, 6958, 1), erc1155TestOwner); err != nil {
		t.Fatal(err)
	}
	key := ownerTokenRefKey(etherfiMembershipNFT, big.NewInt(6958))
	if _, held := refs[key]; !held {
		t.Fatalf("mint did not record the token: %v", refKeys(refs))
	}

	zero := transferSingleLog(erc1155TestOther, erc1155TestOwner, 6958, 0)
	if err := applyERC1155TailLog(refs, zero, erc1155TestOwner); err != nil {
		t.Fatalf("zero-unit TransferSingle failed the scan: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("zero-unit transfer changed ownership: %v", refKeys(refs))
	}

	// A zero-unit transfer away from the owner must not drop the row either.
	away := transferSingleLog(erc1155TestOwner, erc1155TestOther, 6958, 0)
	if err := applyERC1155TailLog(refs, away, erc1155TestOwner); err != nil {
		t.Fatalf("zero-unit TransferSingle failed the scan: %v", err)
	}
	if _, held := refs[key]; !held {
		t.Fatalf("zero-unit transfer dropped the token: %v", refKeys(refs))
	}
}

func TestERC1155TailRejectsMultiUnitTransferSingle(t *testing.T) {
	refs := map[string]ownerTokenRef{}
	log := transferSingleLog(common.Address{}, erc1155TestOwner, 6958, 2)
	if err := applyERC1155TailLog(refs, log, erc1155TestOwner); err == nil {
		t.Fatal("expected a multi-unit TransferSingle to fail the scan")
	}
}

func TestERC1155TailSkipsZeroUnitBatchItems(t *testing.T) {
	refs := map[string]ownerTokenRef{}
	log := transferBatchLog(t, common.Address{}, erc1155TestOwner, []int64{11, 12}, []int64{0, 1})
	if err := applyERC1155TailLog(refs, log, erc1155TestOwner); err != nil {
		t.Fatalf("zero-unit batch item failed the scan: %v", err)
	}
	if _, held := refs[ownerTokenRefKey(etherfiMembershipNFT, big.NewInt(11))]; held {
		t.Fatal("a zero-unit batch item recorded ownership")
	}
	if _, held := refs[ownerTokenRefKey(etherfiMembershipNFT, big.NewInt(12))]; !held {
		t.Fatalf("a unit batch item was dropped: %v", refKeys(refs))
	}
}

func TestERC1155TailRejectsMultiUnitBatchItem(t *testing.T) {
	refs := map[string]ownerTokenRef{}
	log := transferBatchLog(t, common.Address{}, erc1155TestOwner, []int64{11, 12}, []int64{1, 2})
	if err := applyERC1155TailLog(refs, log, erc1155TestOwner); err == nil {
		t.Fatal("expected a multi-unit batch item to fail the scan")
	}
}
