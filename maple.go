package portfolio

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"
)

var mapleQueueABI = MustABI(`[
  {"type":"function","name":"pool","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"asset","stateMutability":"view","inputs":[],"outputs":[{"type":"address"}]},
  {"type":"function","name":"userEscrowedShares","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint256"}]},
  {"type":"function","name":"requestIds","stateMutability":"view","inputs":[{"type":"address"}],"outputs":[{"type":"uint128"}]},
  {"type":"function","name":"requests","stateMutability":"view","inputs":[{"type":"uint128"}],"outputs":[{"type":"address"},{"type":"uint256"}]}
]`)

type mapleQueue struct {
	Address         common.Address
	Pool            common.Address
	Asset           Token
	Share           Token
	ActivationBlock uint64
	Legacy          bool
	OutputShares    bool
}

type MapleAdapter struct {
	adapterBase
	vaults map[ChainID][]vaultConfig
	queues []mapleQueue
}

func mapleVault(name, address string, activation uint64, asset Token) vaultConfig {
	lower := strings.ToLower(address)
	return vaultConfig{
		ID: "vault:" + lower, Label: "Yield · " + name, Address: common.HexToAddress(address),
		Asset: asset, ActivationBlock: activation,
	}
}

func newMapleAdapter() Adapter {
	usdc := token(Ethereum, "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "USDC", 6)
	usdt := token(Ethereum, "0xdac17f958d2ee523a2206206994597c13d831ec7", "USDT", 6)
	usdg := token(Ethereum, "0xe343167631d89b6ffc58b88d6b7fb0228795491d", "USDG", 6)
	weth := token(Ethereum, "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2", "WETH", 18)
	usdck1 := token(Ethereum, "0x9bbc017431da809d94dab738453cfa0415e78cd8", "USDC.k1", 6)
	syrup := token(Ethereum, "0x643c4e15d7d62ad0abec4a9bd4b001aa3ef52d66", "SYRUP", 18)
	vaults := []vaultConfig{
		mapleVault("syrupUSDC", "0x80ac24aa929eaf5013f6436cda2a7ba190f5cc0b", 19_920_366, usdc),
		mapleVault("syrupUSDT", "0x356b8d89c1e1239cbbb9de4815c39a1474d5ba7d", 20_434_756, usdt),
		mapleVault("syrupUSDG", "0x87b65c4aaffa76881f9e96f3e7ed945ddfc3cd7a", 25_173_105, usdg),
		mapleVault("securedLendingUSDC", "0xc39a5a616f0ad1ff45077fa2de3f79ab8eb8b8b9", 19_363_393, usdc),
		mapleVault("LendAndLongUSDC1", "0x37154b07d58cd736a09ed93ced06613a06f93081", 21_667_004, usdc),
		mapleVault("LendAndLongUSDC2", "0xc9c9bab51b02b4e60a828a09803305772ae1d2eb", 21_831_616, usdc),
		mapleVault("aqru", "0xe9d33286f0e37f517b1204aa6da085564414996d", 16_485_840, usdc),
		mapleVault("blueChipSecuredUSDC", "0xc1dd3f011290f212227170f0d02f511ebf57e433", 17_877_843, usdc),
		mapleVault("highYieldCorpUSDC", "0x6174a27160f4d7885db4ffed1c0b5fbd66c87f3a", 18_778_956, usdc),
		mapleVault("highYieldCorpWETH", "0xccbc525ed9d85ad8325b7b6c4c6a79f5566dea3b", 19_335_435, weth),
		mapleVault("cashUSDC", "0xfe119e9c24ab79f1bdd5dd884b86ceea2ee75d92", 17_081_094, usdc),
		mapleVault("cashUSDT", "0xf05681a33a9adf14076990789a89ab3da3f6b536", 17_629_617, usdt),
		mapleVault("cicada", "0xf025edfa685c9ea873ea4b22da85e7e1fba24381", 18_370_317, usdc),
		mapleVault("icebreaker", "0x137f2ea5cfb0fe59408bab2779e33ee868f1810e", 16_162_589, usdc),
		mapleVault("laser", "0xd020c197497db6db12cff97a8575451c6faa54b3", 17_933_731, usdck1),
		mapleVault("mavenPermissioned", "0x00e0c1ea2085e30e5233e98cfa940ca8cbb1b0b7", 16_162_315, usdc),
		mapleVault("mavenUSDC3", "0xd2b01f8327eeca47829efc731f1a89c6d07e6b92", 16_926_128, usdc),
		mapleVault("mavenUSDC", "0xd3cd37a7299b963bbc69592e5ba933388f70dc88", 16_162_536, usdc),
		mapleVault("mavenWeth", "0xfff9a1caf78b2e5b0a49355a8637ea78b43fb6c3", 16_162_554, weth),
		mapleVault("orthogonal", "0x79400a2c9a5e2431419cac98bf46893c86e8bdd7", 16_162_570, usdc),
		mapleVault("stSYRUP", "0xc7e8b36e0766d9b04c93de68a9d47dd11f260b45", 20_735_662, syrup),
	}
	share := func(address, symbol string, decimals uint8) Token {
		return token(Ethereum, address, symbol, decimals)
	}
	queue := func(index int, address string, activation uint64, legacy, outputShares bool, shareToken Token) mapleQueue {
		return mapleQueue{
			Address: common.HexToAddress(address), Pool: vaults[index].Address, Asset: vaults[index].Asset,
			Share: shareToken, ActivationBlock: activation, Legacy: legacy, OutputShares: outputShares,
		}
	}
	queues := []mapleQueue{
		queue(0, "0x1bc47a0dd0fdab96e9ef982fdf1f34dc6207cfe3", 19_920_366, false, false, share(vaults[0].Address.Hex(), "syrupUSDC", 6)),
		queue(1, "0x86ebdf902d800f2a82038290b6dbb2a5ee29eb8c", 20_434_756, false, false, share(vaults[1].Address.Hex(), "syrupUSDT", 6)),
		queue(2, "0xaf63c06970086d535f338565d77c5fa3bdc5fd79", 25_173_105, false, false, share(vaults[2].Address.Hex(), "syrupUSDG", 6)),
		queue(3, "0x8a665131e796203a5232527fac441480e02fbb7f", 19_363_393, false, false, share(vaults[3].Address.Hex(), "MPLhysUSDC1", 6)),
		queue(4, "0x98c0d6cd8af6274801de98aead27dc9ef03c6ab2", 21_667_004, false, false, share(vaults[4].Address.Hex(), "MAPLE_L+L_1", 6)),
		queue(5, "0xc512e614ac4d0d4ff9e548f4cad8dfe63b8a36c1", 21_831_616, false, false, share(vaults[5].Address.Hex(), "MAPLE_L+L_2", 6)),
		queue(7, "0xf18066db3a9590c401e1841598ad90663b4c6d23", 18_970_300, false, false, share(vaults[7].Address.Hex(), "MPLdirUSDC1", 6)),
		queue(8, "0xeb7b1e9c750190214cdfbbaf0abe398a5e47d230", 18_821_432, false, false, share(vaults[8].Address.Hex(), "MPLohyUSDC1", 6)),
		queue(9, "0x58a534945f357aa0d2fb56b8bdf7dfa1073bd7a1", 19_335_435, false, false, share(vaults[9].Address.Hex(), "MPLhycWETH1", 18)),
		queue(10, "0x447dcea1d616f792645ed6e71bc32955a0dbcbaa", 18_821_432, false, false, share(vaults[10].Address.Hex(), "MPLcashUSDC", 6)),
		queue(11, "0xf4dd63ee071178a6485e2035ed279839f5453512", 18_821_432, true, true, share(vaults[11].Address.Hex(), "MPLcashUSDT", 6)),
	}
	return &MapleAdapter{
		adapterBase: adapterBase{info: ProtocolInfo{ID: "maple", Name: "Maple", Chains: []ChainID{Ethereum}}},
		vaults:      map[ChainID][]vaultConfig{Ethereum: vaults}, queues: queues,
	}
}

func (a *MapleAdapter) queuePositions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	active := make([]mapleQueue, 0, len(a.queues))
	for _, queue := range a.queues {
		if block.Number >= queue.ActivationBlock {
			active = append(active, queue)
		}
	}
	identityCalls := make([]ContractCall, 0, len(active)*2)
	for _, queue := range active {
		identityCalls = append(identityCalls,
			ContractCall{Contract: queue.Address, ABI: mapleQueueABI, Method: "pool"},
			ContractCall{Contract: queue.Address, ABI: mapleQueueABI, Method: "asset"},
		)
	}
	identities, err := client.ParallelCalls(ctx, block, identityCalls)
	if err != nil {
		return nil, fmt.Errorf("Maple queue identities: %w", err)
	}
	type pendingQueue struct {
		queue     mapleQueue
		shares    *big.Int
		requestID *big.Int
	}
	pending := make([]pendingQueue, 0)
	for index, queue := range active {
		actualPool, decodeErr := AddressAt(identities[index*2], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		actualAsset, decodeErr := AddressAt(identities[index*2+1], 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if actualPool != queue.Pool || actualAsset != queue.Asset.Address {
			return nil, fmt.Errorf("Maple queue %s identity changed: pool %s asset %s", queue.Address, actualPool, actualAsset)
		}
		if !queue.Legacy {
			row, callErr := client.Call(ctx, block, queue.Address, mapleQueueABI, "userEscrowedShares", account)
			if callErr != nil {
				return nil, callErr
			}
			shares, decodeErr := BigIntAt(row, 0)
			if decodeErr != nil {
				return nil, decodeErr
			}
			if shares.Sign() > 0 {
				pending = append(pending, pendingQueue{queue: queue, shares: shares})
			}
			continue
		}
		idRow, callErr := client.Call(ctx, block, queue.Address, mapleQueueABI, "requestIds", account)
		if callErr != nil {
			return nil, callErr
		}
		requestID, decodeErr := BigIntAt(idRow, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if requestID.Sign() == 0 {
			continue
		}
		requestRow, callErr := client.Call(ctx, block, queue.Address, mapleQueueABI, "requests", requestID)
		if callErr != nil {
			return nil, callErr
		}
		owner, decodeErr := AddressAt(requestRow, 0)
		if decodeErr != nil {
			return nil, decodeErr
		}
		shares, decodeErr := BigIntAt(requestRow, 1)
		if decodeErr != nil {
			return nil, decodeErr
		}
		if shares.Sign() == 0 && owner == (common.Address{}) {
			continue
		}
		if owner != account {
			return nil, fmt.Errorf("Maple queue %s request %s owner changed from %s to %s", queue.Address, requestID, account, owner)
		}
		if shares.Sign() > 0 {
			pending = append(pending, pendingQueue{queue: queue, shares: shares, requestID: requestID})
		}
	}
	groups := make([]Group, 0, len(pending))
	for _, item := range pending {
		amount := new(big.Int).Set(item.shares)
		token := item.queue.Share
		method := "requests(requestIds(account))"
		if !item.queue.Legacy {
			method = "userEscrowedShares(account)"
		}
		if !item.queue.OutputShares {
			row, callErr := client.Call(ctx, block, item.queue.Pool, erc4626ABI, "convertToAssets", item.shares)
			if callErr != nil {
				return nil, callErr
			}
			amount, err = BigIntAt(row, 0)
			if err != nil {
				return nil, err
			}
			token = item.queue.Asset
		}
		if amount.Sign() == 0 {
			continue
		}
		component := NewComponent("asset", token, amount, Source{Contract: item.queue.Address, Method: method})
		component.Metadata = map[string]any{"shares": item.shares.String(), "outputShares": item.queue.OutputShares}
		if item.requestID != nil {
			component.Metadata["requestId"] = item.requestID.String()
		}
		marketID := "queue:" + strings.ToLower(item.queue.Address.Hex())
		groups = append(groups, Group{
			ID: marketID, MarketID: marketID, Label: "Deposit · " + item.queue.Share.Symbol + " withdrawal queue",
			Components: []Component{component}, Metadata: map[string]any{"pool": item.queue.Pool, "withdrawalManager": item.queue.Address},
		})
	}
	return groups, nil
}

func (a *MapleAdapter) Positions(
	ctx context.Context,
	client *RPCClient,
	block BlockRef,
	account common.Address,
) ([]Group, error) {
	if block.ChainID != Ethereum {
		return nil, nil
	}
	vaults, err := readVaultPositions(ctx, client, block, account, a.vaults[Ethereum])
	if err != nil {
		return nil, err
	}
	queues, err := a.queuePositions(ctx, client, block, account)
	if err != nil {
		return nil, err
	}
	return append(vaults, queues...), nil
}
