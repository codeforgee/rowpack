package index

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
)

// page_proto_bench_test.go — S3-⑦ 前置原型基准。
//
// 测量决策点 #2（Index Page 2048 vs 4096 条）与 #6（Index Page 是否复用数据压缩
// 级别）。固定种子，构造不计入读/编码计时；每套负载 × 每个 PageSize × 每个 zstd
// level 并列报告 rawB/entry、storedB/entry、encode/decode ns/entry、per-page
// 条目数、Fence 目录字节，并对照现状 chunk delta 编码的 rawB/entry。
//
// 度量口径（设计 §13.3）：delay/throughput/B 并列，禁止只报 ns/op。

const (
	pageBenchSeed = int64(0x52504B32) // 与 benchspec_test.go 的 benchSeed 同值（跨包不可直接引用）
	benchN        = 200_000           // 每套负载构造 20 万行（设计“约 200k–1M 行”）
)

// protoBenchLoad 命名一套固定的排序行索引负载。
type protoBenchLoad uint8

const (
	protoSeq        protoBenchLoad = iota // 顺序：RowID 单调 +1、单表、块连续 run
	protoRandomID                         // 乱序：RowID 固定种子打乱后再排序，块按插入序分散
	protoSmallDelta                       // 小 DELTA：RowID 小幅抖动、变化频繁但值接近
)

func (l protoBenchLoad) String() string {
	switch l {
	case protoSeq:
		return "seq"
	case protoRandomID:
		return "rand"
	case protoSmallDelta:
		return "delta"
	}
	return "?"
}

// blockRows 是每数据块的行数（决定 Index Page 的 BlockID run 边界粒度）。
const blockRows = 4096

