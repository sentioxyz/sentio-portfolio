package portfolio

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// suiMainnetGenesisDigest is the base58 genesis checkpoint digest Sui GraphQL returns as the
// mainnet chain identifier. Its first four bytes are SuiMainnetChainIdentifier.
const suiMainnetGenesisDigest = "4btiuiMPvEENsttpZC7CZ53DruC3MAgfznDbASZ7DR6S"

func TestParseSuiAddressPadsShortFormsToTheLongForm(t *testing.T) {
	address, err := ParseSuiAddress("0x2")
	if err != nil {
		t.Fatal(err)
	}
	want := "0x0000000000000000000000000000000000000000000000000000000000000002"
	if address.Hex() != want {
		t.Fatalf("address = %s, want %s", address.Hex(), want)
	}
	mixed, err := ParseSuiAddress("0x3CD40490B1E5A583F8717284291C6DB97F604BD3D812B6E214591823D985B900")
	if err != nil {
		t.Fatal(err)
	}
	if mixed.Hex() != "0x3cd40490b1e5a583f8717284291c6db97f604bd3d812b6e214591823d985b900" {
		t.Fatalf("address = %s, want lowercase", mixed.Hex())
	}
	for _, invalid := range []string{
		"", "0x", "2", "0x" + strings.Repeat("0", 65), "0xzz", "0x 2", "0x3cd4::sui::SUI",
	} {
		if _, err := ParseSuiAddress(invalid); err == nil {
			t.Errorf("ParseSuiAddress(%q) accepted an invalid address", invalid)
		}
	}
}

func TestNormalizeMoveTypeMatchesTheChainRepr(t *testing.T) {
	suiLong := "0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI"
	cases := map[string]string{
		"0x2::sui::SUI":   suiLong,
		" 0x2::sui::SUI ": suiLong,
		"0X02::sui::SUI":  suiLong,
		suiLong:           suiLong,
		"0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<0x2::sui::SUI, 0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>": "0xba153169476e8c3114962261d1edc70de5ad9781b83cc617ecc8c1923191cae0::pair::LP<" +
			suiLong + ",0x72f3d911745a18145cff3d697a64a59fce6908a5580141ca149da2188b831fd2::fash::FASH>",
		"0x1::option::Option<vector<u8>>": "0x0000000000000000000000000000000000000000000000000000000000000001::option::Option<vector<u8>>",
		"0xa::m::S<u64,bool, address>":    "0x000000000000000000000000000000000000000000000000000000000000000a::m::S<u64,bool,address>",
	}
	for input, want := range cases {
		got, err := NormalizeMoveType(input)
		if err != nil {
			t.Errorf("NormalizeMoveType(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeMoveType(%q) = %q, want %q", input, got, want)
		}
	}
	for _, invalid := range []string{
		"", "sui::SUI", "0x2::sui", "0x2::sui::", "0x2::9sui::SUI", "0x2::sui::SUI<", "0x2::sui::SUI<u64",
		"0x2::sui::SUI<u64;bool>", "0x2::sui::SUI<notatype>", "0x2::sui::SUI extra", "0x2::sui::SUI<vector>",
		"0x" + strings.Repeat("f", 65) + "::a::B", "0x2::su-i::SUI",
	} {
		if _, err := NormalizeMoveType(invalid); err == nil {
			t.Errorf("NormalizeMoveType(%q) accepted an invalid type", invalid)
		}
	}
	nested := "0x1::a::A"
	for depth := 0; depth < moveTypeMaxDepth+1; depth++ {
		nested = "0x1::a::A<" + nested + ">"
	}
	if _, err := NormalizeMoveType(nested); err == nil {
		t.Error("NormalizeMoveType accepted a type nested past the depth bound")
	}
}

