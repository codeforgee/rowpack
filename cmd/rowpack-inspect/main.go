// Command rowpack-inspect is a read-only debugging tool for RowPack stores.
//
// Usage:
//
//	rowpack-inspect header <base>
//	rowpack-inspect list <base>
//	rowpack-inspect verify <base>
//	rowpack-inspect dump <base> <snapshotID> <tableID>
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/rowpack/rowpack"
)

func main() {
	flag.Parse()
	args := flag.Args()
	if len(args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, base := args[0], args[1]
	ctx := context.Background()

	switch cmd {
	case "header":
		header(base)
	case "list":
		list(ctx, base)
	case "verify":
		verify(ctx, base)
	case "dump":
		if len(args) < 4 {
			usage()
			os.Exit(2)
		}
		var snap uint64
		var table uint32
		fmt.Sscanf(args[2], "%d", &snap)
		fmt.Sscanf(args[3], "%d", &table)
		dump(ctx, base, snap, table)
	default:
		usage()
		os.Exit(2)
	}
}

func formatValue(v rowpack.Value) string {
	if v.IsNull() {
		return "NULL"
	}
	switch v.Type() {
	case rowpack.TypeString:
		s, _ := v.String()
		return s
	case rowpack.TypeBool:
		b, _ := v.Bool()
		return fmt.Sprintf("%v", b)
	case rowpack.TypeInt8:
		x, _ := v.Int8()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeInt16:
		x, _ := v.Int16()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeInt32:
		x, _ := v.Int32()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeInt64:
		x, _ := v.Int64()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeUint8:
		x, _ := v.Uint8()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeUint16:
		x, _ := v.Uint16()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeUint32:
		x, _ := v.Uint32()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeUint64:
		x, _ := v.Uint64()
		return fmt.Sprintf("%d", x)
	case rowpack.TypeFloat32:
		x, _ := v.Float32()
		return fmt.Sprintf("%v", x)
	case rowpack.TypeFloat64:
		x, _ := v.Float64()
		return fmt.Sprintf("%v", x)
	case rowpack.TypeBytes:
		x, _ := v.Bytes()
		return fmt.Sprintf("%x", x)
	case rowpack.TypeDate:
		x, _ := v.Date()
		return x.Time(time.UTC).Format("2006-01-02")
	case rowpack.TypeTime:
		x, _ := v.Time()
		return time.Unix(0, int64(x)).UTC().Format("15:04:05.000000000")
	case rowpack.TypeDateTime:
		x, _ := v.DateTimeValue()
		return x.Format(time.RFC3339Nano)
	case rowpack.TypeDecimal:
		d, _ := v.Decimal()
		return fmt.Sprintf("%s/%d", d.Unscaled.String(), d.Scale)
	}
	return "?"
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rowpack-inspect {header|list|verify|dump} <base> [snapshotID tableID]")
}

func open(base string) *rowpack.Store {
	db, err := rowpack.Open(base, rowpack.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, "open:", err)
		os.Exit(1)
	}
	return db
}

func header(base string) {
	db := open(base)
	defer db.Close()
	st := db.Stats()
	fmt.Printf("path: %s.rpk\n", db.Path())
	fmt.Printf("uuid: %x\n", db.UUID())
	fmt.Printf("readonly: %v\n", db.ReadOnly())
	fmt.Printf("snapshots=%d blocks=%d tables=%d logicalRows=%d\n", st.Snapshots, st.Blocks, st.Tables, st.LogicalRows)
	fmt.Printf("dataBytes=%d rawBytes=%d storedBytes=%d indexMemory=%d\n",
		st.DataFileBytes, st.RawBytes, st.StoredBytes, st.IndexMemoryBytes)
	fmt.Printf("cache: hits=%d misses=%d evictions=%d used=%d/%d\n",
		st.Cache.Hits, st.Cache.Misses, st.Cache.Evictions, st.Cache.UsedBytes, st.Cache.CapacityBytes)
	fmt.Printf("recovery: performed=%v dataTail=%d indexTail=%d rebuilt=%d\n",
		st.Recovery.Performed, st.Recovery.DataTailIgnored, st.Recovery.IndexTailIgnored, st.Recovery.SnapshotsRebuilt)
}

func list(ctx context.Context, base string) {
	db := open(base)
	defer db.Close()
	snaps, err := db.ListSnapshots(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "list:", err)
		os.Exit(1)
	}
	for _, s := range snaps {
		typ := "FULL"
		if s.Type == rowpack.SnapshotDelta {
			typ = "DELTA"
		}
		fmt.Printf("snapshot %d %s parent=%d created=%s blocks=%d changes=%d raw=%d stored=%d\n",
			s.ID, typ, s.Parent, s.CreatedAt.Format("2006-01-02 15:04:05"), s.BlockCount, s.ChangeCount, s.RawBytes, s.StoredBytes)
		tables, err := db.Tables(ctx, s.ID)
		if err == nil {
			for _, t := range tables {
				fmt.Printf("  table %d %q latestVersion=%d\n", t.ID, t.Name, t.LatestVersion)
			}
		}
	}
}

func verify(ctx context.Context, base string) {
	db := open(base)
	defer db.Close()
	rep, err := db.Verify(ctx, rowpack.VerifyFull)
	if err != nil {
		fmt.Fprintln(os.Stderr, "verify:", err)
		os.Exit(1)
	}
	fmt.Printf("OK snapshots=%d blocks=%d rows=%d bytes=%d duration=%s\n",
		rep.SnapshotsChecked, rep.BlocksChecked, rep.RowsChecked, rep.DataBytesRead, rep.Duration)
}

func dump(ctx context.Context, base string, snap uint64, table uint32) {
	db := open(base)
	defer db.Close()
	// Resolve the internal table ID to its name for the name-based Scan.
	tables, err := db.Tables(ctx, snap)
	if err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
		os.Exit(1)
	}
	var name string
	for _, t := range tables {
		if uint32(t.ID) == table {
			name = t.Name
			break
		}
	}
	if name == "" {
		fmt.Fprintf(os.Stderr, "dump: table %d not found in snapshot %d\n", table, snap)
		os.Exit(1)
	}
	it, err := db.Scan(ctx, snap, name, rowpack.ScanOptions{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
		os.Exit(1)
	}
	defer it.Close()
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		fmt.Printf("%d", it.RowID())
		for _, v := range row {
			fmt.Print("\t" + formatValue(v))
		}
		fmt.Println()
	}
	if err := it.Err(); err != nil {
		fmt.Fprintln(os.Stderr, "dump:", err)
		os.Exit(1)
	}
}
