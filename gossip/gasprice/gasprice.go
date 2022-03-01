// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package gasprice

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"sync"
	"time"

	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/core/types"
	notify "github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rpc"
	lru "github.com/hashicorp/golang-lru"

	"github.com/Fantom-foundation/go-opera/evmcore"
	"github.com/Fantom-foundation/go-opera/inter"
	"github.com/Fantom-foundation/go-opera/opera"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

const baseFeeBlockTime = uint64((10 * time.Second) / time.Millisecond)

const tipFeeHistoryBlocks = 20
const tipFeeHistorySamples = 3
const tipFeeHistoryPercent = 60

var DefaultMaxTipCap = big.NewInt(100000 * params.GWei)
var baseFeeBlockTimeBn = big.NewInt(int64(baseFeeBlockTime))

type Config struct {
	MaxTipCap                   *big.Int `toml:",omitempty"`
	MaxTipCapMultiplierRatio    *big.Int `toml:",omitempty"`
	MiddleTipCapMultiplierRatio *big.Int `toml:",omitempty"`
	GasPowerWallRatio           *big.Int `toml:",omitempty"`
	MaxHeaderHistory            int
	MaxBlockHistory             int
}

// EthBackend includes all necessary background APIs for oracle.
type EthBackend interface {
	PendingBlockAndReceipts() (*types.Block, types.Receipts)
	GetReceipts(ctx context.Context, block common.Hash) (types.Receipts, error)
	HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*evmcore.EvmHeader, error)
	BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*evmcore.EvmBlock, error)
	SubscribeNewBlockNotify(ch chan<- evmcore.ChainHeadNotify) notify.Subscription
}

type Reader interface {
	GetLatestBlockIndex() idx.Block
	TotalGasPowerLeft() uint64
	GetRules() opera.Rules
	GetPendingRules() opera.Rules
}

type baseFeeCacheResut struct {
	baseFee *big.Int
}

type tipFeeCacheEntry struct {
	blockNumber uint64
	fee         *big.Int
}

type tipFeeCache struct {
	// tipFeeCache
	num   int
	cache []tipFeeCacheEntry
}

type fees struct {
	// Lock for headValue
	lock      sync.RWMutex
	blockTime inter.Timestamp

	// latest evaluated block
	head    idx.Block
	baseFee *big.Int
	tipFee  *big.Int

	// cache of tip fees
	tipCache tipFeeCache
}

// Oracle recommends gas prices based on the content of recent
// blocks. Suitable for both light and full clients.
type Oracle struct {
	backend    Reader
	ethBackend EthBackend
	cfg        Config
	fees       fees

	// FeeHistory
	maxHeaderHistory int
	maxBlockHistory  int
	historyCache     *lru.Cache
	baseFeeCache     *lru.Cache
}

func sanitizeBigInt(val, min, max, _default *big.Int, name string) *big.Int {
	if val == nil || (val.Sign() == 0 && _default.Sign() != 0) {
		log.Warn(fmt.Sprintf("Sanitizing invalid parameter %s of gasprice oracle", name), "provided", val, "updated", _default)
		return _default
	}
	if min != nil && val.Cmp(min) < 0 {
		log.Warn(fmt.Sprintf("Sanitizing invalid parameter %s of gasprice oracle", name), "provided", val, "updated", min)
		return min
	}
	if max != nil && val.Cmp(max) > 0 {
		log.Warn(fmt.Sprintf("Sanitizing invalid parameter %s of gasprice oracle", name), "provided", val, "updated", max)
		return max
	}
	return val
}

