package portfolio

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var skyLockstakeEngineABI = MustABI(`[
  {"type":"function","name":"ownerUrnsCount","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"ownerUrns","stateMutability":"view","inputs":[{"type":"address"},{"type":"uint256"}],"outputs":[{"type":"address"}]},
  {"type":"function","name":"urnFarms","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"address"}]}
]`)

var skyFarmABI = MustABI(`[
  {"type":"function","name":"earned","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"balanceOf","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"stakingToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"rewardsToken","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]}
]`)

var (
	skyUSDS = token(Ethereum, "0xdC035D45d973E3EC169d2276DDab16f1e407384F", "USDS", 18)
	skySKY  = token(Ethereum, "0x56072C95FAA701256059aa122697B133aDEd9279", "SKY", 18)
)

// skySavingsVaults are Sky's two ERC-4626 savings vaults over USDS. Both are proxies
// registered in Sky's on-chain ChainLog (0xdA0Ab1e0017DEbCd72Be8599041a2aa3bA7e740F, keys
// SUSDS and STUSDS), so the static pair is a closed set rather than a snapshot of a moving
// one: an implementation upgrade rewires a proxy without moving it, and no permissionless
// factory lets a third party mint another Sky vault. That is the inverse of the Moolah
// manifest lesson recorded in lista.go — nothing here can be created behind our backs, so the
// list cannot silently rot; only a new Sky product would extend it, and that arrives as a
// governance action rather than as drift.
//
// Activation blocks are the first canonical block with contract code, established by archive
// eth_getCode binary search. The two launched almost a year apart, so a shared anchor would
// have repeated the Aave v4 defect across the 2.5M blocks between them.
var skySavingsVaults = []vaultConfig{
	{
		ID:              "susds",
		Label:           "Sky Savings USDS",
		Address:         common.HexToAddress("0xa3931d71877C0E7a3148CB7Eb4463524FEc27fbD"),
		ActivationBlock: 20_677_434,
		Asset:           skyUSDS,
	},
	{
		ID:              "stusds",
		Label:           "Sky Staked USDS",
		Address:         common.HexToAddress("0x99CD4Ec3f88A45940936F469E4bB72A2A701EEB9"),
		ActivationBlock: 23_219_535,
		Asset:           skyUSDS,
	},
}

// skyStakingFarm is a Sky StakingRewards deployment an owner stakes USDS into directly. The
// staked balance and the pending reward are both per-account views on the farm itself.
type skyStakingFarm struct {
	ID      string
	Label   string
	Address common.Address
	Window  deploymentWindow
}

// skyUSDSFarms lists only the USDS farms DeBank files under its `sky` project, which is not
// the same thing as every REWARDS_USDS_* key in the ChainLog. Which project a farm belongs to
// is an off-chain attribution rather than something the chain states, and it is genuinely
// split across three projects:
//
//   - REWARDS_USDS_SKY  0x0650CAF1… → `sky`, DeBank adapter_id sky_proxy_staked. Pays SKY.
//   - REWARDS_USDS_01   0x10ab606B… → `sky`, same adapter_id. rewardsToken() is the zero
//     address: this farm distributed off-chain points, so it has a staked balance and no
//     on-chain reward leg.
//   - REWARDS_USDS_SPK  0x173e314C… → `spark`. Excluded — reading it here would double-count
//     against a Spark adapter and over-report against DeBank's `sky`.
//   - REWARDS_USDS_GROVE 0x4E41488C… → `makerdao`. Excluded for the same reason; that farm is
//     a coverage gap in MakerDAOAdapter, not in this one.
//
// Because the split is editorial, this list cannot be derived from the ChainLog and will not
// self-update: a farm Sky launches tomorrow needs a deliberate decision about which project
// owns it before it is added here. That is a real maintenance obligation, stated rather than
// hidden — unlike the vault list above, whose closure the chain itself guarantees.
var skyUSDSFarms = []skyStakingFarm{
	{
		ID:      "farm:usds-sky",
		Label:   "Sky USDS Farm · SKY rewards",
		Address: common.HexToAddress("0x0650CAF159C5A49f711e8169D4336ECB9b950275"),
		Window:  deploymentWindow{ActivationBlock: 20_692_595},
	},
	{
		ID:      "farm:usds-01",
		Label:   "Sky USDS Farm",
		Address: common.HexToAddress("0x10ab606B067C9C461d8893c47C7512472E19e2Ce"),
		Window:  deploymentWindow{ActivationBlock: 20_677_829},
	},
}

