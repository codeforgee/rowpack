package metadata

import (
	"testing"

	"github.com/rowpack/rowpack/internal/fileformat"
)

// Decoders must never panic on arbitrary input.

func seedFuzz(f *testing.F, seeds ...[]byte) {
	f.Add([]byte(nil))
	f.Add([]byte{0xFF, 0xFE, 0xFD})
	for _, s := range seeds {
		f.Add(s)
		if len(s) > 0 {
			f.Add(s[:len(s)/2])
		}
	}
}

func validRecordBytes() []byte {
	b, err := headerRecord().Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	if err != nil {
		panic(err)
	}
	return b
}

func FuzzMetadataRecordDecode(f *testing.F) {
	seedFuzz(f, validRecordBytes(), func() []byte {
		b, _ := tableRecord().Encode(nil)
		return b
	}())
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&Record{}).Decode(data, nil)
		_ = (&Record{}).Decode(data, CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	})
}

func FuzzMetadataPayloadParse(f *testing.F) {
	b, err := headerRecord().Encode(CoreFieldSchemas[uint32(fileformat.RecordHeader)])
	if err != nil {
		panic(err)
	}
	payload, err := Build([]DirectoryEntry{{ObjectID: 1, Revision: 1, RecordType: 1, Operation: fileformat.OperationUpsert}}, [][]byte{b})
	if err != nil {
		panic(err)
	}
	seedFuzz(f, payload)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}
