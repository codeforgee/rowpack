package block

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeforgee/rowpack/internal/codec"
	"github.com/codeforgee/rowpack/internal/format"
)

// Rows 块页容器的几何关卡。页 CRC 之外，容器头与目录区由块头 RawCRC32C 认证；一旦
// CRC 对上，剩下的唯一防线就是 checkBounds/validateRowCounts 的几何判定——它们决定
// 「损坏」还是「越界读/panic」。这里用「改写目录 + 重算 RawCRC32C」的方式，让伪造的
// 容器通过 CRC 关卡，逐条钉住几何判定。

type forgedContainer struct {
	stored []byte
	hdr    format.BlockHeader
	rh     format.RowsBlockHeader
	dir    []format.RowsPageDirEntry
	pages  [][]byte
	dirEnd int
	// post 在目录回写之后按字节改写容器，用于构造「连 RowsBlockHeader 自身的
	// 交叉校验都放行不了」的伪造（例如 DirectoryBytes 与 PageCount 不一致）。
	post func(stored []byte)
}

// relayout re-encodes the (possibly forged) directory and moves the pages so
// they start right behind it. Directory entries are varint-encoded, so a
// mutation can change the directory length: rebuilding keeps the container
// well-formed apart from whatever the mutation actually broke, instead of
// leaving a stale length that would fail for the wrong reason.
//
// The page bytes are the ones captured before the mutation ran, so a mutation
// that inflates StoredSize cannot read past the end of the fixture: the
// inflated size is what the parser is meant to reject.
func (f *forgedContainer) relayout() {
	// StoredOffset is not part of an entry's encoding, so forging it has no
	// on-disk effect: it is recomputed here exactly as the parser will.
	dirBytes := 0
	for i := range f.dir {
		dirBytes += f.dir[i].EncodedLen()
	}
	f.rh.DirectoryBytes = uint32(dirBytes)
	f.dirEnd = format.RowsBlockHeaderSize + dirBytes
	off := f.dirEnd
	for i := range f.dir {
		f.dir[i].StoredOffset = uint64(off)
		off += int(f.dir[i].StoredSize)
	}
	out := make([]byte, 0, off)
	var hdr [format.RowsBlockHeaderSize]byte
	if err := f.rh.MarshalTo(hdr[:]); err != nil {
		panic("relayout: container header rejected: " + err.Error())
	}
	out = append(out, hdr[:]...)
	for i := range f.dir {
		out = f.dir[i].AppendTo(out)
	}
	for _, p := range f.pages {
		out = append(out, p...)
	}
	f.stored = out
	f.hdr.StoredSize = uint32(len(out))
}

