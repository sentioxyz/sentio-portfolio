package portfolio

import (
	"context"
	"testing"

	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

type lineageLedgerStub struct {
	rpcv2.LedgerServiceClient
	object           *rpcv2.Object
	tx               *rpcv2.ExecutedTransaction
	requestedVersion uint64
}

func (s *lineageLedgerStub) GetObject(_ context.Context, r *rpcv2.GetObjectRequest, _ ...grpc.CallOption) (*rpcv2.GetObjectResponse, error) {
	s.requestedVersion = r.GetVersion()
	return &rpcv2.GetObjectResponse{Object: s.object}, nil
}
func (s *lineageLedgerStub) GetTransaction(_ context.Context, _ *rpcv2.GetTransactionRequest, _ ...grpc.CallOption) (*rpcv2.GetTransactionResponse, error) {
	return &rpcv2.GetTransactionResponse{Transaction: s.tx}, nil
}

func TestSuiLineagePinsObjectVersion(t *testing.T) {
	fields, _ := structpb.NewValue(map[string]any{"id": naviAddress("0x11")})
	stub := &lineageLedgerStub{object: &rpcv2.Object{ObjectId: proto.String(naviAddress("0x11")), Version: proto.Uint64(7), ObjectType: proto.String(naviStorageType), Owner: &rpcv2.Owner{Kind: rpcv2.Owner_SHARED.Enum()}, Json: fields, PreviousTransaction: proto.String(suiTestDigest)}}
	c := &SuiGRPCClient{ledger: stub}
	object, err := c.ObjectAtVersion(context.Background(), "0x11", 7)
	if err != nil || stub.requestedVersion != 7 || object.PreviousTransaction != suiTestDigest {
		t.Fatalf("%+v %v", object, err)
	}
	if _, err := c.ObjectAtVersion(context.Background(), "0x11", 6); err == nil {
		t.Fatal("accepted latest under a previous version")
	}
	stub.object.Json = nil // Packages have no Move JSON.
	if digest, err := c.PreviousTransaction(context.Background(), "0x11"); err != nil || digest != suiTestDigest {
		t.Fatalf("%s %v", digest, err)
	}
}

func TestSuiLineageRejectsMissingTransactionAndDeduplicatesChanges(t *testing.T) {
	stub := &lineageLedgerStub{}
	c := &SuiGRPCClient{ledger: stub}
	if _, err := c.TransactionObjectChanges(context.Background(), suiTestDigest); err == nil {
		t.Fatal("accepted missing transaction")
	}
	change := &rpcv2.ChangedObject{ObjectId: proto.String(naviAddress("0x11")), IdOperation: rpcv2.ChangedObject_CREATED.Enum(), InputState: rpcv2.ChangedObject_INPUT_OBJECT_STATE_DOES_NOT_EXIST.Enum(), OutputState: rpcv2.ChangedObject_OUTPUT_OBJECT_STATE_OBJECT_WRITE.Enum(), OutputOwner: &rpcv2.Owner{Kind: rpcv2.Owner_SHARED.Enum()}, OutputVersion: proto.Uint64(7)}
	stub.tx = &rpcv2.ExecutedTransaction{Digest: proto.String(suiTestDigest), Effects: &rpcv2.TransactionEffects{Status: &rpcv2.ExecutionStatus{Success: proto.Bool(true)}, ChangedObjects: []*rpcv2.ChangedObject{change}}}
	got, err := c.TransactionObjectChanges(context.Background(), suiTestDigest)
	if err != nil || len(got) != 1 || !got[0].Created || got[0].OutputVersion != 7 {
		t.Fatalf("%+v %v", got, err)
	}
	stub.tx.Effects.ChangedObjects = append(stub.tx.Effects.ChangedObjects, change)
	if _, err := c.TransactionObjectChanges(context.Background(), suiTestDigest); err == nil {
		t.Fatal("accepted duplicate object changes")
	}
}
