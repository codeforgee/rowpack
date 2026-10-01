// Package inspect implements the read-only rowpack-inspect commands behind a
// testable entry point: Run parses arguments, executes one command and writes
// human-readable output to the given writers, so the CLI in cmd/ is a thin
// os.Exit wrapper and every command path is covered by tests.
package inspect

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/codeforgee/rowpack"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailure = 1
	ExitUsage   = 2
)

// ExitError carries the process exit status a command decided on. Run prints
// its own diagnostics, so main only maps the code.
type ExitError struct {
	Code int
	Err  error
}

// Error implements error.
func (e *ExitError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("rowpack-inspect: exit %d", e.Code)
}

// Unwrap exposes the underlying failure for errors.Is.
func (e *ExitError) Unwrap() error { return e.Err }

// Usage is the command summary, shared by the usage error and the flag set.
const Usage = "usage: rowpack-inspect {header|list|verify|dump} <base> [snapshotID tableID]"

// Run executes one inspect command. argv must not include the program name.
// Diagnostics go to stderr, results to stdout. A nil return means success.
func Run(ctx context.Context, argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("rowpack-inspect", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, Usage) }
	if err := fs.Parse(argv); err != nil {
		return &ExitError{Code: ExitUsage, Err: err}
	}
	args := fs.Args()
	if len(args) < 2 {
		fmt.Fprintln(stderr, Usage)
		return &ExitError{Code: ExitUsage, Err: fmt.Errorf("missing command or store path")}
	}
	cmd, base := args[0], args[1]
	switch cmd {
	case "header":
		return header(base, stdout, stderr)
	case "list":
		return list(ctx, base, stdout, stderr)
	case "verify":
		return verify(ctx, base, stdout, stderr)
	case "dump":
		if len(args) < 4 {
			fmt.Fprintln(stderr, Usage)
			return &ExitError{Code: ExitUsage, Err: fmt.Errorf("dump needs <snapshotID> <tableID>")}
		}
		snap, err := strconv.ParseUint(args[2], 10, 64)
		if err != nil {
			fmt.Fprintf(stderr, "dump: bad snapshotID %q: %v\n", args[2], err)
			return &ExitError{Code: ExitUsage, Err: err}
		}
		table, err := strconv.ParseUint(args[3], 10, 32)
		if err != nil {
			fmt.Fprintf(stderr, "dump: bad tableID %q: %v\n", args[3], err)
			return &ExitError{Code: ExitUsage, Err: err}
		}
		return dump(ctx, base, snap, uint32(table), stdout, stderr)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s\n", cmd, Usage)
		return &ExitError{Code: ExitUsage, Err: fmt.Errorf("unknown command %q", cmd)}
	}
}

// openStore opens the store read-only, mapping failures to ExitFailure.
func openStore(base string, stderr io.Writer) (*rowpack.Store, error) {
	db, err := rowpack.Open(base, rowpack.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintln(stderr, "open:", err)
		return nil, &ExitError{Code: ExitFailure, Err: err}
	}
	return db, nil
}

func header(base string, stdout, stderr io.Writer) error {
	db, err := openStore(base, stderr)
	if err != nil {
		return err
	}
	defer db.Close()
	st := db.Stats()
	fmt.Fprintf(stdout, "path: %s.rpk\n", db.Path())
	fmt.Fprintf(stdout, "uuid: %x\n", db.UUID())
	fmt.Fprintf(stdout, "readonly: %v\n", db.ReadOnly())
	fmt.Fprintf(stdout, "snapshots=%d blocks=%d tables=%d logicalRows=%d\n",
		st.Snapshots, st.Blocks, st.Tables, st.LogicalRows)
	fmt.Fprintf(stdout, "dataBytes=%d rawBytes=%d storedBytes=%d indexMemory=%d oversizedRowPages=%d\n",
		st.DataFileBytes, st.RawBytes, st.StoredBytes, st.IndexMemoryBytes, st.OversizedRowPages)
	fmt.Fprintf(stdout, "cache: hits=%d misses=%d evictions=%d used=%d/%d\n",
		st.Cache.Hits, st.Cache.Misses, st.Cache.Evictions, st.Cache.UsedBytes, st.Cache.CapacityBytes)
	fmt.Fprintf(stdout, "scanCache: hits=%d misses=%d evictions=%d used=%d/%d\n",
		st.ScanCache.Hits, st.ScanCache.Misses, st.ScanCache.Evictions, st.ScanCache.UsedBytes, st.ScanCache.CapacityBytes)
	fmt.Fprintf(stdout, "pageIO: loads=%d rawBytes=%d storedBytes=%d\n",
		st.Read.PageLoads, st.Read.PageRawBytes, st.Read.PageStoredBytes)
	fmt.Fprintf(stdout, "recovery: performed=%v dataTail=%d indexTail=%d rebuilt=%d\n",
		st.Recovery.Performed, st.Recovery.DataTailIgnored, st.Recovery.IndexTailIgnored, st.Recovery.SnapshotsRebuilt)
	return nil
}

