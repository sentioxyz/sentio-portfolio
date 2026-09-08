package portfolio

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Sui differs from the EVM chains in three ways that shape this file. It has no numbered blocks:
// the unit of finality is the checkpoint, named by a sequence number and a digest. It has no
// contract ledgers to poll: the chain itself enumerates what an address holds, per coin type, so
// holdings need no external discovery provider. And its JSON-RPC cannot read the past:
// suix_getAllBalances answers only for the head, so a balance read that way could never be
// attributed to the checkpoint a scan pinned, and Mysten has withdrawn JSON-RPC from their public
// fullnodes altogether. The chain's GraphQL RPC answers "what did this address hold at checkpoint
// N" for a bounded recent window, which is the settled-block contract the rest of the kernel
// keeps, so the Sui reader is a GraphQL client.
//
// The reader is deliberately independent of the engine's chain and token model: it speaks in
// checkpoints, Sui addresses and normalized coin types, leaving the mapping onto BlockRef, Token
// and Group to the wallet adapter that will consume it. That keeps the Sui transport testable on
// its own and lets the same shapes serve Sui testnet or IOTA later.

const (
	suiRequestTimeout   = 15 * time.Second
	suiAttempts         = 3
	suiRetryInitial     = 300 * time.Millisecond
	suiMaxResponseBytes = 8 << 20
	// suiBalancePageSize is the largest page the GraphQL service serves for Address.balances.
	suiBalancePageSize = 50
	// suiMaxBalancePages bounds one address's holdings enumeration. Past it the read fails
	// rather than returning a partial inventory, as the Uniswap NFT enumeration does at its own
	// bound: a truncated list of holdings is a wrong portfolio nobody can see is wrong.
	suiMaxBalancePages = 200
	// suiMetadataBatchSize is how many coin types one metadata request aliases together.
	suiMetadataBatchSize = 25
)

// SuiMainnetChainIdentifier is how Sui names its mainnet: the first four bytes of the genesis
// checkpoint digest, in hex. DialSui compares the endpoint's identifier against it the way
// DialRPC compares eth_chainId, so a misconfigured endpoint fails at dial time rather than
// answering with another network's balances.
const SuiMainnetChainIdentifier = "35834a8a"

// SuiAddress is a 32-byte Sui account or object address. Sui renders addresses as 0x-prefixed
// hex and accepts short forms with leading zeros dropped (0x2 is the framework package), so the
// parser pads and the formatter always emits the 64-digit long form.
type SuiAddress [32]byte

func ParseSuiAddress(value string) (SuiAddress, error) {
	digits, err := moveAddressDigits(strings.TrimSpace(value))
	if err != nil {
		return SuiAddress{}, err
	}
	raw, err := hex.DecodeString(digits[2:])
	if err != nil {
		return SuiAddress{}, fmt.Errorf("invalid Sui address: %w", err)
	}
	var address SuiAddress
	copy(address[:], raw)
	return address, nil
}

// Hex renders the canonical lowercase long form.
func (a SuiAddress) Hex() string {
	return "0x" + hex.EncodeToString(a[:])
}

func (a SuiAddress) String() string {
	return a.Hex()
}

const (
	moveAddressHexDigits = 64
	moveTypeMaxDepth     = 8
	moveTypeMaxLength    = 4096
)

// moveAddressDigits normalizes one 0x-prefixed Move address to its lowercase 64-digit long form.
func moveAddressDigits(value string) (string, error) {
	if len(value) < 3 || value[0] != '0' || (value[1] != 'x' && value[1] != 'X') {
		return "", errors.New("Move address must start with 0x")
	}
	digits := value[2:]
	if len(digits) > moveAddressHexDigits {
		return "", fmt.Errorf("Move address exceeds %d hexadecimal digits", moveAddressHexDigits)
	}
	for _, character := range digits {
		if !isHexDigit(byte(character)) {
			return "", errors.New("Move address contains a non-hexadecimal character")
		}
	}
	return "0x" + strings.Repeat("0", moveAddressHexDigits-len(digits)) + strings.ToLower(digits), nil
}

