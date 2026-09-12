package portfolio

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	rpcv2 "github.com/sentioxyz/sui-apis/sui/rpc/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// SuiGRPCClient reads a Sui network through the sui.rpc.v2 gRPC services a fullnode, or a proxy
// in front of one, serves: LedgerService for checkpoints and the network's identity, StateService
// for an address's balances and a coin's metadata. Those services answer for the head only, so a
// holdings read is bracketed by two head observations and reported as a window (see SuiHoldings),
// and a pinned read returns no balances, marked HistoryUnsupported, by decision.
//
// The endpoint's scheme selects the transport security: grpcs:// and https:// use TLS, grpc://
// and http:// are plaintext for an endpoint reached inside a private network, and a bare host:port
// is TLS, because an unmarked endpoint is more likely public than not.
type SuiGRPCClient struct {
	chainID ChainID
	conn    *grpc.ClientConn
	state   rpcv2.StateServiceClient
	ledger  rpcv2.LedgerServiceClient
}

var _ SuiReader = (*SuiGRPCClient)(nil)

const (
	suiGRPCAttempts     = 3
	suiGRPCCallTimeout  = 20 * time.Second
	suiGRPCRetryInitial = 300 * time.Millisecond
	// suiGRPCBalancePageSize is how many balances one ListBalances page asks for; the server may
	// answer fewer and hand back a token.
	suiGRPCBalancePageSize = 500
	// suiGRPCMaxBalancePages bounds an enumeration: an address with more coin types than this
	// many pages hold is reported as a failure rather than truncated.
	suiGRPCMaxBalancePages = 200
	// suiGRPCMetadataConcurrency is how many GetCoinInfo calls are in flight at once; the service
	// has no batch form of it.
	suiGRPCMetadataConcurrency = 8
	// suiGRPCMaxMessageBytes bounds one response. A balances page or a coin's metadata is far
	// smaller, so hitting the bound means the server is not answering the question asked.
	suiGRPCMaxMessageBytes = 32 << 20
)

// suiCheckpointReadMask asks GetCheckpoint for what a pin needs and nothing else; without a mask
// the service returns the checkpoint's contents too.
var suiCheckpointReadMask = []string{"sequence_number", "digest", "summary.timestamp"}

// suiGRPCTarget splits an endpoint into the target grpc dials and the transport security its
// scheme selects. Anything beyond host:port is refused: a path or credentials in the endpoint
// would be silently ignored by gRPC, and an endpoint whose spelling the client does not fully
// honour is one it should not connect to.
func suiGRPCTarget(endpoint string) (string, credentials.TransportCredentials, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", nil, errors.New("Sui gRPC endpoint is not configured")
	}
	scheme, hostPort, found := strings.Cut(endpoint, "://")
	if !found {
		scheme, hostPort = "grpcs", endpoint
	}
	hostPort = strings.TrimSuffix(hostPort, "/")
	if hostPort == "" || strings.ContainsAny(hostPort, "/?#@ ") {
		return "", nil, errors.New("Sui gRPC endpoint must be [scheme://]host:port")
	}
	switch strings.ToLower(scheme) {
	case "grpcs", "https":
		return hostPort, credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12}), nil
	case "grpc", "http":
		return hostPort, insecure.NewCredentials(), nil
	default:
		return "", nil, fmt.Errorf("Sui gRPC endpoint scheme %q is not grpc, grpcs, http or https", scheme)
	}
}

// DialSuiGRPC connects to a Sui gRPC endpoint and verifies it serves the network named by
// chainIdentifier. The endpoint is never quoted in errors.
func DialSuiGRPC(
	ctx context.Context,
	chainID ChainID,
	endpoint string,
	chainIdentifier string,
) (*SuiGRPCClient, error) {
	target, transportCredentials, err := suiGRPCTarget(endpoint)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(
		target,
		grpc.WithTransportCredentials(transportCredentials),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(suiGRPCMaxMessageBytes)),
		grpc.WithUserAgent("sentio-portfolio"),
	)
	if err != nil {
		return nil, redactedError{message: "Sui gRPC endpoint is not a target grpc can dial", cause: err}
	}
	client := &SuiGRPCClient{
		chainID: chainID,
		conn:    conn,
		state:   rpcv2.NewStateServiceClient(conn),
		ledger:  rpcv2.NewLedgerServiceClient(conn),
	}
	info, err := client.serviceInfo(ctx)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("read Sui chain identifier: %w", err)
	}
	if err := checkSuiChainIdentifier(info.GetChainId(), chainIdentifier); err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

