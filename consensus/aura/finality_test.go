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

package aura

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	libcommon "github.com/erigontech/erigon-lib/common"
)

func TestRollingFinality(t *testing.T) {
	t.Run("RejectsUnknownSigners", func(t *testing.T) {
		f := NewRollingFinality([]libcommon.Address{{1}, {2}, {3}})
		_, err := f.push(libcommon.Hash{}, 0, []libcommon.Address{{0}, {4}})
		assert.Error(t, err)
		_, err = f.push(libcommon.Hash{}, 0, []libcommon.Address{{0}, {1}, {4}})
		assert.Error(t, err)
	})
	t.Run("FinalizeMultiple", func(t *testing.T) {
		signers := []libcommon.Address{{0}, {1}, {2}, {3}, {4}, {5}}
		f := NewRollingFinality(signers)
		// 3 / 6 signers is < 51% so no finality.
		for i := 0; i < 6; i++ {
			l, err := f.push(libcommon.Hash{byte(i)}, uint64(i%3), []libcommon.Address{signers[i%3]})
			require.NoError(t, err)
			assert.Equal(t, 0, len(l))
		}
		// after pushing a block signed by a fourth validator, the first four
		// blocks of the unverified chain become verified.
		l, err := f.push(libcommon.Hash{byte(6)}, 6, []libcommon.Address{signers[4]})
		require.NoError(t, err)
		for i := uint64(0); i < 4; i++ {
			assert.Equal(t, libcommon.Hash{byte(i)}, l[i].hash)
		}
		assert.Equal(t, 4, len(l))
	})
	t.Run("FromAncestry", func(t *testing.T) {
		signers := []libcommon.Address{{0}, {1}, {2}, {3}, {4}, {5}}
		f := NewRollingFinality(signers)
		i := 12
		get := func(hash libcommon.Hash) ([]libcommon.Address, libcommon.Hash, libcommon.Hash, uint64, bool) {
			i--
			if i == -1 {
				return nil, libcommon.Hash{}, libcommon.Hash{}, 0, false
			}
			return []libcommon.Address{signers[i%6]}, libcommon.Hash{byte(i)}, libcommon.Hash{byte(i - 1)}, uint64(i), true
		}
		err := f.buildAncestrySubChain(get, libcommon.Hash{11}, libcommon.Hash{99})
		require.NoError(t, err)
		assert.Equal(t, 3, f.headers.l.Len())
		assert.Equal(t, libcommon.Hash{11}, *f.lastPushed)
	})
	t.Run("FromAncestryMultipleSigners", func(t *testing.T) {
		signers := []libcommon.Address{{0}, {1}, {2}, {3}, {4}, {5}}
		f := NewRollingFinality(signers)
		i := 12
		get := func(hash libcommon.Hash) ([]libcommon.Address, libcommon.Hash, libcommon.Hash, uint64, bool) {
			i--
			if i == -1 {
				return nil, libcommon.Hash{}, libcommon.Hash{}, 0, false
			}
			return []libcommon.Address{signers[i%6], signers[(i+1)%6], signers[(i+2)%6]}, libcommon.Hash{byte(i)}, libcommon.Hash{byte(i - 1)}, uint64(i), true
		}
		err := f.buildAncestrySubChain(get, libcommon.Hash{11}, libcommon.Hash{99})
		require.NoError(t, err)

		// only the last hash has < 51% of authorities' signatures
		assert.Equal(t, 1, f.headers.l.Len())
		assert.Equal(t, libcommon.Hash{11}, f.headers.Front().hash)
		assert.Equal(t, libcommon.Hash{11}, *f.lastPushed)
	})

}