// forgeRowsContainer 建一个合法的 3 页不压缩容器，套用 mutate，然后按改写后的容器
// 头/目录重算块头 RawCRC32C（ParseContainer 的 CRC 校验由此放行）。
func forgeRowsContainer(tb testing.TB, mutate func(*forgedContainer)) *forgedContainer {
	tb.Helper()
	schema := pageTestSchema()
	var want []expectedPageRow
	var bodies [][]byte
	for i := uint64(1); i <= 30; i++ {
		body := pageTestRow(tb, schema, i)
		want = append(want, expectedPageRow{rowID: i, version: 1, ct: format.ChangeInsert, bodyLen: len(body)})
		bodies = append(bodies, body)
	}
	fb, rc := buildContainer(tb, 256, 1<<20, format.CompressionNone, want, bodies)
	if rc.PageCount() < 3 {
		tb.Fatalf("forged fixture must span at least 3 pages, got %d", rc.PageCount())
	}
	f := &forgedContainer{stored: append([]byte(nil), fb.Stored...), hdr: fb.Header}
	if err := f.rh.Unmarshal(f.stored[:format.RowsBlockHeaderSize]); err != nil {
		tb.Fatalf("unmarshal container header: %v", err)
	}
	f.dir = make([]format.RowsPageDirEntry, f.rh.PageCount)
	f.dirEnd = format.RowsBlockHeaderSize + int(f.rh.DirectoryBytes)
	pos := format.RowsBlockHeaderSize
	// StoredOffset is not encoded, so recover it the way the parser does:
	// the first page starts where the directory ends.
	off := f.dirEnd
	for i := range f.dir {
		n, err := f.dir[i].Unmarshal(f.stored[pos:f.dirEnd])
		if err != nil {
			tb.Fatalf("unmarshal dir %d: %v", i, err)
		}
		pos += n
		f.dir[i].StoredOffset = uint64(off)
		off += int(f.dir[i].StoredSize)
	}
	// Capture the page bytes before the mutation runs: mutating StoredSize
	// must not be able to read past the end of the fixture.
	f.pages = make([][]byte, len(f.dir))
	for i := range f.dir {
		start := int(f.dir[i].StoredOffset)
		f.pages[i] = append([]byte(nil), f.stored[start:start+int(f.dir[i].StoredSize)]...)
	}
	if mutate != nil {
		mutate(f)
	}
	// 把（可能被改写的）目录写回容器，并按容器头+目录区重算块头 CRC。
	if err := f.rh.MarshalTo(f.stored[:format.RowsBlockHeaderSize]); err != nil {
		tb.Fatalf("marshal container header: %v", err)
	}
	f.relayout()
	if f.post != nil {
		f.post(f.stored)
	}
	dirEnd := min(f.dirEnd, len(f.stored))
	f.hdr.RawCRC32C = format.CRC32C(f.stored[:dirEnd])
	return f
}

func parseForged(tb testing.TB, f *forgedContainer) error {
	tb.Helper()
	_, err := ParseContainer(f.stored, f.hdr, DefaultLimits())
	return err
}

func TestContainerGeometryGate(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*forgedContainer)
		wanted string
	}{
		{"clean", nil, ""},
		{"item-count-disagrees", func(f *forgedContainer) { f.hdr.ItemCount++ }, "total records"},
		{"directory-size-mismatch", func(f *forgedContainer) {
			// RowsBlockHeader.MarshalTo 自己就拒绝越界的 DirectoryBytes，
			// 只能绕过序列化直接改字节。条目是变长的，所以校验的是区间而非
			// 精确值：0 落在下界之下。
			f.post = func(stored []byte) {
				binary.LittleEndian.PutUint32(stored[16:], 0)
			}
		}, "directory bytes"},
		{"page-ordinal", func(f *forgedContainer) { f.dir[1].PageOrdinal = 7 }, "out of order"},
		{"first-record-ordinal", func(f *forgedContainer) { f.dir[1].FirstRecordOrdinal++ }, "first ordinal"},
		{"empty-page", func(f *forgedContainer) { f.dir[0].RecordCount = 0 }, "zero records"},
		{"stored-size-over-limit", func(f *forgedContainer) { f.dir[0].StoredSize = DefaultLimits().MaxStoredBytes + 1 }, "stored size"},
		{"raw-size-over-limit", func(f *forgedContainer) { f.dir[0].RawSize = DefaultLimits().MaxRawBytes + 1 }, "raw size"},
		{"page-escapes-container", func(f *forgedContainer) { f.dir[len(f.dir)-1].StoredSize += 8 }, "escape container"},
		// 保持 stored==raw（否则先撞 none-compression 判定），只让页总长度
		// 短于容器长度。
		{"pages-short-of-container", func(f *forgedContainer) {
			last := len(f.dir) - 1
			f.dir[last].StoredSize -= 8
			f.dir[last].RawSize -= 8
		}, "pages end at"},
		{"none-page-stored-ne-raw", func(f *forgedContainer) { f.dir[0].RawSize = f.dir[0].StoredSize - 1 }, "none-compressed page"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := parseForged(t, forgeRowsContainer(t, tc.mut))
			if tc.wanted == "" {
				require.NoError(t, err, "clean forged container rejected")
				return
			}
			require.NotNil(t, err, "%s: container accepted", tc.name)
			require.Contains(t, err.Error(), tc.wanted, "%s: err = %v, want diagnosis %q", tc.name, err, tc.wanted)
			if strings.Contains(err.Error(), "panic") {
				t.Fatalf("geometry failures must be errors, not panics: %v", err)
			}
		})
	}
}

