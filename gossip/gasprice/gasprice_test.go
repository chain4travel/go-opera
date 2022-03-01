package gasprice

import (
	"math/big"
	"testing"
	"time"

	"github.com/Fantom-foundation/lachesis-base/inter/idx"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	"github.com/Fantom-foundation/go-opera/evmcore"
	"github.com/Fantom-foundation/go-opera/inter"
	"github.com/Fantom-foundation/go-opera/opera"
)

type TestBackend struct {
	blockNumber       idx.Block
	blockTime         inter.Timestamp
	totalGasPowerLeft uint64
	rules             opera.Rules
	pendingRules      opera.Rules
}

func (t *TestBackend) GetLatestBlockIndex() idx.Block {
	return t.blockNumber
}

func (t *TestBackend) TotalGasPowerLeft() uint64 {
	return t.totalGasPowerLeft
}

func (t *TestBackend) GetRules() opera.Rules {
	return t.rules
}

func (t *TestBackend) GetPendingRules() opera.Rules {
	return t.pendingRules
}

func (t *TestBackend) createBlocks(
	numBlocks int,
	duration inter.Timestamp,
	gasUsed uint64,
) []*evmcore.EvmBlock {
	blocks := make([]*evmcore.EvmBlock, 0, 10)
	for i := 0; i < numBlocks; i++ {
		blocks = append(blocks, &evmcore.EvmBlock{
			EvmHeader: evmcore.EvmHeader{
				Time:     t.blockTime,
				GasLimit: 1000000,
				GasUsed:  gasUsed,
				Number:   big.NewInt(int64(t.blockNumber)),
			},
			Transactions: []*types.Transaction{},
		})
		t.blockNumber++
		t.blockTime += duration
	}
	return blocks
}

func advanceBlock(gpo *Oracle, block *evmcore.EvmBlock, maxPrio *big.Int) {
	_, value := gpo.GetBaseFee()
	block.BaseFee = value
	gasFee := new(big.Int).Add(value, maxPrio)

	block.Transactions = []*types.Transaction{
		types.NewTx(&types.DynamicFeeTx{
			GasTipCap: maxPrio,
			GasFeeCap: gasFee,
			Gas:       block.GasUsed / uint64(4),
		}),
		types.NewTx(&types.DynamicFeeTx{
			GasTipCap: new(big.Int).Add(maxPrio, common.Big1),
			GasFeeCap: new(big.Int).Add(gasFee, common.Big1),
			Gas:       block.GasUsed / uint64(4),
		}),
		types.NewTx(&types.DynamicFeeTx{
			GasTipCap: new(big.Int).Add(maxPrio, common.Big3),
			GasFeeCap: new(big.Int).Add(gasFee, common.Big3),
			Gas:       block.GasUsed / uint64(4),
		}),
		types.NewTx(&types.DynamicFeeTx{
			GasTipCap: new(big.Int).Add(maxPrio, common.Big2),
			GasFeeCap: new(big.Int).Add(gasFee, common.Big2),
			Gas:       block.GasUsed / uint64(4),
		}),
	}
}

func newTestBackend() *TestBackend {
	return &TestBackend{
		blockNumber:       1,
		blockTime:         inter.Timestamp(1 * time.Hour),
		totalGasPowerLeft: 0,
		rules:             opera.FakeNetRules(),
		pendingRules:      opera.FakeNetRules(),
	}
}

func TestFees(t *testing.T) {
	backend := newTestBackend()
	gpo := NewOracle(backend, nil, Config{})
	lowPrio := backend.rules.Economy.MinGasTip
	highPrio := new(big.Int).Mul(lowPrio, common.Big2)

	// Run 5 blocks with high gas, this should increase BaseFee
	blocks := backend.createBlocks(5, inter.Timestamp(10*time.Second), 2000000)

	for _, block := range blocks {
		// Aply current base fee
		advanceBlock(gpo, block, highPrio)
		gpo.handleHeadEvent(block, true)
	}
	head, value := gpo.GetBaseFee()
	prioFee := gpo.SuggestTipCap()

	require.Equal(t, uint64(0x124f33749), value.Uint64())
	require.Equal(t, uint64(5), uint64(head))
	require.Equal(t, backend.rules.Economy.MinGasTip, prioFee)
}