func isHexDigit(character byte) bool {
	return (character >= '0' && character <= '9') ||
		(character >= 'a' && character <= 'f') ||
		(character >= 'A' && character <= 'F')
}

func isMoveIdentifierStart(character byte) bool {
	return character == '_' ||
		(character >= 'a' && character <= 'z') ||
		(character >= 'A' && character <= 'Z')
}

func isMoveIdentifierPart(character byte) bool {
	return isMoveIdentifierStart(character) || (character >= '0' && character <= '9')
}

var movePrimitiveTypes = map[string]struct{}{
	"bool": {}, "u8": {}, "u16": {}, "u32": {}, "u64": {}, "u128": {}, "u256": {},
	"address": {}, "signer": {},
}

// NormalizeMoveType canonicalizes a Move struct type tag, which is how Sui names a coin:
// `0x2::sui::SUI`, or a generic instance such as `0xabc::pair::LP<0x2::sui::SUI, 0xdef::x::X>`.
//
// Every address collapses to the zero-padded lowercase long form, because 0x2 and its padded
// spelling are one address, while module and struct names stay byte-exact, because Move
// identifiers are case-sensitive. Generic parameters are re-joined with a bare comma. The result
// matches the `repr` the Sui GraphQL service returns and the form the host's price service
// resolves, so one string identifies a coin from the chain through valuation.
func NormalizeMoveType(value string) (string, error) {
	parser := &moveTypeParser{input: strings.TrimSpace(value)}
	if parser.input == "" {
		return "", errors.New("Move type must not be empty")
	}
	normalized, err := parser.parseStructType(0)
	if err != nil {
		return "", err
	}
	parser.skipBlanks()
	if parser.position != len(parser.input) {
		return "", errors.New("Move type has trailing characters")
	}
	if len(normalized) > moveTypeMaxLength {
		return "", fmt.Errorf("normalized Move type exceeds %d characters", moveTypeMaxLength)
	}
	return normalized, nil
}

type moveTypeParser struct {
	input    string
	position int
}

func (p *moveTypeParser) skipBlanks() {
	for p.position < len(p.input) && (p.input[p.position] == ' ' || p.input[p.position] == '\t') {
		p.position++
	}
}

func (p *moveTypeParser) peek() (byte, bool) {
	if p.position >= len(p.input) {
		return 0, false
	}
	return p.input[p.position], true
}

func (p *moveTypeParser) parseAddress() (string, error) {
	start := p.position
	if character, ok := p.peek(); !ok || character != '0' {
		return "", errors.New("Move type address must start with 0x")
	}
	p.position++
	if character, ok := p.peek(); !ok || (character != 'x' && character != 'X') {
		return "", errors.New("Move type address must start with 0x")
	}
	p.position++
	for {
		character, ok := p.peek()
		if !ok || !isHexDigit(character) {
			break
		}
		p.position++
	}
	return moveAddressDigits(p.input[start:p.position])
}

func (p *moveTypeParser) parseSeparatedIdentifier() (string, error) {
	if !strings.HasPrefix(p.input[p.position:], "::") {
		return "", errors.New("Move type must be address::module::name")
	}
	p.position += 2
	start := p.position
	if character, ok := p.peek(); !ok || !isMoveIdentifierStart(character) {
		return "", errors.New("Move identifier must start with a letter or underscore")
	}
	for {
		character, ok := p.peek()
		if !ok || !isMoveIdentifierPart(character) {
			break
		}
		p.position++
	}
	return p.input[start:p.position], nil
}

func (p *moveTypeParser) parseStructType(depth int) (string, error) {
	if depth > moveTypeMaxDepth {
		return "", fmt.Errorf("Move type nests deeper than %d levels", moveTypeMaxDepth)
	}
	address, err := p.parseAddress()
	if err != nil {
		return "", err
	}
	module, err := p.parseSeparatedIdentifier()
	if err != nil {
		return "", err
	}
	name, err := p.parseSeparatedIdentifier()
	if err != nil {
		return "", err
	}
	normalized := address + "::" + module + "::" + name
	if character, ok := p.peek(); !ok || character != '<' {
		return normalized, nil
	}
	p.position++
	parameters := make([]string, 0, 2)
	for {
		parameter, err := p.parseType(depth + 1)
		if err != nil {
			return "", err
		}
		parameters = append(parameters, parameter)
		p.skipBlanks()
		character, ok := p.peek()
		if !ok {
			return "", errors.New("Move type generic parameters are unterminated")
		}
		p.position++
		switch character {
		case ',':
		case '>':
			return normalized + "<" + strings.Join(parameters, ",") + ">", nil
		default:
			return "", errors.New("Move type generic parameters must be comma separated")
		}
	}
}

