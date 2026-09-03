package portfolio

import (
	"context"
	"fmt"

	"github.com/ethereum/go-ethereum/common"
)

type Adapter interface {
	Info() ProtocolInfo
	// Positions may return verified groups together with an error when an independent
	// protocol surface fails. The engine preserves those groups and reports the error.
	Positions(
		ctx context.Context,
		client *RPCClient,
		block BlockRef,
		account common.Address,
	) ([]Group, error)
}

// chainAvailability describes every interval in which one adapter can safely run on a
// chain. Multiple windows support migrations with intentional gaps. This outer envelope
// does not replace deployment windows for markets, factories, or other nested contracts.
type chainAvailability map[ChainID][]availabilityWindow

type registeredAdapter struct {
	Adapter
	availability chainAvailability
}

func (r registeredAdapter) ActiveAt(chainID ChainID, block uint64) bool {
	for _, window := range r.availability[chainID] {
		if window.ActiveAt(block) {
			return true
		}
	}
	return false
}

func registerAdapters(
	adapters []Adapter,
	availabilityByProtocol map[string]chainAvailability,
) (map[string]registeredAdapter, error) {
	registrations := make(map[string]registeredAdapter, len(adapters))
	for _, adapter := range adapters {
		info := adapter.Info()
		protocolID := info.ID
		if _, exists := registrations[protocolID]; exists {
			return nil, fmt.Errorf("duplicate adapter protocol ID %q", protocolID)
		}
		availability := availabilityByProtocol[protocolID]
		advertisedChains := make(map[ChainID]struct{}, len(info.Chains))
		for _, chainID := range info.Chains {
			if _, duplicate := advertisedChains[chainID]; duplicate {
				return nil, fmt.Errorf(
					"protocol %q advertises chain %d more than once",
					protocolID,
					chainID,
				)
			}
			advertisedChains[chainID] = struct{}{}
			windows := availability[chainID]
			if len(windows) == 0 {
				return nil, fmt.Errorf(
					"protocol %q has no availability for advertised chain %d",
					protocolID,
					chainID,
				)
			}
			for index, window := range windows {
				if !window.configured {
					return nil, fmt.Errorf(
						"protocol %q chain %d availability window %d is not configured",
						protocolID,
						chainID,
						index,
					)
				}
				bounds := window.deploymentWindow
				if bounds.ActivationBlock == 0 && bounds.DeactivationBlock != 0 {
					return nil, fmt.Errorf(
						"protocol %q chain %d availability window %d has an implicit genesis start",
						protocolID,
						chainID,
						index,
					)
				}
				if bounds.DeactivationBlock != 0 &&
					bounds.DeactivationBlock < bounds.ActivationBlock {
					return nil, fmt.Errorf(
						"protocol %q chain %d availability window %d ends before it starts",
						protocolID,
						chainID,
						index,
					)
				}
				if index == 0 {
					continue
				}
				previous := windows[index-1].deploymentWindow
				if bounds.ActivationBlock <= previous.ActivationBlock {
					return nil, fmt.Errorf(
						"protocol %q chain %d availability windows are not ordered",
						protocolID,
						chainID,
					)
				}
				if previous.DeactivationBlock == 0 ||
					bounds.ActivationBlock <= previous.DeactivationBlock {
					return nil, fmt.Errorf(
						"protocol %q chain %d availability windows overlap",
						protocolID,
						chainID,
					)
				}
			}
		}
		for chainID := range availability {
			if _, advertised := advertisedChains[chainID]; !advertised {
				return nil, fmt.Errorf(
					"protocol %q has availability for unadvertised chain %d",
					protocolID,
					chainID,
				)
			}
		}
		registrations[protocolID] = registeredAdapter{
			Adapter:      adapter,
			availability: availability,
		}
	}
	for protocolID := range availabilityByProtocol {
		if _, exists := registrations[protocolID]; !exists {
			return nil, fmt.Errorf("availability configured for unknown protocol %q", protocolID)
		}
	}
	return registrations, nil
}

func mustRegisterAdapters(
	adapters []Adapter,
	availabilityByProtocol map[string]chainAvailability,
) map[string]registeredAdapter {
	registrations, err := registerAdapters(adapters, availabilityByProtocol)
	if err != nil {
		panic(fmt.Sprintf("register portfolio adapters: %v", err))
	}
	return registrations
}

type adapterBase struct {
	info ProtocolInfo
}

// deploymentWindow uses inclusive block bounds. Hard-coded deployments activate
// at the first canonical block where the views used by the adapter are usable,
// which is never earlier than the first block with contract code.
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

// availabilityWindow is an explicitly configured inclusive block window. The
// configured bit makes the zero value invalid instead of treating an omitted
// deployment as active from genesis.
type availabilityWindow struct {
	deploymentWindow deploymentWindow
	configured       bool
}

func availableFromGenesis() availabilityWindow {
	return availabilityWindow{configured: true}
}

func availableFrom(block uint64) availabilityWindow {
	if block == 0 {
		panic("availability from block zero is ambiguous; use availableFromGenesis")
	}
	return availabilityWindow{
		deploymentWindow: deploymentWindow{ActivationBlock: block},
		configured:       true,
	}
}

func availableBetween(firstBlock uint64, lastBlock uint64) availabilityWindow {
	if firstBlock == 0 {
		panic("availability from block zero is ambiguous; use availableFromGenesis")
	}
	if lastBlock == 0 {
		panic("availability without an end is ambiguous; use availableFrom")
	}
	if lastBlock < firstBlock {
		panic("availability ends before it starts")
	}
	return availabilityWindow{
		deploymentWindow: deploymentWindow{
			ActivationBlock:   firstBlock,
			DeactivationBlock: lastBlock,
		},
		configured: true,
	}
}

func (w availabilityWindow) ActiveAt(block uint64) bool {
	return w.configured && w.deploymentWindow.ActiveAt(block)
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
