package portfolio

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"
)

// Sui differs from the EVM chains in three ways that shape the Sui reader. It has no numbered
// blocks: the unit of finality is the checkpoint, named by a sequence number and a digest. It has
// no contract ledgers to poll: the chain itself enumerates what an address holds, per coin type,
// so holdings need no external discovery provider. And its transports differ in what they can
// pin: the GraphQL RPC reads holdings at a named checkpoint inside a recent consistent range,
// while JSON-RPC and gRPC read only the head. SuiReader is the interface both transports
// implement (sui_graphql.go, sui_jsonrpc.go); this file holds what they share.
//
// The reader is deliberately independent of the engine's chain and token model: it speaks in
// checkpoints, Sui addresses and normalized coin types, leaving the mapping onto BlockRef, Token
// and Group to the wallet adapter that will consume it. That keeps the Sui transports testable
// on their own and lets the same shapes serve Sui testnet or IOTA later.

// SuiMainnetChainIdentifier is how Sui names its mainnet: the first four bytes of the genesis
// checkpoint digest, in hex. The dialers compare the endpoint's identifier against it the way
// DialRPC compares eth_chainId, so a misconfigured endpoint fails at dial time rather than
// answering with another network's balances.
const SuiMainnetChainIdentifier = "35834a8a"

// SuiReader reads one Sui network. Both transports verify the network at dial time, normalize
// every coin type they return, and never invent coin metadata.
type SuiReader interface {
	// LatestCheckpoint is the newest checkpoint the endpoint serves.
	LatestCheckpoint(ctx context.Context) (SuiCheckpoint, error)
	// CheckpointBySequence resolves a checkpoint by sequence number; an unknown one is
	// errSuiCheckpointUnavailable.
	CheckpointBySequence(ctx context.Context, sequence uint64) (SuiCheckpoint, error)
	// Holdings enumerates every coin type owner holds. A nil pin reads the head; a non-nil pin
	// must be honoured exactly, and a transport that cannot read at a fixed checkpoint returns
	// errSuiPinnedReadUnsupported rather than reading the head instead.
	Holdings(ctx context.Context, owner SuiAddress, pin *SuiCheckpoint) (SuiHoldings, error)
	// CoinMetadata reads the on-chain metadata of normalized coin types. The first map holds
	// every coin whose metadata is usable; the second names each coin whose metadata is absent
	// or unusable and why, so the caller reports a gap rather than guessing a symbol or a
	// precision.
	CoinMetadata(ctx context.Context, coinTypes []string) (map[string]SuiCoinMetadata, map[string]error, error)
	Close()
}

// SuiHoldings is what an address holds and which chain state that claim is about.
//
// A GraphQL read is Exact: every balance is the state at Checkpoint. A JSON-RPC read is not: the
// balances were read from the head somewhere between two checkpoints the reader observed, and
// Checkpoint is the later of them, so the balances belong to a state no later than Checkpoint
// and no earlier than HeadBeforeRead. A consumer must surface that distinction rather than label
// a head read as the state at a checkpoint.
type SuiHoldings struct {
	Balances   []SuiCoinBalance
	Checkpoint SuiCheckpoint
	Exact      bool
	// HeadBeforeRead is the head sequence observed before a head read began. It equals
	// Checkpoint.Sequence when Exact.
	HeadBeforeRead uint64
}

var (
	// errSuiOutsideConsistentRange marks a read scoped to a checkpoint the GraphQL service no
	// longer holds consistent state for. Balances are only answerable inside a recent window, so
	// a fixed-checkpoint scan older than that must fail explicitly rather than read the head.
	errSuiOutsideConsistentRange = errors.New("checkpoint is outside the Sui GraphQL consistent range")
	// errSuiCheckpointUnavailable marks a checkpoint the endpoint does not know, which for a
	// sequence number above the head means the scan asked for the future.
	errSuiCheckpointUnavailable = errors.New("checkpoint is not available")
	// errSuiPinnedReadUnsupported marks a transport that reads only the head being asked for a
	// fixed checkpoint. Answering from the head would label one state with another's name.
	errSuiPinnedReadUnsupported = errors.New("this Sui transport cannot read holdings at a fixed checkpoint")
)

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
// resolves, so one string identifies a coin from the chain through valuation whichever transport
// read it: JSON-RPC returns short addresses and spaced generics, and both normalize to the same.
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

