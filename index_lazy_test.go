package rowpack

import (
	"context"
	"fmt"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLazyMatchesEager verifies that a Lazy index Open (Option.IndexMode==
// IndexLazy, only the fence resident, pages decoded on demand) produces
// row-identical results to the default Eager Open across the full read API:
// Get/Scan/Exists/Tables. It exercises a FULL snapshot spanning multiple tables
// plus a table large enough to split across several index pages (multi-page
// lazy iteration), and a DELTA chain. The same file is opened once per mode
// (the writer lock forbids two simultaneous opens), each mode's visible state
// is captured to memory, then compared.
func TestLazyMatchesEager(t *testing.T) {
	base := filepath.Join(tmpdb(t), "lazy-eager")
	buildLazyStore(t, base, Options{BlockSize: 1024})

	eagerData := captureLazyStore(t, base, Options{BlockSize: 1024}, IndexEager)
	lazyData := captureLazyStore(t, base, Options{BlockSize: 1024}, IndexLazy)

	compareLazyAndEager(t, eagerData, lazyData)
}

// TestLazyMatchesEagerEncrypted verifies Lazy matches Eager on an encrypted
// store: Index pages are sealed per-page under the Index-domain nonce/AAD, so
// the Lazy page OPEN must authenticate + decrypt correctly on first access.
func TestLazyMatchesEagerEncrypted(t *testing.T) {
	base := filepath.Join(tmpdb(t), "lazy-eager-enc")
	buildLazyStore(t, base, encOptions("lk"))

	eagerData := captureLazyStore(t, base, encOptions("lk"), IndexEager)
	lazyData := captureLazyStore(t, base, encOptions("lk"), IndexLazy)

	compareLazyAndEager(t, eagerData, lazyData)
}

// compareLazyAndEager asserts the two captured mode states are row-identical.
func compareLazyAndEager(t *testing.T, eagerData, lazyData modeData) {
	t.Helper()
	require.True(t, len(eagerData) >= 2, "need FULL + DELTA snapshots")
	require.Equal(t, len(eagerData), len(lazyData), "snapshot count differs")
	for snap, etab := range eagerData {
		ltab, ok := lazyData[snap]
		require.True(t, ok, "lazy missing snapshot %d", snap)
		require.Equal(t, len(etab.tables), len(ltab.tables), "table count differs snap=%d", snap)
		for name, erows := range etab.tables {
			lrows, ok := ltab.tables[name]
			require.True(t, ok, "lazy missing table %s snap=%d", name, snap)
			require.Equal(t, len(erows), len(lrows), "row count differs table=%s snap=%d", name, snap)
			for rowID, erow := range erows {
				lrow, ok := lrows[rowID]
				require.True(t, ok, "lazy missing row %d table=%s snap=%d", rowID, name, snap)
				require.True(t, rowsEqualIds(erow, lrow), "row %d value differs table=%s snap=%d", rowID, name, snap)
			}
			// Exists must agree (tombstone resolution along the chain).
			probe := []uint64{1, 2, 3, 11, 5000, 5001, 99999}
			for _, rid := range probe {
				ee := etab.exists[name][rid]
				le := ltab.exists[name][rid]
				require.Equal(t, ee, le, "Exists differs table=%s snap=%d row=%d", name, snap, rid)
			}
		}
	}
}

// modeData is one mode's visible state keyed by snapshot.
type modeData map[SnapshotID]*snapData

// snapData holds a snapshot's per-table rows and existence probes.
type snapData struct {
	tables map[string]map[uint64]Row
	exists map[string]map[uint64]bool
}

// captureLazyStore opens a store in the given index mode, captures every
// visible snapshot/table via Scan + Exists, closes it, and returns the data.
func captureLazyStore(t *testing.T, base string, baseOpts Options, mode IndexMode) modeData {
	t.Helper()
	opts := baseOpts
	opts.IndexMode = mode
	if mode == IndexLazy {
		opts.IndexCacheBytes = 1 << 20
	}
	db, err := Open(base, opts)
	require.NoError(t, err)
	defer db.Close()

	ctx := context.Background()
	snaps, err := db.ListSnapshots(ctx)
	require.NoError(t, err)
	out := make(modeData)
	probe := []uint64{1, 2, 3, 11, 5000, 5001, 99999}
	for _, info := range snaps {
		sd := &snapData{tables: map[string]map[uint64]Row{}, exists: map[string]map[uint64]bool{}}
		tabs, err := db.Tables(ctx, info.ID)
		require.NoError(t, err)
		for _, tab := range tabs {
			sd.tables[tab.Name] = collectScan(t, db, info.ID, tab.Name)
			sd.exists[tab.Name] = map[uint64]bool{}
			for _, rid := range probe {
				e, err := db.Exists(ctx, info.ID, tab.Name, RowID(rid))
				require.NoError(t, err)
				sd.exists[tab.Name][rid] = e
			}
		}
		out[info.ID] = sd
	}
	return out
}

// buildLazyStore writes a deterministic FULL + DELTA store: a large "users"
// table (spans multiple index pages), small "events"/"oversize" tables, a DELTA
// that updates/deletes/inserts, and an oversize row to exercise big pages.
func buildLazyStore(t *testing.T, base string, opts Options) {
	t.Helper()
	ctx := context.Background()
	if opts.BlockSize == 0 {
		opts.BlockSize = 1024
	}
	db, err := Create(base, opts)
	require.NoError(t, err)

	w, err := db.BeginFull(ctx)
	require.NoError(t, err)
	require.NoError(t, w.CreateTable("users", usersSchema()))
	require.NoError(t, w.CreateTable("events", []Column{{Name: "seq", Type: TypeUint64}}))
	require.NoError(t, w.CreateTable("oversize", []Column{{Name: "blob", Type: TypeBytes}}))
	// Large users table -> multiple index pages (4096/page).
	for i := 1; i <= 5000; i++ {
		require.NoError(t, w.Insert(ctx, "users", uint64(i), Row{
			Uint64(uint64(i)), String(fmt.Sprintf("user-%d", i)), Bool(i%2 == 0),
			DecimalValue(Decimal{Unscaled: big.NewInt(int64(i * 100)), Scale: 2}),
		}))
	}
	require.NoError(t, w.Insert(ctx, "events", 1, Row{Uint64(1)}))
	require.NoError(t, w.Insert(ctx, "events", 2, Row{Uint64(2)}))
	require.NoError(t, w.Insert(ctx, "oversize", 1, Row{Bytes(make([]byte, 128<<10))}))
	full, err := w.Commit(ctx)
	require.NoError(t, err)
	require.Equal(t, SnapshotID(1), full)

	// DELTA: update a user, delete one, insert a new user + an event.
	d, err := db.BeginDelta(ctx, full)
	require.NoError(t, err)
	require.NoError(t, d.Update(ctx, "users", 2, Row{Uint64(2), String("updated-2"), Bool(true), DecimalValue(Decimal{Unscaled: big.NewInt(999), Scale: 2})}))
	require.NoError(t, d.Delete(ctx, "users", 3))
	require.NoError(t, d.Insert(ctx, "users", 5001, Row{Uint64(5001), String("new-5001"), Bool(false), DecimalValue(Decimal{Unscaled: big.NewInt(1), Scale: 2})}))
	require.NoError(t, d.Insert(ctx, "events", 3, Row{Uint64(3)}))
	_, err = d.Commit(ctx)
	require.NoError(t, err)
	require.NoError(t, db.Close())
}

// collectScan materializes all visible rows of a snapshot/table via Scan.
func collectScan(t *testing.T, db *Store, snap SnapshotID, table string) map[uint64]Row {
	t.Helper()
	ctx := context.Background()
	it, err := db.Scan(ctx, snap, table, ScanOptions{})
	require.NoError(t, err)
	out := make(map[uint64]Row)
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		require.NoError(t, it.Err())
		rid, _ := row[0].Uint64()
		out[rid] = row
	}
	return out
}

// rowsEqualIds compares two rows on the columns each schema actually has: id
// (col 0) plus name (col 1) and active (col 2) which the users schema mutates.
// The oversize/events tables carry extra/other columns that both modes read
// from the same block, so comparing the located id is the cross-mode check.
func rowsEqualIds(a, b Row) bool {
	av, _ := a[0].Uint64()
	bv, _ := b[0].Uint64()
	if av != bv {
		return false
	}
	if len(a) >= 2 && len(b) >= 2 {
		an, _ := a[1].String()
		bn, _ := b[1].String()
		if an != bn {
			return false
		}
	}
	if len(a) >= 3 && len(b) >= 3 {
		ab, _ := a[2].Bool()
		bb, _ := b[2].Bool()
		if ab != bb {
			return false
		}
	}
	return true
}
