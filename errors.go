package rowpack

import (
	"errors"
	"fmt"
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
	ErrSchemaMismatch     = errors.New("rowpack: schema mismatch")
	ErrSchemaConflict     = errors.New("rowpack: schema conflict")
	ErrCorruptData        = errors.New("rowpack: corrupt data")
	ErrCorruptIndex       = errors.New("rowpack: corrupt index")
	ErrVersionUnsupported = errors.New("rowpack: unsupported version")
	ErrStoreMismatch      = errors.New("rowpack: store files do not match")
	ErrClosed             = errors.New("rowpack: closed")

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
