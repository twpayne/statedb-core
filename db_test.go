// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package statedb

import (
	"bytes"
	"context"
	"errors"
	"expvar"
	"fmt"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"

	"github.com/cilium/hive"
	"github.com/cilium/hive/cell"
	"github.com/cilium/statedb/index"
	"github.com/cilium/stream"
)

func TestMain(m *testing.M) {
	// Catch any leaks of goroutines from these tests.
	goleak.VerifyTestMain(m)
}

type testObject struct {
	ID   uint64
	Tags []string
}

func (t testObject) String() string {
	return fmt.Sprintf("testObject{ID: %d, Tags: %v}", t.ID, t.Tags)
}

var (
	idIndex = Index[testObject, uint64]{
		Name: "id",
		FromObject: func(t testObject) index.KeySet {
			return index.NewKeySet(index.Uint64(t.ID))
		},
		FromKey: index.Uint64,
		Unique:  true,
	}

	tagsIndex = Index[testObject, string]{
		Name: "tags",
		FromObject: func(t testObject) index.KeySet {
			return index.StringSlice(t.Tags)
		},
		FromKey: index.String,
		Unique:  false,
	}
)

const (
	INDEX_TAGS    = true
	NO_INDEX_TAGS = false
)

// Do not log debug&info level logs in tests.
var logger = slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
	Level: slog.LevelError,
}))

func newTestDB(t testing.TB, secondaryIndexers ...Indexer[testObject]) (*DB, RWTable[testObject], *ExpVarMetrics) {
	var (
		db *DB
	)
	table, err := NewTable[testObject](
		"test",
		idIndex,
		secondaryIndexers...,
	)
	require.NoError(t, err, "NewTable[testObject]")

	metrics := NewExpVarMetrics(false)

	h := hive.NewWithOptions(
		hive.Options{Logger: logger},

		cell.Provide(func() Metrics { return metrics }),
		Cell, // DB
		cell.Invoke(func(db_ *DB) {
			db_.RegisterTable(table)

			// Use a short GC interval.
			db_.setGCRateLimitInterval(50 * time.Millisecond)

			db = db_
		}),
	)

	require.NoError(t, h.Start(context.TODO()))
	t.Cleanup(func() {
		assert.NoError(t, h.Stop(context.TODO()))
	})
	return db, table, metrics
}

func TestDB_Insert_SamePointer(t *testing.T) {
	db, _ := NewDB(nil, NewExpVarMetrics(false))
	idIndex := Index[*testObject, uint64]{
		Name: "id",
		FromObject: func(t *testObject) index.KeySet {
			return index.NewKeySet(index.Uint64(t.ID))
		},
		FromKey: index.Uint64,
		Unique:  true,
	}
	table, _ := NewTable[*testObject]("test", idIndex)
	db.RegisterTable(table)

	txn := db.WriteTxn(table)
	obj := &testObject{ID: 1}
	table.Insert(txn, obj)
	txn.Commit()

	defer func() {
		if err := recover(); err == nil {
			t.Fatalf("Inserting the same object again didn't fatal")
		}
	}()

	txn = db.WriteTxn(table)
	table.Insert(txn, obj)
	txn.Commit()

}