func TestDecodeBase58RecoversTheMainnetChainIdentifier(t *testing.T) {
	digest, err := decodeBase58(suiMainnetGenesisDigest)
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 32 || hex.EncodeToString(digest[:4]) != SuiMainnetChainIdentifier {
		t.Fatalf("digest = %x", digest)
	}
	leading, err := decodeBase58("11a")
	if err != nil || len(leading) != 3 || leading[0] != 0 || leading[1] != 0 || leading[2] != 33 {
		t.Fatalf("decodeBase58(11a) = %x, %v", leading, err)
	}
	if _, err := decodeBase58("0OIl"); err == nil {
		t.Error("decodeBase58 accepted characters outside the alphabet")
	}
	for _, spelling := range []string{suiMainnetGenesisDigest, "35834A8A"} {
		identifier, err := suiChainIdentifierHex(spelling)
		if err != nil || identifier != SuiMainnetChainIdentifier {
			t.Errorf("suiChainIdentifierHex(%q) = %q, %v", spelling, identifier, err)
		}
	}
}

// suiTestServer answers GraphQL operations by name so a test can assert the variables the client
// sent and script the responses page by page.
type suiTestServer struct {
	t        *testing.T
	handlers map[string]func(variables map[string]any, calls int) (status int, body string)
	calls    map[string]*atomic.Int32
	requests []suiGraphQLRequest
}

func newSuiTestServer(t *testing.T) *suiTestServer {
	return &suiTestServer{
		t:        t,
		handlers: make(map[string]func(map[string]any, int) (int, string)),
		calls:    make(map[string]*atomic.Int32),
	}
}

func (s *suiTestServer) handle(operation string, handler func(variables map[string]any, calls int) (int, string)) {
	s.handlers[operation] = handler
	s.calls[operation] = new(atomic.Int32)
}

func (s *suiTestServer) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	var payload suiGraphQLRequest
	if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
		s.t.Errorf("decode request: %v", err)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	s.requests = append(s.requests, payload)
	handler, exists := s.handlers[payload.OperationName]
	if !exists {
		s.t.Errorf("unexpected operation %q", payload.OperationName)
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	calls := int(s.calls[payload.OperationName].Add(1))
	status, body := handler(payload.Variables, calls)
	writer.Header().Set("content-type", "application/json")
	writer.WriteHeader(status)
	_, _ = writer.Write([]byte(body))
}

func (s *suiTestServer) identifier(identifier string) {
	s.handle("SuiChainIdentifier", func(map[string]any, int) (int, string) {
		return http.StatusOK, fmt.Sprintf(`{"data":{"chainIdentifier":%q}}`, identifier)
	})
}