// parseType accepts what a generic parameter may be: a primitive, a vector of one element type,
// or a nested struct type.
func (p *moveTypeParser) parseType(depth int) (string, error) {
	if depth > moveTypeMaxDepth {
		return "", fmt.Errorf("Move type nests deeper than %d levels", moveTypeMaxDepth)
	}
	p.skipBlanks()
	if character, ok := p.peek(); ok && character == '0' {
		return p.parseStructType(depth)
	}
	start := p.position
	for {
		character, ok := p.peek()
		if !ok || !isMoveIdentifierPart(character) {
			break
		}
		p.position++
	}
	word := p.input[start:p.position]
	if word == "vector" {
		if character, ok := p.peek(); !ok || character != '<' {
			return "", errors.New("Move vector type requires one element type")
		}
		p.position++
		element, err := p.parseType(depth + 1)
		if err != nil {
			return "", err
		}
		p.skipBlanks()
		if character, ok := p.peek(); !ok || character != '>' {
			return "", errors.New("Move vector type requires one element type")
		}
		p.position++
		return "vector<" + element + ">", nil
	}
	if _, primitive := movePrimitiveTypes[word]; primitive {
		return word, nil
	}
	return "", fmt.Errorf("Move type parameter %q is not a type", word)
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

var base58Values = func() [256]int8 {
	var table [256]int8
	for index := range table {
		table[index] = -1
	}
	for index := 0; index < len(base58Alphabet); index++ {
		table[base58Alphabet[index]] = int8(index)
	}
	return table
}()

// decodeBase58 decodes the Bitcoin-alphabet base58 Sui uses for digests and chain identifiers.
// Leading '1' characters are leading zero bytes.
func decodeBase58(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("base58 value is empty")
	}
	leadingZeros := 0
	for leadingZeros < len(value) && value[leadingZeros] == '1' {
		leadingZeros++
	}
	number := new(big.Int)
	radix := big.NewInt(58)
	for index := 0; index < len(value); index++ {
		digit := base58Values[value[index]]
		if digit < 0 {
			return nil, fmt.Errorf("base58 value contains invalid character %q", value[index])
		}
		number.Mul(number, radix)
		number.Add(number, big.NewInt(int64(digit)))
	}
	decoded := number.Bytes()
	result := make([]byte, leadingZeros+len(decoded))
	copy(result[leadingZeros:], decoded)
	return result, nil
}

// SuiCheckpoint is the Sui analogue of a block pin: the sequence number is what reads are scoped
// to, the digest proves which checkpoint that sequence number named, and the timestamp is what
// a fixed-checkpoint scan is valued at.
type SuiCheckpoint struct {
	Sequence  uint64
	Digest    [32]byte
	Timestamp time.Time
}

// SuiCoinBalance is one coin type an address holds, summed over every coin object and address
// balance, in the coin's smallest unit.
type SuiCoinBalance struct {
	CoinType string
	Amount   *big.Int
}

// SuiCoinMetadata is what the coin's on-chain CoinMetadata object declares. The kernel never
// invents either field: a coin without usable metadata is reported as a gap, not labelled.
type SuiCoinMetadata struct {
	CoinType string
	Symbol   string
	Name     string
	Decimals uint8
}

var (
	// errSuiOutsideConsistentRange marks a read scoped to a checkpoint the GraphQL service no
	// longer holds consistent state for. Balances are only answerable inside a recent window, so
	// a fixed-checkpoint scan older than that must fail explicitly rather than read the head.
	errSuiOutsideConsistentRange = errors.New("checkpoint is outside the Sui GraphQL consistent range")
	// errSuiCheckpointUnavailable marks a checkpoint the service does not know, which for a
	// sequence number above the head means the scan asked for the future.
	errSuiCheckpointUnavailable = errors.New("checkpoint is not available")
)