func list(ctx context.Context, base string, stdout, stderr io.Writer) error {
	db, err := openStore(base, stderr)
	if err != nil {
		return err
	}
	defer db.Close()
	snaps, err := db.ListSnapshots(ctx)
	if err != nil {
		fmt.Fprintln(stderr, "list:", err)
		return &ExitError{Code: ExitFailure, Err: err}
	}
	for _, s := range snaps {
		typ := "FULL"
		if s.Type == rowpack.SnapshotDelta {
			typ = "DELTA"
		}
		fmt.Fprintf(stdout, "snapshot %d %s parent=%d created=%s blocks=%d changes=%d raw=%d stored=%d\n",
			s.ID, typ, s.Parent, s.CreatedAt.Format("2006-01-02 15:04:05"),
			s.BlockCount, s.ChangeCount, s.RawBytes, s.StoredBytes)
		tables, err := db.Tables(ctx, s.ID)
		if err != nil {
			// A snapshot whose catalog cannot be read is worth reporting: an
			// inspect tool must not hide a broken store behind an empty list.
			fmt.Fprintf(stderr, "  table list failed at snapshot %d: %v\n", s.ID, err)
			continue
		}
		for _, t := range tables {
			fmt.Fprintf(stdout, "  table %d %q latestVersion=%d ns=%s address=%s\n",
				t.ID, t.Name, t.LatestVersion, t.NS, rowpack.Qualify(t.NS, t.Name))
		}
	}
	return nil
}

func verify(ctx context.Context, base string, stdout, stderr io.Writer) error {
	db, err := openStore(base, stderr)
	if err != nil {
		return err
	}
	defer db.Close()
	rep, err := db.Verify(ctx, rowpack.VerifyFull, rowpack.VerifyScope{})
	if err != nil {
		fmt.Fprintln(stderr, "verify:", err)
		return &ExitError{Code: ExitFailure, Err: err}
	}
	fmt.Fprintf(stdout, "OK snapshots=%d blocks=%d rows=%d bytes=%d duration=%s\n",
		rep.SnapshotsChecked, rep.BlocksChecked, rep.RowsChecked, rep.DataBytesRead,
		rep.Duration.Round(time.Microsecond))
	return nil
}

func dump(ctx context.Context, base string, snap uint64, table uint32, stdout, stderr io.Writer) error {
	db, err := openStore(base, stderr)
	if err != nil {
		return err
	}
	defer db.Close()
	// Tables are addressed by (ns, name): the bare name only resolves in the
	// default ns, so a table living in another ns must be qualified or Scan
	// reports it missing.
	tables, err := db.Tables(ctx, snap)
	if err != nil {
		fmt.Fprintln(stderr, "dump:", err)
		return &ExitError{Code: ExitFailure, Err: err}
	}
	var addr string
	for _, t := range tables {
		if uint32(t.ID) == table {
			addr = rowpack.Qualify(t.NS, t.Name)
			break
		}
	}
	if addr == "" {
		err := fmt.Errorf("table %d not found in snapshot %d", table, snap)
		fmt.Fprintln(stderr, "dump:", err)
		return &ExitError{Code: ExitFailure, Err: err}
	}
	it, err := db.Scan(ctx, snap, addr, rowpack.ScanOptions{})
	if err != nil {
		fmt.Fprintln(stderr, "dump:", err)
		return &ExitError{Code: ExitFailure, Err: err}
	}
	defer it.Close()
	for {
		row, ok := it.Next()
		if !ok {
			break
		}
		fmt.Fprint(stdout, it.RowID())
		for _, v := range row {
			fmt.Fprint(stdout, "\t"+Value(v))
		}
		fmt.Fprintln(stdout)
	}
	if err := it.Err(); err != nil {
		fmt.Fprintln(stderr, "dump:", err)
		return &ExitError{Code: ExitFailure, Err: err}
	}
	return nil
}

// Value renders one cell value in the tool's stable text form.
func Value(v rowpack.Value) string {
	if v.IsNull() {
		return "NULL"
	}
	switch v.Type() {
	case rowpack.TypeBool:
		x, _ := v.Bool()
		return strconv.FormatBool(x)
	case rowpack.TypeInt8:
		x, _ := v.Int8()
		return strconv.FormatInt(int64(x), 10)
	case rowpack.TypeInt16:
		x, _ := v.Int16()
		return strconv.FormatInt(int64(x), 10)
	case rowpack.TypeInt32:
		x, _ := v.Int32()
		return strconv.FormatInt(int64(x), 10)
	case rowpack.TypeInt64:
		x, _ := v.Int64()
		return strconv.FormatInt(x, 10)
	case rowpack.TypeUint8:
		x, _ := v.Uint8()
		return strconv.FormatUint(uint64(x), 10)
	case rowpack.TypeUint16:
		x, _ := v.Uint16()
		return strconv.FormatUint(uint64(x), 10)
	case rowpack.TypeUint32:
		x, _ := v.Uint32()
		return strconv.FormatUint(uint64(x), 10)
	case rowpack.TypeUint64:
		x, _ := v.Uint64()
		return strconv.FormatUint(x, 10)
	case rowpack.TypeFloat32:
		x, _ := v.Float32()
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case rowpack.TypeFloat64:
		x, _ := v.Float64()
		return strconv.FormatFloat(x, 'g', -1, 64)
	case rowpack.TypeString:
		s, _ := v.String()
		return s
	case rowpack.TypeBytes:
		b, _ := v.Bytes()
		return fmt.Sprintf("%x", b)
	case rowpack.TypeDate:
		x, _ := v.Date()
		return x.Time(time.UTC).Format("2006-01-02")
	case rowpack.TypeTime:
		x, _ := v.Time()
		return time.Unix(0, int64(x)).UTC().Format("15:04:05.000000000")
	case rowpack.TypeDateTime:
		x, _ := v.DateTimeValue()
		return x.UTC().Format(time.RFC3339Nano)
	case rowpack.TypeDecimal:
		d, _ := v.Decimal()
		return fmt.Sprintf("%s/%d", d.Unscaled.String(), d.Scale)
	}
	return "?"
}