// buildProtoRows 构造已按 (TableID, RowID) 升序的 []ProtoRowEntry。
func buildProtoRows(load protoBenchLoad, n int) []ProtoRowEntry {
	switch load {
	case protoSeq:
		rows := make([]ProtoRowEntry, n)
		for i := 0; i < n; i++ {
			rows[i] = ProtoRowEntry{
				TableID:       1,
				RowID:         uint64(i) + 1,
				BlockID:       uint64(i / blockRows),
				RecordOrdinal: uint32(i % blockRows),
				ChangeType:    benchChange(i),
			}
		}
		return rows

	case protoRandomID:
		// geomRandomID 分布：固定种子打乱 1..n；blockID 按插入（打乱）位置分段，
		// 排序后 BlockID 碎片化（index 的难例）。
		rng := rand.New(rand.NewSource(pageBenchSeed))
		ids := make([]uint64, n)
		for i := range ids {
			ids[i] = uint64(i) + 1
		}
		rng.Shuffle(n, func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
		rows := make([]ProtoRowEntry, n)
		for pos, id := range ids {
			rows[pos] = ProtoRowEntry{
				TableID:       1,
				RowID:         id,
				BlockID:       uint64(pos / blockRows),
				RecordOrdinal: uint32(pos % blockRows),
				ChangeType:    benchChange(pos),
			}
		}
		sortProtoRows(rows)
		return rows

	default: // protoSmallDelta
		// RowID 在缓慢递增基准上加小幅抖动（值接近），既有频繁的小 delta，
		// 也有偶发跨块；block 按 rowID 分段使块边界较密。
		rng := rand.New(rand.NewSource(pageBenchSeed))
		rows := make([]ProtoRowEntry, n)
		for i := 0; i < n; i++ {
			jitter := rng.Intn(5) // 0..4
			rowID := uint64(i)*5 + uint64(jitter)
			rows[i] = ProtoRowEntry{
				TableID:       1,
				RowID:         rowID,
				BlockID:       rowID / uint64(blockRows),
				RecordOrdinal: uint32(rowID % uint64(blockRows)),
				ChangeType:    benchChange(i),
			}
		}
		sortProtoRows(rows)
		return rows
	}
}

func benchChange(i int) fileformat.ChangeType {
	switch i % 17 {
	case 3, 10:
		return fileformat.ChangeUpdate
	case 7, 14:
		return fileformat.ChangeDelete
	default:
		return fileformat.ChangeInsert
	}
}

// pageMetrics 汇总一次全量分页编码的字节/页指标。
type pageMetrics struct {
	pages       int
	entries     int
	rawBytes    int64
	storedBytes int64
}

// encodeAllPages 把 rows 按 pageSize 切成页，逐页 zstd 压缩，返回聚合指标。
// 构造（rows）已在调用方准备，不计入计时。
func encodeAllPages(rows []ProtoRowEntry, pageSize, level int) (pageMetrics, error) {
	var m pageMetrics
	m.entries = len(rows)
	for start := 0; start < len(rows); start += pageSize {
		end := start + pageSize
		if end > len(rows) {
			end = len(rows)
		}
		page, _, _, _, err := encodePage(rows[start:end], pageSize)
		if err != nil {
			return m, err
		}
		m.pages++
		m.rawBytes += int64(len(page))
		stored, err := block.Compress(fileformat.CompressionZstd, level, page)
		if err != nil {
			return m, err
		}
		m.storedBytes += int64(len(stored))
	}
	return m, nil
}

// encodePagesForDecode 预编码全部页（不含压缩），供 decode 基准复用（不计时）。
// 返回页列表与聚合指标（raw/stored 由调用方单独提供时使用全量编码指标）。
func encodePagesForDecode(rows []ProtoRowEntry, pageSize int) ([][]byte, error) {
	pages := make([][]byte, 0, (len(rows)+pageSize-1)/pageSize)
	for start := 0; start < len(rows); start += pageSize {
		end := start + pageSize
		if end > len(rows) {
			end = len(rows)
		}
		page, _, _, _, err := encodePage(rows[start:end], pageSize)
		if err != nil {
			return nil, err
		}
		pages = append(pages, page)
	}
	return pages, nil
}

// decodeAllPages 解码全部页，返回还原的总条目数（用于 ns/entry 口径）。
func decodeAllPages(pages [][]byte) (int, error) {
	total := 0
	for _, p := range pages {
		entries, err := decodePage(p)
		if err != nil {
			return total, err
		}
		total += len(entries)
	}
	return total, nil
}

// chunkRawPerEntry 用现状 chunk delta（rowEncoder）对同一输入编码，返回
// rawB/entry（纯 delta 流，不含 chunk 头/目录的固定开销）。
func chunkRawPerEntry(rows []ProtoRowEntry) float64 {
	enc := &rowEncoder{}
	for i := range rows {
		enc.encode(&fileformat.RowIndexEntry{
			TableID:     rows[i].TableID,
			RowID:       rows[i].RowID,
			BlockID:     rows[i].BlockID,
			ItemOrdinal: rows[i].RecordOrdinal,
			ChangeType:  rows[i].ChangeType,
		})
	}
	return float64(len(enc.buf)) / float64(len(rows))
}

// reportPageMetrics 把聚合指标折算为 per-entry/per-page，写入 b.ReportMetric。
// n 非 nil 时额外报告 pages/1M。
func reportPageMetrics(b *testing.B, m pageMetrics, n *int, encNS, decNS float64) {
	entries := float64(m.entries)
	pages := float64(m.pages)
	rawPerEntry := float64(m.rawBytes) / entries
	storedPerEntry := float64(m.storedBytes) / entries
	ratio := float64(m.storedBytes) / float64(m.rawBytes)
	entsPerPage := entries / pages
	fencePerRow := float64(protoFenceSize) / entsPerPage

	b.ReportMetric(rawPerEntry, "rawB/entry")
	b.ReportMetric(storedPerEntry, "storedB/entry")
	b.ReportMetric(ratio, "stored/raw")
	b.ReportMetric(entsPerPage, "entries/page")
	b.ReportMetric(fencePerRow, "fenceB/row")
	b.ReportMetric(float64(protoFenceSize), "fenceB/page")
	if encNS > 0 {
		b.ReportMetric(encNS, "encNS/entry")
	}
	if decNS > 0 {
		b.ReportMetric(decNS, "decNS/entry")
	}
	if n != nil {
		b.ReportMetric(pages/entries*float64(*n), "pages/1M")
	}
}

// BenchmarkIndexPageEncode 编码全量（分页 + zstd level=3），输出 rawB/entry、
// storedB/entry、encNS/entry、entries/page、fence 字节。
func BenchmarkIndexPageEncode(b *testing.B) {
	for _, load := range []protoBenchLoad{protoSeq, protoRandomID, protoSmallDelta} {
		rows := buildProtoRows(load, benchN)
		for _, ps := range []int{2048, 4096} {
			b.Run(fmt.Sprintf("%s/ps%d", load, ps), func(b *testing.B) {
				b.ReportAllocs()
				var m pageMetrics
				var err error
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					m, err = encodeAllPages(rows, ps, fileformat.DefaultCompressionLvl)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				encNS := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(m.entries)
				n := benchN
				reportPageMetrics(b, m, &n, encNS, 0)
			})
		}
	}
}

// BenchmarkIndexPageDecode 解码分页数据（预编码完毕，不参与计时），输出 decNS。
func BenchmarkIndexPageDecode(b *testing.B) {
	for _, load := range []protoBenchLoad{protoSeq, protoRandomID, protoSmallDelta} {
		rows := buildProtoRows(load, benchN)
		for _, ps := range []int{2048, 4096} {
			pages, err := encodePagesForDecode(rows, ps)
			if err != nil {
				b.Fatal(err)
			}
			// 非计时地算一次完整编码，拿到页格式指标（raw/stored/fence）用于并列报告。
			m, err := encodeAllPages(rows, ps, fileformat.DefaultCompressionLvl)
			if err != nil {
				b.Fatal(err)
			}
			b.Run(fmt.Sprintf("%s/ps%d", load, ps), func(b *testing.B) {
				b.ReportAllocs()
				var total int
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					total, err = decodeAllPages(pages)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				decNS := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(total)
				n := benchN
				reportPageMetrics(b, m, &n, 0, decNS)
			})
		}
	}
}

// BenchmarkIndexPageSizes 决策点 #2：同一负载下不同 PageSize 的 rawB/entry、
// storedB/entry、enc/dec ns/entry。取乱序负载作代表（index 难例）。
func BenchmarkIndexPageSizes(b *testing.B) {
	rows := buildProtoRows(protoRandomID, benchN)
	for _, ps := range []int{1024, 2048, 4096, 8192} {
		for _, phase := range []string{"enc", "dec"} {
			b.Run(fmt.Sprintf("ps%d/%s", ps, phase), func(b *testing.B) {
				b.ReportAllocs()
				if phase == "enc" {
					var m pageMetrics
					var err error
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						m, err = encodeAllPages(rows, ps, fileformat.DefaultCompressionLvl)
						if err != nil {
							b.Fatal(err)
						}
					}
					b.StopTimer()
					encNS := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(m.entries)
					n := benchN
					reportPageMetrics(b, m, &n, encNS, 0)
					return
				}
				pages, err := encodePagesForDecode(rows, ps)
				if err != nil {
					b.Fatal(err)
				}
				m, err := encodeAllPages(rows, ps, fileformat.DefaultCompressionLvl)
				if err != nil {
					b.Fatal(err)
				}
				var total int
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					total, err = decodeAllPages(pages)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				decNS := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(total)
				n := benchN
				reportPageMetrics(b, m, &n, 0, decNS)
			})
		}
	}
}

