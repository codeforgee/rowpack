package index

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"testing"
	"time"

	"github.com/codeforgee/rowpack/internal/format"
)

// peakHeap runs fn with the GC enabled and samples HeapAlloc from a
// background goroutine, returning the maximum heap seen minus the baseline:
// this captures RETENTION (pinned buffers) rather than cumulative churn,
// which is what the fold-and-drop CRC path changes.
func peakHeap(t *testing.T, fn func()) uint64 {
	t.Helper()
	// Tight GC target: with the default GOGC the heap peak is dominated by
	// pacing (~2x live set), which masks the retention delta this probe
	// measures. GOGC=20 keeps the heap near the live set.
	old := debug.SetGCPercent(20)
	defer debug.SetGCPercent(old)
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	stop := make(chan struct{})
	done := make(chan uint64)
	go func() {
		var peak uint64
		tick := time.NewTicker(200 * time.Microsecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				done <- peak
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak {
					peak = m.HeapAlloc
				}
			}
		}
	}()
	fn()
	close(stop)
	peak := <-done
	if peak < before.HeapAlloc {
		peak = before.HeapAlloc
	}
	return peak - before.HeapAlloc
}

// TestBuildStoredBodyPeak measures the peak heap of the body build itself
// (builder state excluded). It is a measurement probe for memory work, not a
// correctness test.
func TestBuildStoredBodyPeak(t *testing.T) {
	if testing.Short() {
		t.Skip("peak probe: long")
	}
	for _, rows := range []int{200_000, 1_000_000} {
		rows := rows
		t.Run(fmt.Sprintf("rows=%d", rows), func(t *testing.T) {
			b := NewBuilder(7)
			b.SetRowDedup(false)
			_ = b.SetSnapshot(format.SnapshotIndexEntry{
				SnapshotID:      7,
				SnapshotType:    format.SnapshotFull,
				BlockCount:      uint32(rows / 1000),
				RowRecordCount:  uint64(rows),
				CreatedUnixNano: 1,
			})
			// Metadata + block entries: these are the chunk streams whose
			// plaintext the old rawParts pinned until the final CRC pass.
			for i := 0; i < rows/1000; i++ {
				_ = b.AddMetadata(format.MetadataIndexEntry{
					SnapshotID: 7,
					ObjectID:   uint64(i),
					Revision:   1,
					RecordType: 1,
					Operation:  format.OperationUpsert,
				})
				_ = b.AddBlock(format.BlockIndexEntry{
					BlockID:    uint64(i),
					SnapshotID: 7,
					TableID:    uint32(i % 4),
					BlockKind:  format.BlockKindRows,
					RawSize:    64 << 10,
					StoredSize: 32 << 10,
					ItemCount:  1000,
				})
			}
			for i := 0; i < rows; i++ {
				_ = b.AddRow(format.RowIndexEntry{
					SnapshotID:  7,
					TableID:     uint32(i % 4),
					ChangeType:  format.ChangeInsert,
					RowID:       uint64(i),
					BlockID:     uint64(i / 1000),
					ItemOrdinal: uint32(i % 1000),
				})
			}
			var bodyLen int
			peak := peakHeap(t, func() {
				body, _, _, err := b.BuildStoredBody(nil, 3, nil)
				if err != nil {
					t.Fatal(err)
				}
				bodyLen = len(body)
			})
			t.Logf("rows=%d body=%d B peak-heap=%.1f MB (%.2f B/row)", rows, bodyLen, float64(peak)/(1<<20), float64(peak)/float64(rows))
		})
	}
}
