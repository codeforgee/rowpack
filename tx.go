package rowpack

import (
	"context"
	"fmt"
)

// NoParent starts a complete baseline snapshot when passed to Begin.
const NoParent SnapshotID = 0

// Latest asks Begin to use the latest committed snapshot as its parent.
// It is invalid for an empty store; use NoParent for the first snapshot.
const Latest SnapshotID = ^SnapshotID(0)

// Batch change kinds. The shorter names are intended for Change literals;
// ChangeInsert/ChangeUpdate/ChangeDelete remain the on-disk vocabulary.
const (
	Insert ChangeType = ChangeInsert
	Update ChangeType = ChangeUpdate
	Delete ChangeType = ChangeDelete
)

// Change is one row mutation consumed by Tx.Apply or Tx.ApplyBatch. Row must
// be nil for ChangeDelete and must contain the complete row for insert/update.
type Change struct {
	Type  ChangeType
	Table string
	RowID RowID
	Row   Row
}

// Tx is a streaming snapshot transaction. It accepts changes incrementally;
// callers do not need to retain their source rows or Change batches after a
// method returns.
type Tx struct {
	w   *Writer
	ctx context.Context
}

// Begin starts a snapshot transaction. NoParent creates a FULL baseline;
// any committed snapshot ID creates a DELTA based on that snapshot. Latest
// selects the latest committed snapshot.
func (s *Store) Begin(ctx context.Context, parent SnapshotID) (*Tx, error) {
	if parent == Latest {
		s.readMu.RLock()
		st, err := s.captureState()
		if err != nil {
			s.readMu.RUnlock()
			return nil, err
		}
		latest := st.view.LatestSnapshot()
		s.readMu.RUnlock()
		if latest == nil {
			return nil, fmt.Errorf("%w: no committed snapshot", ErrInvalidParent)
		}
		parent = SnapshotID(latest.ID)
	}
	typ := SnapshotDelta
	if parent == NoParent {
		// FULL baseline: a complete snapshot that may be committed at any
		// time (checkpointing resets the chain depth).
		typ = SnapshotFull
	} else if st := s.state.Load(); st == nil || st.view.Snapshot(uint64(parent)) == nil {
		return nil, fmt.Errorf("%w: DELTA parent %d not committed", ErrInvalidParent, parent)
	}
	w, err := s.newWriter(ctx, typ, parent)
	if err != nil {
		return nil, err
	}
	return &Tx{w: w, ctx: ctx}, nil
}

// ID returns the snapshot ID reserved for this transaction.
func (tx *Tx) ID() SnapshotID { return tx.w.ID() }

// Parent returns NoParent for a FULL transaction or its DELTA parent.
func (tx *Tx) Parent() SnapshotID { return tx.w.Parent() }

// DefineTable defines a table for the snapshot.
func (tx *Tx) DefineTable(name string, columns []Column) error {
	return tx.w.CreateTable(name, columns)
}

// Insert records a newly-created row.
func (tx *Tx) Insert(table string, id RowID, row Row) error {
	return tx.w.Insert(tx.ctx, table, id, row)
}

// Update records the complete replacement value of an existing row.
func (tx *Tx) Update(table string, id RowID, row Row) error {
	return tx.w.Update(tx.ctx, table, id, row)
}

// Delete records removal of an existing row.
func (tx *Tx) Delete(table string, id RowID) error {
	return tx.w.Delete(tx.ctx, table, id)
}

// Apply dispatches one typed row mutation.
func (tx *Tx) Apply(change Change) error {
	switch change.Type {
	case ChangeInsert:
		return tx.Insert(change.Table, change.RowID, change.Row)
	case ChangeUpdate:
		return tx.Update(change.Table, change.RowID, change.Row)
	case ChangeDelete:
		if change.Row != nil {
			return fmt.Errorf("%w: DELETE row payload must be nil", ErrInvalidArgument)
		}
		return tx.Delete(change.Table, change.RowID)
	default:
		return fmt.Errorf("%w: change type %d", ErrInvalidArgument, change.Type)
	}
}

// ApplyBatch consumes changes in order without retaining or copying the input
// slice. If one change fails, earlier changes in the batch remain part of the
// transaction; Rollback discards the entire transaction.
func (tx *Tx) ApplyBatch(changes []Change) error {
	for i := range changes {
		if err := tx.Apply(changes[i]); err != nil {
			return fmt.Errorf("rowpack: apply change %d: %w", i, err)
		}
	}
	return nil
}

// Commit publishes the transaction as a new snapshot.
func (tx *Tx) Commit(ctx context.Context) (SnapshotID, error) {
	return tx.w.Commit(ctx)
}

// Rollback discards the transaction. It is safe to defer immediately after
// Begin; after a successful Commit it returns ErrSnapshotCommitted.
func (tx *Tx) Rollback() error { return tx.w.Abort() }