func newSuiTestClient(t *testing.T, server *suiTestServer) *SuiClient {
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	client, err := DialSui(context.Background(), 0, httpServer.URL, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func TestDialSuiRejectsAnotherNetwork(t *testing.T) {
	server := newSuiTestServer(t)
	server.identifier("4c78adac")
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	_, err := DialSui(context.Background(), 0, httpServer.URL, SuiMainnetChainIdentifier)
	if err == nil || !strings.Contains(err.Error(), "4c78adac") {
		t.Fatalf("DialSui error = %v, want a chain identifier mismatch", err)
	}
	if _, err := DialSui(context.Background(), 0, "", SuiMainnetChainIdentifier); err == nil {
		t.Fatal("DialSui accepted an empty endpoint")
	}
}

func suiCheckpointJSON(sequence uint64, digest string, timestamp string) string {
	return fmt.Sprintf(`{"sequenceNumber":%d,"digest":%q,"timestamp":%q}`, sequence, digest, timestamp)
}

const suiTestDigest = "FP87xzMYCoratjcphxoF9HrSoRc1MXaWuSEWLGpdJ88i"

func TestSuiCheckpointsDecodeDigestAndTimestamp(t *testing.T) {
	server := newSuiTestServer(t)
	server.identifier(suiMainnetGenesisDigest)
	server.handle("SuiLatestCheckpoint", func(map[string]any, int) (int, string) {
		return http.StatusOK, `{"data":{"checkpoint":` +
			suiCheckpointJSON(320105168, suiTestDigest, "2026-09-08T06:53:28.219Z") + `}}`
	})
	server.handle("SuiCheckpoint", func(variables map[string]any, _ int) (int, string) {
		if variables["sequence"] != float64(320000000) {
			return http.StatusOK, `{"data":{"checkpoint":null}}`
		}
		return http.StatusOK, `{"data":{"checkpoint":` +
			suiCheckpointJSON(320000000, "HCLvSWjodwUuzJVLRUVd2LnhDNuAfeiW1hw2RNkJ1ekY", "2026-09-08T00:11:30.073Z") + `}}`
	})
	client := newSuiTestClient(t, server)

	latest, err := client.LatestCheckpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantDigest, _ := decodeBase58(suiTestDigest)
	if latest.Sequence != 320105168 || hex.EncodeToString(latest.Digest[:]) != hex.EncodeToString(wantDigest) {
		t.Fatalf("latest = %+v", latest)
	}
	if !latest.Timestamp.Equal(time.Date(2026, 9, 8, 6, 53, 28, 219_000_000, time.UTC)) {
		t.Fatalf("latest timestamp = %s", latest.Timestamp)
	}

	pinned, err := client.CheckpointBySequence(context.Background(), 320000000)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.Sequence != 320000000 ||
		!pinned.Timestamp.Equal(time.Date(2026, 9, 8, 0, 11, 30, 73_000_000, time.UTC)) {
		t.Fatalf("pinned = %+v", pinned)
	}
	_, err = client.CheckpointBySequence(context.Background(), 999_999_999_999)
	if !errors.Is(err, errSuiCheckpointUnavailable) {
		t.Fatalf("future checkpoint error = %v, want errSuiCheckpointUnavailable", err)
	}
}

func suiBalancePage(hasNext bool, cursor string, rows ...string) string {
	return fmt.Sprintf(
		`{"data":{"address":{"balances":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[%s]}}}}`,
		hasNext, cursor, strings.Join(rows, ","),
	)
}

func suiBalanceRow(coinType string, amount string) string {
	return fmt.Sprintf(`{"coinType":{"repr":%q},"totalBalance":%q}`, coinType, amount)
}

func TestSuiBalancesPaginateAtThePinnedCheckpoint(t *testing.T) {
	server := newSuiTestServer(t)
	server.identifier(suiMainnetGenesisDigest)
	owner, _ := ParseSuiAddress("0x3cd40490b1e5a583f8717284291c6db97f604bd3d812b6e214591823d985b900")
	server.handle("SuiBalances", func(variables map[string]any, calls int) (int, string) {
		if variables["owner"] != owner.Hex() || variables["checkpoint"] != float64(320000000) ||
			variables["first"] != float64(suiBalancePageSize) {
			t.Errorf("balances variables = %v", variables)
		}
		switch calls {
		case 1:
			if _, hasAfter := variables["after"]; hasAfter {
				t.Errorf("first page carried a cursor: %v", variables)
			}
			return http.StatusOK, suiBalancePage(true, "cursor-1",
				suiBalanceRow("0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI", "57673653528"),
				suiBalanceRow("0xdead::spent::SPENT", "0"),
			)
		case 2:
			if variables["after"] != "cursor-1" {
				t.Errorf("second page cursor = %v", variables["after"])
			}
			return http.StatusOK, suiBalancePage(false, "",
				suiBalanceRow("0x5ae9d8b7c4c4c3a55ff5a2d1a6cd6b9dd0c1f0d4dcd8c43a4eabf287bd63c932::gift::GIFT", "2595000000000"),
			)
		default:
			t.Errorf("unexpected balances page %d", calls)
			return http.StatusInternalServerError, ""
		}
	})
	client := newSuiTestClient(t, server)

	balances, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 320000000}, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(balances) != 2 {
		t.Fatalf("balances = %+v, want SUI and GIFT with the zero row dropped", balances)
	}
	if balances[0].CoinType != "0x0000000000000000000000000000000000000000000000000000000000000002::sui::SUI" ||
		balances[0].Amount.String() != "57673653528" {
		t.Fatalf("first balance = %+v", balances[0])
	}
	if !strings.HasSuffix(balances[1].CoinType, "::gift::GIFT") || balances[1].Amount.String() != "2595000000000" {
		t.Fatalf("second balance = %+v", balances[1])
	}
}

func TestSuiBalancesReportTheConsistentRangeAndMalformedRows(t *testing.T) {
	server := newSuiTestServer(t)
	server.identifier(suiMainnetGenesisDigest)
	server.handle("SuiBalances", func(variables map[string]any, _ int) (int, string) {
		switch variables["checkpoint"] {
		case float64(200000000):
			return http.StatusOK, `{"data":{"address":{"balances":null}},"errors":[{"message":"Request is outside consistent range","path":["address","balances"],"extensions":{"code":"BAD_USER_INPUT"}}]}`
		case float64(1):
			return http.StatusOK, suiBalancePage(false, "",
				suiBalanceRow("0x2::sui::SUI", "1"), suiBalanceRow("0x02::sui::SUI", "2"))
		case float64(2):
			return http.StatusOK, suiBalancePage(false, "", suiBalanceRow("0x2::sui::SUI", "-1"))
		case float64(3):
			return http.StatusOK, suiBalancePage(true, "", suiBalanceRow("0x2::sui::SUI", "1"))
		case float64(4):
			return http.StatusOK, `{"data":{"address":null}}`
		default:
			return http.StatusOK, `{"data":{"address":{"balances":{"pageInfo":{"hasNextPage":false,"endCursor":null},"nodes":[]}}}}`
		}
	})
	client := newSuiTestClient(t, server)
	owner, _ := ParseSuiAddress("0xdeadbeef")

	_, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 200000000}, owner)
	if !errors.Is(err, errSuiOutsideConsistentRange) {
		t.Fatalf("old checkpoint error = %v, want errSuiOutsideConsistentRange", err)
	}
	if _, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 1}, owner); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate coin type error = %v", err)
	}
	if _, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 2}, owner); err == nil ||
		!strings.Contains(err.Error(), "invalid amount") {
		t.Fatalf("negative amount error = %v", err)
	}
	if _, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 3}, owner); err == nil ||
		!strings.Contains(err.Error(), "without a cursor") {
		t.Fatalf("missing cursor error = %v", err)
	}
	if _, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 4}, owner); err == nil {
		t.Fatal("a null address selection without an error was accepted")
	}
	empty, err := client.Balances(context.Background(), SuiCheckpoint{Sequence: 5}, owner)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty address = %+v, %v", empty, err)
	}
}

