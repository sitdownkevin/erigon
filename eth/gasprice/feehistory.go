// Copyright 2021 The go-ethereum Authors
// (original work)
// Copyright 2024 The Erigon Authors
// (modifications)
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package gasprice

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"sync/atomic"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/execution/consensus/misc"
	"github.com/erigontech/erigon/rpc"
)

var (
	ErrInvalidPercentile = errors.New("invalid reward percentile")
	ErrRequestBeyondHead = errors.New("request beyond head block")
)

const (
	// maxFeeHistory is the maximum number of blocks that can be retrieved for a
	// fee history request.
	maxFeeHistory = 1024
	// maxQueryLimit is the max number of requested percentiles.
	maxQueryLimit = 100
)

// blockFees represents a single block for processing
type blockFees struct {
	// set by the caller
	blockNumber uint64
	header      *types.Header
	block       *types.Block // only set if reward percentiles are requested
	receipts    types.Receipts
	// filled by processBlock
	reward                       []*big.Int
	baseFee, nextBaseFee         *big.Int
	blobBaseFee, nextBlobBaseFee *big.Int
	gasUsedRatio                 float64
	blobGasUsedRatio             float64
	err                          error
}

// txGasAndReward is sorted in ascending order based on reward
type (
	txGasAndReward struct {
		gasUsed uint64
		reward  *big.Int
	}
	sortGasAndReward []txGasAndReward
)

func (s sortGasAndReward) Len() int { return len(s) }
func (s sortGasAndReward) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}
func (s sortGasAndReward) Less(i, j int) bool {
	return s[i].reward.Cmp(s[j].reward) < 0
}

// processBlock takes a blockFees structure with the blockNumber, the header and optionally
// the block field filled in, retrieves the block from the backend if not present yet and
// fills in the rest of the fields.
func (oracle *Oracle) processBlock(bf *blockFees, percentiles []float64) {
	chainconfig := oracle.backend.ChainConfig()
	if bf.baseFee = bf.header.BaseFee; bf.baseFee == nil {
		bf.baseFee = new(big.Int)
	}
	if chainconfig.IsLondon(bf.blockNumber + 1) {
		bf.nextBaseFee = misc.CalcBaseFee(chainconfig, bf.header)
	} else {
		bf.nextBaseFee = new(big.Int)
	}

	// Fill in blob base fee and next blob base fee.
	if excessBlobGas := bf.header.ExcessBlobGas; excessBlobGas != nil {
		blobBaseFee256, err := misc.GetBlobGasPrice(chainconfig, *excessBlobGas, bf.header.Time)
		if err != nil {
			bf.err = err
			return
		}
		nextBlockTime := bf.header.Time + chainconfig.SecondsPerSlot()
		nextBlobBaseFee256, err := misc.GetBlobGasPrice(chainconfig, misc.CalcExcessBlobGas(chainconfig, bf.header, nextBlockTime), nextBlockTime)
		if err != nil {
			bf.err = err
			return
		}
		bf.blobBaseFee = blobBaseFee256.ToBig()
		bf.nextBlobBaseFee = nextBlobBaseFee256.ToBig()

	} else {
		bf.blobBaseFee = new(big.Int)
		bf.nextBlobBaseFee = new(big.Int)
	}
	bf.gasUsedRatio = float64(bf.header.GasUsed) / float64(bf.header.GasLimit)

	if blobGasUsed := bf.header.BlobGasUsed; blobGasUsed != nil && chainconfig.GetMaxBlobGasPerBlock(bf.header.Time) != 0 {
		bf.blobGasUsedRatio = float64(*blobGasUsed) / float64(chainconfig.GetMaxBlobGasPerBlock(bf.header.Time))
	}

	if len(percentiles) == 0 {
		// rewards were not requested, return null
		return
	}

	if bf.block == nil || (bf.receipts == nil && len(bf.block.Transactions()) != 0) {
		oracle.log.Error("Block or receipts are missing while reward percentiles are requested")
		return
	}

	bf.reward = make([]*big.Int, len(percentiles))
	if len(bf.block.Transactions()) == 0 {
		// return an all zero row if there are no transactions to gather data from
		for i := range bf.reward {
			bf.reward[i] = new(big.Int)
		}
		return
	}

	sorter := make(sortGasAndReward, len(bf.block.Transactions()))
	baseFee := uint256.NewInt(0)
	if bf.block.BaseFee() != nil {
		baseFee.SetFromBig(bf.block.BaseFee())
	}
	for i, txn := range bf.block.Transactions() {
		reward := txn.GetEffectiveGasTip(baseFee)
		sorter[i] = txGasAndReward{gasUsed: bf.receipts[i].GasUsed, reward: reward.ToBig()}
	}
	sort.Sort(sorter)

	var txIndex int
	sumGasUsed := sorter[0].gasUsed

	for i, p := range percentiles {
		thresholdGasUsed := uint64(float64(bf.block.GasUsed()) * p / 100)
		for sumGasUsed < thresholdGasUsed && txIndex < len(bf.block.Transactions())-1 {
			txIndex++
			sumGasUsed += sorter[txIndex].gasUsed
		}
		bf.reward[i] = sorter[txIndex].reward
	}
}

