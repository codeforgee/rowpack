// Package rowpack implements the RowPack single-file embedded table storage
// engine.
//
// RowPack stores two-dimensional table data, its version history and database
// metadata in one append-only file <base>.rpk: data blocks and the per-snapshot
// IndexTxn stream share the single file and are committed together by the
// extended SnapshotFooter (BINARY_FORMAT_V1.md). There is no separate index
// file, no UUID pairing and no cross-file recovery.
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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codeforgee/rowpack/internal/block"
	"github.com/codeforgee/rowpack/internal/format"
	"github.com/codeforgee/rowpack/internal/index"
	"github.com/codeforgee/rowpack/internal/iofile"
	"github.com/codeforgee/rowpack/internal/lockfile"
	"github.com/codeforgee/rowpack/internal/seal"
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
	header   format.DataFileHeader
	readOnly bool

	// encCipher seals block payloads on the write path (single writer, so no
	// concurrency). decrypter authenticates and decrypts on the read path
	// (cached per epoch, concurrent-safe). Both are non-nil only for
	// encrypted stores and are built during initOpen from the on-disk header.
	encCipher *seal.Cipher
	decrypter *decrypter

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

	// mustReopen latches when a commit fails with an unknown outcome
	// (failure at or after the durability sync): the file may already contain
	// the snapshot while the in-memory view does not. New writers are refused
	// with ErrMustReopen until Close+Open replays the file and realigns the
	// view. Reads stay allowed and self-consistent.
	mustReopen atomic.Bool

	lastSnapshotID atomic.Uint64
	lastBlockID    atomic.Uint64
	maxTableID     atomic.Uint32
	maxObjectID    atomic.Uint64
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

	// oversizedPages counts Rows-page-container pages that hold a single
	// record larger than the page target (Flags bit 0), accumulated on the
	// write path.
	oversizedPages atomic.Uint64

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

// Create creates a new empty single-file store at basePath (no extension). It
// fails with ErrInvalidPath if basePath ends in .rpk or .rpi, and never
// overwrites existing files.
func Create(basePath string, opts Options) (*Store, error) {
	resolved, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	truncate := resolved.Truncate
	basePath, dataPath, err := storePaths(basePath)
	if err != nil {
		return nil, err
	}
	uuid, err := effectiveUUID()
	if err != nil {
		return nil, fmt.Errorf("rowpack: generate uuid: %w", err)
	}
	now := effectiveNow()
	dataHdr := format.DataFileHeader{}
	dataHdr.FileHeader = format.FileHeader{
		StoreUUID:          uuid,
		CreatedUnixNano:    now,
		RequiredFeatures:   format.RequiredFeaturesV1,
		DefaultBlockSize:   uint32(resolved.BlockSize),
		DefaultCompression: resolved.diskCompression(),
		DefaultRowEncoding: format.RowEncodingTypedTuple,
	}
	// Encryption is fixed at Create: resolve the initial key and stamp the
	// header. A plain store keeps all encryption bytes zero.
	encCipher, err := newEncryptor(resolved.Encryption)
	if err != nil {
		return nil, err
	}
	if encCipher != nil {
		dataHdr.EncryptionAlgorithm = format.EncAES256GCM
		dataHdr.NonceScheme = format.NonceCounterV1
		dataHdr.KeyID = []byte(resolved.Encryption.KeyID)
	}

	var dh [format.DataFileHeaderSize]byte
	if err := dataHdr.MarshalTo(dh[:]); err != nil {
		return nil, err
	}
	if err := iofile.CreateSingle(dataPath, dh[:], truncate); err != nil {
		return nil, err
	}
	s, err := openStore(basePath, dataPath, resolved, uuid, dataHdr, false)
	if err != nil {
		if removeErr := os.Remove(dataPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return nil, errors.Join(err, fmt.Errorf("rowpack: clean up failed create %s: %w", dataPath, removeErr))
		}
		return nil, err
	}
	return s, nil
}

// Open opens an existing single-file store read-write (or read-only with
// opts.ReadOnly).
func Open(basePath string, opts Options) (*Store, error) {
	resolved, err := opts.resolved()
	if err != nil {
		return nil, err
	}
	basePath, dataPath, err := storePaths(basePath)
	if err != nil {
		return nil, err
	}
	if !iofile.Exists(dataPath) {
		return nil, fmt.Errorf("%w: missing store file %s", ErrNotFound, dataPath)
	}
	return openStore(basePath, dataPath, resolved, [16]byte{}, format.DataFileHeader{}, resolved.ReadOnly)
}