func TestSuiClientRetriesTransientResponsesAndRedactsTheEndpoint(t *testing.T) {
	server := newSuiTestServer(t)
	server.identifier(suiMainnetGenesisDigest)
	server.handle("SuiLatestCheckpoint", func(_ map[string]any, calls int) (int, string) {
		switch calls {
		case 1:
			return http.StatusServiceUnavailable, "upstream unavailable"
		case 2:
			return http.StatusTooManyRequests, "slow down"
		default:
			return http.StatusOK, `{"data":{"checkpoint":` +
				suiCheckpointJSON(7, suiTestDigest, "2026-09-08T06:53:28Z") + `}}`
		}
	})
	server.handle("SuiBalancesRange", func(_ map[string]any, calls int) (int, string) {
		return http.StatusUnauthorized, "who are you"
	})
	server.handle("SuiCheckpoint", func(map[string]any, int) (int, string) {
		return http.StatusOK, `{"data":null,"errors":[{"message":"Failed to parse \"UInt53\""}]}`
	})
	httpServer := httptest.NewServer(server)
	t.Cleanup(httpServer.Close)
	observer := &recordingObserver{}
	ctx := withObserver(context.Background(), observer)
	client, err := DialSui(ctx, Ethereum, httpServer.URL, SuiMainnetChainIdentifier)
	if err != nil {
		t.Fatal(err)
	}

	checkpoint, err := client.LatestCheckpoint(ctx)
	if err != nil || checkpoint.Sequence != 7 {
		t.Fatalf("latest after retries = %+v, %v", checkpoint, err)
	}
	if got := server.calls["SuiLatestCheckpoint"].Load(); got != 3 {
		t.Fatalf("latest checkpoint attempts = %d, want 3", got)
	}

	_, _, err = client.BalancesRange(ctx)
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("unauthorized error = %v", err)
	}
	if got := server.calls["SuiBalancesRange"].Load(); got != 1 {
		t.Fatalf("a 401 was retried %d times", got)
	}

	_, err = client.CheckpointBySequence(ctx, 1)
	if err == nil || !strings.Contains(err.Error(), "UInt53") {
		t.Fatalf("GraphQL rejection error = %v", err)
	}
	if got := server.calls["SuiCheckpoint"].Load(); got != 1 {
		t.Fatalf("a GraphQL rejection was retried %d times", got)
	}

	host := strings.TrimPrefix(httpServer.URL, "http://")
	rpcObservations := 0
	for _, observation := range observer.rpcs {
		rpcObservations++
		if observation.ChainID != Ethereum || !strings.HasPrefix(observation.Method, "graphql Sui") {
			t.Fatalf("observation = %+v", observation)
		}
		if observation.Err != nil && strings.Contains(observation.Err.Error(), host) {
			t.Fatalf("observation quotes the endpoint: %v", observation.Err)
		}
	}
	// dial + three latest attempts + one range + one checkpoint.
	if rpcObservations != 6 {
		t.Fatalf("RPC observations = %d, want 6", rpcObservations)
	}
	_, err = client.LatestCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	httpServer.Close()
	_, err = client.LatestCheckpoint(ctx)
	if err == nil || strings.Contains(err.Error(), host) {
		t.Fatalf("transport error = %v, want a redacted failure", err)
	}
}

