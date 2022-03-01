// Copyright 2021 The go-ethereum Authors
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
	"errors"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	notify "github.com/ethereum/go-ethereum/event"
	"github.com/ethereum/go-ethereum/rpc"

	"github.com/Fantom-foundation/go-opera/evmcore"
	"github.com/Fantom-foundation/go-opera/opera"
	"github.com/Fantom-foundation/lachesis-base/inter/idx"
)

// EthBackend includes all necessary background APIs for oracle.
type TestEthBackend struct {
	block idx.Block
	rules opera.Rules
}

func (t *TestEthBackend) GetRules() opera.Rules {
	return t.rules
}

func (t TestEthBackend) PendingBlockAndReceipts() (*types.Block, types.Receipts) {
	return nil, nil
}

func (t TestEthBackend) GetReceipts(ctx context.Context, block common.Hash) (types.Receipts, error) {
	return nil, nil
}

func (t TestEthBackend) HeaderByNumber(ctx context.Context, number rpc.BlockNumber) (*evmcore.EvmHeader, error) {
	if number == rpc.LatestBlockNumber {
		number = rpc.BlockNumber(t.block)
	}
	return &evmcore.EvmHeader{
		Number:  big.NewInt(number.Int64()),
		BaseFee: opera.FakeNetRules().Economy.MinGasPrice,
	}, nil
}

func (t TestEthBackend) BlockByNumber(ctx context.Context, number rpc.BlockNumber) (*evmcore.EvmBlock, error) {
	return &evmcore.EvmBlock{}, nil
}

func (t TestEthBackend) SubscribeNewBlockNotify(ch chan<- evmcore.ChainHeadNotify) notify.Subscription {
	return nil
}

func newEthBackend(block idx.Block) *TestEthBackend {
	return &TestEthBackend{
		block: block,
		rules: opera.FakeNetRules(),
	}
}

func TestFeeHistory(t *testing.T) {
	var cases = []struct {
		pending             bool
		maxHeader, maxBlock int
		count               int
		last                rpc.BlockNumber
		percent             []float64
		expFirst            uint64
		expCount            int
		expErr              error
	}{
		{false, 1000, 1000, 10, 30, nil, 21, 10, nil},
		{false, 1000, 1000, 10, 30, []float64{0, 10}, 21, 10, nil},
		{false, 1000, 1000, 10, 30, []float64{20, 10}, 0, 0, errInvalidPercentile},
		{false, 1000, 1000, 1000000000, 30, nil, 1, 30, nil},
		{false, 1000, 1000, 1000000000, rpc.LatestBlockNumber, nil, 1, 32, nil},
		{false, 1000, 1000, 10, 40, nil, 0, 0, errRequestBeyondHead},
		{true, 1000, 1000, 10, 40, nil, 0, 0, errRequestBeyondHead},
		{false, 20, 2, 100, rpc.LatestBlockNumber, nil, 13, 20, nil},
		{false, 20, 2, 100, rpc.LatestBlockNumber, []float64{0, 10}, 31, 2, nil},
		{false, 20, 2, 100, 32, []float64{0, 10}, 31, 2, nil},
		{false, 1000, 1000, 1, rpc.PendingBlockNumber, nil, 0, 0, nil},
		{false, 1000, 1000, 2, rpc.PendingBlockNumber, nil, 32, 1, nil},
		{true, 1000, 1000, 2, rpc.PendingBlockNumber, nil, 32, 1, nil},
		{true, 1000, 1000, 2, rpc.PendingBlockNumber, []float64{0, 10}, 32, 1, nil},
	}
	for i, c := range cases {
		config := Config{
			MaxHeaderHistory: c.maxHeader,
			MaxBlockHistory:  c.maxBlock,
		}
		oracle := NewOracle(newTestBackend(), newEthBackend(32), config)
		// also fast forward fees
		oracle.handleHeadEvent(&evmcore.EvmBlock{
			EvmHeader: evmcore.EvmHeader{
				Number: big.NewInt(32),
			},
		}, true)

		first, reward, baseFee, ratio, err := oracle.FeeHistory(context.Background(), c.count, c.last, c.percent)

		expReward := c.expCount
		if len(c.percent) == 0 {
			expReward = 0
		}
		expBaseFee := c.expCount
		if expBaseFee != 0 {
			expBaseFee++
		}

		if first.Uint64() != c.expFirst {
			t.Fatalf("Test case %d: first block mismatch, want %d, got %d", i, c.expFirst, first)
		}
		if len(reward) != expReward {
			t.Fatalf("Test case %d: reward array length mismatch, want %d, got %d", i, expReward, len(reward))
		}
		if len(baseFee) != expBaseFee {
			t.Fatalf("Test case %d: baseFee array length mismatch, want %d, got %d", i, expBaseFee, len(baseFee))
		}
		if len(ratio) != c.expCount {
			t.Fatalf("Test case %d: gasUsedRatio array length mismatch, want %d, got %d", i, c.expCount, len(ratio))
		}
		if err != c.expErr && !errors.Is(err, c.expErr) {
			t.Fatalf("Test case %d: error mismatch, want %v, got %v", i, c.expErr, err)
		}
	}
}