// resolveBlockRange resolves the specified block range to absolute block numbers while also
// enforcing backend specific limitations. The pending block and corresponding receipts are
// also returned if requested and available.
// Note: an error is only returned if retrieving the head header has failed. If there are no
// retrievable blocks in the specified range then zero block count is returned with no error.
func (oracle *Oracle) resolveBlockRange(ctx context.Context, lastBlock rpc.BlockNumber, blocks, maxHistory int) (*types.Block, []*types.Receipt, uint64, int, error) {
	var (
		headBlock       rpc.BlockNumber
		pendingBlock    *types.Block
		pendingReceipts types.Receipts
	)
	// query either pending block or head header and set headBlock
	if lastBlock == rpc.PendingBlockNumber {
		if pendingBlock, pendingReceipts = oracle.backend.PendingBlockAndReceipts(); pendingBlock != nil {
			lastBlock = rpc.BlockNumber(pendingBlock.NumberU64())
			headBlock = lastBlock - 1
		} else {
			// pending block not supported by backend, process until latest block
			lastBlock = rpc.LatestBlockNumber
			blocks--
			if blocks == 0 {
				return nil, nil, 0, 0, nil
			}
		}
	}
	if pendingBlock == nil {
		// if pending block is not fetched then we retrieve the head header to get the head block number
		if latestHeader, err := oracle.backend.HeaderByNumber(ctx, rpc.LatestBlockNumber); err == nil {
			headBlock = rpc.BlockNumber(latestHeader.Number.Uint64())
		} else {
			return nil, nil, 0, 0, err
		}
	}
	if lastBlock == rpc.LatestBlockNumber {
		lastBlock = headBlock
	} else if pendingBlock == nil && lastBlock > headBlock {
		return nil, nil, 0, 0, fmt.Errorf("%w: requested %d, head %d", ErrRequestBeyondHead, lastBlock, headBlock)
	}
	if maxHistory != 0 {
		// limit retrieval to the given number of latest blocks
		if tooOldCount := int64(headBlock) - int64(maxHistory) - int64(lastBlock) + int64(blocks); tooOldCount > 0 {
			// tooOldCount is the number of requested blocks that are too old to be served
			if int64(blocks) > tooOldCount {
				blocks -= int(tooOldCount)
			} else {
				return nil, nil, 0, 0, nil
			}
		}
	}
	// ensure not trying to retrieve before genesis
	if rpc.BlockNumber(blocks) > lastBlock+1 {
		blocks = int(lastBlock + 1)
	}
	return pendingBlock, pendingReceipts, uint64(lastBlock), blocks, nil
}