func TestSuiCoinMetadataBatchesAliasesAndNamesUnusableCoins(t *testing.T) {
	server := newSuiTestServer(t)
	server.identifier(suiMainnetGenesisDigest)
	coinTypes := make([]string, 0, suiMetadataBatchSize+3)
	for index := 0; index < suiMetadataBatchSize+3; index++ {
		coinTypes = append(coinTypes, fmt.Sprintf(
			"0x%064x::coin::C%d", index+1, index,
		))
	}
	server.handle("SuiCoinMetadata", func(variables map[string]any, calls int) (int, string) {
		entries := make([]string, 0, len(variables))
		for index := 0; ; index++ {
			coinType, present := variables[fmt.Sprintf("t%d", index)]
			if !present {
				break
			}
			alias := fmt.Sprintf("m%d", index)
			switch coinType {
			case coinTypes[0]:
				entries = append(entries, fmt.Sprintf(`%q:{"decimals":9,"symbol":"SUI","name":"Sui"}`, alias))
			case coinTypes[1]:
				entries = append(entries, fmt.Sprintf(`%q:null`, alias))
			case coinTypes[2]:
				entries = append(entries, fmt.Sprintf(`%q:{"decimals":null,"symbol":"X","name":"x"}`, alias))
			case coinTypes[3]:
				entries = append(entries, fmt.Sprintf(`%q:{"decimals":40,"symbol":"X","name":"x"}`, alias))
			case coinTypes[4]:
				entries = append(entries, fmt.Sprintf(`%q:{"decimals":6,"symbol":"  ","name":"blank"}`, alias))
			default:
				entries = append(entries, fmt.Sprintf(`%q:{"decimals":6,"symbol":"C%d","name":"c"}`, alias, index))
			}
		}
		if calls == 1 && len(entries) != suiMetadataBatchSize {
			t.Errorf("first metadata batch aliased %d coins, want %d", len(entries), suiMetadataBatchSize)
		}
		return http.StatusOK, `{"data":{` + strings.Join(entries, ",") + `}}`
	})
	client := newSuiTestClient(t, server)

	metadata, unusable, err := client.CoinMetadata(context.Background(), coinTypes)
	if err != nil {
		t.Fatal(err)
	}
	if got := server.calls["SuiCoinMetadata"].Load(); got != 2 {
		t.Fatalf("metadata requests = %d, want 2", got)
	}
	if len(metadata) != len(coinTypes)-4 || len(unusable) != 4 {
		t.Fatalf("usable = %d, unusable = %d", len(metadata), len(unusable))
	}
	sui := metadata[coinTypes[0]]
	if sui.Symbol != "SUI" || sui.Decimals != 9 || sui.Name != "Sui" || sui.CoinType != coinTypes[0] {
		t.Fatalf("SUI metadata = %+v", sui)
	}
	for index, want := range map[int]string{1: "no on-chain metadata", 2: "no decimals", 3: "invalid", 4: "empty"} {
		reason, exists := unusable[coinTypes[index]]
		if !exists || !strings.Contains(reason.Error(), want) {
			t.Errorf("unusable[%d] = %v, want %q", index, reason, want)
		}
	}
}
