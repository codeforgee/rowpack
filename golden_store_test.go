package rowpack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/stretchr/testify/require"
)

func TestGoldenManifest(t *testing.T) {
	want := map[string]string{
		// S2: Rows blocks are now page containers (v2 page layout); the
		// full-delta, encrypted and rows-payload samples were regenerated on
		// this format switch (see docs/REFACTOR_EXECUTION_PLAN.md §5).
		"empty-store.rpk":            "cd0a96b72ad858d8bceb946b4ae77b1667b6bf17b9d79d72c9b282a52ddc34f7",
		"rows-payload-all-types.bin": "ae6f94f72c1b08f8c0a6727c97cb57cfad18b6f0ffc732a625db23be907b8769",
		"full-delta-store.rpk":       "88f04ea38e6475bbffd804a95e503d424acb91b99bdceb0ca0bc49f83eb8f596",
		"encrypted-store.rpk":        "c098a6ab6183ca6683d54455027bb3954d80157cc23770336cb65cf9a92b2349",
	}
	for name, digest := range want {
		data, err := os.ReadFile(goldenPath(name))
		require.NoError(t, err, "read golden %s", name)
		require.Equal(t, digest, fmt.Sprintf("%x", sha256.Sum256(data)),
			"golden %s changed; disk-format changes require versioning and explicit manifest review", name)
	}
}

// updateGolden regenerates store golden samples. Enable with
// `go test ./... -run TestGolden -args -update-golden` (see Makefile).
var updateGolden = flag.Bool("update-golden", false, "regenerate golden files")

func goldenPath(name string) string {
	return filepath.Join("testdata", "golden", name)
}

// buildFullDeltaStore writes a deterministic FULL + DELTA + empty DELTA store
// with an oversize row, used both to generate and to verify the golden
// samples.
func buildFullDeltaStore(t *testing.T, base string) {
	t.Helper()
	uuid := [16]byte{0xAA, 0xBB, 0xCC, 0xDD, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C}
	testUUIDOverride = &uuid
	testNowOverride = 1757400000000000000
	nonce := uint64(0x4E4F4E4345474F4C) // "NOCEGOL" — fixed so golden bytes are deterministic
	testNonceOverride = &nonce
	t.Cleanup(func() {
		testUUIDOverride = nil
		testNowOverride = 0
		testNonceOverride = nil
	})
	opts := Options{}
	opts.BlockSize = 1024
	db, err := Create(base, opts)
	require.NoError(t, err)
	// FULL with three tables.
	w, _ := db.BeginFull(context.Background())
	w.CreateTable("users", []Column{
		{Name: "id", Type: TypeUint64}, {Name: "name", Type: TypeString}, {Name: "active", Type: TypeBool},
		{Name: "balance", Type: TypeDecimal, Scale: 2},
	})
	w.CreateTable("empty", []Column{{Name: "x", Type: TypeInt64}})
	w.CreateTable("oversize", []Column{{Name: "blob", Type: TypeBytes}})
	for i := uint64(1); i <= 30; i++ {
		if err := w.Insert(context.Background(), "users", i, Row{
			Uint64(i), String(fmt.Sprintf("user-%d", i)), Bool(i%2 == 0),
			DecimalValue(Decimal{Unscaled: bigI(int64(i * 100)), Scale: 2}),
		}); err != nil {
			require.NoError(t, err)
		}
	}
	// Oversize row (> 1024 target block) on table 3.
	big := make([]byte, 4096)
	for i := range big {
		big[i] = byte(i)
	}
	require.NoError(t, w.Insert(context.Background(), "oversize", 1, Row{Bytes(big)}))
	full, err := w.Commit(context.Background())
	require.NoError(t, err)
	// DELTA: update + delete + insert.
	d, _ := db.BeginDelta(context.Background(), full)
	require.NoError(t, d.Update(context.Background(), "users", 2, Row{Uint64(2), String("updated-2"), Bool(true), DecimalValue(Decimal{Unscaled: bigI(777), Scale: 2})}))
	require.NoError(t, d.Delete(context.Background(), "users", 3))
	require.NoError(t, d.Insert(context.Background(), "users", 31, Row{Uint64(31), String("new-31"), Bool(false), DecimalValue(Decimal{Unscaled: bigI(1), Scale: 2})}))
	delta, err := d.Commit(context.Background())
	require.NoError(t, err)
	// Empty DELTA.
	e, _ := db.BeginDelta(context.Background(), delta)
	empty, err := e.Commit(context.Background())
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_ = empty
}

