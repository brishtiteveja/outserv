package badger

import (
	"context"
	"sync"

	"github.com/dgraph-io/badger/value"
	"github.com/dgraph-io/badger/y"
)

type KVItem struct {
	sync.WaitGroup
	key  []byte
	vptr []byte
	meta byte
	val  []byte
}

// Key returns the key. If nil, the iteration is done and you should break out of channel loop.
func (kv *KVItem) Key() []byte {
	return kv.key
}

// Value returns the value, generally fetched from the value log. This can block while
// the fetch workers populate the value.
func (kv *KVItem) Value() []byte {
	kv.Wait()
	return kv.val
}

type iteratorOp struct {
	seekExtreme int    // 0 = FIRST, 1 = LAST
	seekTo      []byte // specific key
	direction   int    // 0 = FWD, 1 = REV
	close       bool
}

func (op iteratorOp) Set(i y.Iterator) {
	if op.seekTo != nil {
		i.Seek(op.seekTo)
		return
	}
	if op.seekExtreme == 0 {
		i.SeekToFirst()
		return
	}
	y.Fatalf("Unhandled seek operation.")
}

type Iterator struct {
	wg      sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
	ch      chan *KVItem
	fetchCh chan *KVItem
	db      *KV
	iitr    y.Iterator
	seekCh  chan iteratorOp
}

func (itr *Iterator) Ch() <-chan *KVItem {
	return itr.ch
}

func (itr *Iterator) clearCh() {
	for {
		select {
		case <-itr.ch:
		case <-itr.fetchCh:
			// These will continue until channels are empty.
		default:
			return
		}
	}
}

func (itr *Iterator) prefetch() {
	i := itr.iitr
	var op iteratorOp
TOP:
	select {
	case op = <-itr.seekCh:
		y.Trace(itr.ctx, "Got op: %+v", op)
		itr.clearCh()
	case <-itr.ctx.Done():
		return
	}

	for !op.close {
		op.Set(i)
	INTERNAL:
		for ; i.Valid(); i.Next() {
			vptr, meta := i.Value()
			if (meta & value.BitDelete) != 0 {
				// Tombstone encountered.
				continue
			}

			kv := &KVItem{
				key:  i.Key(),
				vptr: vptr,
				meta: meta,
			}
			kv.Add(1)

			select {
			case op = <-itr.seekCh:
				y.Trace(itr.ctx, "Got op: %+v", op)
				itr.clearCh()
				break INTERNAL
			case itr.ch <- kv: // We must have incremented sync.WaitGroup before pushing to ch.
				y.Trace(itr.ctx, "Pushed key to ch: %s\n", kv.Key())
				itr.fetchCh <- kv
			case <-itr.ctx.Done():
				return
			}
		}
		if !i.Valid() {
			itr.ch <- &KVItem{key: nil}
			// Reached end of iterator.
			goto TOP
		}
	}
}

func (itr *Iterator) SeekToFirst() {
	itr.seekCh <- iteratorOp{seekExtreme: 0}
}

func (itr *Iterator) Seek(key []byte) {
	itr.seekCh <- iteratorOp{seekTo: key}
}

func (itr *Iterator) fetchValue() {
	for {
		select {
		case kv := <-itr.fetchCh:
			kv.val = itr.db.decodeValue(itr.ctx, kv.vptr, kv.meta)
			kv.Done()
		case <-itr.ctx.Done():
			return
		}
	}
}

func (itr *Iterator) Close() {
	itr.seekCh <- iteratorOp{close: true}
	itr.cancel()
	itr.iitr.Close()
}

// NewIterator returns a store wide iterator.
// Note: This acquires references to underlying tables. Remember to close the returned iterator.
func (s *KV) NewIterator(ctx context.Context, prefetchSize, workers int) *Iterator {
	// The order we add these iterators is important.
	// Imagine you add level0 first, then add imm. In between, the initial imm might be moved into
	// level0, and be completely missed. On the other hand, if you add imm first and it got moved
	// to level 0, you would just have that data appear twice which is fine.
	mem, imm := s.getMemTables()
	var iters []y.Iterator
	if mem != nil {
		iters = append(iters, mem.NewIterator())
	}
	if imm != nil {
		iters = append(iters, imm.NewIterator())
	}
	iters = s.lc.appendIterators(iters) // This will increment references.

	itr := &Iterator{
		db:      s,
		iitr:    y.NewMergeIterator(iters),
		ch:      make(chan *KVItem, prefetchSize),
		fetchCh: make(chan *KVItem, prefetchSize),
		seekCh:  make(chan iteratorOp), // unbuffered channel
	}
	itr.ctx, itr.cancel = context.WithCancel(ctx)
	for i := 0; i < workers; i++ {
		go itr.fetchValue()
	}
	itr.wg.Add(workers)
	go itr.prefetch()
	return itr
}
