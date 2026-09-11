package portfolio

import (
	"context"
	"fmt"
	"strings"

	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

const suiObjectLimit = 4096
const suiObjectPageSize = 100

var suiObjectReadMask = []string{"object_id", "version", "owner", "object_type", "json", "previous_transaction"}
var _ SuiObjectReader = (*SuiGRPCClient)(nil)

func decodeSuiObject(object *rpcv2.Object) (SuiObject, error) {
	if object == nil || object.Version == nil || object.GetVersion() == 0 || object.Json == nil || object.Owner == nil {
		return SuiObject{}, fmt.Errorf("incomplete Sui Move object")
	}
	id, err := ParseSuiAddress(object.GetObjectId())
	if err != nil {
		return SuiObject{}, err
	}
	typ, err := NormalizeMoveType(object.GetObjectType())
	if err != nil {
		return SuiObject{}, err
	}
	content, err := object.Json.MarshalJSON()
	if err != nil {
		return SuiObject{}, fmt.Errorf("invalid Sui object JSON")
	}
	fields, err := suiObjectFields(string(content))
	if err != nil {
		return SuiObject{}, err
	}
	contentID, err := fields.address("id")
	if err != nil || contentID != id.Hex() {
		return SuiObject{}, fmt.Errorf("Sui object content identity mismatch")
	}
	owner := ""
	kind := object.Owner.GetKind()
	switch kind {
	case rpcv2.Owner_ADDRESS, rpcv2.Owner_CONSENSUS_ADDRESS, rpcv2.Owner_OBJECT:
		address, err := ParseSuiAddress(object.Owner.GetAddress())
		if err != nil {
			return SuiObject{}, err
		}
		owner = address.Hex()
	case rpcv2.Owner_SHARED, rpcv2.Owner_IMMUTABLE:
	default:
		return SuiObject{}, fmt.Errorf("unknown Sui object owner kind")
	}
	return SuiObject{ID: id.Hex(), ObjectType: typ, Owner: owner, OwnerKind: kind.String(), Content: string(content), Version: object.GetVersion(), PreviousTransaction: object.GetPreviousTransaction()}, nil
}

// Objects batches point reads. Only explicit NotFound entries are omitted;
// malformed, missing or failed batch results fail the read.
func (c *SuiGRPCClient) Objects(ctx context.Context, ids []string) (map[string]SuiObject, error) {
	if len(ids) > suiObjectLimit {
		return nil, fmt.Errorf("too many Sui object reads")
	}
	unique := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, raw := range ids {
		id, err := ParseSuiAddress(raw)
		if err != nil {
			return nil, err
		}
		if !seen[id.Hex()] {
			unique = append(unique, id.Hex())
			seen[id.Hex()] = true
		}
	}
	result := make(map[string]SuiObject)
	for start := 0; start < len(unique); start += suiObjectPageSize {
		batch := unique[start:min(start+suiObjectPageSize, len(unique))]
		requests := make([]*rpcv2.GetObjectRequest, len(batch))
		for i, id := range batch {
			requests[i] = &rpcv2.GetObjectRequest{ObjectId: proto.String(id)}
		}
		var response *rpcv2.BatchGetObjectsResponse
		err := c.invoke(ctx, "BatchGetObjects", func(ctx context.Context) error {
			var e error
			response, e = c.ledger.BatchGetObjects(ctx, &rpcv2.BatchGetObjectsRequest{Requests: requests, ReadMask: &fieldmaskpb.FieldMask{Paths: suiObjectReadMask}})
			return e
		})
		if err != nil {
			return nil, err
		}
		if response == nil || len(response.Objects) != len(batch) {
			return nil, fmt.Errorf("incomplete Sui object batch")
		}
		for i, item := range response.Objects {
			if failure := item.GetError(); failure != nil {
				if codes.Code(failure.Code) == codes.NotFound {
					continue
				}
				return nil, fmt.Errorf("Sui object batch entry failed: %s", codes.Code(failure.Code))
			}
			object, err := decodeSuiObject(item.GetObject())
			if err != nil {
				return nil, err
			}
			if object.ID != batch[i] {
				return nil, fmt.Errorf("Sui object batch identity mismatch")
			}
			result[object.ID] = object
		}
	}
	return result, nil
}

func (c *SuiGRPCClient) OwnedObjects(ctx context.Context, owner SuiAddress, objectType string) ([]SuiObject, error) {
	typ, err := NormalizeMoveType(objectType)
	if err != nil {
		return nil, err
	}
	var token []byte
	seenTokens, seenObjects := map[string]bool{}, map[string]bool{}
	result := []SuiObject{}
	for page := 0; page < suiObjectLimit; page++ {
		var response *rpcv2.ListOwnedObjectsResponse
		err := c.invoke(ctx, "ListOwnedObjects", func(ctx context.Context) error {
			var e error
			response, e = c.state.ListOwnedObjects(ctx, &rpcv2.ListOwnedObjectsRequest{Owner: proto.String(owner.Hex()), ObjectType: proto.String(typ), PageSize: proto.Uint32(suiObjectPageSize), PageToken: token, ReadMask: &fieldmaskpb.FieldMask{Paths: suiObjectReadMask}})
			return e
		})
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, fmt.Errorf("missing Sui owned objects page")
		}
		for _, raw := range response.Objects {
			object, err := decodeSuiObject(raw)
			if err != nil {
				return nil, err
			}
			if object.Owner != owner.Hex() || (object.OwnerKind != "ADDRESS" && object.OwnerKind != "CONSENSUS_ADDRESS") || !suiObjectTypeMatches(object.ObjectType, typ) || seenObjects[object.ID] {
				return nil, fmt.Errorf("invalid or duplicate Sui owned object")
			}
			seenObjects[object.ID] = true
			result = append(result, object)
		}
		if len(result) > suiObjectLimit {
			return nil, fmt.Errorf("Sui owned object enumeration exceeds limit")
		}
		token = response.NextPageToken
		if len(token) == 0 {
			return result, nil
		}
		if seenTokens[string(token)] {
			return nil, fmt.Errorf("repeated Sui owned object page token")
		}
		seenTokens[string(token)] = true
	}
	return nil, fmt.Errorf("Sui owned object pagination exceeds limit")
}

