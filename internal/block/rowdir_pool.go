package block

import (
	"sync"

	"github.com/codeforgee/rowpack/internal/format"
)

// rowDirPool recycles RowsBuilder directory slices across commits. Flush
// transfers FlushedBlock.Rows to the caller, which retains it until the
// commit's index build has consumed the entries; only then is the backing
// array dead and safe to return. Without the pool every block's directory
// re-walks the append-growth chain from zero (doubling copies plus clears).
//
// sync.Pool (not a retained free-list): a GC cycle dropping the pool merely
// reverts to the old grow-from-zero behavior, never to unbounded retention.
var rowDirPool sync.Pool

// maxRowDirPoolCap bounds a recyclable directory: bigger slices are dropped
// so an oversized block cannot pin its directory forever. 64Ki entries is
// 1 MiB of RowDirectoryEntry — well above any default block's record count.
const maxRowDirPoolCap = 1 << 16

// getRowDirBuf returns an empty directory slice, reusing a pooled backing
// array when one is idle.
func getRowDirBuf() []format.RowDirectoryEntry {
	if v, ok := rowDirPool.Get().(*[]format.RowDirectoryEntry); ok && v != nil {
		buf := *v
		*v = nil
		return buf[:0]
	}
	return nil
}

// PutRowDirBuf returns a dead directory slice to the pool. The caller must
// guarantee no reader of the slice remains. Oversized and zero-capacity
// slices are dropped.
func PutRowDirBuf(s []format.RowDirectoryEntry) {
	if cap(s) == 0 || cap(s) > maxRowDirPoolCap {
		return
	}
	buf := s[:0]
	rowDirPool.Put(&buf)
}