// skyLockstakeDeployment anchors the Lockstake Engine, where an owner locks SKY into one or
// more urns, optionally draws USDS against them, and optionally stakes the resulting lsSKY
// into a reward farm. The urn set is enumerated from the engine at the pinned block and the
// farm is read off the urn itself, so neither needs a static list.
//
// Only the v2 (LSEV2-SKY-A) engine is read, and that is a scope decision with a known edge.
// At head the MKR-denominated predecessor (LOCKSTAKE_ENGINE_OLD_V1, ilk LSE-MKR-A) is spent:
// the Vat reports zero Art on that ilk and lsMKR totalSupply is 11.46 MKR, which bounds the
// remaining collateral because the engine mints and burns the lsToken 1:1 against urn ink.
// It is dormant rather than sealed — the debt ceiling is zero so no new debt can be drawn, but
// the engine still holds its Vat and lsMKR authorisations and can still accept locks.
//
// The edge: those facts hold at head, not historically. At this engine's own activation block
// LSE-MKR-A still carried roughly 152,919 MKR of collateral and ~$44.9M of debt, and Art only
// reached zero at block 22,945,162. A fixed-block scan before that block therefore omits real
// v1 positions that DeBank does report under `sky`. Covering v1 is deliberately left out here
// because it cannot be reconciled: every v1 position is closed at head, so there is no live
// corpus to validate a v1 read against, and this repository does not merge protocol surfaces
// that no oracle can check.
var skyLockstakeDeployment = struct {
	Engine              common.Address
	Ilk                 [32]byte
	Window              deploymentWindow
	MaximumUrnsPerOwner uint64
}{
	Engine: common.HexToAddress("0xCe01C90dE7FD1bcFa39e237FE6D8D9F569e8A6a3"),
	Ilk: [32]byte(common.HexToHash(
		"0x4c534556322d534b592d41000000000000000000000000000000000000000000",
	)),
	Window:              deploymentWindow{ActivationBlock: 22_370_185},
	MaximumUrnsPerOwner: 1_024,
}

type SkyAdapter struct{ adapterBase }

func newSkyAdapter() Adapter {
	return &SkyAdapter{adapterBase: adapterBase{info: ProtocolInfo{
		ID:     "sky",
		Name:   "Sky",
		Chains: []ChainID{Ethereum},
	}}}
}

// Positions reads Sky's savings vaults, its USDS farms, and its lockstake urns. The three
// surfaces are independent: each contributes whatever it verified and the failures are joined,
// so one unreadable surface cannot discard the others. That is the behaviour adapter.go
// documents and engine.go relies on.
//
// Ethereum only, deliberately: the Base and Arbitrum sUSDS tokens are Sky's own bridged
// SUsds ERC-20, not vaults — they expose neither asset() nor convertToAssets(), DeBank has no
// portfolio support for base_sky or arb_sky, and a holder's balance belongs to their wallet
// rather than to a protocol position.
func (a *SkyAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum {
		return nil, nil
	}
	groups := make([]Group, 0, 4)
	failures := make([]error, 0, 3)

	vaults, err := readVaultPositions(ctx, client, block, account, skySavingsVaults)
	groups = append(groups, vaults...)
	if err != nil {
		failures = append(failures, err)
	}
	farms, err := skyFarmGroups(ctx, client, block, account)
	groups = append(groups, farms...)
	if err != nil {
		failures = append(failures, err)
	}
	lockstake, err := skyLockstakeGroups(ctx, client, block, account)
	groups = append(groups, lockstake...)
	if err != nil {
		failures = append(failures, err)
	}
	return groups, errors.Join(failures...)
}

