// Package rowpack implements the RowPack v1 embedded table storage engine.
//
// RowPack stores two-dimensional table data, its version history and database
// metadata in a pair of append-only files:
//
//   - <base>.rpk: the data file, the authoritative source of committed facts.
//   - <base>.rpi: the derived navigation index, rebuildable from the data file.
//
// The on-disk format is fixed by the v1 binary and metadata specifications
// (BINARY_FORMAT_V1.md, METADATA_FORMAT_V1.md). See the DEVELOPMENT_PLAN.md
// milestone notes for the implementation order.
package rowpack

// FormatVersion is the on-disk format version implemented by this module.
//
// The format is frozen for the v1 line: unknown major versions must be
// rejected when opening a store, and higher minor versions are only opened
// when all required feature bits are recognized. Published enum values, field
// numbers and golden files must not be changed without a format version bump.
const FormatVersion = "1.0"