func checkSuiChainIdentifier(actual string, expected string) error {
	identifier, err := suiChainIdentifierHex(actual)
	if err != nil {
		return fmt.Errorf("read Sui chain identifier: %w", err)
	}
	if identifier != strings.ToLower(strings.TrimSpace(expected)) {
		return fmt.Errorf("Sui endpoint returned chain identifier %s, expected %s", identifier, expected)
	}
	return nil
}

// SuiCheckpoint is the Sui analogue of a block pin: the sequence number is what reads are scoped
// to, the digest proves which checkpoint that sequence number named, and the timestamp is what
// a fixed-checkpoint scan is valued at.
type SuiCheckpoint struct {
	Sequence  uint64
	Digest    [32]byte
	Timestamp time.Time
}

// newSuiCheckpoint assembles a checkpoint from the wire spellings both transports share: a
// base58 digest and a timestamp.
func newSuiCheckpoint(sequence uint64, digest string, timestamp time.Time) (SuiCheckpoint, error) {
	decoded, err := decodeBase58(digest)
	if err != nil {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d digest: %w", sequence, err)
	}
	if len(decoded) != len(SuiCheckpoint{}.Digest) {
		return SuiCheckpoint{}, fmt.Errorf(
			"checkpoint %d digest is %d bytes, want %d",
			sequence, len(decoded), len(SuiCheckpoint{}.Digest),
		)
	}
	if timestamp.IsZero() {
		return SuiCheckpoint{}, fmt.Errorf("checkpoint %d has no timestamp", sequence)
	}
	result := SuiCheckpoint{Sequence: sequence, Timestamp: timestamp.UTC()}
	copy(result.Digest[:], decoded)
	return result, nil
}

// SuiCoinBalance is one coin type an address holds, summed over every coin object and address
// balance, in the coin's smallest unit.
type SuiCoinBalance struct {
	CoinType string
	Amount   *big.Int
}

func sortSuiBalances(balances []SuiCoinBalance) {
	sort.Slice(balances, func(left, right int) bool {
		return balances[left].CoinType < balances[right].CoinType
	})
}

// SuiCoinMetadata is what the coin's on-chain CoinMetadata object declares. The kernel never
// invents either field: a coin without usable metadata is reported as a gap, not labelled.
type SuiCoinMetadata struct {
	CoinType string
	Symbol   string
	Name     string
	Decimals uint8
}

// suiCoinMetadataFrom validates one coin's metadata as either transport returns it. A nil
// decimals or symbol is a declaration the chain did not make, and an out-of-range precision or an
// unusable symbol is one the kernel will not use.
func suiCoinMetadataFrom(coinType string, decimals *int, symbol *string, name string) (SuiCoinMetadata, error) {
	if decimals == nil {
		return SuiCoinMetadata{}, errors.New("coin metadata declares no decimals")
	}
	if *decimals < 0 || *decimals > 36 {
		return SuiCoinMetadata{}, fmt.Errorf("coin metadata decimals %d are invalid", *decimals)
	}
	if symbol == nil {
		return SuiCoinMetadata{}, errors.New("coin metadata declares no symbol")
	}
	validated, err := validatedWalletTokenSymbol(*symbol)
	if err != nil {
		return SuiCoinMetadata{}, fmt.Errorf("coin metadata symbol: %w", err)
	}
	return SuiCoinMetadata{
		CoinType: coinType,
		Symbol:   validated,
		Name:     name,
		Decimals: uint8(*decimals),
	}, nil
}

// suiBalanceRow is one balance as either transport spells it before normalization.
type suiBalanceRow struct {
	coinType string
	amount   string
}

// suiBalancesFrom normalizes one enumeration of an address's balances: every coin type
// canonical, no coin type twice, no malformed amount, zero rows dropped, sorted by coin type.
func suiBalancesFrom(rows []suiBalanceRow) ([]SuiCoinBalance, error) {
	balances := make([]SuiCoinBalance, 0, len(rows))
	seen := make(map[string]struct{}, len(rows))
	for _, row := range rows {
		coinType, err := NormalizeMoveType(row.coinType)
		if err != nil {
			return nil, fmt.Errorf("coin type %q: %w", row.coinType, err)
		}
		if _, duplicate := seen[coinType]; duplicate {
			return nil, fmt.Errorf("Sui endpoint returned coin type %s twice", coinType)
		}
		seen[coinType] = struct{}{}
		amount, err := parseSuiAmount(row.amount)
		if err != nil {
			return nil, fmt.Errorf("coin type %s balance: %w", coinType, err)
		}
		if amount.Sign() == 0 {
			continue
		}
		balances = append(balances, SuiCoinBalance{CoinType: coinType, Amount: amount})
	}
	sortSuiBalances(balances)
	return balances, nil
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