// NewOracle returns a new gasprice oracle which can recommend suitable
// gasprice for newly created transaction.
func NewOracle(backend Reader, ethBackend EthBackend, params Config) *Oracle {
	params.MaxTipCap = sanitizeBigInt(params.MaxTipCap, nil, nil, DefaultMaxTipCap, "MaxTipCap")

	oracle := &Oracle{
		backend:          backend,
		ethBackend:       ethBackend,
		cfg:              params,
		maxHeaderHistory: params.MaxHeaderHistory,
		maxBlockHistory:  params.MaxBlockHistory,
	}

	oracle.historyCache, _ = lru.New(2048)
	oracle.baseFeeCache, _ = lru.New(128)
	oracle.fees.tipCache.cache = make([]tipFeeCacheEntry, tipFeeHistoryBlocks*tipFeeHistorySamples)

	oracle.fees.baseFee = backend.GetRules().Economy.MinGasPrice
	oracle.fees.tipFee = backend.GetRules().Economy.MinGasTip

	if ethBackend != nil {
		headEvent := make(chan evmcore.ChainHeadNotify, 1)
		ethBackend.SubscribeNewBlockNotify(headEvent)
		go func() {
			var lastHead common.Hash
			for ev := range headEvent {
				oracle.handleHeadEvent(ev.Block, ev.Block.ParentHash == lastHead)
				lastHead = ev.Block.Hash
			}
		}()
	}
	return oracle
}

func (gpo *Oracle) handleHeadEvent(block *evmcore.EvmBlock, contiguous bool) {
	if !contiguous {
		/* To clarify: why ??
		gpo.historyCache.Purge()*/
		gpo.fees.blockTime = 0
	}

	duration := baseFeeBlockTime
	blockTime := block.Time

	if blockTime > gpo.fees.blockTime {
		duration = uint64(blockTime-gpo.fees.blockTime) / uint64(time.Millisecond)
		if duration > baseFeeBlockTime {
			duration = baseFeeBlockTime
		}
	} else {
		// Default for wrong timestamps: 1ms
		duration = 1
	}

	baseFee := calculateBaseFee(block, duration)
	if baseFee == nil || baseFee.Cmp(gpo.backend.GetRules().Economy.MinGasPrice) < 0 {
		baseFee = gpo.backend.GetRules().Economy.MinGasPrice
	}

	tipFee := gpo.calculateTipFee(block)
	if tipFee.Cmp(gpo.backend.GetRules().Economy.MinGasTip) < 0 {
		baseFee = gpo.backend.GetRules().Economy.MinGasTip
	}

	gpo.fees.lock.Lock()
	defer gpo.fees.lock.Unlock()

	gpo.fees.head = idx.Block(block.Number.Uint64())
	gpo.fees.baseFee = baseFee
	gpo.fees.tipFee = tipFee
	gpo.fees.blockTime = blockTime
}

func (gpo *Oracle) GetBaseFee() (idx.Block, *big.Int) {
	gpo.fees.lock.RLock()
	defer gpo.fees.lock.RUnlock()

	return gpo.fees.head, gpo.fees.baseFee
}

// SuggestTipCap returns a tip cap so that newly created transaction can have a
// very high chance to be included in the following blocks.
//
// Note, for legacy transactions and the legacy eth_gasPrice RPC call, it will be
// necessary to add the basefee to the returned number to fall back to the legacy
// behavior.
func (gpo *Oracle) SuggestTipCap() *big.Int {
	gpo.fees.lock.RLock()
	defer gpo.fees.lock.RUnlock()

	return gpo.fees.tipFee
}

