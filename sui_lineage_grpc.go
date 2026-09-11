package portfolio

import (
	"context"
	"fmt"

	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

var _ SuiObjectLineageReader = (*SuiGRPCClient)(nil)

func validateSuiTransactionDigest(digest string) error {
	decoded, err := decodeBase58(digest)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("invalid Sui transaction digest")
	}
	return nil
}

func (c *SuiGRPCClient) ObjectAtVersion(ctx context.Context, raw string, version uint64) (SuiObject, error) {
	id, err := ParseSuiAddress(raw)
	if err != nil || version == 0 {
		return SuiObject{}, fmt.Errorf("invalid Sui object version request")
	}
	var response *rpcv2.GetObjectResponse
	err = c.invoke(ctx, "GetObject", func(ctx context.Context) error {
		var e error
		response, e = c.ledger.GetObject(ctx, &rpcv2.GetObjectRequest{ObjectId: proto.String(id.Hex()), Version: &version, ReadMask: &fieldmaskpb.FieldMask{Paths: suiObjectReadMask}})
		return e
	})
	if err != nil {
		return SuiObject{}, err
	}
	object, err := decodeSuiObject(response.GetObject())
	if err != nil {
		return SuiObject{}, err
	}
	if object.ID != id.Hex() || object.Version != version {
		return SuiObject{}, fmt.Errorf("Sui object version identity mismatch")
	}
	return object, nil
}

// PreviousTransaction also accepts immutable packages, which have no Move
// object JSON. It is used to find objects created by a package's publication.
func (c *SuiGRPCClient) PreviousTransaction(ctx context.Context, raw string) (string, error) {
	id, err := ParseSuiAddress(raw)
	if err != nil {
		return "", err
	}
	var response *rpcv2.GetObjectResponse
	err = c.invoke(ctx, "GetObject", func(ctx context.Context) error {
		var e error
		response, e = c.ledger.GetObject(ctx, &rpcv2.GetObjectRequest{ObjectId: proto.String(id.Hex()), ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"object_id", "previous_transaction"}}})
		return e
	})
	if err != nil {
		return "", err
	}
	object := response.GetObject()
	gotID, err := ParseSuiAddress(object.GetObjectId())
	if err != nil || gotID != id {
		return "", fmt.Errorf("Sui object transaction identity mismatch")
	}
	digest := object.GetPreviousTransaction()
	if err := validateSuiTransactionDigest(digest); err != nil {
		return "", err
	}
	return digest, nil
}

func (c *SuiGRPCClient) TransactionObjectChanges(ctx context.Context, digest string) ([]SuiObjectChange, error) {
	if err := validateSuiTransactionDigest(digest); err != nil {
		return nil, err
	}
	var response *rpcv2.GetTransactionResponse
	err := c.invoke(ctx, "GetTransaction", func(ctx context.Context) error {
		var e error
		response, e = c.ledger.GetTransaction(ctx, &rpcv2.GetTransactionRequest{Digest: &digest, ReadMask: &fieldmaskpb.FieldMask{Paths: []string{"digest", "effects.status", "effects.changed_objects"}}})
		return e
	})
	if err != nil {
		return nil, err
	}
	tx := response.GetTransaction()
	execution := tx.GetEffects().GetStatus()
	if tx.GetDigest() != digest || execution == nil || execution.Success == nil || !execution.GetSuccess() {
		return nil, fmt.Errorf("missing or unsuccessful Sui discovery transaction")
	}
	if len(tx.GetEffects().GetChangedObjects()) > suiObjectLimit {
		return nil, fmt.Errorf("too many Sui transaction object changes")
	}
	result := []SuiObjectChange{}
	seen := map[string]bool{}
	for _, change := range tx.GetEffects().GetChangedObjects() {
		id, err := ParseSuiAddress(change.GetObjectId())
		if err != nil || seen[id.Hex()] {
			return nil, fmt.Errorf("invalid or duplicate Sui transaction object")
		}
		seen[id.Hex()] = true
		typ := change.GetObjectType()
		if typ != "" && typ != "package" {
			typ, err = NormalizeMoveType(typ)
			if err != nil {
				return nil, err
			}
		}
		result = append(result, SuiObjectChange{ID: id.Hex(), ObjectType: typ,
			OwnerKind:    change.GetOutputOwner().GetKind().String(),
			InputVersion: change.GetInputVersion(), OutputVersion: change.GetOutputVersion(),
			Created: change.GetIdOperation() == rpcv2.ChangedObject_CREATED && change.GetInputState() == rpcv2.ChangedObject_INPUT_OBJECT_STATE_DOES_NOT_EXIST && change.GetOutputState() == rpcv2.ChangedObject_OUTPUT_OBJECT_STATE_OBJECT_WRITE})
	}
	return result, nil
}
