package rowpack

import (
	"context"
	"fmt"
	"os"

	"github.com/rowpack/rowpack/internal/block"
	"github.com/rowpack/rowpack/internal/fileformat"
	"github.com/rowpack/rowpack/internal/index"
	"github.com/rowpack/rowpack/internal/iofile"
	"github.com/rowpack/rowpack/internal/lockfile"
)

// RebuildOptions configures RebuildIndex.
type RebuildOptions struct {
	ReplaceCorrupt bool
	Durability     Durability
}

// RebuildIndex rebuilds the .rpi index file from the authoritative .rpk data
// file. It writes a temporary file, syncs it, and atomically renames it over
// the existing index (never editing in place). The data file is scanned for
// all committed snapshots and a fresh index transaction is written for each.
func RebuildIndex(ctx context.Context, basePath string, opts RebuildOptions) error {
	if ctx != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
	dataPath, indexPath, err := pairPaths(basePath)
	if err != nil {
		return err
	}
	if !iofile.Exists(dataPath) {
		return fmt.Errorf("%w: data file %s", ErrNotFound, dataPath)
	}

	// Cross-process writer lock so no live writer races the rebuild.
	lock, err := lockfile.Acquire(basePath + ".lock")
	if err != nil {
		return err
	}
	defer lock.Release()

	dataAppender, err := iofile.OpenAppender(dataPath, false)
	if err != nil {
		return fmt.Errorf("rowpack: open data: %w", err)
	}
	defer dataAppender.Close()

	// Read and validate the data header.
	var dhBuf [fileformat.DataFileHeaderSize]byte
	if _, err := dataAppender.ReadAt(dhBuf[:], 0); err != nil {
		return err
	}
	var dh fileformat.DataFileHeader
	if err := dh.Unmarshal(dhBuf[:]); err != nil {
		return err
	}

	// Reuse the store scanner by opening the data file through a minimal
	// store-like reader. We can't use Open because the index is the thing
	// being rebuilt, so drive the scanner directly.
	s := &Store{
		dataPath:  dataPath,
		indexPath: indexPath,
		data:      dataAppender,
	}
	s.reader = block.NewReader(dataAppender, block.Limits{
		MaxRawBytes:    fileformat.DefaultMaxRawBlockBytes,
		MaxStoredBytes: fileformat.DefaultMaxStoredBlockBytes,
	})
	s.loader = newBlockLoader(s.reader, 0)
	committed, _, err := s.scanDataFile()
	if err != nil {
		return err
	}
	// Validate the parent chain ordering and depth.
	view := index.EmptyView()
	for _, c := range committed {
		txn, err := s.buildIndexTxnFromData(&c)
		if err != nil {
			return fmt.Errorf("rowpack: rebuild snapshot %d: %w", c.snapshotID, err)
		}
		if _, err := view.Apply(txn, fileformat.DefaultMaxSnapshotDepth); err != nil {
			return fmt.Errorf("rowpack: snapshot %d chain invalid: %w", c.snapshotID, err)
		}
	}

	// Write a fresh index to a temp file and atomically rename.
	tmp := indexPath + ".rebuild.tmp"
	_ = os.Remove(tmp)
	inf, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("rowpack: create temp index: %w", err)
	}
	cleanup := func() { inf.Close(); os.Remove(tmp) }
	var ihBuf [fileformat.IndexFileHeaderSize]byte
	var ih fileformat.IndexFileHeader
	ih.FileHeader = dh.FileHeader
	if err := ih.MarshalTo(ihBuf[:]); err != nil {
		cleanup()
		return err
	}
	if _, err := inf.Write(ihBuf[:]); err != nil {
		cleanup()
		return err
	}
	off := int64(fileformat.IndexFileHeaderSize)
	seq := uint64(0)
	for _, c := range committed {
		txn, err := s.buildIndexTxnFromData(&c)
		if err != nil {
			cleanup()
			return err
		}
		seq++
		b := marshalTxn(txn, seq, off)
		if _, err := inf.Write(b); err != nil {
			cleanup()
			return err
		}
		off += int64(len(b))
	}
	if opts.Durability == SyncCommit {
		if err := inf.Sync(); err != nil {
			cleanup()
			return err
		}
	}
	if err := inf.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, indexPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rowpack: replace index: %w", err)
	}
	return nil
}
