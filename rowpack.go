// Package rowpack implements the RowPack v1 embedded table storage engine.
//
// RowPack stores two-dimensional table data, its version history and database
// metadata in a pair of append-only files:
//
//   - <base>.rpk: the data file, the authoritative source of committed facts.
//   - <base>.rpi: the derived navigation index, rebuildable from the data file.
//
// The on-disk format is fixed by the v1 binary and metadata specifications
// (BINARY_FORMAT_V1.md, METADATA_FORMAT_V1.md). The format version is frozen
// as Major=1, Minor=0 (see internal/fileformat): unknown major versions are
// rejected when opening a store, and higher minor versions are only opened
// when all required feature bits are recognized.
package rowpack

import (
	"crypto/rand"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/iofile"
	"github.com/rowpack/rowpack/internal/lockfile"
)

// Test-only hooks, set only by tests in this package so golden files are
// byte-deterministic. The public API surface is unchanged.
var (
	testNowOverride  int64
	testUUIDOverride *[16]byte
)

func effectiveNow() int64 {
	if testNowOverride != 0 {
		return testNowOverride
	}
	return time.Now().UTC().UnixNano()
}

func effectiveUUID() ([16]byte, error) {
	if testUUIDOverride != nil {
		return *testUUIDOverride, nil
	}
	var uuid [16]byte
	if _, err := rand.Read(uuid[:]); err != nil {
		return uuid, err
	}
	return uuid, nil
}

// publishedState is the atomically-published combination of the immutable
// index view and the derived schema index. Readers load it once.
type publishedState struct {
	view    *index.View
	schemas *schemaIndex
}

// Store is a RowPack store backed by <base>.rpk and <base>.rpi.
type Store struct {
	basePath  string
	dataPath  string
	indexPath string
	opts      Options

	uuid     [16]byte
	header   fileformat.DataFileHeader
	readOnly bool

	data   *iofile.Appender
	index  *iofile.Appender
	reader *block.Reader
	loader *blockLoader
	lock   *lockfile.Lock

	// writeMu serializes all writers, Commit and Close.
	writeMu sync.Mutex
	// readMu guards Close against in-flight reads (RLock per read op).
	readMu sync.RWMutex
	state  atomic.Pointer[publishedState]
	writer atomic.Pointer[SnapshotWriter]
	closed atomic.Bool

	lastSnapshotID atomic.Uint64
	lastBlockID    atomic.Uint64
	txnSeq         atomic.Uint64

	// zstdEnc is the store-level persistent zstd encoder for the write path.
	// A store has at most one active writer and block flushing is sequential,
	// so the encoder is never used concurrently; owning it (instead of going
	// through the sync.Pool) keeps the ~1 MiB encoder histogram allocated
	// across GC cycles, which would otherwise clear the pool and force a
	// re-allocation every few blocks. Guarded by zstdEncMu; released on Close.
	zstdEncMu sync.Mutex
	zstdEnc   *block.ZstdEncoder

	recoveryStats atomic.Value // holds recoveryReport
}

// Create creates a new empty store at basePath (no extension). It fails with
// ErrInvalidPath if basePath ends in .rpk or .rpi, and never overwrites
// existing files.
func Create(basePath string, opts Options) (*Store, error) {
	resolved, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	dataPath, indexPath, err := pairPaths(basePath)
	if err != nil {
		return nil, err
	}
	uuid, err := effectiveUUID()
	if err != nil {
		return nil, fmt.Errorf("rowpack: generate uuid: %w", err)
	}
	now := effectiveNow()
	dataHdr := fileformat.DataFileHeader{}
	dataHdr.FileHeader = fileformat.FileHeader{
		StoreUUID:          uuid,
		CreatedUnixNano:    now,
		RequiredFeatures:   fileformat.RequiredFeaturesV1,
		DefaultBlockSize:   uint32(resolved.BlockSize),
		DefaultCompression: resolved.diskCompression(),
		DefaultRowEncoding: fileformat.RowEncodingTypedTuple,
	}
	idxHdr := fileformat.IndexFileHeader{}
	idxHdr.FileHeader = dataHdr.FileHeader

	var dh [fileformat.DataFileHeaderSize]byte
	if err := dataHdr.MarshalTo(dh[:]); err != nil {
		return nil, err
	}
	var ih [fileformat.IndexFileHeaderSize]byte
	if err := idxHdr.MarshalTo(ih[:]); err != nil {
		return nil, err
	}
	if err := iofile.CreatePair(dataPath, indexPath, dh[:], ih[:]); err != nil {
		return nil, err
	}
	return openFiles(basePath, dataPath, indexPath, resolved, uuid, dataHdr, false)
}