func TestDB_LowerBound_ByRevision(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t, tagsIndex)

	{
		txn := db.WriteTxn(table)
		table.Insert(txn, testObject{ID: 42, Tags: []string{"hello", "world"}})
		txn.Commit()

		txn = db.WriteTxn(table)
		table.Insert(txn, testObject{ID: 71, Tags: []string{"foo"}})
		txn.Commit()
	}

	txn := db.ReadTxn()

	iter, watch := table.LowerBound(txn, ByRevision[testObject](0))
	obj, rev, ok := iter.Next()
	require.True(t, ok, "expected ByRevision(rev1) to return results")
	require.EqualValues(t, 42, obj.ID)
	prevRev := rev
	obj, rev, ok = iter.Next()
	require.True(t, ok)
	require.EqualValues(t, 71, obj.ID)
	require.Greater(t, rev, prevRev)
	_, _, ok = iter.Next()
	require.False(t, ok)

	iter, _ = table.LowerBound(txn, ByRevision[testObject](prevRev+1))
	obj, _, ok = iter.Next()
	require.True(t, ok, "expected ByRevision(rev2) to return results")
	require.EqualValues(t, 71, obj.ID)
	_, _, ok = iter.Next()
	require.False(t, ok)

	select {
	case <-watch:
		t.Fatalf("expected LowerBound watch to not be closed before changes")
	default:
	}

	{
		txn := db.WriteTxn(table)
		table.Insert(txn, testObject{ID: 71, Tags: []string{"foo", "modified"}})
		txn.Commit()
	}

	select {
	case <-watch:
	case <-time.After(time.Second):
		t.Fatalf("expected LowerBound watch to close after changes")
	}

	txn = db.ReadTxn()
	iter, _ = table.LowerBound(txn, ByRevision[testObject](rev+1))
	obj, _, ok = iter.Next()
	require.True(t, ok, "expected ByRevision(rev2+1) to return results")
	require.EqualValues(t, 71, obj.ID)
	_, _, ok = iter.Next()
	require.False(t, ok)

}

