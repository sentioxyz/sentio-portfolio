package portfolio

import (
	"bytes"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

func MustABI(definition string) abi.ABI {
	parsed, err := abi.JSON(strings.NewReader(definition))
	if err != nil {
		panic(err)
	}
	return parsed
}

func BigIntAt(values []any, index int) (*big.Int, error) {
	if index < 0 || index >= len(values) {
		return nil, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].(*big.Int)
	if !ok || value == nil {
		return nil, fmt.Errorf("result %d is %T, expected *big.Int", index, values[index])
	}
	return new(big.Int).Set(value), nil
}

func AddressAt(values []any, index int) (common.Address, error) {
	if index < 0 || index >= len(values) {
		return common.Address{}, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("result %d is %T, expected address", index, values[index])
	}
	return value, nil
}

func AddressSliceAt(values []any, index int) ([]common.Address, error) {
	if index < 0 || index >= len(values) {
		return nil, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].([]common.Address)
	if !ok {
		return nil, fmt.Errorf("result %d is %T, expected []address", index, values[index])
	}
	return append([]common.Address(nil), value...), nil
}

func Bytes32At(values []any, index int) (common.Hash, error) {
	if index < 0 || index >= len(values) {
		return common.Hash{}, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].([32]byte)
	if !ok {
		return common.Hash{}, fmt.Errorf("result %d is %T, expected bytes32", index, values[index])
	}
	return common.Hash(value), nil
}

func BoolAt(values []any, index int) (bool, error) {
	if index < 0 || index >= len(values) {
		return false, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].(bool)
	if !ok {
		return false, fmt.Errorf("result %d is %T, expected bool", index, values[index])
	}
	return value, nil
}

func StringAt(values []any, index int) (string, error) {
	if index < 0 || index >= len(values) {
		return "", fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].(string)
	if !ok {
		return "", fmt.Errorf("result %d is %T, expected string", index, values[index])
	}
	return value, nil
}

func Uint8At(values []any, index int) (uint8, error) {
	if index < 0 || index >= len(values) {
		return 0, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].(uint8)
	if !ok {
		return 0, fmt.Errorf("result %d is %T, expected uint8", index, values[index])
	}
	return value, nil
}

func Uint64At(values []any, index int) (uint64, error) {
	if index < 0 || index >= len(values) {
		return 0, fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].(uint64)
	if !ok {
		return 0, fmt.Errorf("result %d is %T, expected uint64", index, values[index])
	}
	return value, nil
}

var erc20ABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"symbol","stateMutability":"view","inputs":[],"outputs":[{"type":"string"}]},
  {"type":"function","name":"decimals","stateMutability":"view","inputs":[],"outputs":[{"type":"uint8"}]}
]`)

var erc20Bytes32SymbolABI = MustABI(`[
  {"type":"function","name":"symbol","stateMutability":"view","inputs":[],"outputs":[{"type":"bytes32"}]}
]`)

func Bytes32StringAt(values []any, index int) (string, error) {
	if index < 0 || index >= len(values) {
		return "", fmt.Errorf("missing result at index %d", index)
	}
	value, ok := values[index].([32]byte)
	if !ok {
		return "", fmt.Errorf("result %d is %T, expected bytes32", index, values[index])
	}
	return string(bytes.TrimRight(value[:], "\x00")), nil
}

var erc4626ABI = MustABI(`[
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"convertToAssets","stateMutability":"view","inputs":[{"name":"shares","type":"uint256"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {
    "type":"function",
    "name":"cooldowns",
    "stateMutability":"view",
    "inputs":[{"name":"account","type":"address"}],
    "outputs":[
      {"name":"cooldownEnd","type":"uint104"},
      {"name":"underlyingAmount","type":"uint256"}
    ]
  }
]`)
