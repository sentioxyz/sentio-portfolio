package portfolio

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// suiGRPCTestServer is a scripted sui.rpc.v2 endpoint on a loopback listener. It answers by
// method, advances its head on every head read so a test can see the window a head read is
// reported with, pages balances the way the service does, and fails calls on demand.
type suiGRPCTestServer struct {
	rpcv2.UnimplementedStateServiceServer
	rpcv2.UnimplementedLedgerServiceServer
	t *testing.T

	mu       sync.Mutex
	chainID  string
	head     uint64
	headStep uint64
	pageSize int // balances per page the server is willing to return; zero means every one
	balances []*rpcv2.Balance
	metadata map[string]*rpcv2.CoinMetadata // absent coin -> NotFound; nil entry -> no metadata
	failNext map[string][]codes.Code        // statuses to answer per method before serving normally
	calls    []string
	owners   []string

	address string
	server  *grpc.Server
}

func newSuiGRPCTestServer(t *testing.T) *suiGRPCTestServer {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &suiGRPCTestServer{
		t:        t,
		chainID:  suiMainnetGenesisDigest,
		head:     320_000_000,
		headStep: 3,
		metadata: map[string]*rpcv2.CoinMetadata{},
		failNext: map[string][]codes.Code{},
		address:  listener.Addr().String(),
		server:   grpc.NewServer(),
	}
	rpcv2.RegisterStateServiceServer(server.server, server)
	rpcv2.RegisterLedgerServiceServer(server.server, server)
	go func() { _ = server.server.Serve(listener) }()
	t.Cleanup(server.server.Stop)
	return server
}

func (s *suiGRPCTestServer) endpoint() string {
	return "grpc://" + s.address
}

// record logs a call and returns the failure scripted for it, if any.
func (s *suiGRPCTestServer) record(method string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, method)
	if queue := s.failNext[method]; len(queue) > 0 {
		s.failNext[method] = queue[1:]
		return status.Error(queue[0], "scripted failure")
	}
	return nil
}

func (s *suiGRPCTestServer) fail(method string, statuses ...codes.Code) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext[method] = statuses
}

func (s *suiGRPCTestServer) methodCalls(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, call := range s.calls {
		if call == method {
			count++
		}
	}
	return count
}

// advanceHead returns the head and moves it on, as a live network does between two reads.
func (s *suiGRPCTestServer) advanceHead() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	head := s.head
	s.head += s.headStep
	return head
}

func (s *suiGRPCTestServer) checkpoint(sequence uint64) *rpcv2.Checkpoint {
	return &rpcv2.Checkpoint{
		SequenceNumber: proto.Uint64(sequence),
		Digest:         proto.String(suiTestDigest),
		Summary:        &rpcv2.CheckpointSummary{Timestamp: timestamppb.New(suiTestCheckpointTime)},
	}
}

func (s *suiGRPCTestServer) GetServiceInfo(
	context.Context, *rpcv2.GetServiceInfoRequest,
) (*rpcv2.GetServiceInfoResponse, error) {
	if err := s.record("GetServiceInfo"); err != nil {
		return nil, err
	}
	head := s.advanceHead()
	s.mu.Lock()
	chainID := s.chainID
	s.mu.Unlock()
	return &rpcv2.GetServiceInfoResponse{
		ChainId:                   proto.String(chainID),
		Chain:                     proto.String("mainnet"),
		CheckpointHeight:          proto.Uint64(head),
		LowestAvailableCheckpoint: proto.Uint64(0),
	}, nil
}

func (s *suiGRPCTestServer) GetCheckpoint(
	_ context.Context, request *rpcv2.GetCheckpointRequest,
) (*rpcv2.GetCheckpointResponse, error) {
	if err := s.record("GetCheckpoint"); err != nil {
		return nil, err
	}
	if len(request.GetReadMask().GetPaths()) == 0 {
		s.t.Errorf("GetCheckpoint asked for the whole checkpoint instead of a read mask")
	}
	switch selector := request.GetCheckpointId().(type) {
	case nil:
		return &rpcv2.GetCheckpointResponse{Checkpoint: s.checkpoint(s.advanceHead())}, nil
	case *rpcv2.GetCheckpointRequest_SequenceNumber:
		s.mu.Lock()
		head := s.head
		s.mu.Unlock()
		if selector.SequenceNumber > head {
			return nil, status.Errorf(codes.NotFound, "Checkpoint %d not found", selector.SequenceNumber)
		}
		return &rpcv2.GetCheckpointResponse{Checkpoint: s.checkpoint(selector.SequenceNumber)}, nil
	default:
		return nil, status.Error(codes.InvalidArgument, "unsupported checkpoint selector")
	}
}