// Open opens an existing store read-write (or read-only with opts.ReadOnly).
func Open(basePath string, opts Options) (*Store, error) {
	resolved, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	dataPath, indexPath, err := pairPaths(basePath)
	if err != nil {
		return nil, err
	}
	if !iofile.Exists(dataPath) || !iofile.Exists(indexPath) {
		return nil, fmt.Errorf("%w: missing data or index file (%s, %s)", ErrNotFound, dataPath, indexPath)
	}
	return openFiles(basePath, dataPath, indexPath, resolved, [16]byte{}, fileformat.DataFileHeader{}, resolved.ReadOnly)
}

func openFiles(basePath, dataPath, indexPath string, opts Options, uuid [16]byte, header fileformat.DataFileHeader, readOnly bool) (*Store, error) {
	df, err := iofile.OpenAppender(dataPath, false)
	if err != nil {
		return nil, fmt.Errorf("rowpack: open data file: %w", err)
	}
	inf, err := iofile.OpenAppender(indexPath, false)
	if err != nil {
		df.Close()
		return nil, fmt.Errorf("rowpack: open index file: %w", err)
	}
	s := &Store{
		basePath:  basePath,
		dataPath:  dataPath,
		indexPath: indexPath,
		opts:      opts,
		readOnly:  readOnly,
		data:      df,
		index:     inf,
	}
	s.reader = block.NewReader(df, block.Limits{
		MaxRawBytes:    opts.Limits.MaxRawBlockBytes,
		MaxStoredBytes: opts.Limits.MaxStoredBlockBytes,
	})
	s.loader = newBlockLoader(s.reader, opts.CacheBytes)
	s.uuid = uuid
	s.header = header
	// Cross-process single-writer lock for read-write opens.
	if !readOnly {
		lock, err := lockfile.Acquire(basePath + ".lock")
		if err != nil {
			df.Close()
			inf.Close()
			return nil, err
		}
		s.lock = lock
	}
	if err := s.initOpen(); err != nil {
		df.Close()
		inf.Close()
		if s.lock != nil {
			s.lock.Release()
		}
		return nil, err
	}
	return s, nil
}

// Path returns the store base path.
func (s *Store) Path() string { return s.basePath }

// ReadOnly reports whether the store was opened read-only.
func (s *Store) ReadOnly() bool { return s.readOnly }

// UUID returns the store's pairing UUID shared by the .rpk and .rpi files.
func (s *Store) UUID() [16]byte { return s.uuid }

// Close aborts any active writer, flushes, and closes the files. It is
// idempotent and safe to call concurrently; in-flight reads are allowed to
// finish.
// zstdEncoder lazily creates and returns the store's persistent zstd
// encoder. Callers must hold the writer slot (single writer) or otherwise
// serialize writes; concurrent EncodeAll is safe but internally serialized.
func (s *Store) zstdEncoder() *block.ZstdEncoder {
	s.zstdEncMu.Lock()
	defer s.zstdEncMu.Unlock()
	if s.zstdEnc == nil {
		s.zstdEnc = block.NewZstdEncoder(s.opts.CompressionLevel)
	}
	return s.zstdEnc
}

// releaseZstdEncoder closes the store's persistent encoder, if any.
func (s *Store) releaseZstdEncoder() {
	s.zstdEncMu.Lock()
	defer s.zstdEncMu.Unlock()
	if s.zstdEnc != nil {
		s.zstdEnc.Close()
		s.zstdEnc = nil
	}
}

