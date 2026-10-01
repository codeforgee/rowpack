package rowpack

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// mergeSchema includes a Bytes column so the scan's arena sink exercises both
// materialization paths (string and bytes payloads).
func mergeSchema() []Column {
	return []Column{
		{Name: "id", Type: TypeUint64},
		{Name: "name", Type: TypeString},
		{Name: "blob", Type: TypeBytes},
	}
}

func insertMergeRow(t testing.TB, tx *Tx, id uint64, name string, blob []byte, ct string) {
	t.Helper()
	switch ct {
	case "insert":
		require.NoError(t, tx.Insert(context.Background(), "t", id, Row{Uint64(id), String(name), Bytes(blob)}))
	case "update":
		require.NoError(t, tx.Update(context.Background(), "t", id, Row{Uint64(id), String(name), Bytes(blob)}))
	case "delete":
		require.NoError(t, tx.Delete(context.Background(), "t", id))
	}
}

// TestScanMergeLayers covers the parent-chain merged scan: the heap-based
// k-way merge over multiple snapshot layers, override/tombstone filtering,
// iterator.ChangeType, and writer.ID/Parent accessors.
func TestScanMergeLayers(t *testing.T) {
	db := testDB(t, Options{})
	ctx := context.Background()

	w, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, w.DefineTable("t", mergeSchema()))
	require.EqualValues(t, 0, w.Parent(), "FULL snapshot has no parent")
	for i := 1; i <= 10; i++ {
		insertMergeRow(t, w, uint64(i), "v1", []byte{byte(i), 1}, "insert")
	}
	full, err := w.Commit(ctx)
	require.NoError(t, err)
	require.EqualValues(t, full, w.ID(), "Writer.ID reports the committed snapshot")

	d, err := db.Begin(ctx, full)
	require.NoError(t, err)
	require.EqualValues(t, full, d.Parent(), "DELTA parent is the base snapshot")
	// Update row 5, delete row 7, insert row 11.
	insertMergeRow(t, d, 5, "v2", []byte{5, 2}, "update")
	insertMergeRow(t, d, 7, "", nil, "delete")
	insertMergeRow(t, d, 11, "v1", []byte{11, 1}, "insert")
	delta, err := d.Commit(ctx)
	require.NoError(t, err)
	require.Greater(t, delta, full)

	// Release the writer lock before reopening the store.
	require.NoError(t, db.Close())

	// Reopen so the scan runs against the replayed index.
	re, err := Open(db.Path(), Options{})
	require.NoError(t, err)
	defer re.Close()

	it, err := re.Scan(ctx, delta, "t", ScanOptions{})
	require.NoError(t, err)
	defer it.Close()

	type row struct {
		id   RowID
		ct   ChangeType
		name string
		blob []byte
	}
	var got []row
	for {
		r, ok := it.Next()
		if !ok {
			break
		}
		name, _ := r[1].String()
		blob, _ := r[2].Bytes()
		got = append(got, row{it.RowID(), it.ChangeType(), name, append([]byte(nil), blob...)})
	}
	require.NoError(t, it.Err())

	// Expected visible rows: 1..6 (v1), 8..10 (v1), 11 (v1). Row 5 shows the
	// delta's UPDATE, tombstoned row 7 is hidden.
	wantIDs := []RowID{1, 2, 3, 4, 5, 6, 8, 9, 10, 11}
	require.Len(t, got, len(wantIDs))
	for i, g := range got {
		require.Equal(t, wantIDs[i], g.id)
		if g.id == 5 {
			require.Equal(t, ChangeUpdate, g.ct, "overriding update must win the merge")
		} else {
			require.Equal(t, ChangeInsert, g.ct)
		}
	}
	require.Equal(t, "v2", got[4].name, "row 5 must carry the delta update")
	require.Equal(t, []byte{5, 2}, got[4].blob)

	// Scanning the FULL snapshot still shows the original rows (immutability).
	it2, err := re.Scan(ctx, full, "t", ScanOptions{})
	require.NoError(t, err)
	defer it2.Close()
	var n int
	for {
		_, ok := it2.Next()
		if !ok {
			break
		}
		n++
		require.Equal(t, ChangeInsert, it2.ChangeType())
	}
	require.NoError(t, it2.Err())
	require.EqualValues(t, 10, n)
}

// TestCommitErrorMessage covers the CommitError message formats and unwrap.
func TestCommitErrorMessage(t *testing.T) {
	cause := errors.New("disk on fire")
	known := &CommitError{SnapshotID: 7, Err: cause}
	require.EqualError(t, known, "rowpack: commit of snapshot 7: disk on fire")
	require.ErrorIs(t, known, cause)

	unknown := &CommitError{SnapshotID: 8, Unknown: true, Err: cause}
	require.EqualError(t, unknown, "rowpack: commit of snapshot 8: outcome unknown: disk on fire")
	require.ErrorIs(t, unknown, cause)
}
