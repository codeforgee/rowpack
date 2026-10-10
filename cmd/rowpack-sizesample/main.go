// Command rowpack-sizesample builds one deterministic, deliberately
// complicated store and reports its on-disk footprint.
//
// The point is a stable yardstick for format-size work. The 31-row golden
// store is too small to judge anything: its fixed structures (headers,
// directories, fences) dominate, so a change that saves 40 bytes per page
// looks like a 5% win there and is invisible at a million rows. This sample
// pushes the other way — hundreds of thousands of rows, many tables with
// different shapes, a long DELTA chain mixing INSERT/UPDATE/DELETE, NULLs and
// variable-length values — so per-row and per-page costs are what the numbers
// actually measure.
//
// Everything is driven by one seed, so the same flags always produce the same
// bytes and the reported SHA-256 is reproducible. Feed the report to
// scripts/size-diff.sh to compare two revisions.
//
// Usage:
//
//	rowpack-sizesample -out testdata/size-sample [-scale 1] [-snapshots 20]
package main

import (
	"context"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"time"

	"github.com/codeforgee/rowpack"
)

// words feeds the variable-length string columns. Realistic text is neither
// random nor constant: it repeats, so the compressor finds something, but not
// so much that the payload collapses to nothing.
var words = []string{
	"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel",
	"india", "juliet", "kilo", "lima", "mike", "november", "oscar", "papa",
	"quebec", "romeo", "sierra", "tango", "uniform", "victor", "whiskey",
	"xray", "yankee", "zulu",
}

// tableSpec is one table in the sample: its schema, how many rows the first
// snapshot seeds it with, and how a row is filled.
type tableSpec struct {
	name string
	cols []rowpack.Column
	rows int
	// maxGap caps the RowID stride for this table when -rowid-gap is set. Only
	// lookup needs it: its primary key is uint16, so a large stride would
	// overflow the column.
	maxGap int
	fill   func(rnd *rand.Rand, id uint64) rowpack.Row
}

func text(rnd *rand.Rand, id uint64, maxWords int) string {
	n := rnd.Intn(maxWords + 1)
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(words[rnd.Intn(len(words))])
	}
	fmt.Fprintf(&b, " %d", id%997)
	return b.String()
}

func blob(rnd *rand.Rand, id uint64, maxLen int) []byte {
	b := make([]byte, rnd.Intn(maxLen+1))
	// Semi-compressible: a repeating fill tagged by the row id, so pages are
	// neither pure entropy nor pure zeros.
	fill := byte(id%251) + 1
	for i := range b {
		b[i] = fill + byte(i%7)
	}
	return b
}

var baseTime = time.Unix(1757400000, 0).UTC()

