package portfolio

import (
	"fmt"
	"math"
	"math/big"
)

func componentValueUSD(component Component, price float64) (float64, error) {
	if price <= 0 || math.IsInf(price, 0) || math.IsNaN(price) {
		return 0, fmt.Errorf("invalid price %v", price)
	}
	amount, ok := new(big.Int).SetString(component.AmountRaw, 10)
	if !ok {
		return 0, fmt.Errorf("invalid amount %q", component.AmountRaw)
	}
	denominator := big.NewInt(1)
	if component.AmountDenominatorRaw != "" {
		denominator, ok = new(big.Int).SetString(component.AmountDenominatorRaw, 10)
		if !ok || denominator.Sign() <= 0 {
			return 0, fmt.Errorf("invalid denominator %q", component.AmountDenominatorRaw)
		}
	}
	scale := new(big.Int).Exp(
		big.NewInt(10),
		big.NewInt(int64(component.Token.Decimals)),
		nil,
	)
	quantity := new(big.Rat).SetInt(amount)
	quantity.Quo(quantity, new(big.Rat).SetInt(denominator))
	quantity.Quo(quantity, new(big.Rat).SetInt(scale))
	priceRat := new(big.Rat).SetFloat64(price)
	if priceRat == nil {
		return 0, fmt.Errorf("price %v cannot be represented", price)
	}
	value, _ := new(big.Rat).Mul(quantity, priceRat).Float64()
	if math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, fmt.Errorf("calculated value is not finite")
	}
	return value, nil
}

func applyFloorZero(value float64, policy string) float64 {
	if policy == "floor-zero" && value < 0 {
		return 0
	}
	return value
}

func applyValuations(
	snapshots []Snapshot,
	prices map[string]float64,
	protocols []ProtocolInfo,
) ([]ProtocolSummary, error) {
	type summaryState struct {
		summary ProtocolSummary
		chains  map[ChainID]struct{}
	}
	states := make(map[string]*summaryState)
	for snapshotIndex := range snapshots {
		snapshot := &snapshots[snapshotIndex]
		state := states[snapshot.ProtocolID]
		if state == nil {
			state = &summaryState{
				summary: ProtocolSummary{
					ProtocolID:   snapshot.ProtocolID,
					ProtocolName: snapshot.ProtocolName,
				},
				chains: make(map[ChainID]struct{}),
			}
			states[snapshot.ProtocolID] = state
		}
		state.chains[snapshot.ChainID] = struct{}{}

		snapshotTotal := 0.0
		snapshotPriced := false
		for groupIndex := range snapshot.Groups {
			group := &snapshot.Groups[groupIndex]
			groupTotal := 0.0
			groupPriced := false
			for componentIndex := range group.Components {
				component := &group.Components[componentIndex]
				state.summary.ComponentCount++
				price, exists := prices[PriceKey(component.Token)]
				if !exists {
					continue
				}
				value, err := componentValueUSD(*component, price)
				if err != nil {
					return nil, fmt.Errorf(
						"%s %s: %w",
						snapshot.ProtocolID,
						component.Token.Symbol,
						err,
					)
				}
				priceCopy := price
				valueCopy := value
				component.PriceUSD = &priceCopy
				component.ValueUSD = &valueCopy
				state.summary.PricedComponents++
				groupPriced = true
				snapshotPriced = true
				switch component.Kind {
				case "debt":
					state.summary.DebtUSD += value
					groupTotal -= value
				case "reward":
					state.summary.RewardUSD += value
					groupTotal += value
				default:
					state.summary.AssetUSD += value
					groupTotal += value
				}
			}
			if groupPriced {
				groupTotal = applyFloorZero(groupTotal, group.NetValuePolicy)
				groupValue := groupTotal
				group.ValueUSD = &groupValue
				snapshotTotal += groupTotal
			}
		}
		if snapshotPriced {
			snapshotTotal = applyFloorZero(snapshotTotal, snapshot.NetValuePolicy)
			snapshotValue := snapshotTotal
			snapshot.ValueUSD = &snapshotValue
			state.summary.TotalUSD += snapshotTotal
		}
	}

	summaries := make([]ProtocolSummary, 0, len(states))
	for _, protocol := range protocols {
		state := states[protocol.ID]
		if state == nil {
			continue
		}
		for _, chainID := range SupportedChainIDs {
			if _, exists := state.chains[chainID]; exists {
				state.summary.ChainIDs = append(state.summary.ChainIDs, chainID)
			}
		}
		summaries = append(summaries, state.summary)
	}
	return summaries, nil
}