// SuiClient reads one Sui network through its GraphQL RPC.
type SuiClient struct {
	chainID    ChainID
	endpoint   string
	httpClient *http.Client
}

// DialSui connects to a Sui GraphQL endpoint and verifies it serves the network named by
// chainIdentifier (SuiMainnetChainIdentifier for mainnet). chainID is the kernel's identity for
// that network and only labels observations; the endpoint is never quoted in errors.
func DialSui(
	ctx context.Context,
	chainID ChainID,
	endpoint string,
	chainIdentifier string,
) (*SuiClient, error) {
	if strings.TrimSpace(endpoint) == "" {
		return nil, errors.New("Sui GraphQL endpoint is not configured")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 8
	client := &SuiClient{
		chainID:    chainID,
		endpoint:   endpoint,
		httpClient: &http.Client{Timeout: suiRequestTimeout, Transport: transport},
	}
	var payload struct {
		ChainIdentifier string `json:"chainIdentifier"`
	}
	if _, err := client.query(ctx, "SuiChainIdentifier",
		`query SuiChainIdentifier { chainIdentifier }`, nil, &payload); err != nil {
		return nil, fmt.Errorf("read Sui chain identifier: %w", err)
	}
	actual, err := suiChainIdentifierHex(payload.ChainIdentifier)
	if err != nil {
		return nil, fmt.Errorf("read Sui chain identifier: %w", err)
	}
	if actual != strings.ToLower(chainIdentifier) {
		return nil, fmt.Errorf("Sui GraphQL returned chain identifier %s, expected %s", actual, chainIdentifier)
	}
	return client, nil
}

// suiChainIdentifierHex accepts both spellings of a Sui chain identifier: the eight-hex-digit
// form JSON-RPC uses, and the base58 genesis digest GraphQL returns, whose first four bytes are
// that same identifier.
func suiChainIdentifierHex(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", errors.New("chain identifier is empty")
	}
	if len(value) == 8 {
		if _, err := hex.DecodeString(value); err == nil {
			return strings.ToLower(value), nil
		}
	}
	digest, err := decodeBase58(value)
	if err != nil {
		return "", fmt.Errorf("chain identifier %q: %w", value, err)
	}
	if len(digest) < 4 {
		return "", fmt.Errorf("chain identifier %q is too short", value)
	}
	return hex.EncodeToString(digest[:4]), nil
}

func (c *SuiClient) Close() {
	c.httpClient.CloseIdleConnections()
}

type suiGraphQLRequest struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName,omitempty"`
	Variables     map[string]any `json:"variables,omitempty"`
}

type suiGraphQLError struct {
	Message    string `json:"message"`
	Path       []any  `json:"path"`
	Extensions struct {
		Code string `json:"code"`
	} `json:"extensions"`
}

func (e suiGraphQLError) Error() string {
	if len(e.Path) == 0 {
		return e.Message
	}
	parts := make([]string, 0, len(e.Path))
	for _, element := range e.Path {
		parts = append(parts, fmt.Sprint(element))
	}
	return strings.Join(parts, ".") + ": " + e.Message
}

type suiGraphQLResponse struct {
	Data   json.RawMessage   `json:"data"`
	Errors []suiGraphQLError `json:"errors"`
}

func retryableSuiStatus(status int) bool {
	return status == http.StatusTooManyRequests || status >= http.StatusInternalServerError
}