func (c *SuiGRPCClient) Close() {
	_ = c.conn.Close()
}

// suiGRPCTransportCodes are the statuses whose message describes the connection rather than the
// request. grpc spells the dial target out in them, so only the code is kept.
var suiGRPCTransportCodes = map[codes.Code]struct{}{
	codes.Unavailable:      {},
	codes.DeadlineExceeded: {},
	codes.Canceled:         {},
	codes.Unknown:          {},
	codes.Internal:         {},
	codes.DataLoss:         {},
}

// describeSuiGRPCError names a failed call without quoting where it went. A status the server
// issued about the request (NotFound, InvalidArgument, ...) keeps its message, which describes
// the request; a transport-level status keeps only its code. The original error stays reachable
// through errors.As and status.FromError, so callers still branch on the code.
func describeSuiGRPCError(method string, err error) error {
	if err == nil {
		return nil
	}
	responseStatus, ok := status.FromError(err)
	if !ok {
		return redactedError{message: "Sui gRPC " + method + " failed", cause: err}
	}
	if _, transport := suiGRPCTransportCodes[responseStatus.Code()]; transport {
		return redactedError{
			message: fmt.Sprintf("Sui gRPC %s: %s", method, responseStatus.Code()),
			cause:   err,
		}
	}
	message := publicURLPattern.ReplaceAllString(responseStatus.Message(), "[redacted URL]")
	return redactedError{
		message: fmt.Sprintf("Sui gRPC %s: %s: %s", method, responseStatus.Code(), message),
		cause:   err,
	}
}

// retryableSuiGRPCError retries the statuses that describe a transient condition of the
// connection or the server. A verdict on the request itself (NotFound, InvalidArgument,
// Unauthenticated, Unimplemented, ...) is final.
func retryableSuiGRPCError(err error) bool {
	switch status.Code(err) {
	case codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.DeadlineExceeded,
		codes.Unknown, codes.Internal:
		return true
	default:
		return false
	}
}

// invoke runs one unary call under a per-attempt deadline, retrying transient statuses with
// backoff. Every attempt reports an RPCObservation under the gRPC method name.
func (c *SuiGRPCClient) invoke(ctx context.Context, method string, call func(ctx context.Context) error) error {
	var last error
	for attempt := 0; attempt < suiGRPCAttempts; attempt++ {
		callCtx, cancel := context.WithTimeout(ctx, suiGRPCCallTimeout)
		startedAt := time.Now()
		last = describeSuiGRPCError(method, call(callCtx))
		cancel()
		observeSuiRequest(ctx, c.chainID, method, attempt, startedAt, last)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil || !retryableSuiGRPCError(last) {
			return last
		}
		if attempt+1 < suiGRPCAttempts {
			if err := waitSuiRetry(ctx, suiGRPCRetryInitial<<attempt); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("Sui gRPC failed after %d attempts: %w", suiGRPCAttempts, last)
}

func (c *SuiGRPCClient) serviceInfo(ctx context.Context) (*rpcv2.GetServiceInfoResponse, error) {
	var info *rpcv2.GetServiceInfoResponse
	err := c.invoke(ctx, "GetServiceInfo", func(ctx context.Context) error {
		var callErr error
		info, callErr = c.ledger.GetServiceInfo(ctx, &rpcv2.GetServiceInfoRequest{})
		return callErr
	})
	if err != nil {
		return nil, err
	}
	if info == nil || info.ChainId == nil {
		return nil, errors.New("Sui gRPC GetServiceInfo did not name the chain")
	}
	return info, nil
}

// latestSequence is the head the endpoint advertises. It is one cheap call, which is what
// bracketing a head read needs; LatestCheckpoint adds the digest and timestamp with a second.
func (c *SuiGRPCClient) latestSequence(ctx context.Context) (uint64, error) {
	info, err := c.serviceInfo(ctx)
	if err != nil {
		return 0, err
	}
	if info.CheckpointHeight == nil {
		return 0, errors.New("Sui gRPC GetServiceInfo did not report a checkpoint height")
	}
	return info.GetCheckpointHeight(), nil
}

func (c *SuiGRPCClient) getCheckpoint(ctx context.Context, request *rpcv2.GetCheckpointRequest) (SuiCheckpoint, error) {
	request.ReadMask = &fieldmaskpb.FieldMask{Paths: suiCheckpointReadMask}
	var response *rpcv2.GetCheckpointResponse
	err := c.invoke(ctx, "GetCheckpoint", func(ctx context.Context) error {
		var callErr error
		response, callErr = c.ledger.GetCheckpoint(ctx, request)
		return callErr
	})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return SuiCheckpoint{}, fmt.Errorf("%w: %v", errSuiCheckpointUnavailable, err)
		}
		return SuiCheckpoint{}, err
	}
	checkpoint := response.GetCheckpoint()
	if checkpoint == nil {
		return SuiCheckpoint{}, errSuiCheckpointUnavailable
	}
	if checkpoint.SequenceNumber == nil {
		return SuiCheckpoint{}, errors.New("Sui gRPC returned a checkpoint without a sequence number")
	}
	timestamp := checkpoint.GetSummary().GetTimestamp()
	if timestamp == nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d has no timestamp", checkpoint.GetSequenceNumber())
	}
	return newSuiCheckpoint(checkpoint.GetSequenceNumber(), checkpoint.GetDigest(), timestamp.AsTime())
}