func (s *Store) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	// Wait for in-flight reads to finish.
	s.readMu.Lock()
	defer s.readMu.Unlock()

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	var errs []error
	if w := s.writer.Load(); w != nil {
		if err := w.abort(); err != nil && !errors.Is(err, ErrSnapshotCommitted) {
			errs = append(errs, err)
		}
	}
	if err := s.data.Close(); err != nil {
		errs = append(errs, err)
	}
	if err := s.index.Close(); err != nil {
		errs = append(errs, err)
	}
	s.releaseZstdEncoder()
	s.state.Store(nil)
	if s.lock != nil {
		if err := s.lock.Release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Store) readDataHeader() (fileformat.DataFileHeader, error) {
	var h fileformat.DataFileHeader
	buf := make([]byte, fileformat.DataFileHeaderSize)
	if _, err := s.data.ReadAt(buf, 0); err != nil {
		return h, fmt.Errorf("rowpack: read data header: %w", err)
	}
	if err := h.Unmarshal(buf); err != nil {
		if fileformat.IsVersionError(err) {
			return h, fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
		}
		return h, err
	}
	return h, nil
}

func (s *Store) readIndexHeader() (fileformat.IndexFileHeader, error) {
	var h fileformat.IndexFileHeader
	buf := make([]byte, fileformat.IndexFileHeaderSize)
	if _, err := s.index.ReadAt(buf, 0); err != nil {
		return h, fmt.Errorf("rowpack: read index header: %w", err)
	}
	if err := h.Unmarshal(buf); err != nil {
		if fileformat.IsVersionError(err) {
			return h, fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
		}
		return h, err
	}
	return h, nil
}

func (s *Store) checkOpen() error {
	if s.closed.Load() {
		return ErrClosed
	}
	return nil
}

func (s *Store) initOpen() error {
	// Read both headers and verify pairing.
	dataHdr, err := s.readDataHeader()
	if err != nil {
		return err
	}
	idxHdr, err := s.readIndexHeader()
	if err != nil {
		return err
	}
	if dataHdr.StoreUUID != idxHdr.StoreUUID {
		return fmt.Errorf("%w: data uuid %x index uuid %x", ErrStoreMismatch, dataHdr.StoreUUID, idxHdr.StoreUUID)
	}
	if err := dataHdr.CheckVersion(); err != nil {
		return fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
	}
	if err := idxHdr.CheckVersion(); err != nil {
		return fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
	}
	s.uuid = dataHdr.StoreUUID
	s.header = dataHdr
	return s.recover()
}

// dataFooterVerifier supplies the data footer CRC during index replay.
type dataFooterVerifier struct {
	store *Store
}

func (v *dataFooterVerifier) DataFooterCRC(snapshotID uint64, dataStart, dataEnd uint64) (uint32, error) {
	if dataEnd < dataStart+fileformat.SnapshotFooterSize {
		return 0, index.ErrDataFooterMismatch
	}
	footerOff := int64(dataEnd - fileformat.SnapshotFooterSize)
	var fb [fileformat.SnapshotFooterSize]byte
	if _, err := v.store.data.ReadAt(fb[:], footerOff); err != nil {
		return 0, index.ErrDataFooterMismatch
	}
	var f fileformat.SnapshotFooter
	if err := f.Unmarshal(fb[:]); err != nil {
		return 0, index.ErrDataFooterMismatch
	}
	if f.SnapshotID != snapshotID {
		return 0, index.ErrDataFooterMismatch
	}
	return le32(fb[84:]), nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// footerCRCValue returns the stored SnapshotFooter FooterCRC32C (offset 84).
func footerCRCValue(fb []byte) uint32 { return le32(fb[84:]) }

func pairPaths(basePath string) (string, string, error) {
	if strings.HasSuffix(basePath, ".rpk") || strings.HasSuffix(basePath, ".rpi") {
		return "", "", fmt.Errorf("%w: base path %q must not carry an extension", ErrInvalidPath, basePath)
	}
	clean := filepath.Clean(basePath)
	return clean + ".rpk", clean + ".rpi", nil
}