func bigI(v int64) *big.Int {
	return big.NewInt(v)
}

// TestGoldenStoreSamples locks the FULL+DELTA+empty store and verifies it.
func TestGoldenStoreSamples(t *testing.T) {
	base := filepath.Join(tmpdb(t), "golden-store")
	buildFullDeltaStore(t, base)
	generated, err := os.ReadFile(base + ".rpk")
	require.NoError(t, err)
	if *updateGolden {
		require.NoError(t, os.WriteFile(goldenPath("full-delta-store.rpk"), generated, 0o644))
		return
	}
	data, err := os.ReadFile(goldenPath("full-delta-store.rpk"))
	require.NoError(t, err, "read golden (regenerate with make golden): %v", err)
	require.Equal(t, data, generated, "writer output differs from locked golden (regenerate with make golden only for an intentional format change)")
	// Re-open a copy of the locked bytes, keeping semantic compatibility
	// verification independent from the just-generated file.
	base = filepath.Join(tmpdb(t), "golden-store-open")
	require.NoError(t, os.WriteFile(base+".rpk", data, 0o644))
	db, err := Open(base, Options{})
	require.NoError(t, err)
	defer db.Close()
	snaps, err := db.ListSnapshots(context.Background())
	require.NoError(t, err)
	require.Len(t, snaps, 3, "snapshots: %v %v", snaps, err)
	// FULL content.
	r, err := db.Get(context.Background(), snaps[0].ID, "users", 1, nil)
	require.NoError(t, err)
	if n, _ := r[1].String(); n != "user-1" {
		require.Fail(t, "full row1 name = %q", n)
	}
	// DELTA content.
	if n, _ := func() (string, bool) {
		rr, e := db.Get(context.Background(), snaps[1].ID, "users", 2, nil)
		if e != nil {
			return "", false
		}
		s, _ := rr[1].String()
		return s, true
	}(); n != "updated-2" {
		require.Fail(t, "delta row2 name = %q", n)
	}
	_, err = db.Get(context.Background(), snaps[1].ID, "users", 3, nil)
	require.Error(t, err, "delta row3 not deleted")
	_, err = db.Get(context.Background(), snaps[1].ID, "users", 31, nil)
	require.NoError(t, err, "delta row31")
	// Empty delta sees delta state.
	_, err = db.Get(context.Background(), snaps[2].ID, "users", 31, nil)
	require.NoError(t, err, "empty delta row31")
	// Oversize row round trip.
	big, err := db.Get(context.Background(), snaps[0].ID, "oversize", 1, nil)
	require.NoError(t, err)
	b, _ := big[0].Bytes()
	require.Len(t, b, 4096, "oversize row corrupted: len=%d", len(b))
	require.Equal(t, byte(0), b[0])
	require.Equal(t, byte(255), b[255])
	// CreatedAt from the golden (deterministic override).
	require.True(t, snaps[0].CreatedAt.Equal(time.Unix(0, 1757400000000000000)), "createdAt = %v", snaps[0].CreatedAt)
}

// ---- 从 internal/block、internal/fileformat 合并过来的 golden 生成器 ----
// 它们原来分别是 internal/block/golden_test.go 与 internal/fileformat/golden_test.go，
// 与上面的 store golden 共用同一个 -update-golden flag 和 goldenPath，
// 因此 `make golden` 一条命令即可再生全部样本。

// goldenCaptureSink captures flushed blocks from a block builder.
type goldenCaptureSink struct {
	blocks []*block.FlushedBlock
}

func (s *goldenCaptureSink) flush(fb *block.FlushedBlock) error {
	s.blocks = append(s.blocks, fb)
	return nil
}

