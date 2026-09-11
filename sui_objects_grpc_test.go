package portfolio

import (
	"context"
	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	statuspb "google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"testing"
)

type objectLedgerStub struct {
	rpcv2.LedgerServiceClient
	response *rpcv2.BatchGetObjectsResponse
}

func (s *objectLedgerStub) BatchGetObjects(_ context.Context, r *rpcv2.BatchGetObjectsRequest, _ ...grpc.CallOption) (*rpcv2.BatchGetObjectsResponse, error) {
	return s.response, nil
}
func TestSuiObjectBatchDistinguishesAbsentAndFailed(t *testing.T) {
	fields, _ := structpb.NewValue(map[string]any{"id": naviAddress("0x11"), "value": "9007199254740993"})
	obj := &rpcv2.Object{ObjectId: proto.String(naviAddress("0x11")), Version: proto.Uint64(1), Owner: &rpcv2.Owner{Kind: rpcv2.Owner_SHARED.Enum()}, ObjectType: proto.String(naviStorageType), Json: fields}
	for _, tc := range []struct {
		name    string
		items   []*rpcv2.GetObjectResult
		wantErr bool
		count   int
	}{
		{"present", []*rpcv2.GetObjectResult{{Result: &rpcv2.GetObjectResult_Object{Object: obj}}}, false, 1},
		{"absent", []*rpcv2.GetObjectResult{{Result: &rpcv2.GetObjectResult_Error{Error: &statuspb.Status{Code: 5}}}}, false, 0},
		{"failed", []*rpcv2.GetObjectResult{{Result: &rpcv2.GetObjectResult_Error{Error: &statuspb.Status{Code: 14}}}}, true, 0},
		{"empty result", []*rpcv2.GetObjectResult{{}}, true, 0},
		{"truncated batch", nil, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &SuiGRPCClient{ledger: &objectLedgerStub{response: &rpcv2.BatchGetObjectsResponse{Objects: tc.items}}}
			got, err := c.Objects(context.Background(), []string{"0x11"})
			if (err != nil) != tc.wantErr || len(got) != tc.count {
				t.Fatalf("%v %v", got, err)
			}
			if tc.count == 1 {
				f, _ := suiObjectFields(got[naviAddress("0x11")].Content)
				n, _ := f.uint("value")
				if n.String() != "9007199254740993" {
					t.Fatal("integer lost precision")
				}
			}
		})
	}
}
