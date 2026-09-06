package rowpack

import (
	"errors"
	"fmt"
)

// Sentinel errors. All public APIs return errors that support errors.Is against
// these values. Errors must not be classified by string matching.
var (
	ErrNotFound            = errors.New("rowpack: not found")
	ErrAlreadyExists       = errors.New("rowpack: already exists")
	ErrInvalidPath         = errors.New("rowpack: invalid path")
	ErrInvalidArgument     = errors.New("rowpack: invalid argument")
	ErrReadOnly            = errors.New("rowpack: read only")
	ErrWriterBusy          = errors.New("rowpack: writer busy")
	ErrConcurrentWriterUse = errors.New("rowpack: concurrent writer use")
	ErrSnapshotCommitted   = errors.New("rowpack: snapshot committed")
	ErrSnapshotAborted     = errors.New("rowpack: snapshot aborted")
	ErrSnapshotFailed      = errors.New("rowpack: snapshot failed")
	ErrInvalidParent       = errors.New("rowpack: invalid parent snapshot")
	ErrSchemaMismatch      = errors.New("rowpack: schema mismatch")
	ErrSchemaConflict      = errors.New("rowpack: schema conflict")
	ErrUnsupportedType     = errors.New("rowpack: unsupported type")
	ErrCorruptData         = errors.New("rowpack: corrupt data")
	ErrCorruptIndex        = errors.New("rowpack: corrupt index")
	ErrVersionUnsupported  = errors.New("rowpack: unsupported version")
	ErrStoreMismatch       = errors.New("rowpack: store files do not match")
	ErrMetadataInvalid     = errors.New("rowpack: invalid metadata")
	ErrMetadataUnsupported = errors.New("rowpack: unsupported metadata")
	ErrLimitExceeded       = errors.New("rowpack: limit exceeded")
	ErrClosed              = errors.New("rowpack: closed")
)

// CorruptionError describes a structured integrity failure with the file type,
// byte offset and logical object identifiers available at detection time.
// Unwrap returns ErrCorruptData or ErrCorruptIndex.
type CorruptionError struct {
	File       string
	Offset     int64
	SnapshotID SnapshotID
	TableID    TableID
	BlockID    uint64
	Kind       error
	Reason     string
}

// Error implements error.
func (e *CorruptionError) Error() string {
	return fmt.Sprintf("rowpack: %s: file=%s offset=%d snapshot=%d table=%d block=%d: %s",
		e.Kind, e.File, e.Offset, e.SnapshotID, e.TableID, e.BlockID, e.Reason)
}

// Unwrap returns the underlying error kind.
func (e *CorruptionError) Unwrap() error { return e.Kind }

// CommitError reports a snapshot commit failure. Unknown is true when the
// failure happened after the data file sync began, so the caller cannot know
// whether the snapshot was durably committed; it must query the snapshot ID
// instead of blindly replaying non-idempotent business logic.
type CommitError struct {
	SnapshotID SnapshotID
	Unknown    bool
	Err        error
}

// Error implements error.
func (e *CommitError) Error() string {
	if e.Unknown {
		return fmt.Sprintf("rowpack: commit of snapshot %d: outcome unknown: %v", e.SnapshotID, e.Err)
	}
	return fmt.Sprintf("rowpack: commit of snapshot %d: %v", e.SnapshotID, e.Err)
}

// Unwrap returns the underlying error.
func (e *CommitError) Unwrap() error { return e.Err }

// newCorruption builds a CorruptionError over ErrCorruptData.
func newCorruption(file string, offset int64, snapshot SnapshotID, table TableID, block uint64, reason string) *CorruptionError {
	return &CorruptionError{File: file, Offset: offset, SnapshotID: snapshot, TableID: table, BlockID: block, Kind: ErrCorruptData, Reason: reason}
}
