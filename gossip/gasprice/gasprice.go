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
	"github.com/Fantom-foundation/go-opera/opera"
	"github.com/Fantom-foundation/go-opera/utils/piecefunc"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

const DecimalUnit = piecefunc.DecimalUnit
const baseFeeBlockTime = int64((10 * time.Second) / time.Millisecond)

var DefaultMaxTipCap = big.NewInt(100000 * params.GWei)
var secondBn = big.NewInt(int64(time.Second))
var DecimalUnitBn = big.NewInt(DecimalUnit)
var baseFeeBlockTimeBn = big.NewInt(baseFeeBlockTime)

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

type cache struct {
	head  idx.Block
	lock  sync.RWMutex
	value *big.Int
}

type baseFees struct {
	// Lock for headValue
	lock      sync.RWMutex
	blockTime *time.Time

	// latest evaluated block
	head    idx.Block
	baseFee *big.Int

	// history cache
	cache *lru.Cache
}

// Oracle recommends gas prices based on the content of recent
// blocks. Suitable for both light and full clients.
type Oracle struct {
	backend    Reader
	ethBackend EthBackend
	cfg        Config
	cache      cache
	baseFees   baseFees

	maxHeaderHistory int
	maxBlockHistory  int
	historyCache     *lru.Cache
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
	params.GasPowerWallRatio = sanitizeBigInt(params.GasPowerWallRatio, big.NewInt(1), big.NewInt(DecimalUnit-2), big.NewInt(1), "GasPowerWallRatio")
	params.MaxTipCapMultiplierRatio = sanitizeBigInt(params.MaxTipCapMultiplierRatio, DecimalUnitBn, nil, big.NewInt(10*DecimalUnit), "MaxTipCapMultiplierRatio")
	params.MiddleTipCapMultiplierRatio = sanitizeBigInt(params.MiddleTipCapMultiplierRatio, DecimalUnitBn, params.MaxTipCapMultiplierRatio, big.NewInt(2*DecimalUnit), "MiddleTipCapMultiplierRatio")

	oracle := &Oracle{
		backend:          backend,
		ethBackend:       ethBackend,
		cfg:              params,
		maxHeaderHistory: params.MaxHeaderHistory,
		maxBlockHistory:  params.MaxBlockHistory,
	}

	oracle.historyCache, _ = lru.New(2048)
	oracle.baseFees.cache, _ = lru.New(1024)
	oracle.baseFees.baseFee = backend.GetRules().Economy.MinGasPrice

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
		gpo.baseFees.blockTime = nil
	}

	duration := baseFeeBlockTime
	blockTime := block.Time.Time()

	if gpo.baseFees.blockTime != nil {
		duration = int64(blockTime.Sub(*gpo.baseFees.blockTime) / time.Millisecond)

		if duration > baseFeeBlockTime {
			duration = baseFeeBlockTime
		} else if duration < 0 {
			duration = 1
		}
	}

	baseFee := calculateBaseFee(block, duration)
	if baseFee.Cmp(gpo.backend.GetRules().Economy.MinGasPrice) < 0 {
		baseFee = gpo.backend.GetRules().Economy.MinGasPrice
	}

	gpo.baseFees.lock.Lock()

	gpo.baseFees.head = idx.Block(block.Number.Uint64())
	gpo.baseFees.baseFee = baseFee
	gpo.baseFees.blockTime = &blockTime

	gpo.baseFees.lock.Unlock()
}

func (gpo *Oracle) maxTotalGasPower() *big.Int {
	rules := gpo.backend.GetRules()

	allocBn := new(big.Int).SetUint64(rules.Economy.LongGasPower.AllocPerSec)
	periodBn := new(big.Int).SetUint64(uint64(rules.Economy.LongGasPower.MaxAllocPeriod))
	maxTotalGasPowerBn := new(big.Int).Mul(allocBn, periodBn)
	maxTotalGasPowerBn.Div(maxTotalGasPowerBn, secondBn)
	return maxTotalGasPowerBn
}

func (gpo *Oracle) suggestTipCap() *big.Int {
	max := gpo.maxTotalGasPower()

	current := new(big.Int).SetUint64(gpo.backend.TotalGasPowerLeft())

	freeRatioBn := current.Mul(current, DecimalUnitBn)
	freeRatioBn.Div(freeRatioBn, max)
	freeRatio := freeRatioBn.Uint64()
	if freeRatio > DecimalUnit {
		freeRatio = DecimalUnit
	}

	multiplierFn := piecefunc.NewFunc([]piecefunc.Dot{
		{
			X: 0,
			Y: gpo.cfg.MaxTipCapMultiplierRatio.Uint64(),
		},
		{
			X: gpo.cfg.GasPowerWallRatio.Uint64(),
			Y: gpo.cfg.MaxTipCapMultiplierRatio.Uint64(),
		},
		{
			X: gpo.cfg.GasPowerWallRatio.Uint64() + (DecimalUnit-gpo.cfg.GasPowerWallRatio.Uint64())/2,
			Y: gpo.cfg.MiddleTipCapMultiplierRatio.Uint64(),
		},
		{
			X: DecimalUnit,
			Y: 0,
		},
	})

	multiplier := new(big.Int).SetUint64(multiplierFn(freeRatio))

	minPrice := gpo.backend.GetRules().Economy.MinGasPrice
	adjustedMinPrice := math.BigMax(minPrice, gpo.backend.GetPendingRules().Economy.MinGasPrice)

	// tip cap = (multiplier * adjustedMinPrice + adjustedMinPrice) - minPrice
	tip := multiplier.Mul(multiplier, adjustedMinPrice)
	tip.Div(tip, DecimalUnitBn)
	tip.Add(tip, adjustedMinPrice)
	tip.Sub(tip, minPrice)

	adjustedMinTipCap := math.BigMax(
		gpo.backend.GetRules().Economy.MinGasTip,
		gpo.backend.GetPendingRules().Economy.MinGasTip)

	if tip.Cmp(adjustedMinTipCap) < 0 {
		return adjustedMinTipCap
	}
	if tip.Cmp(gpo.cfg.MaxTipCap) > 0 {
		return gpo.cfg.MaxTipCap
	}
	return tip
}

// SuggestTipCap returns a tip cap so that newly created transaction can have a
// very high chance to be included in the following blocks.
//
// Note, for legacy transactions and the legacy eth_gasPrice RPC call, it will be
// necessary to add the basefee to the returned number to fall back to the legacy
// behavior.
func (gpo *Oracle) SuggestTipCap() *big.Int {
	head := gpo.backend.GetLatestBlockIndex()

	// If the latest gasprice is still available, return it.
	gpo.cache.lock.RLock()
	cachedHead, cachedValue := gpo.cache.head, gpo.cache.value
	gpo.cache.lock.RUnlock()
	if head <= cachedHead {
		return new(big.Int).Set(cachedValue)
	}

	value := gpo.suggestTipCap()

	gpo.cache.lock.Lock()
	if head > gpo.cache.head {
		gpo.cache.head = head
		gpo.cache.value = value
	}
	gpo.cache.lock.Unlock()
	return new(big.Int).Set(value)
}

// calcBaseFee calculates the basefee of the header for EIP1559 blocks.
func calculateBaseFee(block *evmcore.EvmBlock, duration int64) *big.Int {
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
		durationBig              = big.NewInt(duration)
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