// FeeHistory returns data relevant for fee estimation based on the specified range of blocks.
// The range can be specified either with absolute block numbers or ending with the latest
// or pending block. Backends may or may not support gathering data from the pending block
// or blocks older than a certain age (specified in maxHistory). The first block of the
// actually processed range is returned to avoid ambiguity when parts of the requested range
// are not available or when the head has changed during processing this request.
// Three arrays are returned based on the processed blocks:
//   - reward: the requested percentiles of effective priority fees per gas of transactions in each
//     block, sorted in ascending order and weighted by gas used.
//   - baseFee: base fee per gas in the given block
//   - gasUsedRatio: gasUsed/gasLimit in the given block
//
// Note: baseFee includes the next block after the newest of the returned range, because this
// value can be derived from the newest block.
func (oracle *Oracle) FeeHistory(ctx context.Context, blocks int, unresolvedLastBlock rpc.BlockNumber, rewardPercentiles []float64) (*big.Int, [][]*big.Int, []*big.Int, []float64, []*big.Int, []float64, error) {
	if blocks < 1 {
		return common.Big0, nil, nil, nil, nil, nil, nil // returning with no data and no error means there are no retrievable blocks
	}
	if blocks > maxFeeHistory {
		oracle.log.Warn("Sanitizing fee history length", "requested", blocks, "truncated", maxFeeHistory)
		blocks = maxFeeHistory
	}
	if len(rewardPercentiles) > maxQueryLimit {
		return common.Big0, nil, nil, nil, nil, nil, fmt.Errorf("%w: over the query limit %d", ErrInvalidPercentile, maxQueryLimit)
	}
	for i, p := range rewardPercentiles {
		if p < 0 || p > 100 {
			return common.Big0, nil, nil, nil, nil, nil, fmt.Errorf("%w: %f", ErrInvalidPercentile, p)
		}
		if i > 0 && p <= rewardPercentiles[i-1] {
			return common.Big0, nil, nil, nil, nil, nil, fmt.Errorf("%w: #%d:%f >= #%d:%f", ErrInvalidPercentile, i-1, rewardPercentiles[i-1], i, p)
		}
	}
	// Only process blocks if reward percentiles were requested
	maxHistory := oracle.maxHeaderHistory
	if len(rewardPercentiles) != 0 {
		maxHistory = oracle.maxBlockHistory
	}
	var (
		pendingBlock    *types.Block
		pendingReceipts []*types.Receipt
		err             error
	)
	pendingBlock, pendingReceipts, lastBlock, blocks, err := oracle.resolveBlockRange(ctx, unresolvedLastBlock, blocks, maxHistory)
	if err != nil || blocks == 0 {
		return common.Big0, nil, nil, nil, nil, nil, err
	}
	oldestBlock := lastBlock + 1 - uint64(blocks)

	var (
		next = oldestBlock
	)
	var (
		reward           = make([][]*big.Int, blocks)
		baseFee          = make([]*big.Int, blocks+1)
		gasUsedRatio     = make([]float64, blocks)
		blobGasUsedRatio = make([]float64, blocks)
		blobBaseFee      = make([]*big.Int, blocks+1)
		firstMissing     = blocks
	)
	for ; blocks > 0; blocks-- {
		if err = common.Stopped(ctx.Done()); err != nil {
			return common.Big0, nil, nil, nil, nil, nil, err
		}
		// Retrieve the next block number to fetch with this goroutine
		blockNumber := atomic.AddUint64(&next, 1) - 1
		if blockNumber > lastBlock {
			continue
		}

		fees := &blockFees{blockNumber: blockNumber}
		if pendingBlock != nil && blockNumber >= pendingBlock.NumberU64() {
			fees.block, fees.receipts = pendingBlock, pendingReceipts
		} else {
			if len(rewardPercentiles) != 0 {
				fees.block, fees.err = oracle.backend.BlockByNumber(ctx, rpc.BlockNumber(blockNumber))
				if fees.block != nil && fees.err == nil {
					fees.receipts, fees.err = oracle.backend.GetReceiptsGasUsed(ctx, fees.block)
				}
			} else {
				fees.header, fees.err = oracle.backend.HeaderByNumber(ctx, rpc.BlockNumber(blockNumber))
			}
		}
		if fees.block != nil {
			fees.header = fees.block.Header()
		}
		if fees.header != nil {
			oracle.processBlock(fees, rewardPercentiles)
		}

		if fees.err != nil {
			return common.Big0, nil, nil, nil, nil, nil, fees.err
		}
		i := int(fees.blockNumber - oldestBlock)
		if fees.header != nil {
			reward[i], baseFee[i], baseFee[i+1], gasUsedRatio[i] = fees.reward, fees.baseFee, fees.nextBaseFee, fees.gasUsedRatio
			blobGasUsedRatio[i], blobBaseFee[i], blobBaseFee[i+1] = fees.blobGasUsedRatio, fees.blobBaseFee, fees.nextBlobBaseFee
		} else {
			// getting no block and no error means we are requesting into the future (might happen because of a reorg)
			if i < firstMissing {
				firstMissing = i
			}
		}
	}
	if firstMissing == 0 {
		return common.Big0, nil, nil, nil, nil, nil, nil
	}
	if len(rewardPercentiles) != 0 {
		reward = reward[:firstMissing]
	} else {
		reward = nil
	}
	baseFee, gasUsedRatio = baseFee[:firstMissing+1], gasUsedRatio[:firstMissing]
	return new(big.Int).SetUint64(oldestBlock), reward, baseFee, gasUsedRatio, blobBaseFee, blobGasUsedRatio, nil
}