func tables(scale float64) []tableSpec {
	scaled := func(n int) int {
		v := int(float64(n) * scale)
		if v < 1 {
			return 1
		}
		return v
	}
	return []tableSpec{
		{
			// The big one: carries most of the rows, so per-row costs show up
			// here first.
			name: "events",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "kind", Type: rowpack.TypeUint8},
				{Name: "actor", Type: rowpack.TypeUint32},
				{Name: "amount", Type: rowpack.TypeInt64},
				{Name: "ratio", Type: rowpack.TypeFloat64},
				{Name: "ok", Type: rowpack.TypeBool},
				{Name: "at", Type: rowpack.TypeDateTime},
				{Name: "label", Type: rowpack.TypeString},
			},
			rows: scaled(120000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				return rowpack.Row{
					rowpack.Uint64(id),
					rowpack.Uint8(uint8(rnd.Intn(8))),
					rowpack.Uint32(uint32(rnd.Intn(5000))),
					rowpack.Int64(int64(rnd.Intn(1<<40)) - (1 << 39)),
					rowpack.Float64(float64(rnd.Intn(10000)) / 8),
					rowpack.Bool(rnd.Intn(4) != 0),
					rowpack.DateTime(baseTime.Add(time.Duration(id%86400) * time.Second)),
					rowpack.String(text(rnd, id, 6)),
				}
			},
		},
		{
			// Widest schema: every scalar width, so per-row encoding cost is
			// not diluted into one type.
			name: "wide",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "i8", Type: rowpack.TypeInt8},
				{Name: "i16", Type: rowpack.TypeInt16},
				{Name: "i32", Type: rowpack.TypeInt32},
				{Name: "i64", Type: rowpack.TypeInt64},
				{Name: "u8", Type: rowpack.TypeUint8},
				{Name: "u16", Type: rowpack.TypeUint16},
				{Name: "u32", Type: rowpack.TypeUint32},
				{Name: "u64", Type: rowpack.TypeUint64},
				{Name: "f32", Type: rowpack.TypeFloat32},
				{Name: "f64", Type: rowpack.TypeFloat64},
				{Name: "flag", Type: rowpack.TypeBool},
			},
			rows: scaled(40000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				return rowpack.Row{
					rowpack.Uint64(id),
					rowpack.Int8(int8(rnd.Intn(1<<8) - 1<<7)),
					rowpack.Int16(int16(rnd.Intn(1<<16) - 1<<15)),
					rowpack.Int32(int32(rnd.Intn(1<<31) - 1<<30)),
					rowpack.Int64(int64(rnd.Intn(1<<62) - 1<<61)),
					rowpack.Uint8(uint8(rnd.Intn(1 << 8))),
					rowpack.Uint16(uint16(rnd.Intn(1 << 16))),
					rowpack.Uint32(uint32(rnd.Intn(1 << 32))),
					rowpack.Uint64(rnd.Uint64()),
					rowpack.Float32(float32(rnd.Intn(1000)) / 3),
					rowpack.Float64(float64(rnd.Intn(1<<40)) / 7),
					rowpack.Bool(rnd.Intn(2) == 0),
				}
			},
		},
		{
			// NULL-heavy: exercises the null bitmap and NULL encoding.
			name: "nullable",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "a", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "b", Type: rowpack.TypeFloat64, Nullable: true},
				{Name: "c", Type: rowpack.TypeString, Nullable: true},
				{Name: "d", Type: rowpack.TypeBytes, Nullable: true},
				{Name: "e", Type: rowpack.TypeBool, Nullable: true},
				{Name: "f", Type: rowpack.TypeDateTime, Nullable: true},
			},
			rows: scaled(30000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				null := func() bool { return rnd.Intn(10) < 3 }
				pick := func(set bool, v, z rowpack.Value) rowpack.Value {
					if set {
						return v
					}
					return z
				}
				return rowpack.Row{
					rowpack.Uint64(id),
					pick(!null(), rowpack.Int64(int64(rnd.Intn(1<<40))), rowpack.Null()),
					pick(!null(), rowpack.Float64(float64(rnd.Intn(1000))/3), rowpack.Null()),
					pick(!null(), rowpack.String(text(rnd, id, 4)), rowpack.Null()),
					pick(!null(), rowpack.Bytes(blob(rnd, id, 24)), rowpack.Null()),
					pick(!null(), rowpack.Bool(rnd.Intn(2) == 0), rowpack.Null()),
					pick(!null(), rowpack.DateTime(baseTime.Add(time.Duration(id%3600)*time.Second)), rowpack.Null()),
				}
			},
		},
		{
			// Long variable-length values: pushes rows across page boundaries
			// and makes the offset stream carry real deltas.
			name: "documents",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "title", Type: rowpack.TypeString},
				{Name: "body", Type: rowpack.TypeString},
				{Name: "blob", Type: rowpack.TypeBytes},
			},
			rows: scaled(20000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				return rowpack.Row{
					rowpack.Uint64(id),
					rowpack.String(text(rnd, id, 8)),
					rowpack.String(text(rnd, id, 60)),
					rowpack.Bytes(blob(rnd, id, 512)),
				}
			},
		},
		{
			// Narrow: many rows, tiny payloads, so per-row metadata is the
			// whole story.
			name: "counters",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "n", Type: rowpack.TypeInt32},
			},
			rows: scaled(60000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				return rowpack.Row{rowpack.Uint64(id), rowpack.Int32(int32(rnd.Intn(1 << 30)))}
			},
		},
		{
			// Temporal types.
			name: "schedule",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "d", Type: rowpack.TypeDate},
				{Name: "t", Type: rowpack.TypeTime},
				{Name: "dt", Type: rowpack.TypeDateTime},
				{Name: "dttz", Type: rowpack.TypeDateTimeTZ},
			},
			rows: scaled(15000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				t := baseTime.Add(time.Duration(id%86400) * time.Second)
				tod, _ := rowpack.NewTimeOfDay(int(id%24), int(id)%60, int(id)%60, 0)
				return rowpack.Row{
					rowpack.Uint64(id),
					rowpack.DateValue(rowpack.NewDate(t)),
					rowpack.TimeValue(tod),
					rowpack.DateTime(t),
					rowpack.DateTimeTZ(t),
				}
			},
		},
		{
			// Almost entirely NULL: worst case for a bitmap per row.
			name: "sparse",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint64, PrimaryKey: true},
				{Name: "v1", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v2", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v3", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v4", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v5", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v6", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v7", Type: rowpack.TypeInt64, Nullable: true},
				{Name: "v8", Type: rowpack.TypeInt64, Nullable: true},
			},
			rows: scaled(15000),
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				row := rowpack.Row{rowpack.Uint64(id)}
				for i := 0; i < 8; i++ {
					if rnd.Intn(20) == 0 {
						row = append(row, rowpack.Int64(int64(rnd.Intn(1<<40))))
					} else {
						row = append(row, rowpack.Null())
					}
				}
				return row
			},
		},
		{
			// Small: a lookup table that barely changes across snapshots.
			name: "lookup",
			cols: []rowpack.Column{
				{Name: "id", Type: rowpack.TypeUint16, PrimaryKey: true},
				{Name: "code", Type: rowpack.TypeString},
			},
			rows:   scaled(4000),
			maxGap: 16, // uint16 primary key
			fill: func(rnd *rand.Rand, id uint64) rowpack.Row {
				return rowpack.Row{rowpack.Uint16(uint16(id)), rowpack.String(words[int(id)%len(words)])}
			},
		},
	}
}

