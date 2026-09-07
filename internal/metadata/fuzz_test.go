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
	b, err := tableRecord().Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	if err != nil {
		panic(err)
	}
	return b
}

func FuzzMetadataRecordDecode(f *testing.F) {
	seedFuzz(f, validRecordBytes(), func() []byte {
		b, _ := columnRecord().Encode(nil)
		return b
	}())
	f.Fuzz(func(t *testing.T, data []byte) {
		_ = (&Record{}).Decode(data, nil)
		_ = (&Record{}).Decode(data, CoreFieldSchemas[uint32(fileformat.RecordTable)])
	})
}

func FuzzMetadataPayloadParse(f *testing.F) {
	b, err := tableRecord().Encode(CoreFieldSchemas[uint32(fileformat.RecordTable)])
	if err != nil {
		panic(err)
	}
	payload, err := Build([]DirectoryEntry{{ObjectID: 2, Revision: 1, RecordType: 2, Operation: fileformat.OperationUpsert}}, [][]byte{b})
	if err != nil {
		panic(err)
	}
	seedFuzz(f, payload)
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}
