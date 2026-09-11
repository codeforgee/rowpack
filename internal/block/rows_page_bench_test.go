package block

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/rowpack/rowpack/internal/codec"
	"github.com/rowpack/rowpack/internal/format"
)

// Page-size sweep baseline (S2 阶段 2 decision data, design §6.1 / §15 #1).
// The rows are the standard 5-column mixed profile; each page is flushed at
// the target raw size and then zstd-compressed. Smaller pages cut read
// amplification (cold read pulls one page, not the whole block) at the cost
// of compression ratio and more pages overall.

const (
	benchPageSeed  = 0x50414732 // "PAG2"
	benchPageRowsN = 200_000
)

func pageBenchSchema() *codec.Schema {
	return &codec.Schema{
		TableID: 1,
		Version: 1,
		Name:    "t",
		Columns: []codec.Column{
			{Name: "id", Type: codec.TypeUint64},
			{Name: "a", Type: codec.TypeInt64},
			{Name: "c", Type: codec.TypeFloat64, Nullable: true},
			{Name: "s", Type: codec.TypeString},
			{Name: "b", Type: codec.TypeBytes, Nullable: true},
		},
	}
}

// benchPageRows builds body-only encodings for benchPageRows rows, reusing
// buffers via EncodeInto's reuse argument (page build is the benchmark;
// row encoding is done once and cached).
func benchPageRows(b *testing.B, schema *codec.Schema) [][]byte {
	b.Helper()
	rng := rand.New(rand.NewSource(benchPageSeed))
	pages := make([][]byte, 0, benchPageRowsN)
	for i := 0; i < benchPageRowsN; i++ {
		row := []codec.Value{
			codec.Uint64(uint64(i) + 1),
			codec.Int64(int64(i) * 7),
			codec.Float64(float64(i) * 0.25),
			codec.String(""), codec.Bytes(nil),
		}
		if rng.Intn(8) == 0 {
			row[2] = codec.Null()
		} else {
			row[2] = codec.Float64(float64(i) * 0.25)
		}
		if rng.Intn(5) == 0 {
			payload := make([]byte, 64+rng.Intn(512))
			for j := range payload {
				payload[j] = byte(i + j)
			}
			row[4] = codec.Bytes(payload)
		}
		if i%7 == 3 {
			row[3] = codec.String("") // empty string coverage
		} else {
			row[3] = codec.String(fmt.Sprintf("row-%08d-payload", i))
		}
		// Copy the body: pages keep their own bytes for the duration of the
		// benchmark, and EncodeInto may alias the returned buffer.
		body, err := testCodec.EncodeInto(schema, row, nil)
		if err != nil {
			b.Fatal(err)
		}
		pages = append(pages, append([]byte(nil), body...))
	}
	return pages
}

func BenchmarkRowsPageEncode(b *testing.B) {
	schema := pageBenchSchema()
	bodies := benchPageRows(b, schema)
	for _, target := range []int{16 << 10, 32 << 10, 64 << 10, 128 << 10} {
		b.Run(fmtTarget(target), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			var (
				pageCount int
				rawBytes  int
				stored    int
			)
			for i := 0; i < b.N; i++ {
				pageCount, rawBytes, stored = 0, 0, 0
				bld := NewPageBuilder(target)
				buildComp := func() error {
					page, err := bld.Finish()
					if err != nil {
						return err
					}
					if page == nil {
						return nil
					}
					pageCount++
					rawBytes += len(page)
					z, err := Compress(format.CompressionZstd, 3, page)
					if err != nil {
						return err
					}
					stored += len(z)
					return nil
				}
				for batch, body := range bodies {
					if bld.NeedsFlush() {
						if err := buildComp(); err != nil {
							b.Fatal(err)
						}
					}
					// RowID mirrors a monotonic change stream (unique per row) so
					// the builder exercises the delta encoder.
					if err := bld.Add(uint64(batch+1), 1, format.ChangeInsert, body); err != nil {
						b.Fatal(err)
					}
				}
				if err := buildComp(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			rows := float64(len(bodies))
			b.ReportMetric(rows/float64(pageCount), "rows/page")
			b.ReportMetric(float64(stored)/rows, "storedB/row")
			b.ReportMetric(float64(rawBytes)/rows, "rawB/row")
			b.ReportMetric(float64(stored)/float64(rawBytes), "ratio")
		})
	}
}

func BenchmarkRowsPageRecordAt32K(b *testing.B) {
	schema := pageBenchSchema()
	bodies := benchPageRows(b, schema)
	bld := NewPageBuilder(32 << 10)
	for _, body := range bodies[1:] {
		if bld.NeedsFlush() {
			if _, err := bld.Finish(); err != nil {
				b.Fatal(err)
			}
		}
		if err := bld.Add(uint64(1), 1, format.ChangeInsert, body); err != nil {
			b.Fatal(err)
		}
	}
	page, err := bld.Finish()
	if err != nil {
		b.Fatal(err)
	}
	p, err := ParseRowsPage(page)
	if err != nil {
		b.Fatal(err)
	}
	n := p.h.EntryCount
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := p.RecordAt(uint32(i % int(n))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRowsPageRecords32K(b *testing.B) {
	schema := pageBenchSchema()
	bodies := benchPageRows(b, schema)
	bld := NewPageBuilder(32 << 10)
	for _, body := range bodies[1:] {
		if bld.NeedsFlush() {
			if _, err := bld.Finish(); err != nil {
				b.Fatal(err)
			}
		}
		if err := bld.Add(uint64(1), 1, format.ChangeInsert, body); err != nil {
			b.Fatal(err)
		}
	}
	page, err := bld.Finish()
	if err != nil {
		b.Fatal(err)
	}
	p, err := ParseRowsPage(page)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := p.Records(func(rec codec.PageRecord) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRowsPageParse32K isolates page CRC, packed-change validation and
// metadata-stream expansion. It is the primary before/after signal for
// portable SWAR or architecture-specific SIMD work in the page parser.
func BenchmarkRowsPageParse32K(b *testing.B) {
	schema := pageBenchSchema()
	bodies := benchPageRows(b, schema)
	bld := NewPageBuilder(32 << 10)
	for _, body := range bodies {
		if bld.NeedsFlush() {
			break
		}
		if err := bld.Add(uint64(bld.countRows()+1), 1, format.ChangeInsert, body); err != nil {
			b.Fatal(err)
		}
	}
	page, err := bld.Finish()
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(page)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ParseRowsPage(page); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateChangeBits(b *testing.B) {
	const entries = 4096
	stream := make([]byte, entries/4)
	b.SetBytes(entries / 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := validateChangeBits(stream, entries); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkValidateChangeBitsScalar preserves the pre-SWAR algorithm as a
// benchmark-only control. Keeping both implementations in the same process
// makes the speedup measurable without relying on an archived machine run.
func BenchmarkValidateChangeBitsScalar(b *testing.B) {
	const entries = 4096
	stream := make([]byte, entries/4)
	b.SetBytes(entries / 4)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		for ordinal := uint32(0); ordinal < entries; ordinal++ {
			if (stream[ordinal/4]>>((ordinal%4)*2))&3 == 3 {
				b.Fatal("unexpected reserved marker")
			}
		}
	}
}

func fmtTarget(kb int) string {
	switch kb {
	case 16 << 10:
		return "16KiB"
	case 32 << 10:
		return "32KiB"
	case 64 << 10:
		return "64KiB"
	default:
		return "128KiB"
	}
}
