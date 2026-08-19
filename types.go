package portfolio

import (
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum/common"
)

type ChainID uint64

const (
	Ethereum ChainID = 1
	BSC      ChainID = 56
	Base     ChainID = 8453
	Arbitrum ChainID = 42161
)

var SupportedChainIDs = []ChainID{Ethereum, BSC, Base, Arbitrum}

type BlockRef struct {
	ChainID   ChainID     `json:"chainId"`
	Number    uint64      `json:"number"`
	Hash      common.Hash `json:"hash"`
	Timestamp uint64      `json:"timestamp"`
	Fixed     bool        `json:"-"`
}

type Token struct {
	ChainID  ChainID        `json:"chainId"`
	Address  common.Address `json:"address"`
	Symbol   string         `json:"symbol"`
	Decimals uint8          `json:"decimals"`
}

type Source struct {
	Contract common.Address `json:"contract"`
	Method   string         `json:"method"`
}

type Component struct {
	Kind                 string         `json:"kind"`
	Token                Token          `json:"token"`
	AmountRaw            string         `json:"amountRaw"`
	AmountDenominatorRaw string         `json:"amountDenominatorRaw,omitempty"`
	PriceUSD             *float64       `json:"priceUsd,omitempty"`
	ValueUSD             *float64       `json:"valueUsd,omitempty"`
	Source               Source         `json:"-"`
	Metadata             map[string]any `json:"metadata,omitempty"`
}

func NewComponent(kind string, token Token, amount *big.Int, source Source) Component {
	return Component{
		Kind:      kind,
		Token:     token,
		AmountRaw: amount.String(),
		Source:    source,
	}
}

type Group struct {
	ID             string         `json:"id"`
	MarketID       string         `json:"marketId,omitempty"`
	Label          string         `json:"label"`
	Components     []Component    `json:"components"`
	NetValuePolicy string         `json:"netValuePolicy,omitempty"`
	ValueUSD       *float64       `json:"valueUsd,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type Snapshot struct {
	ProtocolID     string         `json:"protocolId"`
	ProtocolName   string         `json:"protocolName"`
	ChainID        ChainID        `json:"chainId"`
	Account        common.Address `json:"account"`
	Block          BlockRef       `json:"block"`
	Groups         []Group        `json:"groups"`
	NetValuePolicy string         `json:"netValuePolicy,omitempty"`
	ValueUSD       *float64       `json:"valueUsd,omitempty"`
}

type ScanError struct {
	Scope        string  `json:"scope"`
	ChainID      ChainID `json:"chainId,omitempty"`
	ProtocolID   string  `json:"protocolId,omitempty"`
	ProtocolName string  `json:"protocolName,omitempty"`
	Message      string  `json:"message"`
}

type ProtocolInfo struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Chains []ChainID `json:"chains"`
}

type ProtocolSummary struct {
	ProtocolID       string    `json:"protocolId"`
	ProtocolName     string    `json:"protocolName"`
	TotalUSD         float64   `json:"totalUsd"`
	AssetUSD         float64   `json:"assetUsd"`
	DebtUSD          float64   `json:"debtUsd"`
	RewardUSD        float64   `json:"rewardUsd"`
	PricedComponents int       `json:"pricedComponents"`
	ComponentCount   int       `json:"componentCount"`
	ChainIDs         []ChainID `json:"chainIds"`
}

type Response struct {
	SchemaVersion      int                `json:"schemaVersion"`
	Address            common.Address     `json:"address"`
	SupportedProtocols []ProtocolInfo     `json:"supportedProtocols"`
	Snapshots          []Snapshot         `json:"snapshots"`
	ProtocolSummaries  []ProtocolSummary  `json:"protocolSummaries"`
	Errors             []ScanError        `json:"errors"`
	ChainBlocks        map[ChainID]uint64 `json:"chainBlocks"`
	Prices             map[string]float64 `json:"prices"`
	CompletedAt        time.Time          `json:"completedAt"`
}

func ParseAddress(value string) (common.Address, error) {
	if !common.IsHexAddress(value) {
		return common.Address{}, fmt.Errorf("invalid EVM address")
	}
	return common.HexToAddress(value), nil
}

var publicURLPattern = regexp.MustCompile(`(?i)\b(?:https?|wss?)://[^\s"'<>]+`)

// redactedError keeps an error's cause reachable through errors.Is and errors.As while its
// message no longer quotes the endpoint.
type redactedError struct {
	message string
	cause   error
}

func (e redactedError) Error() string { return e.message }

func (e redactedError) Unwrap() error { return e.cause }

// redactEndpoints strips endpoint URLs from an error at the layer that produced it. The
// endpoints this service dials carry credentials, and both go-ethereum and net/http quote the
// URL in transport errors, so a raw error is enough to put one in a log line or a test failure.
// PublicError applies the same pattern at the service boundary; redacting at the source covers
// every error that never reaches it.
func redactEndpoints(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	redacted := publicURLPattern.ReplaceAllString(message, "[redacted URL]")
	if redacted == message {
		return err
	}
	return redactedError{message: redacted, cause: err}
}

func PublicError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.SplitN(err.Error(), "\n", 2)[0]
	message = publicURLPattern.ReplaceAllString(message, "[redacted URL]")
	if len(message) > 500 {
		return message[:500]
	}
	return message
}
