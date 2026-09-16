package rowpack

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/index"
)

// Sentinel errors. All public APIs return errors that support errors.Is against
// these values. Errors must not be classified by string matching.
var (
	ErrNotFound           = errors.New("rowpack: not found")
	ErrAlreadyExists      = errors.New("rowpack: already exists")
	ErrInvalidPath        = errors.New("rowpack: invalid path")
	ErrInvalidArgument    = errors.New("rowpack: invalid argument")
	ErrReadOnly           = errors.New("rowpack: read only")
	ErrWriterBusy         = errors.New("rowpack: writer busy")
	ErrSnapshotCommitted  = errors.New("rowpack: snapshot committed")
	ErrSnapshotAborted    = errors.New("rowpack: snapshot aborted")
	ErrSnapshotFailed     = errors.New("rowpack: snapshot failed")
	ErrInvalidParent      = errors.New("rowpack: invalid parent snapshot")
	ErrSchemaMismatch     = codec.ErrSchemaMismatch
	ErrSchemaConflict     = errors.New("rowpack: schema conflict")
	ErrCorruptData        = errors.New("rowpack: corrupt data")
	ErrCorruptIndex       = errors.New("rowpack: corrupt index")
	ErrVersionUnsupported = errors.New("rowpack: unsupported version")
	ErrStoreMismatch      = errors.New("rowpack: store files do not match")
	ErrClosed             = errors.New("rowpack: closed")

	// ErrMustReopen reports that an earlier commit failed with an unknown
	// outcome (the failure happened at or after the durability sync, so the
	// snapshot may or may not be durably committed). The in-memory view can
	// no longer be trusted for writes: the store refuses new writers with
	// this error until it is closed and reopened, letting recovery align the
	// view with the file. Reads remain allowed and stay self-consistent (they
	// see the last fully published state).
	ErrMustReopen = errors.New("rowpack: store must be reopened after unknown-outcome commit failure")

	// Encryption errors.
	ErrKeyRequired    = errors.New("rowpack: encryption key required")
	ErrKeyUnavailable = errors.New("rowpack: encryption key unavailable")
	ErrKeyIDNotFound  = errors.New("rowpack: encryption key id not found")
	ErrAuthFailed     = errors.New("rowpack: block authentication failed")
)

// CorruptionError describes a structured integrity failure with the file type,
// byte offset and logical object identifiers available at detection time.
// Unwrap returns ErrCorruptData or ErrCorruptIndex plus, when set, the
// underlying Cause (e.g. ErrAuthFailed for a failed AEAD authentication).
type CorruptionError struct {
	File       string
	Offset     int64
	SnapshotID SnapshotID
	TableID    TableID
	BlockID    uint64
	Kind       error
	Cause      error // underlying error kept on the chain (optional)
	Reason     string
}

// Error implements error.
func (e *CorruptionError) Error() string {
	return fmt.Sprintf("rowpack: %s: file=%s offset=%d snapshot=%d table=%d block=%d: %s",
		e.Kind, e.File, e.Offset, e.SnapshotID, e.TableID, e.BlockID, e.Reason)
}

// Unwrap returns the error kind and, when present, the underlying cause so
// errors.Is can match both ErrCorruptData and e.g. ErrAuthFailed.
func (e *CorruptionError) Unwrap() []error {
	if e.Cause != nil {
		return []error{e.Kind, e.Cause}
	}
	return []error{e.Kind}
}

// corruptError classifies an integrity failure into the structured
// CorruptionError that public APIs promise (every read failure must be
// matchable with errors.Is, never by string). Causes that already carry a
// corruption/auth/version sentinel pass through unchanged so the chain is
// never double-wrapped; everything else becomes an ErrCorruptData failure with
// the original error kept on the chain, so errors.Is matches the sentinel, the
// underlying Cause (e.g. ErrSchemaMismatch) and ErrCorruptData at once.
func corruptError(file string, offset int64, snap SnapshotID, table TableID, blockID uint64, cause error) error {
	if cause == nil {
		return nil
	}
	for _, sentinel := range []error{ErrCorruptData, ErrCorruptIndex, ErrAuthFailed, ErrVersionUnsupported} {
		if errors.Is(cause, sentinel) {
			return cause
		}
	}
	return &CorruptionError{
		File:       file,
		Offset:     offset,
		SnapshotID: snap,
		TableID:    table,
		BlockID:    blockID,
		Kind:       ErrCorruptData,
		Cause:      cause,
		Reason:     reasonOf(cause),
	}
}

// reasonOf renders a cause for CorruptionError.Reason. The package prefix is
// dropped (the outer error already carries it), and a loader-wrapped failure
// contributes its own Reason instead of its full message, which would repeat
// file/offset/block inside the outer error.
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	var cerr *CorruptionError
	if errors.As(err, &cerr) {
		if cerr.Reason != "" {
			return cerr.Reason
		}
		return reasonOf(cerr.Cause)
	}
	return strings.TrimPrefix(err.Error(), "rowpack: ")
}

// recordError classifies a failure hit while locating or decoding one record
// inside an already-resolved Rows block (page access, schema resolution, tuple
// decode). The loader already wraps block header/directory/AEAD failures; this
// covers everything above it, which would otherwise escape errors.Is.
// bl may be nil (failure before the block was resolved).
func (s *Store) recordError(bl *index.BlockLoc, snap SnapshotID, table TableID, cause error) error {
	if bl == nil {
		return corruptError(s.dataPath, 0, snap, table, 0, cause)
	}
	return corruptError(s.dataPath, int64(bl.DataOffset), snap, table, bl.BlockID, cause)
}

// CommitError reports a snapshot commit failure. Unknown is true when the
// failure happened after the data file sync began, so the caller cannot know
// whether the snapshot was durably committed; it must query the snapshot ID
// (by closing and reopening the store) instead of blindly replaying
// non-idempotent business logic. On an unknown failure the store also latches
// its must-reopen state: new writers are refused with ErrMustReopen until the
// store is reopened.
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