// calcBaseFee calculates the basefee of the header for EIP1559 blocks.
func calculateBaseFee(block *evmcore.EvmBlock, duration uint64) *big.Int {
	// Pre check
	if block.BaseFee == nil {
		return nil
	}

	var (
		parentGasTarget = block.GasLimit / params.ElasticityMultiplier
		gasUsedAdjusted = (block.GasUsed * uint64(baseFeeBlockTime)) / uint64(duration)
	)

	// If the parent gasUsed is the same as the target, the baseFee remains unchanged.
	if gasUsedAdjusted == parentGasTarget {
		return block.BaseFee
	}

	var (
		parentGasTargetBig       = new(big.Int).SetUint64(parentGasTarget)
		baseFeeChangeDenominator = new(big.Int).SetUint64(params.BaseFeeChangeDenominator)
		durationBig              = new(big.Int).SetUint64(duration)
	)

	if gasUsedAdjusted > parentGasTarget {
		// If the parent block used more gas than its target, the baseFee should increase.
		gasUsedDelta := new(big.Int).SetUint64(gasUsedAdjusted - parentGasTarget)
		x := new(big.Int).Mul(block.BaseFee, gasUsedDelta)
		y := x.Div(x, parentGasTargetBig)
		baseFeeDelta := math.BigMax(
			x.Div(y, baseFeeChangeDenominator),
			common.Big1,
		)

		return x.Add(block.BaseFee, baseFeeDelta.
			Mul(durationBig, baseFeeDelta).
			Div(baseFeeDelta, baseFeeBlockTimeBn))
	} else {
		// Otherwise if the parent block used less gas than its target, the baseFee should decrease.
		gasUsedDelta := new(big.Int).SetUint64(parentGasTarget - gasUsedAdjusted)
		x := new(big.Int).Mul(block.BaseFee, gasUsedDelta)
		y := x.Div(x, parentGasTargetBig)
		baseFeeDelta := x.Div(y, baseFeeChangeDenominator)

		return math.BigMax(
			x.Sub(block.BaseFee, baseFeeDelta.
				Mul(durationBig, baseFeeDelta).
				Div(baseFeeDelta, baseFeeBlockTimeBn)),
			common.Big0,
		)
	}
}

// tip fee cache sorter sorts in ascending order
func (c tipFeeCache) Len() int           { return c.num }
func (c tipFeeCache) Swap(i, j int)      { c.cache[i], c.cache[j] = c.cache[j], c.cache[i] }
func (c tipFeeCache) Less(i, j int) bool { return c.cache[i].fee.Cmp(c.cache[j].fee) < 0 }

// calculateTipFee updates tipCache by removing outdated blocks and adding
// up to tipFeeHistorySamples (cheapest) new elements.
// If we have enough data, we get a good candiate by selecting the
// percentile position in our tipCache.
func (gpo *Oracle) calculateTipFee(block *evmcore.EvmBlock) *big.Int {
	// remove all entries older than tipFeeHistoryBlocks
	// also make sure that accidential future blocks are removed
	cache := &gpo.fees.tipCache
	if block.Number.Uint64() >= tipFeeHistoryBlocks {
		removal := block.Number.Uint64() - tipFeeHistoryBlocks
		nextInsertPos := 0
		for i := 0; i < cache.num; i++ {
			if cache.cache[i].blockNumber > removal &&
				cache.cache[i].blockNumber < block.Number.Uint64() {

				if i != nextInsertPos {
					cache.cache[nextInsertPos] = cache.cache[i]
				}
				nextInsertPos++
			}
		}
		cache.num = nextInsertPos
	}

	// get max of tipFeeHistorySamples out of the new block
	if block.BaseFee != nil {
		numResults := 0
		var results [tipFeeHistorySamples]tipFeeCacheEntry

		for _, tx := range block.Transactions {
			// don't use coinbase transactions for calculation
			if tx.GasPrice().Cmp(common.Big0) <= 0 && tx.GasFeeCap().Cmp(common.Big0) <= 0 {
				continue
			}
			if tipFee, err := tx.EffectiveGasTip(block.BaseFee); err == nil {
				insertPos := -1
				if numResults < tipFeeHistorySamples {
					insertPos = numResults
					numResults++
				} else {
					for i := 0; i < tipFeeHistorySamples; i++ {
						if results[i].fee.Cmp(tipFee) > 0 && (insertPos < 0 ||
							results[i].fee.Cmp(results[insertPos].fee) > 0) {
							insertPos = i
						}
					}
				}
				if insertPos >= 0 {
					results[insertPos] = tipFeeCacheEntry{blockNumber: block.Number.Uint64(), fee: tipFee}
				}
			}
		}
		if numResults > 0 {
			for numResults > 0 {
				numResults--
				cache.cache[cache.num] = results[numResults]
				cache.num++
			}
			sort.Sort(cache)
		}
	}
	if cache.num >= tipFeeHistoryBlocks {
		return cache.cache[(cache.num*tipFeeHistoryPercent)/100].fee
	}
	return gpo.backend.GetRules().Economy.MinGasTip
}
