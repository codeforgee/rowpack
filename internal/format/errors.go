package format

import (
	"errors"
	"fmt"
	"strings"
)

// FormatError describes a fixed-structure decode/validate failure. It carries
// the structure name and, when known, the file offset. Higher layers wrap it
// with the file name into the public CorruptionError.
type FormatError struct {
	Structure string // e.g. "DataFileHeader"
	Offset    int64  // -1 when the offset is unknown
	Reason    string
}

// Error implements error.
func (e *FormatError) Error() string {
	if e.Offset >= 0 {
		return fmt.Sprintf("%s at offset %d: %s", e.Structure, e.Offset, e.Reason)
	}
	return fmt.Sprintf("%s: %s", e.Structure, e.Reason)
}

// formatError builds a FormatError.
func formatError(structure string, offset int64, format string, args ...any) *FormatError {
	return &FormatError{Structure: structure, Offset: offset, Reason: fmt.Sprintf(format, args...)}
}

// errShortInput is the shared reason for truncated fixed structures.
const errShortInput = "input too short"

// errBadMagic is the shared reason for an unknown magic.
const errBadMagic = "bad magic"

// errBadSize is the shared reason for an unexpected header size field.
const errBadSize = "bad size field"

// errBadVersion is the shared reason for an unsupported version major.
const errBadVersion = "unsupported version"

// IsVersionError reports whether err is an unsupported-version FormatError.
func IsVersionError(err error) bool {
	var fe *FormatError
	if errors.As(err, &fe) {
		return strings.HasPrefix(fe.Reason, errBadVersion)
	}
	return false
}