func TestDB_DeleteTracker(t *testing.T) {
	t.Parallel()

	db, table, metrics := newTestDB(t, tagsIndex)

	{
		txn := db.WriteTxn(table)
		table.Insert(txn, testObject{ID: 42, Tags: []string{"hello", "world"}})
		table.Insert(txn, testObject{ID: 71, Tags: []string{"foo"}})
		table.Insert(txn, testObject{ID: 83, Tags: []string{"bar"}})
		txn.Commit()
	}

	assert.EqualValues(t, table.Revision(db.ReadTxn()), expvarInt(metrics.RevisionVar.Get("test")), "Revision")
	assert.EqualValues(t, 3, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.EqualValues(t, 0, expvarInt(metrics.GraveyardObjectCountVar.Get("test")), "GraveyardObjectCount")

	// Create two delete trackers
	wtxn := db.WriteTxn(table)
	deleteTracker, err := table.DeleteTracker(wtxn, "test")
	require.NoError(t, err, "failed to create DeleteTracker")
	deleteTracker2, err := table.DeleteTracker(wtxn, "test2")
	require.NoError(t, err, "failed to create DeleteTracker")
	wtxn.Commit()

	assert.EqualValues(t, 2, expvarInt(metrics.DeleteTrackerCountVar.Get("test")), "DeleteTrackerCount")

	// Delete 2/3 objects
	{
		txn := db.WriteTxn(table)
		old, deleted, err := table.Delete(txn, testObject{ID: 42})
		require.True(t, deleted)
		require.EqualValues(t, 42, old.ID)
		require.NoError(t, err)
		old, deleted, err = table.Delete(txn, testObject{ID: 71})
		require.True(t, deleted)
		require.EqualValues(t, 71, old.ID)
		require.NoError(t, err)
		txn.Commit()

		// Reinsert and redelete to test updating graveyard with existing object.
		txn = db.WriteTxn(table)
		table.Insert(txn, testObject{ID: 71, Tags: []string{"foo"}})
		txn.Commit()

		txn = db.WriteTxn(table)
		_, deleted, err = table.Delete(txn, testObject{ID: 71})
		require.True(t, deleted)
		require.NoError(t, err)
		txn.Commit()
	}

	// 1 object should exist.
	txn := db.ReadTxn()
	iter, _ := table.All(txn)
	objs := Collect(iter)
	require.Len(t, objs, 1)

	assert.EqualValues(t, 1, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.EqualValues(t, 2, expvarInt(metrics.GraveyardObjectCountVar.Get("test")), "GraveyardObjectCount")

	// Consume the deletions using the first delete tracker.
	nExist := 0
	nDeleted := 0
	_, err = deleteTracker.IterateWithError(
		txn,
		func(obj testObject, deleted bool, _ Revision) error {
			if deleted {
				nDeleted++
			} else {
				nExist++
			}
			return nil
		})
	require.NoError(t, err)
	require.Equal(t, nDeleted, 2)
	require.Equal(t, nExist, 1)

	// Since the second delete tracker has not processed the deletions,
	// the graveyard index should still hold them.
	require.False(t, db.graveyardIsEmpty())

	// Consume the deletions using the second delete tracker, but
	// with a failure first.
	nExist = 0
	nDeleted = 0
	failErr := errors.New("fail")
	_, err = deleteTracker2.IterateWithError(
		txn,
		func(obj testObject, deleted bool, _ Revision) error {
			if deleted {
				nDeleted++
				return failErr
			}
			nExist++
			return nil
		})
	require.ErrorIs(t, err, failErr)
	require.Equal(t, nExist, 1) // Existing objects are iterated first.
	require.Equal(t, nDeleted, 1)
	nExist = 0
	nDeleted = 0

	// Process again, but this time using Iterate (retrying the failed revision)
	_ = deleteTracker2.Iterate(
		txn,
		func(obj testObject, deleted bool, _ Revision) {
			if deleted {
				nDeleted++
			} else {
				nExist++
			}
		})
	require.Equal(t, nDeleted, 2)
	require.Equal(t, nExist, 0) // This was already processed.

	// Graveyard will now be GCd.
	eventuallyGraveyardIsEmpty(t, db)

	assert.EqualValues(t, table.Revision(db.ReadTxn()), expvarInt(metrics.RevisionVar.Get("test")), "Revision")
	assert.EqualValues(t, 1, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.EqualValues(t, 0, expvarInt(metrics.GraveyardObjectCountVar.Get("test")), "GraveyardObjectCount")

	// After closing the first delete tracker, deletes are still tracked for second one.
	// Delete the last remaining object.
	deleteTracker.Close()
	{
		txn := db.WriteTxn(table)
		table.DeleteAll(txn)
		txn.Commit()
	}
	require.False(t, db.graveyardIsEmpty())

	assert.EqualValues(t, 0, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.EqualValues(t, 1, expvarInt(metrics.GraveyardObjectCountVar.Get("test")), "GraveyardObjectCount")

	// And finally after closing the second tracker deletions are no longer tracked.
	deleteTracker2.Mark(table.Revision(db.ReadTxn()))
	eventuallyGraveyardIsEmpty(t, db)

	assert.EqualValues(t, 0, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.EqualValues(t, 0, expvarInt(metrics.GraveyardObjectCountVar.Get("test")), "GraveyardObjectCount")

	deleteTracker2.Close()
	{
		txn := db.WriteTxn(table)
		table.Insert(txn, testObject{ID: 78, Tags: []string{"world"}})
		txn.Commit()
		txn = db.WriteTxn(table)
		table.DeleteAll(txn)
		txn.Commit()
	}
	require.True(t, db.graveyardIsEmpty())

	assert.EqualValues(t, 0, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.EqualValues(t, 0, expvarInt(metrics.GraveyardObjectCountVar.Get("test")), "GraveyardObjectCount")
}

func TestDB_Observable(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	events := stream.ToChannel(ctx, Observable[testObject](db, table))

	txn := db.WriteTxn(table)
	table.Insert(txn, testObject{ID: uint64(1)})
	table.Insert(txn, testObject{ID: uint64(2)})
	txn.Commit()

	event := <-events
	require.False(t, event.Deleted, "expected insert")
	require.Equal(t, uint64(1), event.Object.ID)
	event = <-events
	require.False(t, event.Deleted, "expected insert")
	require.Equal(t, uint64(2), event.Object.ID)

	txn = db.WriteTxn(table)
	table.Delete(txn, testObject{ID: uint64(1)})
	table.Delete(txn, testObject{ID: uint64(2)})
	txn.Commit()

	event = <-events
	require.True(t, event.Deleted, "expected delete")
	require.Equal(t, uint64(1), event.Object.ID)
	event = <-events
	require.True(t, event.Deleted, "expected delete")
	require.Equal(t, uint64(2), event.Object.ID)

	cancel()
	ev, ok := <-events
	require.False(t, ok, "expected channel to close, got event: %+v", ev)
}

func TestDB_All(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t, tagsIndex)

	{
		txn := db.WriteTxn(table)
		table.Insert(txn, testObject{ID: uint64(1)})
		table.Insert(txn, testObject{ID: uint64(2)})
		table.Insert(txn, testObject{ID: uint64(3)})
		iter, _ := table.All(txn)
		objs := Collect(iter)
		require.Len(t, objs, 3)
		require.EqualValues(t, 1, objs[0].ID)
		require.EqualValues(t, 2, objs[1].ID)
		require.EqualValues(t, 3, objs[2].ID)
		txn.Commit()
	}

	txn := db.ReadTxn()
	iter, watch := table.All(txn)
	objs := Collect(iter)
	require.Len(t, objs, 3)
	require.EqualValues(t, 1, objs[0].ID)
	require.EqualValues(t, 2, objs[1].ID)
	require.EqualValues(t, 3, objs[2].ID)

	select {
	case <-watch:
		t.Fatalf("expected All() watch channel to not close before changes")
	default:
	}

	{
		txn := db.WriteTxn(table)
		table.Delete(txn, testObject{ID: uint64(1)})
		txn.Commit()
	}

	select {
	case <-watch:
	case <-time.After(time.Second):
		t.Fatalf("expceted All() watch channel to close after changes")
	}
}

func TestDB_Revision(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t, tagsIndex)

	startRevision := table.Revision(db.ReadTxn())

	// On aborted write transactions the revision remains unchanged.
	txn := db.WriteTxn(table)
	_, _, err := table.Insert(txn, testObject{ID: 1})
	require.NoError(t, err)
	writeRevision := table.Revision(txn) // Returns new, but uncommitted revision
	txn.Abort()
	require.Equal(t, writeRevision, startRevision+1, "revision incremented on Insert")
	readRevision := table.Revision(db.ReadTxn())
	require.Equal(t, startRevision, readRevision, "aborted transaction does not change revision")

	// Committed write transactions increment the revision
	txn = db.WriteTxn(table)
	_, _, err = table.Insert(txn, testObject{ID: 1})
	require.NoError(t, err)
	writeRevision = table.Revision(txn)
	txn.Commit()
	require.Equal(t, writeRevision, startRevision+1, "revision incremented on Insert")
	readRevision = table.Revision(db.ReadTxn())
	require.Equal(t, writeRevision, readRevision, "committed transaction changed revision")
}

func TestDB_GetFirstLast(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t, tagsIndex)

	// Write test objects 1..10 to table with odd/even/odd/... tags.
	{
		txn := db.WriteTxn(table)
		for i := 1; i <= 10; i++ {
			tag := "odd"
			if i%2 == 0 {
				tag = "even"
			}
			_, _, err := table.Insert(txn, testObject{ID: uint64(i), Tags: []string{tag}})
			require.NoError(t, err)
		}
		// Check that we can query the not-yet-committed write transaction.
		obj, rev, ok := table.First(txn, idIndex.Query(1))
		require.True(t, ok, "expected First(1) to return result")
		require.NotZero(t, rev, "expected non-zero revision")
		require.EqualValues(t, obj.ID, 1, "expected first obj.ID to equal 1")
		obj, rev, ok = table.Last(txn, idIndex.Query(1))
		require.True(t, ok, "expected Last(1) to return result")
		require.NotZero(t, rev, "expected non-zero revision")
		require.EqualValues(t, obj.ID, 1, "expected last obj.ID to equal 1")
		txn.Commit()
	}

	txn := db.ReadTxn()

	// Test Get against the ID index.
	iter, _ := table.Get(txn, idIndex.Query(0))
	items := Collect(iter)
	require.Len(t, items, 0, "expected Get(0) to not return results")

	iter, _ = table.Get(txn, idIndex.Query(1))
	items = Collect(iter)
	require.Len(t, items, 1, "expected Get(1) to return result")
	require.EqualValues(t, items[0].ID, 1, "expected items[0].ID to equal 1")

	iter, getWatch := table.Get(txn, idIndex.Query(2))
	items = Collect(iter)
	require.Len(t, items, 1, "expected Get(2) to return result")
	require.EqualValues(t, items[0].ID, 2, "expected items[0].ID to equal 2")

	// Test First/FirstWatch and Last/LastWatch against the ID index.
	_, _, ok := table.First(txn, idIndex.Query(0))
	require.False(t, ok, "expected First(0) to not return result")

	_, _, ok = table.Last(txn, idIndex.Query(0))
	require.False(t, ok, "expected Last(0) to not return result")

	obj, rev, ok := table.First(txn, idIndex.Query(1))
	require.True(t, ok, "expected First(1) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.EqualValues(t, obj.ID, 1, "expected first obj.ID to equal 1")

	obj, rev, ok = table.Last(txn, idIndex.Query(1))
	require.True(t, ok, "expected Last(1) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.EqualValues(t, obj.ID, 1, "expected last obj.ID to equal 1")

	obj, rev, firstWatch, ok := table.FirstWatch(txn, idIndex.Query(2))
	require.True(t, ok, "expected FirstWatch(2) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.EqualValues(t, obj.ID, 2, "expected obj.ID to equal 2")

	obj, rev, lastWatch, ok := table.LastWatch(txn, idIndex.Query(2))
	require.True(t, ok, "expected LastWatch(2) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.EqualValues(t, obj.ID, 2, "expected obj.ID to equal 2")

	select {
	case <-firstWatch:
		t.Fatalf("FirstWatch channel closed before changes")
	case <-lastWatch:
		t.Fatalf("LastWatch channel closed before changes")
	case <-getWatch:
		t.Fatalf("Get channel closed before changes")
	default:
	}

	// Modify the testObject(2) to trigger closing of the watch channels.
	wtxn := db.WriteTxn(table)
	_, hadOld, err := table.Insert(wtxn, testObject{ID: uint64(2), Tags: []string{"even", "modified"}})
	require.True(t, hadOld)
	require.NoError(t, err)
	wtxn.Commit()

	select {
	case <-firstWatch:
	case <-time.After(time.Second):
		t.Fatalf("FirstWatch channel not closed after change")
	}
	select {
	case <-lastWatch:
	case <-time.After(time.Second):
		t.Fatalf("LastWatch channel not closed after change")
	}
	select {
	case <-getWatch:
	case <-time.After(time.Second):
		t.Fatalf("Get channel not closed after change")
	}

	// Since we modified the database, grab a fresh read transaction.
	txn = db.ReadTxn()

	// Test First and Last against the tags multi-index which will
	// return multiple results.
	obj, rev, _, ok = table.FirstWatch(txn, tagsIndex.Query("even"))
	require.True(t, ok, "expected First(even) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.ElementsMatch(t, obj.Tags, []string{"even", "modified"})
	require.EqualValues(t, 2, obj.ID)

	obj, rev, _, ok = table.LastWatch(txn, tagsIndex.Query("odd"))
	require.True(t, ok, "expected First(even) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.ElementsMatch(t, obj.Tags, []string{"odd"})
	require.EqualValues(t, 9, obj.ID)

	iter, _ = table.Get(txn, tagsIndex.Query("odd"))
	items = Collect(iter)
	require.Len(t, items, 5, "expected Get(odd) to return 5 items")
	for i, item := range items {
		require.EqualValues(t, item.ID, i*2+1, "expected items[%d].ID to equal %d", i, i*2+1)
	}
}

func TestDB_CommitAbort(t *testing.T) {
	t.Parallel()

	db, table, metrics := newTestDB(t, tagsIndex)

	txn := db.WriteTxn(table)
	_, _, err := table.Insert(txn, testObject{ID: 123, Tags: nil})
	require.NoError(t, err)
	txn.Commit()

	assert.EqualValues(t, table.Revision(db.ReadTxn()), expvarInt(metrics.RevisionVar.Get("test")), "Revision")
	assert.EqualValues(t, 1, expvarInt(metrics.ObjectCountVar.Get("test")), "ObjectCount")
	assert.Greater(t, expvarFloat(metrics.WriteTxnAcquisitionVar.Get("statedb")), 0.0, "WriteTxnAcquisition")
	assert.Greater(t, expvarFloat(metrics.WriteTxnDurationVar.Get("statedb")), 0.0, "WriteTxnDuration")

	obj, rev, ok := table.First(db.ReadTxn(), idIndex.Query(123))
	require.True(t, ok, "expected First(1) to return result")
	require.NotZero(t, rev, "expected non-zero revision")
	require.EqualValues(t, obj.ID, 123, "expected obj.ID to equal 123")
	require.Nil(t, obj.Tags, "expected no tags")

	_, _, err = table.Insert(txn, testObject{ID: 123, Tags: []string{"insert-after-commit"}})
	require.ErrorIs(t, err, ErrTransactionClosed)
	txn.Commit() // should be no-op

	txn = db.WriteTxn(table)
	txn.Abort()

	_, _, err = table.Insert(txn, testObject{ID: 123, Tags: []string{"insert-after-abort"}})
	require.ErrorIs(t, err, ErrTransactionClosed)
	txn.Commit() // should be no-op

	// Check that insert after commit and insert after abort do not change the
	// table.
	obj, newRev, ok := table.First(db.ReadTxn(), idIndex.Query(123))
	require.True(t, ok, "expected object to exist")
	require.Equal(t, rev, newRev, "expected unchanged revision")
	require.EqualValues(t, obj.ID, 123, "expected obj.ID to equal 123")
	require.Nil(t, obj.Tags, "expected no tags")
}

func TestDB_CompareAndSwap_CompareAndDelete(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t, tagsIndex)

	// Updating a non-existing object fails and nothing is inserted.
	wtxn := db.WriteTxn(table)
	{
		_, hadOld, err := table.CompareAndSwap(wtxn, 1, testObject{ID: 1})
		require.ErrorIs(t, ErrObjectNotFound, err)
		require.False(t, hadOld)

		objs, _ := table.All(wtxn)
		require.Len(t, Collect(objs), 0)

		wtxn.Abort()
	}

	// Insert a test object and retrieve it.
	wtxn = db.WriteTxn(table)
	table.Insert(wtxn, testObject{ID: 1})
	wtxn.Commit()

	obj, rev1, ok := table.First(db.ReadTxn(), idIndex.Query(1))
	require.True(t, ok)

	// Updating an object with matching revision number works
	wtxn = db.WriteTxn(table)
	obj.Tags = []string{"updated"} // NOTE: testObject stored by value so no explicit copy needed.
	oldObj, hadOld, err := table.CompareAndSwap(wtxn, rev1, obj)
	require.NoError(t, err)
	require.True(t, hadOld)
	require.EqualValues(t, 1, oldObj.ID)
	wtxn.Commit()

	obj, _, ok = table.First(db.ReadTxn(), idIndex.Query(1))
	require.True(t, ok)
	require.Len(t, obj.Tags, 1)
	require.Equal(t, "updated", obj.Tags[0])

	// Updating an object with mismatching revision number fails
	wtxn = db.WriteTxn(table)
	obj.Tags = []string{"mismatch"}
	oldObj, hadOld, err = table.CompareAndSwap(wtxn, rev1, obj)
	require.ErrorIs(t, ErrRevisionNotEqual, err)
	require.True(t, hadOld)
	require.EqualValues(t, 1, oldObj.ID)
	wtxn.Commit()

	obj, _, ok = table.First(db.ReadTxn(), idIndex.Query(1))
	require.True(t, ok)
	require.Len(t, obj.Tags, 1)
	require.Equal(t, "updated", obj.Tags[0])

	// Deleting an object with mismatching revision number fails
	wtxn = db.WriteTxn(table)
	obj.Tags = []string{"mismatch"}
	oldObj, hadOld, err = table.CompareAndDelete(wtxn, rev1, obj)
	require.ErrorIs(t, ErrRevisionNotEqual, err)
	require.True(t, hadOld)
	require.EqualValues(t, 1, oldObj.ID)
	wtxn.Commit()

	obj, rev2, ok := table.First(db.ReadTxn(), idIndex.Query(1))
	require.True(t, ok)
	require.Len(t, obj.Tags, 1)
	require.Equal(t, "updated", obj.Tags[0])

	// Deleting with matching revision number works
	wtxn = db.WriteTxn(table)
	obj.Tags = []string{"mismatch"}
	oldObj, hadOld, err = table.CompareAndDelete(wtxn, rev2, obj)
	require.NoError(t, err)
	require.True(t, hadOld)
	require.EqualValues(t, 1, oldObj.ID)
	wtxn.Commit()

	_, _, ok = table.First(db.ReadTxn(), idIndex.Query(1))
	require.False(t, ok)

	// Deleting non-existing object yields not found
	wtxn = db.WriteTxn(table)
	_, hadOld, err = table.CompareAndDelete(wtxn, rev2, obj)
	require.NoError(t, err)
	require.False(t, hadOld)
	wtxn.Abort()
}

func TestWriteJSON(t *testing.T) {
	t.Parallel()

	db, table, _ := newTestDB(t, tagsIndex)

	buf := new(bytes.Buffer)
	err := db.ReadTxn().WriteJSON(buf)
	require.NoError(t, err)

	txn := db.WriteTxn(table)
	for i := 1; i <= 10; i++ {
		_, _, err := table.Insert(txn, testObject{ID: uint64(i)})
		require.NoError(t, err)
	}
	txn.Commit()
}

func Test_callerPackage(t *testing.T) {
	t.Parallel()

	pkg := func() string {
		return callerPackage()
	}()
	require.Equal(t, "statedb", pkg)
}

func Test_nonUniqueKey(t *testing.T) {
	// empty keys
	key := encodeNonUniqueKey(nil, nil)
	primary, secondary := decodeNonUniqueKey(key)
	assert.Len(t, primary, 0)
	assert.Len(t, secondary, 0)

	// empty primary
	key = encodeNonUniqueKey(nil, []byte("foo"))
	primary, secondary = decodeNonUniqueKey(key)
	assert.Len(t, primary, 0)
	assert.Equal(t, string(secondary), "foo")

	// empty secondary
	key = encodeNonUniqueKey([]byte("quux"), []byte{})
	primary, secondary = decodeNonUniqueKey(key)
	assert.Equal(t, string(primary), "quux")
	assert.Len(t, secondary, 0)

	// non-empty
	key = encodeNonUniqueKey([]byte("foo"), []byte("quux"))
	primary, secondary = decodeNonUniqueKey(key)
	assert.EqualValues(t, primary, "foo")
	assert.EqualValues(t, secondary, "quux")
}

func eventuallyGraveyardIsEmpty(t testing.TB, db *DB) {
	require.Eventually(t,
		db.graveyardIsEmpty,
		5*time.Second,
		100*time.Millisecond,
		"graveyard not garbage collected")
}

func expvarInt(v expvar.Var) int64 {
	if v, ok := v.(*expvar.Int); ok && v != nil {
		return v.Value()
	}
	return -1
}

func expvarFloat(v expvar.Var) float64 {
	if v, ok := v.(*expvar.Float); ok && v != nil {
		return v.Value()
	}
	return -1
}
