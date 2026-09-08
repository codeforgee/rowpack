package block

import (
	"bytes"
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// FuzzReadAtBlockTransient feeds arbitrary bytes to the transient reader:
// it must never panic, never return a block that fails its own header/CRC
// checks, and must release cleanly. Valid blocks are accepted; everything
// else is an error.
func FuzzReadAtBlockTransient(f *testing.F) {
	none := buildBlockForSeed(f, fileformat.CompressionNone)
	zstdBlk := buildBlockForSeed(f, fileformat.CompressionZstd)
	seeds := [][]byte{
		{},
		{0x01},
		appendBlock(nil, &none),
		appendBlock(nil, &zstdBlk),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		rd := NewReader(bytes.NewReader(data), DefaultLimits())
		sc, err := rd.ReadAtBlockTransient(0)
		if err != nil {
			return
		}
		defer sc.Release()
		if uint32(len(sc.Raw)) != sc.Header.RawSize {
			t.Fatalf("raw length %d != header raw size %d", len(sc.Raw), sc.Header.RawSize)
		}
		if fileformat.CRC32C(sc.Raw) != sc.Header.RawCRC32C {
			t.Fatal("accepted block fails its own raw CRC")
		}
	})
}

// buildBlockForSeed builds a small valid block for fuzz seeding.
func buildBlockForSeed(f *testing.F, compress fileformat.Compression) capturedBlock {
	f.Helper()
	var s captureSink
	b := NewRowsBlockBuilder(1, 7, 4096, compress, 3, DefaultLimits(), s.flush)
	if err := b.Add(1, 1, fileformat.ChangeInsert, mkRow(32)); err != nil {
		f.Fatal(err)
	}
	if err := b.Flush(); err != nil {
		f.Fatal(err)
	}
	return s.blocks[0]
}