type liveRows struct {
	ids []uint64
}

func (l *liveRows) add(id uint64) { l.ids = append(l.ids, id) }

// drop removes id by swapping with the last element. Order does not matter
// here — the sample only needs a reproducible set of surviving rows.
func (l *liveRows) drop(id uint64) bool {
	for i, v := range l.ids {
		if v == id {
			last := len(l.ids) - 1
			l.ids[i] = l.ids[last]
			l.ids = l.ids[:last]
			return true
		}
	}
	return false
}

func main() {
	out := flag.String("out", "testdata/size-sample", "directory to write the sample store into")
	scale := flag.Float64("scale", 1, "multiplies every table's row count")
	snapshots := flag.Int("snapshots", 20, "number of DELTA snapshots after the FULL baseline")
	seed := flag.Int64("seed", 20261010, "PRNG seed; fixed so the sample is byte-reproducible")
	rowidGap := flag.Int("rowid-gap", 1,
		"RowID stride: 1 = dense (consecutive IDs), N > 1 = sparse with gaps up to N. "+
			"Only affects -rowid-gap>1 runs; the default matches the committed baseline.")
	keep := flag.Bool("keep", true, "keep the generated store on disk (false removes it after reporting)")
	flag.StringVar(&heapProfilePath, "memprofile", "",
		"write a heap profile of the reopened store to this path (go tool pprof)")
	flag.Parse()

	if err := run(*out, *scale, *snapshots, *seed, *rowidGap, *keep); err != nil {
		fmt.Fprintln(os.Stderr, "rowpack-sizesample:", err)
		os.Exit(1)
	}
}

