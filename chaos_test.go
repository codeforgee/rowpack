package rowpack

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// chaosRow builds one random row for cols, mixing NULLs and edge values.
func chaosRow(r *rand.Rand, cols []Column) Row {
	row := make(Row, len(cols))
	for i, c := range cols {
		if r.Intn(5) == 0 { // 20% NULL
			row[i] = Null()
			continue
		}
		switch c.Type {
		case TypeBool:
			row[i] = Bool(r.Intn(2) == 0)
		case TypeInt8:
			row[i] = Int8(int8(r.Int()))
		case TypeInt16:
			row[i] = Int16(int16(r.Int()))
		case TypeInt32:
			row[i] = Int32(int32(r.Int()))
		case TypeInt64:
			switch r.Intn(6) {
			case 0:
				row[i] = Int64(math.MinInt64)
			case 1:
				row[i] = Int64(math.MaxInt64)
			case 2:
				row[i] = Int64(0)
			default:
				row[i] = Int64(r.Int63())
			}
		case TypeUint8:
			row[i] = Uint8(uint8(r.Int()))
		case TypeUint16:
			row[i] = Uint16(uint16(r.Int()))
		case TypeUint32:
			row[i] = Uint32(uint32(r.Int()))
		case TypeUint64:
			switch r.Intn(6) {
			case 0:
				row[i] = Uint64(0)
			case 1:
				row[i] = Uint64(^uint64(0))
			default:
				row[i] = Uint64(r.Uint64())
			}
		case TypeFloat32:
			row[i] = Float32(float32(r.NormFloat64()))
		case TypeFloat64:
			row[i] = Float64(r.NormFloat64() * 1e100)
		case TypeString:
			switch r.Intn(8) {
			case 0:
				row[i] = String("")
			case 1:
				row[i] = String("中文✓emoji🌡️")
			case 2:
				row[i] = String(string(make([]byte, 4096))) // oversized: > tiny page
			default:
				row[i] = String(fmt.Sprintf("s%d", r.Int63()))
			}
		case TypeBytes:
			switch r.Intn(6) {
			case 0:
				row[i] = Bytes(nil)
			case 1:
				row[i] = Bytes([]byte{})
			case 2:
				row[i] = Bytes(make([]byte, 2048))
			default:
				b := make([]byte, r.Intn(64))
				r.Read(b)
				row[i] = Bytes(b)
			}
		case TypeDate:
			row[i] = DateValue(NewDate(time.Unix(r.Int63n(4000000000)-1000000000, 0).UTC()))
		case TypeTime:
			tod, err := NewTimeOfDay(r.Intn(24), r.Intn(60), r.Intn(60), r.Intn(1e9))
			if err != nil {
				tod = TimeOfDay(0)
			}
			row[i] = TimeValue(tod)
		case TypeDateTime:
			switch r.Intn(5) {
			case 0:
				row[i] = DateTime(time.Unix(0, 0).UTC())
			case 1:
				row[i] = DateTime(time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC))
			default:
				row[i] = DateTime(time.Unix(r.Int63n(4e9)-1e9, r.Int63n(1e9)).UTC())
			}
		case TypeDecimal:
			scale := c.Scale
			switch r.Intn(5) {
			case 0:
				row[i] = DecimalValue(Decimal{Unscaled: big.NewInt(0), Scale: scale})
			case 1:
				row[i] = DecimalValue(Decimal{Unscaled: big.NewInt(-1), Scale: scale})
			case 2:
				u, _ := new(big.Int).SetString("12345678901234567890123456789012345678901234567890", 10)
				row[i] = DecimalValue(Decimal{Unscaled: u, Scale: scale})
			case 3:
				u, _ := new(big.Int).SetString("-98765432109876543210987654321098765432109876543210", 10)
				row[i] = DecimalValue(Decimal{Unscaled: u, Scale: scale})
			default:
				row[i] = DecimalValue(Decimal{Unscaled: big.NewInt(r.Int63() - 1<<62), Scale: scale})
			}
		default:
			row[i] = Null()
		}
	}
	return row
}