// query runs one GraphQL operation. Transport failures and retryable HTTP statuses are retried;
// a response whose data is null is an error; a response carrying data alongside path-scoped
// errors decodes the data and hands the errors back for the caller to classify, because the
// service answers a partly failed selection that way.
func (c *SuiClient) query(
	ctx context.Context,
	operation string,
	graphql string,
	variables map[string]any,
	result any,
) ([]suiGraphQLError, error) {
	body, err := json.Marshal(suiGraphQLRequest{
		Query: graphql, OperationName: operation, Variables: variables,
	})
	if err != nil {
		return nil, err
	}
	var last error
	for attempt := 0; attempt < suiAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		request.Header.Set("content-type", "application/json")
		request.Header.Set("accept", "application/json")
		startedAt := time.Now()
		graphqlErrors, err := c.doQuery(request, result)
		err = describeSuiTransportError(err)
		c.observe(ctx, operation, attempt, startedAt, err)
		if err == nil {
			return graphqlErrors, nil
		}
		last = err
		var httpErr suiHTTPError
		if errors.As(err, &httpErr) && !retryableSuiStatus(httpErr.status) {
			return nil, redactEndpoints(err)
		}
		var responseErr suiResponseError
		if errors.As(err, &responseErr) {
			return nil, redactEndpoints(err)
		}
		if attempt+1 < suiAttempts {
			c.httpClient.CloseIdleConnections()
			timer := time.NewTimer(suiRetryInitial << attempt)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
	}
	return nil, redactEndpoints(fmt.Errorf("Sui GraphQL failed after %d attempts: %w", suiAttempts, last))
}

// describeSuiTransportError names what went wrong on the wire without quoting where. A url.Error
// spells out the endpoint and a dial error the address it resolved to, and redactEndpoints only
// knows URLs, so the message keeps the operation and the innermost cause and the original error
// stays reachable through errors.Is and errors.As.
func describeSuiTransportError(err error) error {
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		return err
	}
	if urlErr.Timeout() {
		return redactedError{message: "Sui GraphQL " + urlErr.Op + " timed out", cause: err}
	}
	cause := urlErr.Err
	var opErr *net.OpError
	if errors.As(cause, &opErr) && opErr.Err != nil {
		cause = opErr.Err
	}
	return redactedError{message: "Sui GraphQL " + urlErr.Op + " failed: " + cause.Error(), cause: err}
}

// suiHTTPError is a non-200 response. Only 429 and 5xx are retried.
type suiHTTPError struct {
	status int
	body   string
}

func (e suiHTTPError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.status, e.body)
}

// suiResponseError is a well-formed GraphQL response that answered the operation with errors and
// no data. Retrying would only repeat the same rejection.
type suiResponseError struct {
	errors []suiGraphQLError
}

func (e suiResponseError) Error() string {
	messages := make([]string, 0, len(e.errors))
	for _, graphqlErr := range e.errors {
		messages = append(messages, graphqlErr.Error())
	}
	return "Sui GraphQL: " + strings.Join(messages, "; ")
}

func (c *SuiClient) doQuery(request *http.Request, result any) ([]suiGraphQLError, error) {
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, suiMaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(payload) > suiMaxResponseBytes {
		return nil, fmt.Errorf("Sui GraphQL response exceeds %d bytes", suiMaxResponseBytes)
	}
	if response.StatusCode != http.StatusOK {
		return nil, suiHTTPError{
			status: response.StatusCode,
			body:   strings.TrimSpace(string(payload[:min(len(payload), 300)])),
		}
	}
	var envelope suiGraphQLResponse
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil, fmt.Errorf("decode Sui GraphQL response: %w", err)
	}
	if len(envelope.Data) == 0 || bytes.Equal(envelope.Data, []byte("null")) {
		if len(envelope.Errors) == 0 {
			return nil, errors.New("Sui GraphQL response carried neither data nor errors")
		}
		return nil, suiResponseError{errors: envelope.Errors}
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return nil, fmt.Errorf("decode Sui GraphQL data: %w", err)
	}
	return envelope.Errors, nil
}

func (c *SuiClient) observe(ctx context.Context, operation string, attempt int, startedAt time.Time, err error) {
	scope := scopeFrom(ctx)
	scope.observer.ObserveRPC(RPCObservation{
		ChainID:    c.chainID,
		ProtocolID: scope.protocolID,
		Method:     "graphql " + operation,
		Attempt:    attempt + 1,
		Duration:   time.Since(startedAt),
		Err:        redactEndpoints(err),
	})
}

type suiCheckpointPayload struct {
	SequenceNumber uint64 `json:"sequenceNumber"`
	Digest         string `json:"digest"`
	Timestamp      string `json:"timestamp"`
}