func (s *suiGRPCTestServer) ListBalances(
	_ context.Context, request *rpcv2.ListBalancesRequest,
) (*rpcv2.ListBalancesResponse, error) {
	if err := s.record("ListBalances"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.owners = append(s.owners, request.GetOwner())
	offset := 0
	if token := request.GetPageToken(); len(token) > 0 {
		parsed, err := strconv.Atoi(string(token))
		if err != nil {
			return nil, status.Error(codes.InvalidArgument, "bad page token")
		}
		offset = parsed
	}
	end := len(s.balances)
	if s.pageSize > 0 && offset+s.pageSize < end {
		end = offset + s.pageSize
	}
	if requested := int(request.GetPageSize()); requested > 0 && offset+requested < end {
		end = offset + requested
	}
	response := &rpcv2.ListBalancesResponse{Balances: s.balances[offset:end]}
	if end < len(s.balances) {
		response.NextPageToken = []byte(strconv.Itoa(end))
	}
	return response, nil
}

func (s *suiGRPCTestServer) GetCoinInfo(
	_ context.Context, request *rpcv2.GetCoinInfoRequest,
) (*rpcv2.GetCoinInfoResponse, error) {
	if err := s.record("GetCoinInfo"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, known := s.metadata[request.GetCoinType()]
	if !known {
		return nil, status.Errorf(codes.NotFound, "Coin type %s not found", request.GetCoinType())
	}
	return &rpcv2.GetCoinInfoResponse{CoinType: proto.String(request.GetCoinType()), Metadata: entry}, nil
}

func suiTestBalance(coinType string, amount uint64) *rpcv2.Balance {
	return &rpcv2.Balance{
		CoinType:    proto.String(coinType),
		Balance:     proto.Uint64(amount),
		CoinBalance: proto.Uint64(amount),
	}
}

func newSuiGRPCTestClient(t *testing.T, ctx context.Context, chainID ChainID, server *suiGRPCTestServer) *SuiGRPCClient {
	client, err := DialSuiGRPC(ctx, chainID, server.endpoint(), SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestSuiGRPCTargetSelectsTransportSecurityFromTheScheme(t *testing.T) {
	for _, valid := range []struct{ endpoint, target, protocol string }{
		{"grpc://sui-node.internal:10001", "sui-node.internal:10001", "insecure"},
		{"http://127.0.0.1:10001/", "127.0.0.1:10001", "insecure"},
		{"grpcs://sui.example.com:443", "sui.example.com:443", "tls"},
		{"HTTPS://sui.example.com:443", "sui.example.com:443", "tls"},
		{" sui.example.com:443 ", "sui.example.com:443", "tls"},
	} {
		target, transportCredentials, err := suiGRPCTarget(valid.endpoint)
		if err != nil {
			t.Errorf("suiGRPCTarget(%q): %v", valid.endpoint, err)
			continue
		}
		if target != valid.target || transportCredentials.Info().SecurityProtocol != valid.protocol {
			t.Errorf("suiGRPCTarget(%q) = %q %s, want %q %s",
				valid.endpoint, target, transportCredentials.Info().SecurityProtocol, valid.target, valid.protocol)
		}
	}
	for _, invalid := range []string{
		"", "grpc://", "ftp://sui.example.com:443", "grpc://sui.example.com:443/v2",
		"grpcs://token@sui.example.com:443", "grpc://sui.example.com:443?key=1",
	} {
		if _, _, err := suiGRPCTarget(invalid); err == nil {
			t.Errorf("suiGRPCTarget(%q) accepted an endpoint it does not fully honour", invalid)
		}
	}
}

func TestDialSuiGRPCRejectsAnotherNetworkAndRedactsTheAddress(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	server.mu.Lock()
	server.chainID = suiTestDigest
	server.mu.Unlock()
	otherIdentifier, _ := suiChainIdentifierHex(suiTestDigest)
	_, err := DialSuiGRPC(context.Background(), 0, server.endpoint(), SuiMainnetChainIdentifier)
	if err == nil || !strings.Contains(err.Error(), otherIdentifier) || !strings.Contains(err.Error(), SuiMainnetChainIdentifier) {
		t.Fatalf("dial error = %v, want both chain identifiers named", err)
	}

	server.server.Stop()
	_, err = DialSuiGRPC(context.Background(), 0, server.endpoint(), SuiMainnetChainIdentifier)
	if err == nil {
		t.Fatal("dial succeeded against a stopped server")
	}
	if strings.Contains(err.Error(), server.address) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("dial error quotes the endpoint: %v", err)
	}
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("dial error code = %s, want Unavailable reachable through the wrapper", status.Code(err))
	}
}

func TestSuiGRPCHoldingsReportTheHeadWindowAndPaginate(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	gift := "0x1f100bc2173899bd12444f61631f74f7f49fe597d4b93feabf287bd63c932e9e::gift::GIFT"
	lpLong := "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<" + suiLongType +
		",0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>"
	server.pageSize = 2
	server.balances = []*rpcv2.Balance{
		suiTestBalance("0x2::sui::SUI", 96383083128),
		suiTestBalance("0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<0x2::sui::SUI, 0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>", 1000),
		suiTestBalance("0xdead::spent::SPENT", 0),
		suiTestBalance(gift, 2595000000000),
	}
	client := newSuiGRPCTestClient(t, context.Background(), 0, server)
	owner, _ := ParseSuiAddress("0x3cd40490b1e5a583f8717284291c6db97f604bd3d812b6e214591823d985b900")

	holdings, err := client.Holdings(context.Background(), owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if holdings.HistoryUnsupported {
		t.Fatal("a head read was marked HistoryUnsupported")
	}
	// The dial's identity check is one head read, then the head is read once before the balances
	// (GetServiceInfo) and once after (GetCheckpoint). The fake advances it by three on each read,
	// so the window must be ordered and the reported checkpoint the later observation.
	if holdings.HeadBeforeRead != 320_000_003 || holdings.Checkpoint.Sequence != 320_000_006 {
		t.Fatalf("window = %d..%d, want 320000003..320000006", holdings.HeadBeforeRead, holdings.Checkpoint.Sequence)
	}
	wantDigest, _ := decodeBase58(suiTestDigest)
	if !holdings.Checkpoint.Timestamp.Equal(suiTestCheckpointTime) || string(holdings.Checkpoint.Digest[:]) != string(wantDigest) {
		t.Fatalf("checkpoint = %+v", holdings.Checkpoint)
	}
	if len(holdings.Balances) != 3 {
		t.Fatalf("balances = %+v, want SUI, GIFT and the LP with the zero row dropped", holdings.Balances)
	}
	if holdings.Balances[0].CoinType != suiLongType || holdings.Balances[0].Amount.String() != "96383083128" {
		t.Fatalf("first balance = %+v, want the short SUI type normalized", holdings.Balances[0])
	}
	if holdings.Balances[1].CoinType != gift || holdings.Balances[1].Amount.String() != "2595000000000" {
		t.Fatalf("second balance = %+v, want GIFT", holdings.Balances[1])
	}
	if holdings.Balances[2].CoinType != lpLong {
		t.Fatalf("LP type = %s, want the spaced generic normalized to %s", holdings.Balances[2].CoinType, lpLong)
	}
	if pages := server.methodCalls("ListBalances"); pages != 2 {
		t.Fatalf("ListBalances calls = %d, want 2 pages of 2", pages)
	}
	server.mu.Lock()
	owners := server.owners
	server.mu.Unlock()
	for _, asked := range owners {
		if asked != owner.Hex() {
			t.Fatalf("ListBalances asked for owner %q, want %s", asked, owner.Hex())
		}
	}
}

func TestSuiGRPCHoldingsAtAFixedCheckpointAreEmptyByDecision(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	server.balances = []*rpcv2.Balance{suiTestBalance("0x2::sui::SUI", 1)}
	client := newSuiGRPCTestClient(t, context.Background(), 0, server)
	owner, _ := ParseSuiAddress("0x1")
	pin, err := newSuiCheckpoint(300_000_000, suiTestDigest, suiTestCheckpointTime)
	if err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	callsBefore := len(server.calls)
	server.mu.Unlock()

	holdings, err := client.Holdings(context.Background(), owner, &pin)
	if err != nil {
		t.Fatal(err)
	}
	if !holdings.HistoryUnsupported || len(holdings.Balances) != 0 {
		t.Fatalf("pinned holdings = %+v, want no balances marked HistoryUnsupported", holdings)
	}
	if holdings.Checkpoint != pin || holdings.HeadBeforeRead != pin.Sequence {
		t.Fatalf("pinned holdings name %d..%+v, want the pin itself", holdings.HeadBeforeRead, holdings.Checkpoint)
	}
	server.mu.Lock()
	callsAfter := len(server.calls)
	server.mu.Unlock()
	if callsAfter != callsBefore {
		t.Fatalf("a pinned read made %d round trips, want none", callsAfter-callsBefore)
	}
}

func TestSuiGRPCHoldingsPreserveHeadObservationsWhenBackendsDiffer(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	server.head = 320_000_010
	server.headStep = ^uint64(0) - 3 // each head read answers four lower, as a lagging backend would
	server.balances = []*rpcv2.Balance{suiTestBalance("0x2::sui::SUI", 1)}
	client := newSuiGRPCTestClient(t, context.Background(), 0, server)
	owner, _ := ParseSuiAddress("0x1")

	holdings, err := client.Holdings(context.Background(), owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	if holdings.HeadBeforeRead != holdings.Checkpoint.Sequence+4 {
		t.Fatalf("head observations were rewritten: %d..%d", holdings.HeadBeforeRead, holdings.Checkpoint.Sequence)
	}
	if len(holdings.Balances) != 1 || holdings.Balances[0].Amount.Int64() != 1 {
		t.Fatalf("head skew lost balances: %+v", holdings.Balances)
	}
}

func TestSuiGRPCHoldingsRejectABalanceWithoutAnAmount(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	server.balances = []*rpcv2.Balance{{CoinType: proto.String("0x2::sui::SUI")}}
	client := newSuiGRPCTestClient(t, context.Background(), 0, server)
	owner, _ := ParseSuiAddress("0x1")
	if _, err := client.Holdings(context.Background(), owner, nil); err == nil {
		t.Fatal("a balance without an amount was accepted as zero")
	}
}

func TestSuiGRPCCheckpointsResolveAndReportUnknownSequences(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	client := newSuiGRPCTestClient(t, context.Background(), 0, server)
	dialCalls := server.methodCalls("GetCheckpoint")

	latest, err := client.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The dial's identity check already moved the fake's head on by one step.
	wantDigest, _ := decodeBase58(suiTestDigest)
	if latest.Sequence != 320_000_003 || string(latest.Digest[:]) != string(wantDigest) {
		t.Fatalf("latest = %+v", latest)
	}
	pinned, err := client.CheckpointBySequence(context.Background(), 320_000_000)
	if err != nil || pinned.Sequence != 320_000_000 || !pinned.Timestamp.Equal(suiTestCheckpointTime) {
		t.Fatalf("pinned = %+v, %v", pinned, err)
	}
	_, err = client.CheckpointBySequence(context.Background(), 999_999_999_999)
	if !errors.Is(err, errSuiCheckpointUnavailable) {
		t.Fatalf("unknown checkpoint error = %v, want errSuiCheckpointUnavailable", err)
	}
	if got := server.methodCalls("GetCheckpoint") - dialCalls; got != 3 {
		t.Fatalf("GetCheckpoint calls = %d, want 3: NotFound must not be retried", got)
	}
}

func TestSuiGRPCRetriesTransientStatusesButNotVerdictsAndRedactsTheAddress(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	observer := &recordingObserver{}
	ctx := withObserver(context.Background(), observer)
	client := newSuiGRPCTestClient(t, ctx, Ethereum, server)

	server.fail("GetCheckpoint", codes.Unavailable, codes.ResourceExhausted)
	if _, err := client.LatestCheckpoint(ctx); err != nil {
		t.Fatalf("latest after two transient statuses: %v", err)
	}
	observer.mu.Lock()
	attempts := 0
	for _, observation := range observer.rpcs {
		if observation.Method == "GetCheckpoint" {
			attempts++
			if observation.ChainID != Ethereum || observation.Attempt != attempts {
				t.Fatalf("observation = %+v, want chain %v attempt %d", observation, Ethereum, attempts)
			}
		}
	}
	observer.mu.Unlock()
	if attempts != 3 {
		t.Fatalf("GetCheckpoint attempts observed = %d, want 3", attempts)
	}

	server.fail("GetCheckpoint", codes.Unauthenticated)
	callsBefore := server.methodCalls("GetCheckpoint")
	_, err := client.LatestCheckpoint(ctx)
	if err == nil || status.Code(err) != codes.Unauthenticated || !strings.Contains(err.Error(), "Unauthenticated") {
		t.Fatalf("unauthenticated error = %v", err)
	}
	if calls := server.methodCalls("GetCheckpoint") - callsBefore; calls != 1 {
		t.Fatalf("Unauthenticated was followed by %d more calls, want none", calls-1)
	}

	server.server.Stop()
	_, err = client.LatestCheckpoint(ctx)
	if err == nil || strings.Contains(err.Error(), server.address) {
		t.Fatalf("transport error = %v, want a redacted failure", err)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, observation := range observer.rpcs {
		if observation.Err != nil && strings.Contains(observation.Err.Error(), server.address) {
			t.Fatalf("observation quotes the endpoint: %v", observation.Err)
		}
	}
}

func TestSuiGRPCCoinMetadataNamesUnusableCoins(t *testing.T) {
	server := newSuiGRPCTestServer(t)
	gift := "0x1f100bc2173899bd12444f61631f74f7f49fe597d4b93feabf287bd63c932e9e::gift::GIFT"
	bad := "0x5ae9d8b7c4c4c3a55ff5a2d1a6cd6b9dd0c1f0d4dcd8c43a4eabf287bd63c932::bad::BAD"
	bare := "0x000000000000000000000000000000000000000000000000000000000000abcd::bare::BARE"
	unknown := "0x0000000000000000000000000000000000000000000000000000000000000001::nonexistent::NOPE"
	server.metadata = map[string]*rpcv2.CoinMetadata{
		suiLongType: {Decimals: proto.Uint32(9), Name: proto.String("Sui"), Symbol: proto.String("SUI")},
		gift:        {Name: proto.String("Gift"), Symbol: proto.String("GIFT")},
		bad:         {Decimals: proto.Uint32(6), Name: proto.String("Bad"), Symbol: proto.String("B\x00AD")},
		bare:        nil,
	}
	client := newSuiGRPCTestClient(t, context.Background(), 0, server)

	metadata, unusable, err := client.CoinMetadata(context.Background(), []string{suiLongType, gift, bad, bare, unknown})
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 || metadata[suiLongType] != (SuiCoinMetadata{CoinType: suiLongType, Symbol: "SUI", Name: "Sui", Decimals: 9}) {
		t.Fatalf("metadata = %+v, want SUI only", metadata)
	}
	if len(unusable) != 4 {
		t.Fatalf("unusable = %v, want four coins", unusable)
	}
	for coinType, fragment := range map[string]string{
		gift:    "declares no decimals",
		bad:     "symbol",
		bare:    "no on-chain metadata",
		unknown: "not found",
	} {
		if reason := unusable[coinType]; reason == nil || !strings.Contains(reason.Error(), fragment) {
			t.Errorf("unusable[%s] = %v, want %q", coinType, reason, fragment)
		}
	}
	if status.Code(unusable[unknown]) != codes.NotFound {
		t.Fatalf("unknown coin reason = %v, want NotFound reachable through the wrapper", unusable[unknown])
	}
	if calls := server.methodCalls("GetCoinInfo"); calls != 5 {
		t.Fatalf("GetCoinInfo calls = %d, want one per coin", calls)
	}

	// A transport failure that outlasts the retries fails the whole read rather than naming the
	// coin as unusable.
	server.fail("GetCoinInfo", codes.Unavailable, codes.Unavailable, codes.Unavailable)
	if _, _, err := client.CoinMetadata(context.Background(), []string{suiLongType}); err == nil || !strings.Contains(err.Error(), "3 attempts") {
		t.Fatalf("metadata after a persistent transport failure: %v, want a failure after 3 attempts", err)
	}

	empty, unusable, err := client.CoinMetadata(context.Background(), nil)
	if err != nil || len(empty) != 0 || len(unusable) != 0 {
		t.Fatalf("empty read = %v %v %v", empty, unusable, err)
	}
}
