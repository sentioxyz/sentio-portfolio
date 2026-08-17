package portfolio

import (
	"context"

	"github.com/ethereum/go-ethereum/common"
)

type Adapter interface {
	Info() ProtocolInfo
	Positions(
		ctx context.Context,
		client *RPCClient,
		block BlockRef,
		account common.Address,
	) ([]Group, error)
}

type adapterBase struct {
	info ProtocolInfo
}

// deploymentWindow uses inclusive block bounds. Hard-coded production
// deployments use the first canonical block with contract code as activation.
type deploymentWindow struct {
	ActivationBlock   uint64
	DeactivationBlock uint64
}

func (d deploymentWindow) ActiveAt(block uint64) bool {
	if block < d.ActivationBlock {
		return false
	}
	return d.DeactivationBlock == 0 || block <= d.DeactivationBlock
}

func (a adapterBase) Info() ProtocolInfo {
	return a.info
}

func supportsChain(chains []ChainID, chainID ChainID) bool {
	for _, candidate := range chains {
		if candidate == chainID {
			return true
		}
	}
	return false
}