// LatestCheckpoint reads the head: GetCheckpoint with no selector answers with the newest
// checkpoint the endpoint has.
func (c *SuiGRPCClient) LatestCheckpoint(ctx context.Context) (SuiCheckpoint, error) {
	checkpoint, err := c.getCheckpoint(ctx, &rpcv2.GetCheckpointRequest{})
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("latest checkpoint: %w", err)
	}
	return checkpoint, nil
}

// CheckpointBySequence resolves a checkpoint. The service answers an unknown sequence with
// NotFound, which is errSuiCheckpointUnavailable and never retried.
func (c *SuiGRPCClient) CheckpointBySequence(ctx context.Context, sequence uint64) (SuiCheckpoint, error) {
	checkpoint, err := c.getCheckpoint(ctx, &rpcv2.GetCheckpointRequest{
		CheckpointId: &rpcv2.GetCheckpointRequest_SequenceNumber{SequenceNumber: sequence},
	})
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d: %w", sequence, err)
	}
	if checkpoint.Sequence != sequence {
		return SuiCheckpoint{}, fmt.Errorf(
			"Sui gRPC returned checkpoint %d for sequence %d", checkpoint.Sequence, sequence,
		)
	}
	return checkpoint, nil
}

// Holdings reads owner's balances from the head when pin is nil and reports head
// observations before and after the read. These need not be ordered across backends.
// A non-nil pin is answered with no balances and HistoryUnsupported, without a round trip:
// the service cannot read the past, and reading the head under the pin's name would label
// one state with another's name.
func (c *SuiGRPCClient) Holdings(
	ctx context.Context,
	owner SuiAddress,
	pin *SuiCheckpoint,
) (SuiHoldings, error) {
	if pin != nil {
		return SuiHoldings{
			Checkpoint:         *pin,
			HeadBeforeRead:     pin.Sequence,
			HistoryUnsupported: true,
		}, nil
	}
	before, err := c.latestSequence(ctx)
	if err != nil {
		return SuiHoldings{}, err
	}
	rows, err := c.listBalances(ctx, owner)
	if err != nil {
		return SuiHoldings{}, err
	}
	balances, err := suiBalancesFrom(rows)
	if err != nil {
		return SuiHoldings{}, err
	}
	after, err := c.LatestCheckpoint(ctx)
	if err != nil {
		return SuiHoldings{}, err
	}
	return SuiHoldings{Balances: balances, Checkpoint: after, HeadBeforeRead: before}, nil
}