func (p suiCheckpointPayload) checkpoint() (SuiCheckpoint, error) {
	digest, err := decodeBase58(p.Digest)
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d digest: %w", p.SequenceNumber, err)
	}
	if len(digest) != len(SuiCheckpoint{}.Digest) {
		return SuiCheckpoint{}, fmt.Errorf(
			"checkpoint %d digest is %d bytes, want %d",
			p.SequenceNumber, len(digest), len(SuiCheckpoint{}.Digest),
		)
	}
	timestamp, err := time.Parse(time.RFC3339Nano, p.Timestamp)
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d timestamp: %w", p.SequenceNumber, err)
	}
	result := SuiCheckpoint{Sequence: p.SequenceNumber, Timestamp: timestamp.UTC()}
	copy(result.Digest[:], digest)
	return result, nil
}

const suiCheckpointSelection = `{ sequenceNumber digest timestamp }`

// LatestCheckpoint is the newest checkpoint the service has indexed, which is the head a live
// scan pins itself to. The service only answers reads at checkpoints it has fully processed, so
// unlike an EVM pool there is no advertised-but-unserved head to lag behind.
func (c *SuiClient) LatestCheckpoint(ctx context.Context) (SuiCheckpoint, error) {
	var payload struct {
		Checkpoint *suiCheckpointPayload `json:"checkpoint"`
	}
	if _, err := c.query(ctx, "SuiLatestCheckpoint",
		`query SuiLatestCheckpoint { checkpoint `+suiCheckpointSelection+` }`, nil, &payload); err != nil {
		return SuiCheckpoint{}, err
	}
	if payload.Checkpoint == nil {
		return SuiCheckpoint{}, errors.New("Sui GraphQL returned no latest checkpoint")
	}
	return payload.Checkpoint.checkpoint()
}

// CheckpointBySequence resolves a fixed checkpoint. A sequence the service does not know is
// errSuiCheckpointUnavailable.
func (c *SuiClient) CheckpointBySequence(ctx context.Context, sequence uint64) (SuiCheckpoint, error) {
	var payload struct {
		Checkpoint *suiCheckpointPayload `json:"checkpoint"`
	}
	if _, err := c.query(ctx, "SuiCheckpoint",
		`query SuiCheckpoint($sequence: UInt53!) { checkpoint(sequenceNumber: $sequence) `+
			suiCheckpointSelection+` }`,
		map[string]any{"sequence": sequence}, &payload); err != nil {
		return SuiCheckpoint{}, err
	}
	if payload.Checkpoint == nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d: %w", sequence, errSuiCheckpointUnavailable)
	}
	checkpoint, err := payload.Checkpoint.checkpoint()
	if err != nil {
		return SuiCheckpoint{}, err
	}
	if checkpoint.Sequence != sequence {
		return SuiCheckpoint{}, fmt.Errorf(
			"Sui GraphQL returned checkpoint %d for sequence %d", checkpoint.Sequence, sequence,
		)
	}
	return checkpoint, nil
}

// BalancesRange reports the checkpoints, inclusive, at which the service can answer
// Address.balances consistently. Fixed-checkpoint scans outside it cannot be served.
func (c *SuiClient) BalancesRange(ctx context.Context) (first uint64, last uint64, err error) {
	var payload struct {
		ServiceConfig struct {
			AvailableRange struct {
				First *struct {
					SequenceNumber uint64 `json:"sequenceNumber"`
				} `json:"first"`
				Last *struct {
					SequenceNumber uint64 `json:"sequenceNumber"`
				} `json:"last"`
			} `json:"availableRange"`
		} `json:"serviceConfig"`
	}
	if _, err := c.query(ctx, "SuiBalancesRange",
		`query SuiBalancesRange { serviceConfig { availableRange(type: "Address", field: "balances") `+
			`{ first { sequenceNumber } last { sequenceNumber } } } }`, nil, &payload); err != nil {
		return 0, 0, err
	}
	available := payload.ServiceConfig.AvailableRange
	if available.First == nil || available.Last == nil {
		return 0, 0, errors.New("Sui GraphQL reported no consistent range for balances")
	}
	if available.Last.SequenceNumber < available.First.SequenceNumber {
		return 0, 0, fmt.Errorf(
			"Sui GraphQL consistent range %d-%d ends before it starts",
			available.First.SequenceNumber, available.Last.SequenceNumber,
		)
	}
	return available.First.SequenceNumber, available.Last.SequenceNumber, nil
}

