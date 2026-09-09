// Package rowpack implements the RowPack single-file embedded table storage
// engine.
//
// RowPack stores two-dimensional table data, its version history and database
// metadata in one append-only file <base>.rpk: data blocks and the per-snapshot
// IndexTxn stream share the single file and are committed together by the
// extended SnapshotFooter (BINARY_FORMAT_V2.md). There is no separate index
// file, no UUID pairing and no cross-file recovery.
//
// The on-disk format is fixed by the v2 binary and metadata specifications
// (BINARY_FORMAT_V2.md, METADATA_FORMAT_V1.md). The format version is frozen
// as Major=2, Minor=0 (see internal/fileformat): unknown major versions are
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
	"github.com/rowpack/rowpack/internal/cache"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/iofile"
	"github.com/rowpack/rowpack/internal/lockfile"
	"github.com/rowpack/rowpack/internal/seal"
)

// Test-only hooks, set only by tests in this package so golden files are
// byte-deterministic. The public API surface is unchanged.
var (
	testNowOverride   int64
	testUUIDOverride  *[16]byte
	testNonceOverride *uint64
)

func effectiveNow() int64 {
	if testNowOverride != 0 {
		return testNowOverride
	}
	return time.Now().UTC().UnixNano()
}

// effectiveWriterNonce returns the SnapshotHeader.WriterNonce value.
func effectiveWriterNonce() uint64 {
	if testNonceOverride != nil {
		return *testNonceOverride
	}
	return randUint64()
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

// Store is a RowPack store backed by the single file <base>.rpk.
type Store struct {
	basePath string
	dataPath string
	opts     Options

	uuid     [16]byte
	header   fileformat.DataFileHeader
	readOnly bool

	// encCipher seals block payloads on the write path (single writer, so no
	// concurrency). decrypter authenticates and decrypts on the read path
	// (cached per epoch, concurrent-safe). Both are non-nil only for
	// encrypted stores and are built during initOpen from the on-disk header.
	encCipher *seal.Cipher
	decrypter *storeDecrypter

	data   *iofile.Appender
	reader *block.Reader
	loader *blockLoader
	lock   *lockfile.Lock

	// writeMu serializes all writers, Commit and Close.
	writeMu sync.Mutex
	// readMu guards Close against in-flight reads (RLock per read op).
	readMu sync.RWMutex
	state  atomic.Pointer[publishedState]
	writer atomic.Pointer[Writer]
	closed atomic.Bool

	lastSnapshotID atomic.Uint64
	lastBlockID    atomic.Uint64
	txnSeq         atomic.Uint64
	// lastFooterOffset is the file offset of the most recently committed
	// SnapshotFooter. Written under writeMu / recover; read by Commit to fill
	// the next footer's PreviousFooterOffset.
	lastFooterOffset uint64

	// ReadBatch cumulative counters (see Stats.Batch).
	batchCalls    atomic.Uint64
	batchRows     atomic.Uint64
	batchBlocks   atomic.Uint64
	batchRawBytes atomic.Uint64

	// zstdEnc is the store-level persistent zstd encoder for the write path.
	// A store has at most one active writer and block flushing is sequential,
	// so the encoder is never used concurrently; owning it (instead of going
	// through the sync.Pool) keeps the ~1 MiB encoder histogram allocated
	// across GC cycles, which would otherwise clear the pool and force a
	// re-allocation every few blocks. Guarded by zstdEncMu; released on Close.
	zstdEncMu sync.Mutex
	zstdEnc   *block.ZstdEncoder

	recoveryStats atomic.Value // holds recoveryReport

	// lazyPages maps snapshot ID -> the per-txn context a Lazy view needs to
	// decode an Index Page on demand (absolute file start of the txn body, the
	// chunk count = page-seal base, and the chunk-crypto context). Populated
	// during recover only when Options.IndexMode == IndexLazy.
	lazyPages map[uint64]lazyPageInfo
	// idxPageSF merges concurrent index page load misses (singleflight).
	idxPageSF cache.Group
}

// lazyPageInfo is the per-snapshot context the store's LazySource uses to load
// and decode a Row Index Page on demand: the absolute offset of the txn body in
// the file, the chunk count (page i seals / opens as chunk Seq+i), and the
// chunk-crypto context (non-nil for plain stores with a nil crypto).
type lazyPageInfo struct {
	txnStart int64
	seq      uint32
	crypto   *index.ChunkCrypto
}

// Create creates a new empty single-file store at basePath (no extension). It
// fails with ErrInvalidPath if basePath ends in .rpk or .rpi, and never
// overwrites existing files.
func Create(basePath string, opts Options) (*Store, error) {
	resolved, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	dataPath, err := dataPathOf(basePath)
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
	// Encryption is fixed at Create: resolve the initial key and stamp the
	// header. A plain store keeps all encryption bytes zero.
	encCipher, err := buildEncryptor(resolved.Encryption)
	if err != nil {
		return nil, err
	}
	if encCipher != nil {
		dataHdr.EncryptionAlgorithm = fileformat.EncAES256GCM
		dataHdr.NonceScheme = fileformat.NonceCounterV1
		dataHdr.KeyID = []byte(resolved.Encryption.KeyID)
	}

	var dh [fileformat.DataFileHeaderSize]byte
	if err := dataHdr.MarshalTo(dh[:]); err != nil {
		return nil, err
	}
	if err := iofile.CreateSingle(dataPath, dh[:]); err != nil {
		return nil, err
	}
	return openStore(basePath, dataPath, resolved, uuid, dataHdr, false)
}

// Open opens an existing single-file store read-write (or read-only with
// opts.ReadOnly).
func Open(basePath string, opts Options) (*Store, error) {
	resolved, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	dataPath, err := dataPathOf(basePath)
	if err != nil {
		return nil, err
	}
	if !iofile.Exists(dataPath) {
		return nil, fmt.Errorf("%w: missing store file %s", ErrNotFound, dataPath)
	}
	return openStore(basePath, dataPath, resolved, [16]byte{}, fileformat.DataFileHeader{}, resolved.ReadOnly)
}

func openStore(basePath, dataPath string, opts Options, uuid [16]byte, header fileformat.DataFileHeader, readOnly bool) (*Store, error) {
	df, err := iofile.OpenAppender(dataPath, false)
	if err != nil {
		return nil, fmt.Errorf("rowpack: open store file: %w", err)
	}
	s := &Store{
		basePath: basePath,
		dataPath: dataPath,
		opts:     opts,
		readOnly: readOnly,
		data:     df,
	}
	s.reader = block.NewReader(df, block.Limits{
		MaxRawBytes:    opts.Limits.MaxRawBlockBytes,
		MaxStoredBytes: opts.Limits.MaxStoredBlockBytes,
	})
	// Only the Lazy index mode keeps a bounded decoded Index Page cache; the
	// Eager default (zero value) materializes the whole row index at Open and
	// never re-reads a page.
	indexBytes := int64(-1)
	if opts.IndexMode == IndexLazy {
		indexBytes = opts.IndexCacheBytes
	}
	s.loader = newBlockLoader(s.reader, dataPath, opts.CacheBytes, opts.ScanCacheBytes, indexBytes)
	s.uuid = uuid
	s.header = header
	// Cross-process single-writer lock for read-write opens.
	if !readOnly {
		lock, err := lockfile.Acquire(basePath + ".lock")
		if err != nil {
			df.Close()
			return nil, err
		}
		s.lock = lock
	}
	if err := s.initOpen(); err != nil {
		df.Close()
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

// UUID returns the store's identity, used as cache key and encryption AAD
// domain seed. Single-file stores carry one UUID in the header.
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
	s.releaseZstdEncoder()
	s.state.Store(nil)
	if s.lock != nil {
		if err := s.lock.Release(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// readDataHeader reads and validates the single-file header.
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

// checkOpen reports ErrClosed after Close.
func (s *Store) checkOpen() error {
	if s.closed.Load() {
		return ErrClosed
	}
	return nil
}

func (s *Store) initOpen() error {
	// Read and validate the single-file header.
	dataHdr, err := s.readDataHeader()
	if err != nil {
		return err
	}
	if err := dataHdr.CheckVersion(); err != nil {
		return fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
	}
	s.uuid = dataHdr.StoreUUID
	s.header = dataHdr
	if err := s.initEncryption(dataHdr); err != nil {
		return err
	}
	return s.recover()
}

// initEncryption wires the read (and write) crypto for an encrypted store and
// enforces the key contract: an encrypted store opened without a KeyProvider
// fails with ErrKeyRequired instead of entering a half-usable state.
func (s *Store) initEncryption(dataHdr fileformat.DataFileHeader) error {
	if dataHdr.EncryptionAlgorithm == fileformat.EncNone {
		return nil
	}
	if dataHdr.EncryptionAlgorithm != fileformat.EncAES256GCM {
		return fmt.Errorf("%w: encryption algorithm %d", ErrVersionUnsupported, dataHdr.EncryptionAlgorithm)
	}
	if dataHdr.NonceScheme != fileformat.NonceCounterV1 {
		return fmt.Errorf("%w: nonce scheme %d", ErrVersionUnsupported, dataHdr.NonceScheme)
	}
	if s.opts.Encryption == nil || s.opts.Encryption.KeyProvider == nil {
		return fmt.Errorf("%w: store %q is encrypted (key id %q)", ErrKeyRequired, s.basePath, dataHdr.KeyID)
	}
	keyID := string(dataHdr.KeyID)
	s.decrypter = newStoreDecrypter(s.opts.Encryption.KeyProvider, keyID, dataHdr.StoreUUID)
	s.reader.SetDecrypter(s.decrypter)
	// Write path (read-write opens only): resolve the initial key up front so
	// provider failures surface at Open, not at the first commit.
	if !s.readOnly {
		c, err := s.decrypter.Cipher(0)
		if err != nil {
			return err
		}
		s.encCipher = c
	}
	return nil
}

func le32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// footerCRCValue returns the stored SnapshotFooter FooterCRC32C.
func footerCRCValue(fb []byte) uint32 { return le32(fb[fileformat.SnapshotFooterCRC32COffset:]) }

// dataPathOf resolves the single store file path. Like v1, base paths
// carrying a .rpk/.rpi extension are rejected so a store never ends up at
// double-extension paths (R21; final `.rpk`-suffix policy is a M0 decision).
func dataPathOf(basePath string) (string, error) {
	if strings.HasSuffix(basePath, ".rpk") || strings.HasSuffix(basePath, ".rpi") {
		return "", fmt.Errorf("%w: base path %q must not carry an extension", ErrInvalidPath, basePath)
	}
	return filepath.Clean(basePath) + ".rpk", nil
}
