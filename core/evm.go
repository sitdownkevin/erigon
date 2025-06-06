// Copyright 2016 The go-ethereum Authors
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

package core

import (
	"math/big"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon-lib/chain"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/execution/consensus"
	"github.com/erigontech/erigon/execution/consensus/merge"
	"github.com/erigontech/erigon/execution/consensus/misc"
)

// NewEVMBlockContext creates a new context for use in the EVM.
func NewEVMBlockContext(header *types.Header, blockHashFunc func(n uint64) common.Hash,
	engine consensus.EngineReader, author *common.Address, config *chain.Config) evmtypes.BlockContext {
	// If we don't have an explicit author (i.e. not mining), extract from the header
	var beneficiary common.Address
	if author == nil {
		beneficiary, _ = engine.Author(header) // Ignore error, we're past header validation
	} else {
		beneficiary = *author
	}
	var baseFee uint256.Int
	if header.BaseFee != nil {
		overflow := baseFee.SetFromBig(header.BaseFee)
		if overflow {
			panic("header.BaseFee higher than 2^256-1")
		}
	}

	var prevRandDao *common.Hash
	if header.Difficulty.Cmp(merge.ProofOfStakeDifficulty) == 0 {
		// EIP-4399. We use ProofOfStakeDifficulty (i.e. 0) as a telltale of Proof-of-Stake blocks.
		prevRandDao = new(common.Hash)
		*prevRandDao = header.MixDigest
	}

	var blobBaseFee *uint256.Int
	if header.ExcessBlobGas != nil {
		var err error
		blobBaseFee, err = misc.GetBlobGasPrice(config, *header.ExcessBlobGas, header.Time)
		if err != nil {
			panic(err)
		}
	}

	var transferFunc evmtypes.TransferFunc
	var postApplyMessageFunc evmtypes.PostApplyMessageFunc
	if engine != nil {
		transferFunc = engine.GetTransferFunc()
		postApplyMessageFunc = engine.GetPostApplyMessageFunc()
	} else {
		transferFunc = consensus.Transfer
		postApplyMessageFunc = nil
	}
	return evmtypes.BlockContext{
		CanTransfer:      CanTransfer,
		Transfer:         transferFunc,
		GetHash:          blockHashFunc,
		PostApplyMessage: postApplyMessageFunc,
		Coinbase:         beneficiary,
		BlockNumber:      header.Number.Uint64(),
		Time:             header.Time,
		Difficulty:       new(big.Int).Set(header.Difficulty),
		BaseFee:          &baseFee,
		GasLimit:         header.GasLimit,
		PrevRanDao:       prevRandDao,
		BlobBaseFee:      blobBaseFee,
	}
}

// NewEVMTxContext creates a new transaction context for a single transaction.
func NewEVMTxContext(msg Message) evmtypes.TxContext {
	return evmtypes.TxContext{
		Origin:     msg.From(),
		GasPrice:   msg.GasPrice(),
		BlobHashes: msg.BlobHashes(),
	}
}

// GetHashFn returns a GetHashFunc which retrieves header hashes by number
func GetHashFn(ref *types.Header, getHeader func(hash common.Hash, number uint64) *types.Header) func(n uint64) common.Hash {
	// Cache will initially contain [refHash.parent],
	// Then fill up with [refHash.p, refHash.pp, refHash.ppp, ...]
	var cache []common.Hash

	return func(n uint64) common.Hash {
		// If there's no hash cache yet, make one
		if len(cache) == 0 {
			cache = append(cache, ref.ParentHash)
		}
		if idx := ref.Number.Uint64() - n - 1; idx < uint64(len(cache)) {
			return cache[idx]
		}
		// No luck in the cache, but we can start iterating from the last element we already know
		lastKnownHash := cache[len(cache)-1]
		lastKnownNumber := ref.Number.Uint64() - uint64(len(cache))

		for {
			header := getHeader(lastKnownHash, lastKnownNumber)
			if header == nil {
				break
			}
			cache = append(cache, header.ParentHash)
			lastKnownHash = header.ParentHash
			lastKnownNumber = header.Number.Uint64() - 1
			if n == lastKnownNumber {
				return lastKnownHash
			}
		}
		return common.Hash{}
	}
}

// CanTransfer checks whether there are enough funds in the address' account to make a transfer.
// This does not take the necessary gas in to account to make the transfer valid.
func CanTransfer(db evmtypes.IntraBlockState, addr common.Address, amount *uint256.Int) (bool, error) {
	balance, err := db.GetBalance(addr)
	if err != nil {
		return false, err
	}
	return !balance.Lt(amount), nil
}
