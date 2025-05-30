// Copyright 2024 The Erigon Authors
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

package jsonrpc

import (
	"bytes"
	"testing"

	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/common/hexutil"

	"github.com/RoaringBitmap/roaring/v2/roaring64"
	"github.com/erigontech/erigon/cmd/rpcdaemon/rpcdaemontest"
	"github.com/stretchr/testify/require"
)

func newMockBackwardChunkLocator(chunks [][]byte) ChunkLocator {
	return func(block uint64) (ChunkProvider, bool, error) {
		for i, v := range chunks {
			bm := roaring64.NewBitmap()
			if _, err := bm.ReadFrom(bytes.NewReader(v)); err != nil {
				return nil, false, err
			}
			if block > bm.Maximum() {
				continue
			}

			return newMockBackwardChunkProvider(chunks[:i+1]), true, nil
		}

		// Not found; return the last to simulate the behavior of returning
		// everything up to the 0xffff... chunk
		if len(chunks) > 0 {
			return newMockBackwardChunkProvider(chunks), true, nil
		}

		return nil, true, nil
	}
}

func newMockBackwardChunkProvider(chunks [][]byte) ChunkProvider {
	i := len(chunks) - 1
	return func() ([]byte, bool, error) {
		if i < 0 {
			return nil, false, nil
		}

		chunk := chunks[i]
		i--
		return chunk, true, nil
	}
}
func TestBackwardBlockProviderWith1Chunk(t *testing.T) {
	// Mocks 1 chunk
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 0)

	checkNext(t, blockProvider, 1010, true)
	checkNext(t, blockProvider, 1005, true)
	checkNext(t, blockProvider, 1000, false)
}

func TestBackwardBlockProviderWith1ChunkMiddleBlock(t *testing.T) {
	// Mocks 1 chunk
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 1005)

	checkNext(t, blockProvider, 1005, true)
	checkNext(t, blockProvider, 1000, false)
}

func TestBackwardBlockProviderWith1ChunkNotExactBlock(t *testing.T) {
	// Mocks 1 chunk
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 1003)

	checkNext(t, blockProvider, 1000, false)
}

func TestBackwardBlockProviderWith1ChunkLastBlock(t *testing.T) {
	// Mocks 1 chunk
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 1000)

	checkNext(t, blockProvider, 1000, false)
}

func TestBackwardBlockProviderWith1ChunkBlockNotFound(t *testing.T) {
	// Mocks 1 chunk
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 900)

	checkNext(t, blockProvider, 0, false)
}

func TestBackwardBlockProviderWithNoChunks(t *testing.T) {
	chunkLocator := newMockBackwardChunkLocator([][]byte{})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 0)

	checkNext(t, blockProvider, 0, false)
}

func TestBackwardBlockProviderWithMultipleChunks(t *testing.T) {
	// Mocks 2 chunks
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})
	chunk2 := createBitmap(t, []uint64{1501, 1600})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1, chunk2})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 0)

	checkNext(t, blockProvider, 1600, true)
	checkNext(t, blockProvider, 1501, true)
	checkNext(t, blockProvider, 1010, true)
	checkNext(t, blockProvider, 1005, true)
	checkNext(t, blockProvider, 1000, false)
}

func TestBackwardBlockProviderWithMultipleChunksBlockBetweenChunks(t *testing.T) {
	// Mocks 2 chunks
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})
	chunk2 := createBitmap(t, []uint64{1501, 1600})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1, chunk2})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 1500)

	checkNext(t, blockProvider, 1010, true)
	checkNext(t, blockProvider, 1005, true)
	checkNext(t, blockProvider, 1000, false)
}

func TestBackwardBlockProviderWithMultipleChunksBlockNotFound(t *testing.T) {
	// Mocks 2 chunks
	chunk1 := createBitmap(t, []uint64{1000, 1005, 1010})
	chunk2 := createBitmap(t, []uint64{1501, 1600})

	chunkLocator := newMockBackwardChunkLocator([][]byte{chunk1, chunk2})
	blockProvider := NewBackwardBlockProvider(chunkLocator, 900)

	checkNext(t, blockProvider, 0, false)
}

func TestSearchTransactionsBefore(t *testing.T) {
	m, _, _ := rpcdaemontest.CreateTestSentry(t)
	api := NewOtterscanAPI(newBaseApiForTest(m), m.DB, 25)

	addr := common.HexToAddress("0x537e697c7ab75a26f9ecf0ce810e3154dfcaaf44")
	t.Run("small page size", func(t *testing.T) {
		require := require.New(t)
		results, err := api.SearchTransactionsBefore(m.Ctx, addr, 10, 2)
		require.NoError(err)
		require.False(results.FirstPage)
		require.False(results.LastPage)
		require.Len(results.Txs, 2)
		require.Len(results.Receipts, 2)
	})
	t.Run("big page size", func(t *testing.T) {
		require := require.New(t)
		results, err := api.SearchTransactionsBefore(m.Ctx, addr, 10, 10)
		require.NoError(err)
		require.False(results.FirstPage)
		require.True(results.LastPage)
		require.Len(results.Txs, 3)
		require.Len(results.Receipts, 3)
	})
	t.Run("filter last block", func(t *testing.T) {
		require := require.New(t)
		results, err := api.SearchTransactionsBefore(m.Ctx, addr, 5, 10)

		require.NoError(err)
		require.False(results.FirstPage)
		require.True(results.LastPage)
		require.Len(results.Txs, 2)
		require.Len(results.Receipts, 2)

		require.Equal(4, int(results.Txs[0].BlockNumber.ToInt().Uint64()))
		require.Equal(0, int(results.Txs[0].Nonce))
		require.Equal(4, int(results.Receipts[0]["blockNumber"].(hexutil.Uint64)))
		require.Equal(common.HexToHash("0x79491e16fd1b1ceea44c46af850b2ef121683055cd579fd4d877beba22e77c1c"), results.Receipts[0]["transactionHash"].(common.Hash))
		require.Equal(common.HexToAddress("0x0D3ab14BBaD3D99F4203bd7a11aCB94882050E7e"), results.Receipts[0]["from"].(common.Address))
		require.Equal(addr, *results.Receipts[0]["to"].(*common.Address))

		require.Equal(3, int(results.Txs[1].BlockNumber.ToInt().Uint64()))
		require.Equal(2, int(results.Txs[1].Nonce))
		require.Equal(3, int(results.Receipts[1]["blockNumber"].(hexutil.Uint64)))
		require.Equal(common.HexToHash("0x6e25f89e24254ba3eb460291393a4715fd3c33d805334cbd05c1b2efe1080f18"), results.Receipts[1]["transactionHash"].(common.Hash))
		require.Equal(common.HexToAddress("0x71562b71999873DB5b286dF957af199Ec94617F7"), results.Receipts[1]["from"].(common.Address))
		require.Nil(results.Receipts[1]["to"].(*common.Address))
	})
}
