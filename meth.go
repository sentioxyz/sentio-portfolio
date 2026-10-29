package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

const (
	methActivationBlock    = 18_290_599
	methMaxIndexedRequests = 4_096
)

var (
	methTokenAddress   = common.HexToAddress("0xd5F7838F5C461fefF7FE49ea5ebaF7728bB0ADfa")
	methStakingAddress = common.HexToAddress("0xe3cBd06D7dadB3F4e6557bAb7EdD924CD1489E8f")
	methManagerAddress = common.HexToAddress("0x38fDF7b489316e03eD8754ad339cb5c4483FDcf9")
	methETHToken       = token(
		Ethereum,
		"0xC02aaA39b223FE8D0A0e5C4F27eAD9083C756Cc2",
		"ETH",
		18,
	)
)

var methABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stakingContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"unstakeRequestsManagerContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var methStakingABI = MustABI(`[
  {"type":"function","name":"mETH","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"unstakeRequestsManager","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"mETHToETH","stateMutability":"view","inputs":[{"name":"mETHAmount","type":"uint256"}],"outputs":[{"type":"uint256"}]}
]`)

var methManagerABI = MustABI(`[
  {"type":"function","name":"mETH","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"stakingContract","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {
    "type":"function",
    "name":"requestByID",
    "stateMutability":"view",
    "inputs":[{"name":"requestID","type":"uint256"}],
    "outputs":[
      {"name":"blockNumber","type":"uint64"},
      {"name":"requester","type":"address"},
      {"name":"id","type":"uint128"},
      {"name":"mETHLocked","type":"uint128"},
      {"name":"ethRequested","type":"uint128"},
      {"name":"cumulativeETHRequested","type":"uint128"}
    ]
  }
]`)

type MethAdapter struct {
	adapterBase
	index *sentioIndex
}

func newMethAdapter(indexer SentioIndexerConfig) Adapter {
	return &MethAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{
			ID: "meth-protocol", Name: "Mantle mETH", Chains: []ChainID{Ethereum},
		}},
		index: &sentioIndex{
			name:   "meth-protocol",
			api:    newSentioAPIClient(),
			config: indexer,
			chains: []ChainID{Ethereum},
		},
	}
}

type methRequestRow struct {
	Owner     string `json:"owner"`
	RequestID string `json:"requestId"`
}

func methRequestStream(account common.Address) *indexStream[methRequestRow, *big.Int] {
	return &indexStream[methRequestRow, *big.Int]{
		Entity: "withdrawalRequests", Account: account, MaxRows: methMaxIndexedRequests,
		Where: func(chainID ChainID) indexWhere {
			return indexWhere{Prefix: indexAccountPrefix(chainID, account)}
		},
		Decode: func(_ ChainID, id string, row methRequestRow) (*big.Int, error) {
			if !strings.EqualFold(row.Owner, account.Hex()) {
				return nil, fmt.Errorf("GraphQL returned invalid owner row %q", id)
			}
			requestID, valid := new(big.Int).SetString(row.RequestID, 10)
			if !valid || requestID.Sign() < 0 {
				return nil, fmt.Errorf("GraphQL returned invalid request ID %q", row.RequestID)
			}
			return requestID, nil
		},
	}
}

// requestIDs returns the withdrawal requests the index records for account. A request claimed
// after the indexed block is still among them and reads empty at the pin (readMethWithdrawals).
func (a *MethAdapter) requestIDs(
	ctx context.Context,
	block BlockRef,
	account common.Address,
) ([]*big.Int, error) {
	requests := methRequestStream(account)
	if _, err := a.index.read(ctx, nil, block, requests); err != nil {
		return nil, err
	}
	requestIDs := requests.Rows()
	sort.Slice(requestIDs, func(left, right int) bool {
		return requestIDs[left].Cmp(requestIDs[right]) < 0
	})
	return requestIDs, nil
}

func assertMethWiring(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
) error {
	rows, err := client.ParallelCalls(ctx, block, []ContractCall{
		{Contract: methTokenAddress, ABI: methABI, Method: "stakingContract"},
		{Contract: methTokenAddress, ABI: methABI, Method: "unstakeRequestsManagerContract"},
		{Contract: methStakingAddress, ABI: methStakingABI, Method: "mETH"},
		{Contract: methStakingAddress, ABI: methStakingABI, Method: "unstakeRequestsManager"},
		{Contract: methManagerAddress, ABI: methManagerABI, Method: "mETH"},
		{Contract: methManagerAddress, ABI: methManagerABI, Method: "stakingContract"},
	})
	if err != nil {
		return err
	}
	expected := []common.Address{
		methStakingAddress,
		methManagerAddress,
		methTokenAddress,
		methManagerAddress,
		methTokenAddress,
		methStakingAddress,
	}
	for index := range expected {
		actual, decodeErr := AddressAt(rows[index], 0)
		if decodeErr != nil {
			return decodeErr
		}
		if actual != expected[index] {
			return fmt.Errorf("wiring call %d returned %s, expected %s", index, actual, expected[index])
		}
	}
	return nil
}

func readMethWithdrawals(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
	requestIDs []*big.Int,
) (*Group, error) {
	if len(requestIDs) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, len(requestIDs))
	for index, requestID := range requestIDs {
		calls[index] = ContractCall{
			Contract: methManagerAddress,
			ABI:      methManagerABI,
			Method:   "requestByID",
			Args:     []any{requestID},
		}
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, err
	}
	amount := new(big.Int)
	active := make([]string, 0)
	for index, row := range rows {
		requester, decodeErr := AddressAt(row, 1)
		if decodeErr != nil {
			return nil, fmt.Errorf("request %s owner: %w", requestIDs[index], decodeErr)
		}
		if requester == (common.Address{}) {
			continue
		}
		storedID, decodeErr := BigIntAt(row, 2)
		if decodeErr != nil {
			return nil, fmt.Errorf("request %s ID: %w", requestIDs[index], decodeErr)
		}
		if requester != account || storedID.Cmp(requestIDs[index]) != 0 {
			return nil, fmt.Errorf("request %s identity changed", requestIDs[index])
		}
		ethRequested, decodeErr := BigIntAt(row, 4)
		if decodeErr != nil {
			return nil, fmt.Errorf("request %s amount: %w", requestIDs[index], decodeErr)
		}
		amount.Add(amount, ethRequested)
		active = append(active, requestIDs[index].String())
	}
	if amount.Sign() == 0 {
		return nil, nil
	}
	component := NewComponent(
		"asset",
		methETHToken,
		amount,
		Source{
			Contract: methManagerAddress,
			Method:   "indexed owner request IDs + requestByID.ethRequested",
		},
	)
	return &Group{
		ID:         "meth-withdrawal",
		MarketID:   "meth-withdrawal",
		Label:      "Deposit · mETH withdrawal",
		Components: []Component{component},
		Metadata:   map[string]any{"requestIds": strings.Join(active, ",")},
	}, nil
}

func (a *MethAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum || block.Number < methActivationBlock {
		return nil, nil
	}
	if err := assertMethWiring(ctx, client, block); err != nil {
		return nil, fmt.Errorf("deployment wiring: %w", err)
	}
	balanceRows, err := client.ParallelCalls(ctx, block, []ContractCall{{
		Contract: methTokenAddress,
		ABI:      methABI,
		Method:   "balanceOf",
		Args:     []any{account},
	}})
	if err != nil {
		return nil, fmt.Errorf("mETH balance: %w", err)
	}
	shares, err := BigIntAt(balanceRows[0], 0)
	if err != nil {
		return nil, err
	}
	requestIDs, err := a.requestIDs(ctx, block, account)
	if err != nil {
		return nil, fmt.Errorf("withdrawal index: %w", err)
	}
	groups := make([]Group, 0, 2)
	if shares.Sign() > 0 {
		converted, convertErr := client.Call(
			ctx,
			block,
			methStakingAddress,
			methStakingABI,
			"mETHToETH",
			shares,
		)
		if convertErr != nil {
			return nil, fmt.Errorf("convert mETH: %w", convertErr)
		}
		amount, decodeErr := BigIntAt(converted, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		component := NewComponent(
			"asset",
			methETHToken,
			amount,
			Source{
				Contract: methStakingAddress,
				Method:   "mETHToETH(mETH.balanceOf)",
				Holds:    []common.Address{methTokenAddress},
			},
		)
		component.Metadata = map[string]any{"shares": shares.String()}
		groups = append(groups, Group{
			ID:         "meth",
			MarketID:   "meth",
			Label:      "Staked · mETH",
			Components: []Component{component},
		})
	}
	withdrawal, err := readMethWithdrawals(ctx, client, block, account, requestIDs)
	if err != nil {
		return nil, fmt.Errorf("withdrawals: %w", err)
	}
	if withdrawal != nil {
		groups = append(groups, *withdrawal)
	}
	return groups, nil
}