const suiBalancesQuery = `query SuiBalances($owner: SuiAddress!, $checkpoint: UInt53!, $first: Int!, $after: String) {
  address(address: $owner, atCheckpoint: $checkpoint) {
    balances(first: $first, after: $after) {
      pageInfo { hasNextPage endCursor }
      nodes { coinType { repr } totalBalance }
    }
  }
}`

// Balances enumerates every coin type owner held at the checkpoint, summed across coin objects
// and address balances, sorted by normalized coin type. The read is scoped to the checkpoint's
// sequence number on every page, so a holding never mixes two states of the chain; a checkpoint
// the service no longer holds state for is errSuiOutsideConsistentRange.
func (c *SuiClient) Balances(
	ctx context.Context,
	checkpoint SuiCheckpoint,
	owner SuiAddress,
) ([]SuiCoinBalance, error) {
	balances := make([]SuiCoinBalance, 0)
	seen := make(map[string]struct{})
	var after *string
	for page := 0; ; page++ {
		if page >= suiMaxBalancePages {
			return nil, fmt.Errorf(
				"address holds more than %d coin types", suiMaxBalancePages*suiBalancePageSize,
			)
		}
		variables := map[string]any{
			"owner": owner.Hex(), "checkpoint": checkpoint.Sequence, "first": suiBalancePageSize,
		}
		if after != nil {
			variables["after"] = *after
		}
		var payload struct {
			Address *struct {
				Balances *struct {
					PageInfo struct {
						HasNextPage bool    `json:"hasNextPage"`
						EndCursor   *string `json:"endCursor"`
					} `json:"pageInfo"`
					Nodes []struct {
						CoinType struct {
							Repr string `json:"repr"`
						} `json:"coinType"`
						TotalBalance string `json:"totalBalance"`
					} `json:"nodes"`
				} `json:"balances"`
			} `json:"address"`
		}
		graphqlErrors, err := c.query(ctx, "SuiBalances", suiBalancesQuery, variables, &payload)
		if err != nil {
			return nil, err
		}
		if len(graphqlErrors) > 0 {
			return nil, classifySuiBalanceErrors(checkpoint.Sequence, graphqlErrors)
		}
		if payload.Address == nil || payload.Address.Balances == nil {
			// The service returns an address with no balances as an empty page; a missing
			// selection without an error is a contract it does not make.
			return nil, errors.New("Sui GraphQL returned no balances selection")
		}
		for _, node := range payload.Address.Balances.Nodes {
			coinType, err := NormalizeMoveType(node.CoinType.Repr)
			if err != nil {
				return nil, fmt.Errorf("coin type %q: %w", node.CoinType.Repr, err)
			}
			if _, duplicate := seen[coinType]; duplicate {
				return nil, fmt.Errorf("Sui GraphQL returned coin type %s twice", coinType)
			}
			seen[coinType] = struct{}{}
			amount, err := parseSuiAmount(node.TotalBalance)
			if err != nil {
				return nil, fmt.Errorf("coin type %s balance: %w", coinType, err)
			}
			if amount.Sign() == 0 {
				continue
			}
			balances = append(balances, SuiCoinBalance{CoinType: coinType, Amount: amount})
		}
		pageInfo := payload.Address.Balances.PageInfo
		if !pageInfo.HasNextPage {
			break
		}
		if pageInfo.EndCursor == nil || *pageInfo.EndCursor == "" {
			return nil, errors.New("Sui GraphQL reported another balances page without a cursor")
		}
		if after != nil && *after == *pageInfo.EndCursor {
			return nil, errors.New("Sui GraphQL repeated a balances cursor")
		}
		after = pageInfo.EndCursor
	}
	sort.Slice(balances, func(left, right int) bool {
		return balances[left].CoinType < balances[right].CoinType
	})
	return balances, nil
}