// skyFarmGroups reads the owner's staked USDS and pending reward on each active farm. The
// stakingToken is asserted rather than assumed: a farm repointed by governance would otherwise
// be valued as USDS whatever it actually holds, which is the same guard readVaultPositions
// applies to a vault's asset().
func skyFarmGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	active := make([]skyStakingFarm, 0, len(skyUSDSFarms))
	for _, farm := range skyUSDSFarms {
		if farm.Window.ActiveAt(block.Number) {
			active = append(active, farm)
		}
	}
	if len(active) == 0 {
		return nil, nil
	}
	calls := make([]ContractCall, 0, len(active)*2)
	for _, farm := range active {
		calls = append(calls,
			ContractCall{Contract: farm.Address, ABI: skyFarmABI, Method: "stakingToken"},
			ContractCall{
				Contract: farm.Address,
				ABI:      skyFarmABI,
				Method:   "balanceOf",
				Args:     []any{account},
			},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Sky farm balances: %w", err)
	}
	groups := make([]Group, 0, len(active))
	for index, farm := range active {
		staking, decodeErr := AddressAt(rows[index*2], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("%s staking token: %w", farm.Label, decodeErr)
		}
		if staking != skyUSDS.Address {
			return groups, fmt.Errorf(
				"%s staking token changed from %s to %s",
				farm.Label,
				skyUSDS.Address,
				staking,
			)
		}
		staked, decodeErr := BigIntAt(rows[index*2+1], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("%s staked balance: %w", farm.Label, decodeErr)
		}
		components := make([]Component, 0, 2)
		if staked.Sign() > 0 {
			component := NewComponent(
				"asset",
				skyUSDS,
				staked,
				Source{Contract: farm.Address, Method: "balanceOf"},
			)
			component.Metadata = map[string]any{"role": "staked-supply"}
			components = append(components, component)
		}
		reward, rewardErr := skyFarmReward(ctx, client, block, account, farm.Address)
		if rewardErr != nil {
			return groups, rewardErr
		}
		if reward != nil {
			components = append(components, *reward)
		}
		if len(components) == 0 {
			continue
		}
		groups = append(groups, Group{
			ID:         farm.ID,
			MarketID:   farm.ID,
			Label:      farm.Label,
			Components: components,
			Metadata:   map[string]any{"farm": farm.Address},
		})
	}
	return groups, nil
}

func skyLockstakeGroups(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if !skyLockstakeDeployment.Window.ActiveAt(block.Number) {
		return nil, nil
	}
	urns, err := skyLockstakeUrns(ctx, client, block, account)
	if err != nil || len(urns) == 0 {
		return nil, err
	}
	// The ilk is read once for the whole owner. Between the engine's activation block and the
	// ilk's own init (22,517,471) the Vat returns rate 0; that is safe rather than lossy,
	// because Vat.frob reverts with Vat/ilk-not-init while rate is zero, so no urn can hold
	// ink or art in that interval — lsSKY totalSupply is zero across all of it.
	ilkRow, err := client.Call(
		ctx,
		block,
		makerDeployment.Vat,
		makerVatABI,
		"ilks",
		skyLockstakeDeployment.Ilk,
	)
	if err != nil {
		return nil, fmt.Errorf("Sky lockstake ilk: %w", err)
	}
	rate, err := BigIntAt(ilkRow, 1)
	if err != nil {
		return nil, fmt.Errorf("Sky lockstake rate: %w", err)
	}

	calls := make([]ContractCall, 0, len(urns)*2)
	for _, urn := range urns {
		calls = append(calls,
			ContractCall{
				Contract: makerDeployment.Vat,
				ABI:      makerVatABI,
				Method:   "urns",
				Args:     []any{skyLockstakeDeployment.Ilk, urn},
			},
			ContractCall{
				Contract: skyLockstakeDeployment.Engine,
				ABI:      skyLockstakeEngineABI,
				Method:   "urnFarms",
				Args:     []any{urn},
			},
		)
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Sky lockstake urn state: %w", err)
	}

	groups := make([]Group, 0, len(urns))
	for index, urn := range urns {
		ink, decodeErr := BigIntAt(rows[index*2], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("Sky lockstake ink: %w", decodeErr)
		}
		art, decodeErr := BigIntAt(rows[index*2], 1)
		if decodeErr != nil {
			return groups, fmt.Errorf("Sky lockstake art: %w", decodeErr)
		}
		farm, decodeErr := AddressAt(rows[index*2+1], 0)
		if decodeErr != nil {
			return groups, fmt.Errorf("Sky lockstake farm: %w", decodeErr)
		}
		components := make([]Component, 0, 3)
		if ink.Sign() > 0 {
			component := NewComponent(
				"asset",
				skySKY,
				ink,
				Source{Contract: makerDeployment.Vat, Method: "urns.ink"},
			)
			component.Metadata = map[string]any{"role": "locked", "urn": urn}
			components = append(components, component)
		}
		if debt := makerDebtRaw(art, rate); debt.Sign() > 0 {
			component := NewComponent(
				"debt",
				skyUSDS,
				debt,
				Source{Contract: makerDeployment.Vat, Method: "urns.art*ilks.rate/1e27"},
			)
			component.Metadata = map[string]any{
				"role": "borrow", "urn": urn,
				"normalizedDebt": art.String(), "rate": rate.String(),
			}
			components = append(components, component)
		}
		// Only the urn's currently selected farm is read. selectFarm withdraws from the
		// previous farm without claiming, so a reward can stay claimable on a farm the urn no
		// longer points at; across all 2,478 urns that orphaned residue measured ~636 SPK and
		// ~654 USDS in total. It is left out because DeBank reads the selected farm too, and
		// reporting it would put this adapter permanently at odds with the only oracle the
		// reconciliation corpus has. The bound is the residue, not the position: locked SKY and
		// debt always come from the Vat, which selectFarm does not touch.
		reward, rewardErr := skyFarmReward(ctx, client, block, urn, farm)
		if rewardErr != nil {
			return groups, rewardErr
		}
		if reward != nil {
			reward.Metadata["urn"] = urn
			components = append(components, *reward)
		}
		if len(components) == 0 {
			continue
		}
		groups = append(groups, Group{
			ID:             "lockstake:" + strings.ToLower(urn.Hex()),
			MarketID:       "lockstake",
			Label:          "Sky Lockstake",
			Components:     components,
			NetValuePolicy: "floor-zero",
			Metadata:       map[string]any{"urn": urn, "farm": farm},
		})
	}
	return groups, nil
}