// equalValue compares two values of column type t through their getters.
func equalValue(t Type, a, b Value) bool {
	if a.IsNull() != b.IsNull() {
		return false
	}
	if a.IsNull() {
		return true
	}
	switch t {
	case TypeBool:
		x, ok1 := a.Bool()
		y, ok2 := b.Bool()
		return ok1 && ok2 && x == y
	case TypeInt8:
		x, ok1 := a.Int8()
		y, ok2 := b.Int8()
		return ok1 && ok2 && x == y
	case TypeInt16:
		x, ok1 := a.Int16()
		y, ok2 := b.Int16()
		return ok1 && ok2 && x == y
	case TypeInt32:
		x, ok1 := a.Int32()
		y, ok2 := b.Int32()
		return ok1 && ok2 && x == y
	case TypeInt64:
		x, ok1 := a.Int64()
		y, ok2 := b.Int64()
		return ok1 && ok2 && x == y
	case TypeUint8:
		x, ok1 := a.Uint8()
		y, ok2 := b.Uint8()
		return ok1 && ok2 && x == y
	case TypeUint16:
		x, ok1 := a.Uint16()
		y, ok2 := b.Uint16()
		return ok1 && ok2 && x == y
	case TypeUint32:
		x, ok1 := a.Uint32()
		y, ok2 := b.Uint32()
		return ok1 && ok2 && x == y
	case TypeUint64:
		x, ok1 := a.Uint64()
		y, ok2 := b.Uint64()
		return ok1 && ok2 && x == y
	case TypeFloat32:
		x, ok1 := a.Float64()
		y, ok2 := b.Float64()
		return ok1 && ok2 && math.Float64bits(float64(float32(x))) == math.Float64bits(float64(float32(y)))
	case TypeFloat64:
		x, ok1 := a.Float64()
		y, ok2 := b.Float64()
		return ok1 && ok2 && math.Float64bits(x) == math.Float64bits(y)
	case TypeString:
		x, ok1 := a.String()
		y, ok2 := b.String()
		return ok1 && ok2 && x == y
	case TypeBytes:
		x, ok1 := a.Bytes()
		y, ok2 := b.Bytes()
		if !ok1 || !ok2 || len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	case TypeDate:
		x, ok1 := a.Date()
		y, ok2 := b.Date()
		return ok1 && ok2 && x == y
	case TypeTime:
		x, ok1 := a.Time()
		y, ok2 := b.Time()
		return ok1 && ok2 && x == y
	case TypeDateTime:
		x, ok1 := a.DateTimeValue()
		y, ok2 := b.DateTimeValue()
		return ok1 && ok2 && x.Equal(y)
	case TypeDecimal:
		x, ok1 := a.Decimal()
		y, ok2 := b.Decimal()
		return ok1 && ok2 && x.Scale == y.Scale && x.Unscaled.Cmp(y.Unscaled) == 0
	}
	return false
}

func equalRow(cols []Column, got, want Row) error {
	if len(got) != len(want) {
		return fmt.Errorf("column count %d, want %d", len(got), len(want))
	}
	for i, c := range cols {
		if !equalValue(c.Type, got[i], want[i]) {
			return fmt.Errorf("col %d (%s): got %v want %v", i, c.Name, got[i], want[i])
		}
	}
	return nil
}

// TestChaosRoundTrip drives the full store pipeline with randomized schemas,
// edge-case values, NULLs, mixed DML, schema evolution, FULL checkpoints and
// multi-page geometry, then cross-checks Get/Scan/ReadBatch/ScanBlocks/Verify
// against an in-memory model for every committed snapshot, before and after a
// reopen.
func TestChaosRoundTrip(t *testing.T) {
	variants := []struct {
		name string
		opts Options
	}{
		{"zstd", Options{PageSize: 256, BlockSize: 2048}},
		{"none", Options{PageSize: 256, BlockSize: 2048, Compression: CompressionNone}},
		{"nocache", Options{PageSize: 256, BlockSize: 2048, CacheBytes: -1}},
		{"tinycache", Options{PageSize: 256, BlockSize: 2048, CacheBytes: 32 << 10}},
	}
	for _, v := range variants {
		for seed := int64(1); seed <= 10; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", v.name, seed), func(t *testing.T) {
				chaosOnce(t, seed, v.opts)
			})
		}
	}
}

