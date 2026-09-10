package rowpack

import (
	"context"
	"encoding/binary"
	"math/rand"
	"path/filepath"
	"testing"
)

// S0 冻结的固定基准数据集几何（FILE_FORMAT_REFACTOR_PLAN.md §12 阶段 0）。
//
// 所有几何共用同一 7 列 schema（benchCols），差异只在 RowID 序列与 Bytes 列
// 负载；随机序列使用固定种子，保证跨阶段、跨提交可比。压测新格式时以这些
// 几何为对照物，禁止在 benchmark 内临时拼数据。

const (
	benchSeed = uint64(0x52504B32) // "RPK2"

	bigBytesLen      = 4 << 10   // geomBigBytes：Bytes 列 4 KiB 负载
	oversizedRowSize = 320 << 10 // geomOversized：大于任一候选 Page、小于默认 BlockSize
)

// benchGeom names a fixed dataset geometry.
type benchGeom uint8

const (
	geomMixed     benchGeom = iota // 顺序 RowID + 小负载（README/历史口径）
	geomRandomID                   // 固定种子乱序 RowID（文件大小与索引排序口径）
	geomBigBytes                   // 4 KiB Bytes 负载（arena/拷贝路径口径）
	geomOversized                  // 超大行：单行 320 KiB，独占整个 Rows Block
)

// benchIDs returns the RowID sequence for n rows. geomRandomID shuffles
// 1..n with the fixed seed (RowID uniqueness is what matters; insertion
// order is irrelevant to the format).
func benchIDs(geom benchGeom, n int) []RowID {
	ids := make([]RowID, n)
	for i := range ids {
		ids[i] = RowID(i) + 1
	}
	if geom == geomRandomID {
		rng := rand.New(rand.NewSource(int64(benchSeed)))
		rng.Shuffle(n, func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
	}
	return ids
}

// benchRowFor returns row `id` of the geometry. Only the Bytes column
// (index 6) differs across geometries; payloads are deterministic so
// compression ratios stay comparable across runs.
func benchRowFor(geom benchGeom, id RowID) Row {
	row := benchRow(uint64(id))
	switch geom {
	case geomBigBytes:
		row[6] = Bytes(benchPayload(id, bigBytesLen))
	case geomOversized:
		row[6] = Bytes(benchPayload(id, oversizedRowSize))
	}
	return row
}

// benchPayload builds a len-byte deterministic payload tagged with the RowID:
// 8-byte header (RowID) then a fixed-seed LCG fill. Compressible enough to
// exercise the zstd path, structured enough to detect corruption in tests.
func benchPayload(id RowID, length int) []byte {
	b := make([]byte, length)
	binary.LittleEndian.PutUint64(b, uint64(id))
	x := uint64(id)*benchSeed + benchSeed
	for i := 8; i < length; i += 8 {
		x = x*6364136223846793005 + 1442695040888963407
		binary.LittleEndian.PutUint64(b[i:], x)
	}
	return b
}

// benchStoreGeom is benchStore over an explicit geometry.
func benchStoreGeom(tb testing.TB, opts Options, geom benchGeom, n int) (*Store, SnapshotID) {
	tb.Helper()
	if opts.BlockSize == 0 {
		opts.BlockSize = 256 << 10 // README reference config
	}
	db, err := Create(filepath.Join(tmpdb(tb), "geom"), opts)
	requireNilErr(tb, err)
	w, err := db.Begin(context.Background(), NoParent)
	requireNilErr(tb, err)
	requireNilErr(tb, w.DefineTable("t", benchCols()))
	ids := benchIDs(geom, n)
	for _, id := range ids {
		requireNilErr(tb, w.Insert("t", uint64(id), benchRowFor(geom, id)))
	}
	snap, err := w.Commit(context.Background())
	requireNilErr(tb, err)
	return db, snap
}