// skyLockstakeUrns enumerates an owner's urns from the engine at the pinned block. Urn
// addresses are therefore self-gating: the engine cannot return an urn it has not created.
func skyLockstakeUrns(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]common.Address, error) {
	countRow, err := client.Call(
		ctx,
		block,
		skyLockstakeDeployment.Engine,
		skyLockstakeEngineABI,
		"ownerUrnsCount",
		account,
	)
	if err != nil {
		return nil, fmt.Errorf("Sky lockstake urn count: %w", err)
	}
	count, err := BigIntAt(countRow, 0)
	if err != nil {
		return nil, fmt.Errorf("Sky lockstake urn count: %w", err)
	}
	if count.Sign() == 0 {
		return nil, nil
	}
	if !count.IsUint64() || count.Uint64() > skyLockstakeDeployment.MaximumUrnsPerOwner {
		return nil, fmt.Errorf(
			"Sky lockstake urn count %s exceeds the %d supported per owner",
			count,
			skyLockstakeDeployment.MaximumUrnsPerOwner,
		)
	}
	calls := make([]ContractCall, 0, count.Uint64())
	for index := uint64(0); index < count.Uint64(); index++ {
		calls = append(calls, ContractCall{
			Contract: skyLockstakeDeployment.Engine,
			ABI:      skyLockstakeEngineABI,
			Method:   "ownerUrns",
			Args:     []any{account, new(big.Int).SetUint64(index)},
		})
	}
	rows, err := client.ParallelCalls(ctx, block, calls)
	if err != nil {
		return nil, fmt.Errorf("Sky lockstake urns: %w", err)
	}
	urns := make([]common.Address, 0, len(rows))
	for _, row := range rows {
		urn, decodeErr := AddressAt(row, 0)
		if decodeErr != nil {
			return nil, fmt.Errorf("Sky lockstake urn: %w", decodeErr)
		}
		urns = append(urns, urn)
	}
	return urns, nil
}

// skyFarmReward reads one holder's pending reward on one farm. The reward token is read from
// the farm rather than assumed, so a farm paying a token this package has never named still
// values correctly; a zero rewardsToken means the farm distributes nothing on-chain.
func skyFarmReward(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	holder common.Address,
	farm common.Address,
) (*Component, error) {
	if farm == (common.Address{}) {
		return nil, nil
	}
	earnedRow, err := client.Call(ctx, block, farm, skyFarmABI, "earned", holder)
	if err != nil {
		return nil, fmt.Errorf("Sky farm %s earned: %w", farm, err)
	}
	earned, err := BigIntAt(earnedRow, 0)
	if err != nil {
		return nil, fmt.Errorf("Sky farm %s earned: %w", farm, err)
	}
	if earned.Sign() == 0 {
		return nil, nil
	}
	rewardsRow, err := client.Call(ctx, block, farm, skyFarmABI, "rewardsToken")
	if err != nil {
		return nil, fmt.Errorf("Sky farm %s rewards token: %w", farm, err)
	}
	rewardsToken, err := AddressAt(rewardsRow, 0)
	if err != nil {
		return nil, fmt.Errorf("Sky farm %s rewards token: %w", farm, err)
	}
	if rewardsToken == (common.Address{}) {
		return nil, nil
	}
	rewardToken := skySKY
	if rewardsToken != skySKY.Address {
		rewardToken, err = readToken(ctx, client, block, rewardsToken)
		if err != nil {
			return nil, fmt.Errorf("Sky farm reward %s metadata: %w", rewardsToken, err)
		}
	}
	component := NewComponent(
		"reward",
		rewardToken,
		earned,
		Source{Contract: farm, Method: "earned"},
	)
	component.Metadata = map[string]any{"role": "reward", "farm": farm}
	return &component, nil
}