// TestGoldenRowsPayloadAllTypes locks the deterministic uncompressed Rows
// payload produced by the codec and block builder for a fixed all-types
// schema and a fixed row set. Any change to TypedTuple or the rows payload
// layout breaks this test. (原 internal/block/golden_test.go)
func TestGoldenRowsPayloadAllTypes(t *testing.T) {
	schema := &codec.Schema{TableID: 1, Version: 1, Name: "golden", Columns: []codec.Column{
		{Name: "b", Type: codec.TypeBool},
		{Name: "i64", Type: codec.TypeInt64},
		{Name: "u32", Type: codec.TypeUint32},
		{Name: "f64", Type: codec.TypeFloat64},
		{Name: "s", Type: codec.TypeString},
		{Name: "by", Type: codec.TypeBytes},
		{Name: "d", Type: codec.TypeDate},
		{Name: "t", Type: codec.TypeTime},
		{Name: "dt", Type: codec.TypeDateTime},
		{Name: "dec", Type: codec.TypeDecimal, Scale: 4},
		{Name: "maybe", Type: codec.TypeString, Nullable: true},
	}}
	// Body-only TypedTuple: the Rows Page layout carries ColumnCount and
	// NullBitmapBytes out of band (resolved from the schema).
	row, err := codec.EncodeBodyInto(schema, Row{
		Bool(true),
		Int64(-987654321012345),
		Uint32(4294967295),
		Float64(3.141592653589793),
		String("黄金行 Δemo😀"),
		Bytes([]byte{0x00, 0x01, 0xFE, 0xFF}),
		DateValue(19723),
		TimeValue(TimeOfDay(43200000000001)),
		DateTime(time.Unix(0, 1700000000123456789).UTC()),
		DecimalValue(Decimal{Unscaled: bigI(-1234567890123), Scale: 4}),
		Null(),
	}, codec.DefaultLimits(), nil)
	require.NoError(t, err)

	var sink goldenCaptureSink
	b := block.NewRowsBlockBuilder(1, 1, 1<<20, fileformat.CompressionNone, 0, block.DefaultLimits(), sink.flush)
	for i := 0; i < 3; i++ {
		require.NoError(t, b.Add(uint64(100+i), 1, fileformat.ChangeInsert, row))
	}
	require.NoError(t, b.Flush())
	require.Len(t, sink.blocks, 1, "got %d blocks, want 1", len(sink.blocks))
	payload := sink.blocks[0].Stored // None compression: container == (header+pages) plaintext

	path := goldenPath("rows-payload-all-types.bin")
	if *updateGolden {
		require.NoError(t, os.WriteFile(path, payload, 0o644))
		return
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err, "read golden %s: %v (regenerate with make golden)", path, err)
	if !bytes.Equal(got, payload) {
		require.Fail(t, "golden %s differs from implementation (regenerate with make golden)", path)
	}
	// The golden must parse back as a valid page container with 3 records.
	rc, err := block.ParseRowsContainer(payload, sink.blocks[0].Header, block.DefaultLimits())
	require.NoError(t, err)
	require.Equal(t, uint32(3), rc.Header.TotalRecords, "golden payload has 3 total records")
}

// fixedStoreUUID is the deterministic UUID used by the empty-store golden
// generator so byte comparison is stable across runs.
var fixedStoreUUID = [16]byte{0x52, 0x4f, 0x57, 0x50, 0x41, 0x43, 0x4b, 0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}

// buildEmptyDataHeader renders the canonical empty-store .rpk header.
func buildEmptyDataHeader() []byte {
	var h fileformat.DataFileHeader
	h.FileHeader = fileformat.FileHeader{
		StoreUUID:          fixedStoreUUID,
		CreatedUnixNano:    1757400000000000000,
		RequiredFeatures:   fileformat.RequiredFeaturesV1,
		OptionalFeatures:   0,
		DefaultBlockSize:   fileformat.DefaultBlockSize,
		DefaultCompression: fileformat.CompressionZstd,
		DefaultRowEncoding: fileformat.RowEncodingTypedTuple,
		Flags:              0,
	}
	buf := make([]byte, fileformat.DataFileHeaderSize)
	_ = h.MarshalTo(buf)
	return buf
}

// TestGoldenEmptyStore locks the single 128-byte "empty store" golden file.
// (原 internal/fileformat/golden_test.go)
func TestGoldenEmptyStore(t *testing.T) {
	data := buildEmptyDataHeader()
	path := goldenPath("empty-store.rpk")
	if *updateGolden {
		require.NoError(t, os.WriteFile(path, data, 0o644))
		return
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err, "read golden %s: %v (regenerate with make golden)", path, err)
	if !bytes.Equal(got, data) {
		require.Fail(t, "golden %s differs from implementation (regenerate with make golden)", path)
	}
	require.Equal(t, fileformat.DataFileHeaderSize, len(got), "golden size = %d, want %d", len(got), fileformat.DataFileHeaderSize)
}