// nextRowID advances the RowID sequence. gap > 1 makes IDs sparse, which is
// what -rowid-gap is for: bit-packed RowIDs only pay off when IDs cluster, so
// the sample has to model both ends or the measurement is meaningless.
func nextRowID(prev uint64, gap int, rnd *rand.Rand) uint64 {
	if gap <= 1 {
		return prev + 1
	}
	return prev + 1 + uint64(rnd.Intn(gap))
}

func run(out string, scale float64, snapshots int, seed int64, gap int, keep bool) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	// A previous run left files behind; Start fresh so the reported size is
	// this run's store and nothing else.
	old, _ := filepath.Glob(filepath.Join(out, "*.rpk"))
	for _, f := range old {
		_ = os.Remove(f)
	}

	specs := tables(scale)
	rnd := rand.New(rand.NewSource(seed))
	ctx := context.Background()
	started := time.Now()

	db, err := rowpack.Create(filepath.Join(out, "sample"), rowpack.Options{})
	if err != nil {
		return err
	}
	defer func() {
		_ = db.Close()
		if !keep {
			_ = os.RemoveAll(out)
		}
	}()

	// Snapshot 1: FULL baseline seeding every table.
	tx, err := db.Begin(ctx, rowpack.NoParent)
	if err != nil {
		return err
	}
	live := make(map[string]*liveRows, len(specs))
	nextID := make(map[string]uint64, len(specs))
	for _, ts := range specs {
		if err := tx.DefineTable(ts.name, ts.cols); err != nil {
			return fmt.Errorf("define %s: %w", ts.name, err)
		}
		live[ts.name] = &liveRows{ids: make([]uint64, 0, ts.rows)}
		// 0, because nextRowID advances before handing out the first id.
		nextID[ts.name] = 0
		tgap := gap
		if ts.maxGap > 0 && tgap > ts.maxGap {
			tgap = ts.maxGap
		}
		for i := 0; i < ts.rows; i++ {
			id := nextRowID(nextID[ts.name], tgap, rnd)
			nextID[ts.name] = id
			if err := tx.Insert(ctx, ts.name, id, ts.fill(rnd, id)); err != nil {
				return fmt.Errorf("insert %s %d: %w", ts.name, id, err)
			}
			live[ts.name].add(id)
		}
	}
	parent, err := tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit baseline: %w", err)
	}

	// DELTA chain: a realistic mix — new rows in, old rows updated, some
	// removed. This is what turns per-row index cost into a visible number,
	// because every snapshot keeps its own entries alive.
	writes := 0
	for s := 0; s < snapshots; s++ {
		tx, err := db.Begin(ctx, parent)
		if err != nil {
			return err
		}
		for _, ts := range specs {
			l := live[ts.name]
			// Only rows this snapshot inherited are eligible for UPDATE or
			// DELETE: a row inserted below must not also be touched, or the
			// transaction sees two changes for one (table, row).
			base := len(l.ids)
			seen := make(map[uint64]bool, base/50+8)

			// Deletes first, so the updates below cannot pick a doomed row.
			del := base / 200
			if base-del < ts.rows/2 {
				del = 0
			}
			delIDs := make([]uint64, 0, del)
			for i := 0; i < del; i++ {
				id := l.ids[rnd.Intn(base)]
				if seen[id] {
					continue
				}
				seen[id] = true
				if err := tx.Delete(ctx, ts.name, id); err != nil {
					return fmt.Errorf("delete %s %d: %w", ts.name, id, err)
				}
				delIDs = append(delIDs, id)
				writes++
			}
			// Updates: rewrite surviving rows in place.
			upd := base / 50
			for done, tries := 0, 0; done < upd && tries < upd*4; tries++ {
				id := l.ids[rnd.Intn(base)]
				if seen[id] {
					continue
				}
				seen[id] = true
				if err := tx.Update(ctx, ts.name, id, ts.fill(rnd, id+uint64(s))); err != nil {
					return fmt.Errorf("update %s %d: %w", ts.name, id, err)
				}
				done++
				writes++
			}
			// Apply the deletes in ascending id order: live ids are removed
			// by swap, so the order must not depend on map iteration or the
			// next snapshot would draw different rows.
			sort.Slice(delIDs, func(i, j int) bool { return delIDs[i] < delIDs[j] })
			for _, id := range delIDs {
				l.drop(id)
			}
			// Inserts last, on fresh ids, so nothing above can collide.
			ins := ts.rows/40 + 1
			tgap := gap
			if ts.maxGap > 0 && tgap > ts.maxGap {
				tgap = ts.maxGap
			}
			for i := 0; i < ins; i++ {
				id := nextRowID(nextID[ts.name], tgap, rnd)
				nextID[ts.name] = id
				if err := tx.Insert(ctx, ts.name, id, ts.fill(rnd, id)); err != nil {
					return fmt.Errorf("insert %s %d: %w", ts.name, id, err)
				}
				l.add(id)
				writes++
			}
		}
		parent, err = tx.Commit(ctx)
		if err != nil {
			return fmt.Errorf("commit delta %d: %w", s, err)
		}
	}

	st := db.Stats()
	mb := db.IndexMemoryBreakdown()
	pp := db.IndexPackProfile()
	storePath := filepath.Join(out, "sample")
	if err := db.Close(); err != nil {
		return err
	}
	// Reopen and measure what the resident index really costs on the heap.
	// IndexMemoryBytes is an estimate with deliberate round numbers in it;
	// this is what shows up in RSS.
	heapBytes := openHeapCost(storePath)

	// Report. The DATA is reproducible from (flags, seed) — same tables, same
	// rows, same change mix — but the FILE is not: FileHeader carries a
	// StoreUUID and CreatedUnixNano, and every snapshot header carries its own
	// CreatedUnixNano and WriterNonce. So byte counts are comparable across
	// revisions and a checksum is not; the shape lines (tables/snapshots/
	// blocks/logicalRows) are what confirm the sample itself did not move.
	dataBytes := st.DataFileBytes
	rows := st.LogicalRows
	var b strings.Builder
	fmt.Fprintf(&b, "# RowPack 体积样本\n")
	fmt.Fprintf(&b, "# date: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "# git: %s\n", gitRev())
	fmt.Fprintf(&b, "# go: %s\n", goVersion())
	fmt.Fprintf(&b, "# os/arch: %s/%s\n", goEnv("GOOS"), goEnv("GOARCH"))
	fmt.Fprintf(&b, "# seed: %d  scale: %g  snapshots: %d\n", seed, scale, snapshots+1)
	fmt.Fprintf(&b, "# config: BlockSize 256K / PageSize 32K / Zstd L3\n")
	fmt.Fprintf(&b, "# tables: %d  snapshots: %d  blocks: %d  logicalRows: %d\n",
		st.Tables, st.Snapshots, st.Blocks, rows)
	fmt.Fprintf(&b, "# writes: %d\n", writes)
	fmt.Fprintf(&b, "# shards: %d  entries: %d  runs: %d\n", mb.Shards, mb.RowEntries, mb.Runs)
	fmt.Fprintf(&b, "# build: %s\n", time.Since(started).Round(time.Millisecond))
	fmt.Fprintf(&b, "# note: 文件头含 StoreUUID/CreatedUnixNano，快照头含 CreatedUnixNano/WriterNonce，\n")
	fmt.Fprintf(&b, "#       故字节不可复现；跨版本可比对的是体积（整数）与上面的形状行。\n")
	fmt.Fprintf(&b, "\n")
	// Byte counts as integers: a size regression is a handful of bytes per
	// page, and rounded output would hide it.
	fmt.Fprintf(&b, "%-20s %d\n", "dataBytes", dataBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "rawBytes", st.RawBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "storedBytes", st.StoredBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "indexMemoryBytes", st.IndexMemoryBytes)
	// Where that estimate goes: the columnar slices, split by column, plus
	// the per-shard floor. Entry counts here are per snapshot, so a row
	// touched in N snapshots is counted N times.
	fmt.Fprintf(&b, "%-20s %d\n", "idxRowIDsBytes", mb.RowIDsBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "idxOrdinalsBytes", mb.OrdinalsBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "idxChangesBytes", mb.ChangesBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "idxRunStartBytes", mb.RunStartBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "idxBlockIDsBytes", mb.BlockIDsBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "idxShardFixedBytes", mb.ShardFixedBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "idxAccountedBytes", mb.AccountedBytes())
	fmt.Fprintf(&b, "%-20s %d\n", "idxSlackBytes", mb.SlackBytes)
	// 两列都已落地打包：Raw 是"若仍是 []uint64/[]uint32"的对照，Packed 是
	// 实际成本。之前的 Var/Frame 是改造前的预估，已被实测取代。
	fmt.Fprintf(&b, "%-20s %d\n", "packRowIDRawBytes", pp.RawRowIDBits/8)
	fmt.Fprintf(&b, "%-20s %d\n", "packRowIDPackedBytes", pp.PackedRowIDBytes())
	fmt.Fprintf(&b, "%-20s %d\n", "packOrdRawBytes", pp.RawOrdinalBits/8)
	fmt.Fprintf(&b, "%-20s %d\n", "packOrdPackedBytes", pp.PackedOrdinalBytes())
	fmt.Fprintf(&b, "%-20s %d\n", "heapAfterOpenBytes", heapBytes)
	fmt.Fprintf(&b, "%-20s %d\n", "oversizedPages", st.OversizedRowPages)
	pct := func(k string, v float64) { fmt.Fprintf(&b, "%-20s %.6f\n", k, v) }
	pct("ratio", float64(st.StoredBytes)/float64(st.RawBytes))
	pct("bytePerRow", float64(dataBytes)/float64(rows))
	pct("bytePerWrittenRow", float64(dataBytes)/float64(rows+uint64(writes)))
	pct("idxBytePerRow", float64(st.IndexMemoryBytes)/float64(rows))
	fmt.Print(b.String())
	return nil
}

