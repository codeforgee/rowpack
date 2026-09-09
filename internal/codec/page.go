package codec

import (
	"github.com/rowpack/rowpack/internal/fileformat"
)

// PageRecord is one decoded Rows Page record (v2 page layout): identity and
// metadata from the page streams plus a view of the body-only TypedTuple.
// Body aliases the page buffer and is empty for deletes; callers decode it
// against the record's schema version via DecodeBodyInto and must not retain
// it beyond the page's lifetime.
type PageRecord struct {
	RowID         uint64
	SchemaVersion uint32
	ChangeType    fileformat.ChangeType
	Body          []byte
}
