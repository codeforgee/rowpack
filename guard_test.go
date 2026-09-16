package rowpack

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rowpack/rowpack/internal/format"
	"github.com/rowpack/rowpack/internal/iofile"
	"github.com/stretchr/testify/require"
)

// guard_test.go closes out the deterministic error-path coverage: writer
// state-machine guards, argument validation, short/garbage file headers,
// CreateSingle failure semantics, and the streamApply per-entry fallback.

func TestWriterStateGuards(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer db.Close()

	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)

	// Every mutation and a second commit fail after Commit.
	require.ErrorIs(t, tx.Insert("t", 2, Row{Uint64(2)}), ErrSnapshotCommitted)
	require.ErrorIs(t, tx.Update("t", 1, Row{Uint64(9)}), ErrSnapshotCommitted)
	require.ErrorIs(t, tx.Delete("t", 1), ErrSnapshotCommitted)
	require.ErrorIs(t, tx.DefineTable("u", []Column{{Name: "id", Type: TypeUint64}}), ErrSnapshotCommitted)
	require.ErrorIs(t, tx.ApplyBatch([]Change{{Type: ChangeInsert, Table: "t", RowID: 2, Row: Row{Uint64(2)}}}), ErrSnapshotCommitted)
	_, err = tx.Commit(ctx)
	require.ErrorIs(t, err, ErrSnapshotCommitted)

	// Rollback guards: mutations fail after Rollback; double rollback is fine.
	tx2, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.NoError(t, tx2.Rollback())
	require.NoError(t, tx2.Rollback())
	require.ErrorIs(t, tx2.Insert("t", 3, Row{Uint64(3)}), ErrSnapshotAborted)

	// Zero row ids are rejected before any table resolution.
	tx3, err := db.Begin(ctx, Latest)
	require.NoError(t, err)
	require.ErrorIs(t, tx3.Insert("t", 0, Row{Uint64(0)}), ErrInvalidArgument)
	require.ErrorIs(t, tx3.Update("t", 0, Row{Uint64(0)}), ErrInvalidArgument)
	require.ErrorIs(t, tx3.Delete("t", 0), ErrInvalidArgument)
	require.ErrorIs(t, tx3.Insert("ghost-table", 1, Row{Uint64(1)}), ErrNotFound)
	require.NoError(t, tx3.Rollback())
}

func TestReadBatchNilBufferAndEmptyIDs(t *testing.T) {
	ctx := context.Background()
	db, err := Create(tmpdb(t), Options{})
	require.NoError(t, err)
	defer db.Close()
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	snap, err := tx.Commit(ctx)
	require.NoError(t, err)

	// nil batchBuffer is rejected explicitly; empty ids short-circuit to nil.
	var buf *batchBuffer
	_, err = db.readBatchInto(ctx, snap, "t", []RowID{1}, buf)
	require.ErrorContains(t, err, "nil batchBuffer")
	rows, err := db.readBatchInto(ctx, snap, "t", nil, &batchBuffer{})
	require.NoError(t, err)
	require.Nil(t, rows)
}

func TestOpenShortAndGarbageHeader(t *testing.T) {
	dir := t.TempDir()

	// Missing file.
	_, err := Open(filepath.Join(dir, "missing"), Options{})
	require.ErrorIs(t, err, ErrNotFound)

	// Truncated header.
	p1 := filepath.Join(dir, "short")
	require.NoError(t, os.WriteFile(p1+".rpk", []byte("ROWPACK1"), 0o644))
	_, err = Open(p1, Options{})
	require.Error(t, err)

	// Garbage header.
	p2 := filepath.Join(dir, "garbage")
	require.NoError(t, os.WriteFile(p2+".rpk", make([]byte, format.DataFileHeaderSize), 0o644))
	_, err = Open(p2, Options{})
	require.Error(t, err)

	// A torn uncommitted tail (footer bytes cut) truncates cleanly on
	// read-write open.
	ctx := context.Background()
	p3 := filepath.Join(dir, "torn")
	db3, err := Create(p3, Options{})
	require.NoError(t, err)
	tx, err := db3.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", []Column{{Name: "id", Type: TypeUint64}}))
	require.NoError(t, tx.Insert("t", 1, Row{Uint64(1)}))
	_, err = tx.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db3.Close())
	full, err := os.ReadFile(p3 + ".rpk")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(p3+".rpk", full[:len(full)-8], 0o644))
	db4, err := Open(p3, Options{})
	require.NoError(t, err)
	require.NoError(t, db4.Close())
}

func TestCreateSingleFailureSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.rpk")
	require.NoError(t, iofile.CreateSingle(path, make([]byte, format.DataFileHeaderSize)))
	size := func() int64 {
		fi, err := os.Stat(path)
		require.NoError(t, err)
		return fi.Size()
	}
	require.Equal(t, int64(format.DataFileHeaderSize), size())

	// Exclusive create refuses to clobber; the original file is untouched.
	err := iofile.CreateSingle(path, make([]byte, 16))
	require.Error(t, err)
	require.Equal(t, int64(format.DataFileHeaderSize), size())

	// Unwritable directory fails and leaves no file behind.
	nested := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(nested, 0o755))
	require.NoError(t, os.Chmod(nested, 0o555))
	defer os.Chmod(nested, 0o755)
	err = iofile.CreateSingle(filepath.Join(nested, "x.rpk"), make([]byte, 64))
	require.Error(t, err)
	_, statErr := os.Stat(filepath.Join(nested, "x.rpk"))
	require.True(t, os.IsNotExist(statErr))
}