// TestContainerHeaderCRCIsEnforced: 不改 RawCRC32C 时，容器头/目录区任意一字节的
// 漂移都必须被 CRC 拦下（几何判定的前提）。
func TestContainerHeaderCRCIsEnforced(t *testing.T) {
	f := forgeRowsContainer(t, nil)
	dirEnd := format.RowsBlockHeaderSize + int(f.rh.DirectoryBytes)
	// 目录区里的字节漂移必须被块头 RawCRC32C 拦下——几何判定跑在 CRC 之后，
	// 没有这道关卡，伪造目录就能直接进 checkBounds。
	for _, off := range []int{format.RowsBlockHeaderSize, format.RowsBlockHeaderSize + 8, dirEnd - 1} {
		g := &forgedContainer{stored: append([]byte(nil), f.stored...), hdr: f.hdr}
		g.stored[off] ^= 0xFF
		// 注意：这里故意不重算 RawCRC32C。
		err := parseForged(t, g)
		if err == nil || !strings.Contains(err.Error(), "CRC mismatch") {
			t.Fatalf("byte %d of the directory escaped the CRC gate: %v", off, err)
		}
	}
	// 容器头 magic 有独立判定，早于 CRC 报错。
	g := &forgedContainer{stored: append([]byte(nil), f.stored...), hdr: f.hdr}
	g.stored[0] ^= 0xFF
	if err := parseForged(t, g); err == nil || !strings.Contains(err.Error(), "magic") {
		t.Fatalf("bad container magic = %v, want a magic diagnosis", err)
	}
}

// TestPageForHoleAndRange: 目录里留下一个「没有页覆盖」的序号空洞（几何上自洽，
// 只是尾部少记录），PageFor 必须拒绝落在空洞里的序号。
func TestPageForHoleAndRange(t *testing.T) {
	f := forgeRowsContainer(t, func(fc *forgedContainer) {
		// 每页记录数减 1，后续页的 FirstRecordOrdinal 同步减 1：衔接关系仍然自洽，
		// 但总覆盖记录数比 TotalRecords 少 len(dir) 个 → 尾部出现空洞。
		for i := range fc.dir {
			fc.dir[i].RecordCount--
			for j := i + 1; j < len(fc.dir); j++ {
				fc.dir[j].FirstRecordOrdinal--
			}
		}
	})
	rc, err := ParseContainer(f.stored, f.hdr, DefaultLimits())
	require.NoError(t, err, "a self-consistent-but-incomplete directory must pass the geometry gate")
	total := rc.Header.TotalRecords
	if _, err := rc.PageFor(total); err == nil || !strings.Contains(err.Error(), "out of range") {
		t.Fatalf("PageFor(%d) = %v, want out-of-range", total, err)
	}
	if _, err := rc.PageFor(total - 1); err == nil || !strings.Contains(err.Error(), "not covered by any page") {
		t.Fatalf("PageFor(%d) = %v, want not-covered-by-any-page", total-1, err)
	}
	// 空洞之外的序号仍要正确定位。
	if pi, err := rc.PageFor(0); err != nil || pi != 0 {
		t.Fatalf("PageFor(0) = %d %v, want page 0", pi, err)
	}
	// 逐页迭代仍受页自身 EntryCount 约束，不得越界读。
	err = rc.ForEach(func(codec.PageRecord) error { return nil })
	require.NoError(t, err, "ForEach over the short directory")
}
