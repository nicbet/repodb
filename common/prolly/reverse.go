package prolly

import (
	"bytes"
	"context"
	"fmt"
	"sort"

	"github.com/nicbet/repodb/common/storage"
)

// EntryIterator is an ordered walk over a tree's entries, ascending
// (Iterator) or descending (ReverseIterator).
type EntryIterator interface {
	Next() (Entry, bool, error)
	Close() error
}

var (
	_ EntryIterator = (*Iterator)(nil)
	_ EntryIterator = (*ReverseIterator)(nil)
)

// ReverseIterator walks a tree in descending key order, keeping one node per
// level. Leaf entries are only length-prefixed forward, so each leaf is
// decoded whole and read backwards.
type ReverseIterator struct {
	ctx   context.Context
	store storage.Store
	// stack holds interior nodes; a frame's next is the index of the next
	// child to visit, counting down, or -1.
	stack []iteratorFrame
	leaf  []Entry
	index int // next leaf entry to return, counting down
	done  bool
}

// ReverseIterator returns an iterator over the entries whose key is < end, in
// descending order. A nil end returns every entry.
func (t *Tree) ReverseIterator(ctx context.Context, end []byte) (*ReverseIterator, error) {
	it := &ReverseIterator{ctx: ctx, store: t.store, index: -1}
	if err := it.seekBelow(t.root, end); err != nil {
		return nil, err
	}
	return it, nil
}

func (it *ReverseIterator) seekBelow(hash storage.Hash, end []byte) error {
	for {
		n, err := readNode(it.ctx, it.store, hash)
		if err != nil {
			return err
		}
		if n.Level == 0 {
			count := len(n.Entries)
			if end != nil {
				count = sort.Search(len(n.Entries), func(i int) bool { return bytes.Compare(n.Entries[i].Key, end) >= 0 })
			}
			it.leaf, it.index = n.Entries, count-1
			if it.index < 0 {
				return it.retreat()
			}
			return nil
		}
		if len(n.Children) == 0 {
			return fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		// The first child whose max key reaches end may still hold smaller
		// keys; every child before it holds only smaller keys.
		i := len(n.Children) - 1
		if end != nil {
			if found := sort.Search(len(n.Children), func(i int) bool { return bytes.Compare(n.Children[i].MaxKey, end) >= 0 }); found < len(n.Children) {
				i = found
			}
		}
		it.stack = append(it.stack, iteratorFrame{node: n, next: i - 1})
		hash = n.Children[i].Hash
	}
}

// descendRight positions the iterator at the last entry under hash.
func (it *ReverseIterator) descendRight(hash storage.Hash) error {
	for {
		n, err := readNode(it.ctx, it.store, hash)
		if err != nil {
			return err
		}
		if n.Level == 0 {
			it.leaf, it.index = n.Entries, len(n.Entries)-1
			if it.index < 0 {
				return it.retreat()
			}
			return nil
		}
		if len(n.Children) == 0 {
			return fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		last := len(n.Children) - 1
		it.stack = append(it.stack, iteratorFrame{node: n, next: last - 1})
		hash = n.Children[last].Hash
	}
}

// retreat moves to the last entry of the previous leaf.
func (it *ReverseIterator) retreat() error {
	it.leaf, it.index = nil, -1
	for len(it.stack) != 0 {
		top := &it.stack[len(it.stack)-1]
		if top.next >= 0 {
			hash := top.node.Children[top.next].Hash
			top.next--
			return it.descendRight(hash)
		}
		it.stack = it.stack[:len(it.stack)-1]
	}
	it.done = true
	return nil
}

// Next returns the next smaller entry, or ok=false at the end. As with
// Iterator, the returned bytes are shared and must not be modified.
func (it *ReverseIterator) Next() (Entry, bool, error) {
	for {
		if it.index >= 0 {
			entry := it.leaf[it.index]
			it.index--
			return entry, true, nil
		}
		if it.done {
			return Entry{}, false, nil
		}
		if err := it.ctx.Err(); err != nil {
			it.Close()
			return Entry{}, false, err
		}
		if err := it.retreat(); err != nil {
			it.Close()
			return Entry{}, false, err
		}
	}
}

// Close releases iterator buffers. It is safe to call more than once.
func (it *ReverseIterator) Close() error {
	it.stack, it.leaf, it.index, it.done = nil, nil, -1, true
	return nil
}