func chaosOnce(t *testing.T, seed int64, opts Options) {
	ctx := context.Background()
	r := rand.New(rand.NewSource(seed))
	dir := filepath.Join(tmpdb(t), "db")

	// Tiny page size forces multi-page blocks.
	opts.PageSize = 256
	opts.BlockSize = 2048
	db, err := Create(dir, opts)
	require.NoError(t, err)

	allTypes := []Type{
		TypeBool, TypeInt64, TypeUint64, TypeFloat64, TypeString, TypeBytes,
		TypeDate, TypeTime, TypeDateTime, TypeDecimal, TypeInt32, TypeUint8,
	}
	ncols := 2 + r.Intn(4)
	cols := make([]Column, ncols)
	for i := range cols {
		cols[i] = Column{Name: fmt.Sprintf("c%d", i), Type: allTypes[r.Intn(len(allTypes))], Nullable: true}
		if cols[i].Type == TypeDecimal {
			cols[i].Scale = int32(r.Intn(10))
		}
	}

	// live holds the expected visible row per RowID; perSnap records the
	// model at each committed snapshot; written counts DML records per txn.
	live := map[RowID]Row{}
	type snapModel struct {
		id      SnapshotID
		rows    map[RowID]Row
		records int
	}
	var snaps []snapModel
	nextID := RowID(1)

	commit := func(tx *Tx, records int) {
		id, err := tx.Commit(ctx)
		require.NoError(t, err)
		cp := make(map[RowID]Row, len(live))
		for k, v := range live {
			cp[k] = v
		}
		snaps = append(snaps, snapModel{id: id, rows: cp, records: records})
	}

	// FULL baseline.
	tx, err := db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", cols))
	for i := 0; i < 120; i++ {
		id := nextID
		nextID++
		row := chaosRow(r, cols)
		require.NoError(t, tx.Insert("t", id, row))
		live[id] = row
	}
	commit(tx, 120)

	// Two DELTAs with mixed DML + schema evolution on the second.
	for d := 0; d < 2; d++ {
		tx, err := db.Begin(ctx, snaps[len(snaps)-1].id)
		require.NoError(t, err)
		records := 0
		touched := map[RowID]bool{} // one mutation per id per txn (writer dedups)
		for i := 0; i < 60; i++ {
			ids := make([]RowID, 0, len(live))
			for id := range live {
				if !touched[id] {
					ids = append(ids, id)
				}
			}
			switch r.Intn(10) {
			case 0, 1: // delete
				if len(ids) == 0 {
					continue
				}
				id := ids[r.Intn(len(ids))]
				require.NoError(t, tx.Delete("t", id))
				delete(live, id)
				touched[id] = true
				records++
			case 2, 3, 4: // insert
				id := nextID
				nextID++
				row := chaosRow(r, cols)
				require.NoError(t, tx.Insert("t", id, row))
				live[id] = row
				touched[id] = true
				records++
			default: // update
				if len(ids) == 0 {
					continue
				}
				id := ids[r.Intn(len(ids))]
				row := chaosRow(r, cols)
				require.NoError(t, tx.Update("t", id, row))
				live[id] = row
				touched[id] = true
				records++
			}
		}
		commit(tx, records)
	}

	// FULL checkpoint re-inserting all live rows.
	tx, err = db.Begin(ctx, NoParent)
	require.NoError(t, err)
	require.NoError(t, tx.DefineTable("t", cols))
	for id, row := range live {
		require.NoError(t, tx.Insert("t", id, row))
	}
	commit(tx, len(live))

	// Final DELTA.
	tx, err = db.Begin(ctx, snaps[len(snaps)-1].id)
	require.NoError(t, err)
	records := 0
	touched := map[RowID]bool{}
	for i := 0; i < 30; i++ {
		ids := make([]RowID, 0, len(live))
		for id := range live {
			if !touched[id] {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			break
		}
		id := ids[r.Intn(len(ids))]
		row := chaosRow(r, cols)
		require.NoError(t, tx.Update("t", id, row))
		live[id] = row
		touched[id] = true
		records++
	}
	commit(tx, records)
	verifySnapshot := func(t *testing.T, db *Store, m snapModel) {
		// Get every live row.
		for id, want := range m.rows {
			got, err := db.Get(ctx, m.id, "t", id, nil)
			require.NoError(t, err, "snap %d id %d", m.id, id)
			require.NoError(t, equalRow(cols, got, want), "snap %d id %d", m.id, id)
		}
		// Deleted/absent ids must be ErrNotFound.
		for id := nextID; id < nextID+50; id++ {
			_, err := db.Get(ctx, m.id, "t", id, nil)
			require.ErrorIs(t, err, ErrNotFound)
		}
		// Scan: exact id order + values.
		it, err := db.Scan(ctx, m.id, "t", ScanOptions{})
		require.NoError(t, err)
		var n int
		prev := RowID(0)
		for {
			row, ok := it.Next()
			if !ok {
				break
			}
			id := it.RowID()
			require.Greater(t, id, prev, "scan order")
			prev = id
			want, ok2 := m.rows[id]
			require.True(t, ok2, "scan emitted id %d absent from model", id)
			require.NoError(t, equalRow(cols, row, want), "scan id %d", id)
			n++
		}
		require.NoError(t, it.Err())
		require.Len(t, m.rows, n)
		it.Close()
		// Scan window subset.
		if len(m.rows) > 0 {
			it, err := db.Scan(ctx, m.id, "t", ScanOptions{Start: RowID(10), End: RowID(500)})
			require.NoError(t, err)
			for {
				row, ok := it.Next()
				if !ok {
					break
				}
				id := it.RowID()
				require.GreaterOrEqual(t, id, RowID(10))
				require.Less(t, id, RowID(500))
				require.NoError(t, equalRow(cols, row, m.rows[id]), "window id %d", id)
			}
			require.NoError(t, it.Err())
			it.Close()
		}
		// ReadBatch: every id, random order, plus duplicates.
		ids := make([]RowID, 0, len(m.rows))
		for id := range m.rows {
			ids = append(ids, id)
		}
		r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		if len(ids) > 0 {
			req := append([]RowID{}, ids[:len(ids)/2]...)
			req = append(req, ids[0], ids[0]) // duplicates
			rows, err := db.ReadBatch(ctx, m.id, "t", req)
			require.NoError(t, err)
			require.Len(t, rows, len(req))
			for k, id := range req {
				require.NoError(t, equalRow(cols, rows[k], m.rows[id]), "batch id %d", id)
			}
		}
		// ScanBlocks: record counts per snapshot's own txn.
		blks, err := db.Blocks(ctx, m.id, "t")
		require.NoError(t, err)
		total := 0
		for _, b := range blks {
			total += int(b.ItemCount)
		}
		require.Equal(t, m.records, total, "snap %d record count", m.id)
		if total > 0 {
			bit, err := db.ScanBlocks(ctx, m.id, "t", blks[0].BlockID, blks[len(blks)-1].BlockID+1)
			require.NoError(t, err)
			seen := 0
			for {
				_, ok := bit.Next()
				if !ok {
					break
				}
				seen++
				if seen > total+10 {
					bit.Close()
					t.Fatalf("ScanBlocks overrun: %d > %d", seen, total)
				}
			}
			require.NoError(t, bit.Err())
			require.Equal(t, total, seen, "snap %d stream count", m.id)
			bit.Close()
		}
		// Full structural verification.
		_, err = db.Verify(ctx, VerifyFull)
		require.NoError(t, err)
	}

	for _, m := range snaps {
		verifySnapshot(t, db, m)
	}
	require.NoError(t, db.Close())

	// Reopen and re-verify every snapshot from disk.
	db2, err := Open(dir, Options{})
	require.NoError(t, err)
	defer db2.Close()
	for _, m := range snaps {
		verifySnapshot(t, db2, m)
	}
}

// quiet os.File sync helper reference (keeps os import used if trimmed).
var _ = os.Getpid