func suiObjectTypeMatches(actual, wanted string) bool {
	return actual == wanted || (!strings.Contains(wanted, "<") && strings.HasPrefix(actual, wanted+"<"))
}

// DynamicFields is for small protocol inventories, never a protocol-wide user
// balance table. User balances are point reads of derived field IDs.
func (c *SuiGRPCClient) DynamicFields(ctx context.Context, parent string) ([]SuiObject, error) {
	id, err := ParseSuiAddress(parent)
	if err != nil {
		return nil, err
	}
	mask := []string{"parent", "field_id"}
	for _, field := range suiObjectReadMask {
		mask = append(mask, "field_object."+field)
	}
	var token []byte
	seenTokens, seenObjects := map[string]bool{}, map[string]bool{}
	result := []SuiObject{}
	for page := 0; page < suiObjectLimit; page++ {
		var response *rpcv2.ListDynamicFieldsResponse
		err := c.invoke(ctx, "ListDynamicFields", func(ctx context.Context) error {
			var e error
			response, e = c.state.ListDynamicFields(ctx, &rpcv2.ListDynamicFieldsRequest{Parent: proto.String(id.Hex()), PageSize: proto.Uint32(suiObjectPageSize), PageToken: token, ReadMask: &fieldmaskpb.FieldMask{Paths: mask}})
			return e
		})
		if err != nil {
			return nil, err
		}
		if response == nil {
			return nil, fmt.Errorf("missing Sui dynamic fields page")
		}
		for _, field := range response.DynamicFields {
			object, err := decodeSuiObject(field.GetFieldObject())
			if err != nil {
				return nil, err
			}
			fieldParent, err := ParseSuiAddress(field.GetParent())
			fieldID, idErr := ParseSuiAddress(field.GetFieldId())
			if err != nil || idErr != nil || fieldParent != id || fieldID.Hex() != object.ID || object.Owner != id.Hex() || object.OwnerKind != "OBJECT" || seenObjects[object.ID] {
				return nil, fmt.Errorf("invalid or duplicate Sui dynamic field")
			}
			seenObjects[object.ID] = true
			result = append(result, object)
		}
		if len(result) > suiObjectLimit {
			return nil, fmt.Errorf("Sui dynamic field enumeration exceeds limit")
		}
		token = response.NextPageToken
		if len(token) == 0 {
			return result, nil
		}
		if seenTokens[string(token)] {
			return nil, fmt.Errorf("repeated Sui dynamic field page token")
		}
		seenTokens[string(token)] = true
	}
	return nil, fmt.Errorf("Sui dynamic field pagination exceeds limit")
}