// BenchmarkIndexPageLevel 决策点 #6：同一 PageSize 下不同 zstd level 的
// storedB/entry、encNS/entry（decode 与 level 无关，见 BenchmarkIndexPageDecode）。
func BenchmarkIndexPageLevel(b *testing.B) {
	rows := buildProtoRows(protoRandomID, benchN)
	const ps = 4096
	for _, lvl := range []int{1, 3, 6} {
		b.Run(fmt.Sprintf("lvl%d", lvl), func(b *testing.B) {
			b.ReportAllocs()
			var m pageMetrics
			var err error
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m, err = encodeAllPages(rows, ps, lvl)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			encNS := float64(b.Elapsed().Nanoseconds()) / float64(b.N) / float64(m.entries)
			n := benchN
			reportPageMetrics(b, m, &n, encNS, 0)
		})
	}
}

// BenchmarkIndexPageVsChunkRaw 对照设计编码 vs 现状 chunk delta 的 rawB/entry。
func BenchmarkIndexPageVsChunkRaw(b *testing.B) {
	for _, load := range []protoBenchLoad{protoSeq, protoRandomID, protoSmallDelta} {
		rows := buildProtoRows(load, benchN)
		for _, ps := range []int{2048, 4096} {
			b.Run(fmt.Sprintf("%s/ps%d", load, ps), func(b *testing.B) {
				b.ReportAllocs()
				var m pageMetrics
				var err error
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					m, err = encodeAllPages(rows, ps, fileformat.DefaultCompressionLvl)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				designRaw := float64(m.rawBytes) / float64(m.entries)
				chunkRaw := chunkRawPerEntry(rows)
				b.ReportMetric(designRaw, "designRawB/entry")
				b.ReportMetric(chunkRaw, "chunkRawB/entry")
				b.ReportMetric(designRaw-chunkRaw, "deltaB/entry")
				b.ReportMetric(designRaw/chunkRaw, "ratio(design/chunk)")
			})
		}
	}
}
