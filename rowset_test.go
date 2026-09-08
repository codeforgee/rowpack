package rowpack

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRowIDSetBasics(t *testing.T) {
	var s rowIDSet
	require.False(t, s.Contains(1), "empty set contains nothing")
	s.Insert(1)
	require.True(t, s.Contains(1))
	require.False(t, s.Contains(2))
	s.Insert(2)
	require.True(t, s.Contains(1))
	require.True(t, s.Contains(2))
	require.Equal(t, 2, s.Len())
}

// Rehash must preserve every entry across many growth cycles, including
// sequential (worst-case-clustered without hashing) and sparse RowIDs.
func TestRowIDSetRehashIntegrity(t *testing.T) {
	var s rowIDSet
	const n = 200_000
	for i := uint64(1); i <= n; i++ {
		require.False(t, s.Contains(i))
		s.Insert(i)
	}
	require.Equal(t, n, s.Len())
	for i := uint64(1); i <= n; i++ {
		require.True(t, s.Contains(i), "lost row %d after rehashes", i)
	}
	require.False(t, s.Contains(n+1))

	// Sparse IDs (multiplied) exercise hash dispersion.
	var sp rowIDSet
	for i := uint64(1); i <= 50_000; i++ {
		sp.Insert(i * 0x9E3779B97F4A7C15)
	}
	for i := uint64(1); i <= 50_000; i++ {
		require.True(t, sp.Contains(i*0x9E3779B97F4A7C15))
	}
}

// Duplicate detection must be exact: Insert is only called on absent values,
// but the set must still report collisions the writer would have hit via
// Contains.
func TestRowIDSetDuplicateDetection(t *testing.T) {
	var s rowIDSet
	for i := uint64(1); i <= 10_000; i++ {
		require.False(t, s.Contains(i))
		s.Insert(i)
		require.True(t, s.Contains(i), "row %d not found right after insert", i)
	}
}

// The packed set must stay small: 1M entries should fit in well under the
// ~90 MB a Go map would take (~11 B/entry at the 0.7 load factor, 2x slack
// from the last rehash).
func TestRowIDSetFootprint(t *testing.T) {
	var s rowIDSet
	const n = 1_000_000
	for i := uint64(1); i <= n; i++ {
		s.Insert(i)
	}
	// slots is a power of two >= n/0.7; bytes = 8 * len(slots).
	require.Less(t, len(s.slots), 4*n, "slot array unreasonably large")
	t.Logf("1M rows: slots=%d (%.1f MB), load=%.2f",
		len(s.slots), float64(len(s.slots)*8)/(1<<20), float64(s.Len())/float64(len(s.slots)))
}