// heapProfilePath, when set, makes the reopen step dump a heap profile. The
// estimate and the real heap disagree by a constant amount per entry, and
// only a profile says which objects that is.
var heapProfilePath string

// openHeapCost reopens a committed store and reports what its resident index
// costs on the Go heap: HeapAlloc after a full GC, minus the pre-open
// baseline. Returns -1 if the store will not open.
//
// The estimate (IndexMemoryBytes) uses round per-entry constants and ignores
// slice capacity overshoot, map cells and every other Go-level cost, so the
// two numbers are expected to disagree — the gap is the point of reporting
// both.
func openHeapCost(path string) int64 {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	db, err := rowpack.Open(path, rowpack.Options{})
	if err != nil {
		return -1
	}
	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	// Profile while the index is still resident, i.e. before Close.
	if heapProfilePath != "" {
		if f, err := os.Create(heapProfilePath); err == nil {
			_ = pprof.WriteHeapProfile(f)
			_ = f.Close()
		}
	}
	_ = db.Close()
	return int64(after.HeapAlloc) - int64(before.HeapAlloc)
}

// gitRev, goVersion and goEnv annotate the report the same way
// scripts/baseline.sh does, so a size report can be pinned to a revision and
// an environment. Failures degrade to "-" rather than aborting the sample.
func gitRev() string {
	out, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return "-"
	}
	return strings.TrimSpace(string(out))
}

func goVersion() string {
	out, err := exec.Command("go", "version").Output()
	if err != nil {
		return "-"
	}
	return strings.TrimSpace(string(out))
}

func goEnv(key string) string {
	out, err := exec.Command("go", "env", key).Output()
	if err != nil {
		return "-"
	}
	return strings.TrimSpace(string(out))
}