func classifySuiBalanceErrors(sequence uint64, graphqlErrors []suiGraphQLError) error {
	joined := make([]error, 0, len(graphqlErrors))
	for _, graphqlErr := range graphqlErrors {
		if strings.Contains(strings.ToLower(graphqlErr.Message), "outside consistent range") {
			return fmt.Errorf("checkpoint %d: %w", sequence, errSuiOutsideConsistentRange)
		}
		joined = append(joined, graphqlErr)
	}
	return fmt.Errorf("Sui GraphQL balances at checkpoint %d: %w", sequence, errors.Join(joined...))
}

func parseSuiAmount(raw string) (*big.Int, error) {
	if raw == "" {
		return nil, errors.New("empty amount")
	}
	for _, character := range raw {
		if character < '0' || character > '9' {
			return nil, fmt.Errorf("invalid amount %q", raw)
		}
	}
	amount, ok := new(big.Int).SetString(raw, 10)
	if !ok {
		return nil, fmt.Errorf("invalid amount %q", raw)
	}
	return amount, nil
}

// CoinMetadata reads the on-chain metadata of the given coin types, several per request. The
// first map holds every coin whose metadata is usable; the second names each coin whose metadata
// is absent or unusable and why, so the caller can report the gap rather than guess a symbol or a
// precision. Coin types must already be normalized.
func (c *SuiClient) CoinMetadata(
	ctx context.Context,
	coinTypes []string,
) (map[string]SuiCoinMetadata, map[string]error, error) {
	metadata := make(map[string]SuiCoinMetadata, len(coinTypes))
	unusable := make(map[string]error)
	for start := 0; start < len(coinTypes); start += suiMetadataBatchSize {
		end := min(start+suiMetadataBatchSize, len(coinTypes))
		batch := coinTypes[start:end]
		var declarations, selections strings.Builder
		variables := make(map[string]any, len(batch))
		for index, coinType := range batch {
			name := "t" + strconv.Itoa(index)
			if index > 0 {
				declarations.WriteString(", ")
			}
			declarations.WriteString("$" + name + ": String!")
			selections.WriteString(" m" + strconv.Itoa(index) + ": coinMetadata(coinType: $" + name +
				") { decimals symbol name }")
			variables[name] = coinType
		}
		graphql := "query SuiCoinMetadata(" + declarations.String() + ") {" + selections.String() + " }"
		var payload map[string]*struct {
			Decimals *int    `json:"decimals"`
			Symbol   *string `json:"symbol"`
			Name     string  `json:"name"`
		}
		graphqlErrors, err := c.query(ctx, "SuiCoinMetadata", graphql, variables, &payload)
		if err != nil {
			return nil, nil, err
		}
		if len(graphqlErrors) > 0 {
			joined := make([]error, 0, len(graphqlErrors))
			for _, graphqlErr := range graphqlErrors {
				joined = append(joined, graphqlErr)
			}
			return nil, nil, fmt.Errorf("Sui GraphQL coin metadata: %w", errors.Join(joined...))
		}
		for index, coinType := range batch {
			entry, present := payload["m"+strconv.Itoa(index)]
			if !present || entry == nil {
				unusable[coinType] = errors.New("coin has no on-chain metadata")
				continue
			}
			if entry.Decimals == nil {
				unusable[coinType] = errors.New("coin metadata declares no decimals")
				continue
			}
			if *entry.Decimals < 0 || *entry.Decimals > 36 {
				unusable[coinType] = fmt.Errorf("coin metadata decimals %d are invalid", *entry.Decimals)
				continue
			}
			if entry.Symbol == nil {
				unusable[coinType] = errors.New("coin metadata declares no symbol")
				continue
			}
			symbol, err := validatedWalletTokenSymbol(*entry.Symbol)
			if err != nil {
				unusable[coinType] = fmt.Errorf("coin metadata symbol: %w", err)
				continue
			}
			metadata[coinType] = SuiCoinMetadata{
				CoinType: coinType,
				Symbol:   symbol,
				Name:     entry.Name,
				Decimals: uint8(*entry.Decimals),
			}
		}
	}
	return metadata, unusable, nil
}