func openStore(basePath, dataPath string, opts Options, uuid [16]byte, header format.DataFileHeader, readOnly bool) (*Store, error) {
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
	s.loader = newLoader(s.reader, dataPath, opts.CacheBytes, opts.ScanCacheBytes)
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
			_ = s.lock.Release()
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
// idempotent and safe to call concurrently. In-flight reads are allowed to
// finish: open iterators (Scan/ScanBlocks) hold the read lock until their
// Close, so a leaked iterator is only released once the GC runs its
// finalizer — always defer Iterator.Close. New writers are refused with
// ErrMustReopen once a commit has failed with an unknown outcome; reopening
// the store runs recovery and realigns the in-memory view with the file.
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

// readHeader reads and validates the single-file header.
func (s *Store) readHeader() (format.DataFileHeader, error) {
	var h format.DataFileHeader
	buf := make([]byte, format.DataFileHeaderSize)
	if _, err := s.data.ReadAt(buf, 0); err != nil {
		return h, fmt.Errorf("rowpack: read data header: %w", err)
	}
	if err := h.Unmarshal(buf); err != nil {
		if format.IsVersionError(err) {
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
	dataHdr, err := s.readHeader()
	if err != nil {
		return err
	}
	if err := dataHdr.CheckVersion(); err != nil {
		return fmt.Errorf("%w: %v", ErrVersionUnsupported, err)
	}
	s.uuid = dataHdr.StoreUUID
	s.header = dataHdr
	// Creation-time defaults are file properties. An Open caller may tune
	// runtime-only options, but must not silently change future block geometry
	// or compression within an existing store.
	if dataHdr.DefaultBlockSize < 64 || dataHdr.DefaultBlockSize > s.opts.Limits.MaxRawBlockBytes {
		return fmt.Errorf("%w: default block size %d outside [64,%d]", ErrCorruptData, dataHdr.DefaultBlockSize, s.opts.Limits.MaxRawBlockBytes)
	}
	s.opts.BlockSize = int(dataHdr.DefaultBlockSize)
	if s.opts.PageSize > s.opts.BlockSize {
		s.opts.PageSize = s.opts.BlockSize
	}
	switch dataHdr.DefaultCompression {
	case format.CompressionNone:
		s.opts.Compression = CompressionNone
	case format.CompressionZstd:
		s.opts.Compression = CompressionZstd
	default:
		return fmt.Errorf("%w: default compression %d", ErrVersionUnsupported, dataHdr.DefaultCompression)
	}
	if err := s.setupEncryption(dataHdr); err != nil {
		return err
	}
	return s.recover()
}

// setupEncryption wires the read (and write) crypto for an encrypted store and
// enforces the key contract: an encrypted store opened without a KeyProvider
// fails with ErrKeyRequired instead of entering a half-usable state.
func (s *Store) setupEncryption(dataHdr format.DataFileHeader) error {
	if dataHdr.EncryptionAlgorithm == format.EncNone {
		return nil
	}
	if dataHdr.EncryptionAlgorithm != format.EncAES256GCM {
		return fmt.Errorf("%w: encryption algorithm %d", ErrVersionUnsupported, dataHdr.EncryptionAlgorithm)
	}
	if dataHdr.NonceScheme != format.NonceCounterV1 {
		return fmt.Errorf("%w: nonce scheme %d", ErrVersionUnsupported, dataHdr.NonceScheme)
	}
	if s.opts.Encryption == nil || s.opts.Encryption.KeyProvider == nil {
		return fmt.Errorf("%w: store %q is encrypted (key id %q)", ErrKeyRequired, s.basePath, dataHdr.KeyID)
	}
	keyID := string(dataHdr.KeyID)
	s.decrypter = newDecrypter(s.opts.Encryption.KeyProvider, keyID, dataHdr.StoreUUID)
	s.reader.SetDecrypter(s.decrypter)
	// Write path (read-write opens only): resolve the initial key up front so
	// provider failures surface at Open, not at the first commit.
	if !s.readOnly {
		c, err := s.decrypter.cipherFor(0)
		if err != nil {
			return err
		}
		s.encCipher = c
	}
	return nil
}

// storePaths resolves the logical base path and the single data file path
// from a caller-supplied base path. Base paths may be passed with or without
// the .rpk extension (a trailing .rpk or .rpi is stripped), so callers can
// hand over either the logical store name or the physical file; the extension
// is appended exactly once.
func storePaths(basePath string) (base, data string, err error) {
	base, err = baseOf(basePath)
	if err != nil {
		return "", "", err
	}
	return base, base + ".rpk", nil
}

// baseOf resolves the logical base path: filepath.Clean with a trailing
// .rpk/.rpi extension stripped.
func baseOf(basePath string) (string, error) {
	base := strings.TrimSuffix(strings.TrimSuffix(filepath.Clean(basePath), ".rpi"), ".rpk")
	if base == "" {
		return "", fmt.Errorf("%w: base path %q is empty", ErrInvalidPath, basePath)
	}
	return base, nil
}