// listBalances pages through ListBalances to completion. Sui sums each coin type over the
// address's coin objects and its address balance into Balance.balance, so one row per coin type
// is the whole holding.
func (c *SuiGRPCClient) listBalances(ctx context.Context, owner SuiAddress) ([]suiBalanceRow, error) {
	rows := make([]suiBalanceRow, 0, 16)
	var token []byte
	for page := 0; page < suiGRPCMaxBalancePages; page++ {
		request := &rpcv2.ListBalancesRequest{
			Owner:     proto.String(owner.Hex()),
			PageSize:  proto.Uint32(suiGRPCBalancePageSize),
			PageToken: token,
		}
		var response *rpcv2.ListBalancesResponse
		err := c.invoke(ctx, "ListBalances", func(ctx context.Context) error {
			var callErr error
			response, callErr = c.state.ListBalances(ctx, request)
			return callErr
		})
		if err != nil {
			return nil, err
		}
		for _, balance := range response.GetBalances() {
			if balance.CoinType == nil {
				return nil, errors.New("Sui gRPC returned a balance without a coin type")
			}
			if balance.Balance == nil {
				return nil, fmt.Errorf("Sui gRPC returned coin type %s without an amount", balance.GetCoinType())
			}
			rows = append(rows, suiBalanceRow{
				coinType: balance.GetCoinType(),
				amount:   new(big.Int).SetUint64(balance.GetBalance()),
			})
		}
		token = response.GetNextPageToken()
		if len(token) == 0 {
			return rows, nil
		}
	}
	return nil, fmt.Errorf("Sui gRPC balances for %s exceed %d pages", owner.Hex(), suiGRPCMaxBalancePages)
}

// coinInfo reads one coin's metadata. A NotFound or InvalidArgument status is the server's
// verdict on the coin and is returned as-is for the caller to record against it; any other
// failure is the transport's.
func (c *SuiGRPCClient) coinInfo(ctx context.Context, coinType string) (*rpcv2.CoinMetadata, error) {
	var response *rpcv2.GetCoinInfoResponse
	err := c.invoke(ctx, "GetCoinInfo", func(ctx context.Context) error {
		var callErr error
		response, callErr = c.state.GetCoinInfo(ctx, &rpcv2.GetCoinInfoRequest{CoinType: proto.String(coinType)})
		return callErr
	})
	if err != nil {
		return nil, err
	}
	return response.GetMetadata(), nil
}

func isSuiCoinVerdict(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.InvalidArgument:
		return true
	default:
		return false
	}
}

// CoinMetadata reads the on-chain metadata of the given coin types, several in flight at once.
// Coin types must already be normalized. A transport failure fails the whole read; a verdict on
// one coin, or metadata the kernel will not use, is named against that coin.
func (c *SuiGRPCClient) CoinMetadata(
	ctx context.Context,
	coinTypes []string,
) (map[string]SuiCoinMetadata, map[string]error, error) {
	metadata := make(map[string]SuiCoinMetadata, len(coinTypes))
	unusable := make(map[string]error)
	if len(coinTypes) == 0 {
		return metadata, unusable, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var mu sync.Mutex
	var firstErr error
	fail := func(err error) {
		mu.Lock()
		defer mu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}
	pending := make(chan string)
	var group sync.WaitGroup
	for worker := 0; worker < min(suiGRPCMetadataConcurrency, len(coinTypes)); worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for coinType := range pending {
				entry, err := c.coinInfo(ctx, coinType)
				mu.Lock()
				switch {
				case err != nil && isSuiCoinVerdict(err):
					unusable[coinType] = fmt.Errorf("coin metadata: %w", err)
				case err != nil:
					mu.Unlock()
					fail(fmt.Errorf("coin metadata %s: %w", coinType, err))
					continue
				case entry == nil:
					unusable[coinType] = errors.New("coin has no on-chain metadata")
				default:
					var decimals *int
					if entry.Decimals != nil {
						value := int(entry.GetDecimals())
						decimals = &value
					}
					validated, validationErr := suiCoinMetadataFrom(coinType, decimals, entry.Symbol, entry.GetName())
					if validationErr != nil {
						unusable[coinType] = validationErr
					} else {
						metadata[coinType] = validated
					}
				}
				mu.Unlock()
			}
		}()
	}
	for _, coinType := range coinTypes {
		select {
		case pending <- coinType:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(pending)
	group.Wait()
	if firstErr != nil {
		return nil, nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	return metadata, unusable, nil
}
