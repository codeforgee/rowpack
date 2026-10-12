package block

import (
	"bytes"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// TestZstdFrameChecksumCompat pins the encoder-CRC decision: our frames are
// written without the zstd content checksum (the format's own page/block
// CRC32C covers integrity), while readers must keep accepting frames produced
// before that change (and by any third-party writer that enables it). The
// klauspost decoder verifies a checksum only when the frame declares one, so
// both variants must round-trip through the pooled decoder unchanged.
func TestZstdFrameChecksumCompat(t *testing.T) {
	src := bytes.Repeat([]byte("rowpack frame checksum compat payload "), 1000)

	withCRC, _ := zstd.NewWriter(nil, zstd.WithEncoderCRC(true), zstd.WithEncoderConcurrency(1))
	legacyFrame := withCRC.EncodeAll(src, nil)
	withCRC.Close()

	ownFrame, err := compressZstd(9, src)
	if err != nil {
		t.Fatal(err)
	}

	dec := decPool.Get().(*zstd.Decoder)
	defer decPool.Put(dec)
	for name, frame := range map[string][]byte{"legacy-with-checksum": legacyFrame, "own-no-checksum": ownFrame} {
		out, err := dec.DecodeAll(frame, nil)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if !bytes.Equal(out, src) {
			t.Fatalf("%s: roundtrip mismatch", name)
		}
	}
}
