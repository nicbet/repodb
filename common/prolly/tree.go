// Package prolly implements a small immutable, content-addressed Prolly tree.
// It is an experimental storage core, not yet a production B-tree replacement.
package prolly

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/bits"
	"sort"

	"github.com/nicbet/repodb/common/storage"
)

var ErrNotFound = errors.New("key not found")

type Entry struct {
	Key   []byte `json:"key"`
	Value []byte `json:"value"`
}

type link struct {
	MaxKey []byte       `json:"max_key"`
	Hash   storage.Hash `json:"hash"`
}

type node struct {
	Level    uint8   `json:"level"`
	Entries  []Entry `json:"entries,omitempty"`
	Children []link  `json:"children,omitempty"`
}

type Options struct {
	MinChunkEntries int
	MaxChunkEntries int
	BoundaryBits    uint8
}

var DefaultOptions = Options{MinChunkEntries: 32, MaxChunkEntries: 128, BoundaryBits: 6}

type Tree struct {
	store   storage.Store
	root    storage.Hash
	options Options
}

// Iterator lazily walks a tree in key order. At most one node per tree level is
// retained, so callers do not need to materialize the complete entry set.
type Iterator struct {
	ctx   context.Context
	store storage.Store
	stack []iteratorFrame
	leaf  []Entry
	index int
	done  bool
}

type iteratorFrame struct {
	node node
	next int
}

// SortedBuilder incrementally constructs the same canonical tree as Build from
// strictly ordered entries. It buffers at most one chunk per tree level.
type SortedBuilder struct {
	ctx         context.Context
	store       storage.Store
	options     Options
	leaves      []Entry
	leafRolling uint64
	levels      []linkBuffer
	lastKey     []byte
	haveKey     bool
	finished    bool
}

type linkBuffer struct {
	links   []link
	rolling uint64
}

func Open(store storage.Store, root storage.Hash) (*Tree, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if !root.Valid() {
		return nil, fmt.Errorf("invalid root hash %q", root)
	}
	return &Tree{store: store, root: root, options: DefaultOptions}, nil
}

func Build(ctx context.Context, store storage.Store, entries []Entry, options Options) (*Tree, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	items := cloneEntries(entries)
	sort.Slice(items, func(i, j int) bool { return bytes.Compare(items[i].Key, items[j].Key) < 0 })
	for i := 1; i < len(items); i++ {
		if bytes.Equal(items[i-1].Key, items[i].Key) {
			return nil, fmt.Errorf("duplicate key %q", items[i].Key)
		}
	}

	if len(items) == 0 {
		hash, err := writeNode(ctx, store, node{Level: 0, Entries: []Entry{}})
		return &Tree{store: store, root: hash, options: options}, err
	}

	groups := chunkEntries(items, options)
	links := make([]link, 0, len(groups))
	for _, group := range groups {
		hash, err := writeNode(ctx, store, node{Level: 0, Entries: group})
		if err != nil {
			return nil, err
		}
		links = append(links, link{MaxKey: clone(group[len(group)-1].Key), Hash: hash})
	}
	if len(links) == 1 {
		return &Tree{store: store, root: links[0].Hash, options: options}, nil
	}

	level := uint8(1)
	for len(links) > 1 {
		groups := chunkLinks(links, options)
		next := make([]link, 0, len(groups))
		for _, group := range groups {
			hash, err := writeNode(ctx, store, node{Level: level, Children: group})
			if err != nil {
				return nil, err
			}
			next = append(next, link{MaxKey: clone(group[len(group)-1].MaxKey), Hash: hash})
		}
		links = next
		level++
	}
	return &Tree{store: store, root: links[0].Hash, options: options}, nil
}

// NewSortedBuilder creates a builder for entries supplied in strictly
// increasing key order.
func NewSortedBuilder(ctx context.Context, store storage.Store, options Options) (*SortedBuilder, error) {
	if store == nil {
		return nil, errors.New("store is required")
	}
	if err := options.validate(); err != nil {
		return nil, err
	}
	return &SortedBuilder{ctx: ctx, store: store, options: options}, nil
}

// Add appends one entry. The key and value are copied before Add returns.
func (b *SortedBuilder) Add(entry Entry) error {
	if b.finished {
		return errors.New("sorted builder is already finished")
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.haveKey && bytes.Compare(b.lastKey, entry.Key) >= 0 {
		return fmt.Errorf("entries are not strictly ordered at key %q", entry.Key)
	}
	item := Entry{Key: clone(entry.Key), Value: clone(entry.Value)}
	b.leaves = append(b.leaves, item)
	b.lastKey = clone(entry.Key)
	b.haveKey = true
	b.leafRolling = bits.RotateLeft64(b.leafRolling, 1) ^ entryFingerprint(item)
	if shouldCut(len(b.leaves), b.leafRolling, b.options) {
		return b.flushLeaf()
	}
	return nil
}

// Finish flushes buffered chunks and returns the completed tree.
func (b *SortedBuilder) Finish() (*Tree, error) {
	if b.finished {
		return nil, errors.New("sorted builder is already finished")
	}
	b.finished = true
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	if len(b.leaves) != 0 {
		if err := b.flushLeaf(); err != nil {
			return nil, err
		}
	} else if !b.haveKey {
		hash, err := writeNode(b.ctx, b.store, node{Level: 0, Entries: []Entry{}})
		return &Tree{store: b.store, root: hash, options: b.options}, err
	}
	for level := 0; ; level++ {
		if level >= len(b.levels) || len(b.levels[level].links) == 0 {
			continue
		}
		if len(b.levels[level].links) == 1 && !b.hasLinksAbove(level) {
			return &Tree{store: b.store, root: b.levels[level].links[0].Hash, options: b.options}, nil
		}
		if err := b.flushLinks(level); err != nil {
			return nil, err
		}
	}
}

func (b *SortedBuilder) flushLeaf() error {
	group := b.leaves
	b.leaves = nil
	b.leafRolling = 0
	hash, err := writeNode(b.ctx, b.store, node{Level: 0, Entries: group})
	if err != nil {
		return err
	}
	return b.addLink(0, link{MaxKey: clone(group[len(group)-1].Key), Hash: hash})
}

func (b *SortedBuilder) addLink(level int, item link) error {
	for len(b.levels) <= level {
		b.levels = append(b.levels, linkBuffer{})
	}
	buffer := &b.levels[level]
	buffer.links = append(buffer.links, item)
	buffer.rolling = bits.RotateLeft64(buffer.rolling, 1) ^ linkFingerprint(item)
	if shouldCut(len(buffer.links), buffer.rolling, b.options) {
		return b.flushLinks(level)
	}
	return nil
}

func (b *SortedBuilder) flushLinks(level int) error {
	buffer := &b.levels[level]
	group := buffer.links
	buffer.links = nil
	buffer.rolling = 0
	hash, err := writeNode(b.ctx, b.store, node{Level: uint8(level + 1), Children: group})
	if err != nil {
		return err
	}
	return b.addLink(level+1, link{MaxKey: clone(group[len(group)-1].MaxKey), Hash: hash})
}

func (b *SortedBuilder) hasLinksAbove(level int) bool {
	for i := level + 1; i < len(b.levels); i++ {
		if len(b.levels[i].links) != 0 {
			return true
		}
	}
	return false
}

func (t *Tree) Root() storage.Hash { return t.root }

// Iterator returns a lazy ordered iterator positioned before the first entry.
func (t *Tree) Iterator(ctx context.Context) (*Iterator, error) {
	it := &Iterator{ctx: ctx, store: t.store}
	if err := it.descend(t.root); err != nil {
		return nil, err
	}
	return it, nil
}

// Next returns the next entry, or ok=false at end of input. Returned bytes are
// owned by the caller and remain valid after subsequent calls.
func (it *Iterator) Next() (entry Entry, ok bool, err error) {
	if it.done {
		return Entry{}, false, nil
	}
	if err := it.ctx.Err(); err != nil {
		it.Close()
		return Entry{}, false, err
	}
	for {
		if it.index < len(it.leaf) {
			entry := it.leaf[it.index]
			it.index++
			return entry, true, nil
		}
		if err := it.advance(); err != nil {
			it.Close()
			return Entry{}, false, err
		}
		if it.done {
			return Entry{}, false, nil
		}
	}
}

// Close releases iterator buffers. It is safe to call more than once.
func (it *Iterator) Close() error {
	it.stack = nil
	it.leaf = nil
	it.done = true
	return nil
}

func (it *Iterator) descend(hash storage.Hash) error {
	for {
		n, err := readNode(it.ctx, it.store, hash)
		if err != nil {
			return err
		}
		if n.Level == 0 {
			it.leaf, it.index = n.Entries, 0
			return nil
		}
		if len(n.Children) == 0 {
			return fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		it.stack = append(it.stack, iteratorFrame{node: n, next: 1})
		hash = n.Children[0].Hash
	}
}

func (it *Iterator) advance() error {
	for len(it.stack) != 0 {
		top := &it.stack[len(it.stack)-1]
		if top.next < len(top.node.Children) {
			hash := top.node.Children[top.next].Hash
			top.next++
			return it.descend(hash)
		}
		it.stack = it.stack[:len(it.stack)-1]
	}
	it.done = true
	it.leaf = nil
	return nil
}

// Entries returns every entry in key order.
func (t *Tree) Entries(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	if err := walk(ctx, t.store, t.root, nil, func(n node) {
		if n.Level == 0 {
			entries = append(entries, cloneEntries(n.Entries)...)
		}
	}); err != nil {
		return nil, err
	}
	return entries, nil
}

// Reachable validates the tree and returns every reachable object hash.
func Reachable(ctx context.Context, store storage.Store, root storage.Hash) ([]storage.Hash, error) {
	seen := make(map[storage.Hash]struct{})
	if err := walk(ctx, store, root, seen, nil); err != nil {
		return nil, err
	}
	hashes := make([]storage.Hash, 0, len(seen))
	for hash := range seen {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	return hashes, nil
}

func walk(ctx context.Context, store storage.Store, hash storage.Hash, seen map[storage.Hash]struct{}, visit func(node)) error {
	if seen != nil {
		if _, ok := seen[hash]; ok {
			return nil
		}
		seen[hash] = struct{}{}
	}
	n, err := readNode(ctx, store, hash)
	if err != nil {
		return fmt.Errorf("read Prolly node %s: %w", hash, err)
	}
	if n.Level == 0 {
		if len(n.Children) != 0 {
			return fmt.Errorf("leaf Prolly node %s has children", hash)
		}
		for i := 1; i < len(n.Entries); i++ {
			if bytes.Compare(n.Entries[i-1].Key, n.Entries[i].Key) >= 0 {
				return fmt.Errorf("Prolly node %s entries are not strictly ordered", hash)
			}
		}
	} else {
		if len(n.Entries) != 0 || len(n.Children) == 0 {
			return fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		for i, child := range n.Children {
			if !child.Hash.Valid() || (i > 0 && bytes.Compare(n.Children[i-1].MaxKey, child.MaxKey) >= 0) {
				return fmt.Errorf("invalid child link in Prolly node %s", hash)
			}
			if err := walk(ctx, store, child.Hash, seen, visit); err != nil {
				return err
			}
		}
	}
	if visit != nil {
		visit(n)
	}
	return nil
}

func (t *Tree) Get(ctx context.Context, key []byte) ([]byte, error) {
	hash := t.root
	for {
		n, err := readNode(ctx, t.store, hash)
		if err != nil {
			return nil, err
		}
		if n.Level == 0 {
			i := sort.Search(len(n.Entries), func(i int) bool {
				return bytes.Compare(n.Entries[i].Key, key) >= 0
			})
			if i == len(n.Entries) || !bytes.Equal(n.Entries[i].Key, key) {
				return nil, ErrNotFound
			}
			return clone(n.Entries[i].Value), nil
		}
		i := sort.Search(len(n.Children), func(i int) bool {
			return bytes.Compare(n.Children[i].MaxKey, key) >= 0
		})
		if i == len(n.Children) {
			return nil, ErrNotFound
		}
		hash = n.Children[i].Hash
	}
}

func (o Options) validate() error {
	if o.MinChunkEntries < 1 || o.MaxChunkEntries < 2 || o.MaxChunkEntries < o.MinChunkEntries {
		return errors.New("chunk sizes must satisfy 1 <= min <= max and max >= 2")
	}
	if o.BoundaryBits < 1 || o.BoundaryBits > 63 {
		return errors.New("boundary bits must be between 1 and 63")
	}
	return nil
}

func chunkEntries(entries []Entry, options Options) [][]Entry {
	return chunk(len(entries), options, func(i int) uint64 {
		return entryFingerprint(entries[i])
	}, func(start, end int) []Entry { return entries[start:end] })
}

func chunkLinks(links []link, options Options) [][]link {
	return chunk(len(links), options, func(i int) uint64 {
		return linkFingerprint(links[i])
	}, func(start, end int) []link { return links[start:end] })
}

func entryFingerprint(entry Entry) uint64 {
	h := fnv.New64a()
	h.Write(entry.Key)
	h.Write([]byte{0})
	h.Write(entry.Value)
	return h.Sum64()
}

func linkFingerprint(item link) uint64 {
	h := fnv.New64a()
	h.Write(item.MaxKey)
	h.Write([]byte(item.Hash))
	return h.Sum64()
}

func shouldCut(size int, rolling uint64, options Options) bool {
	mask := uint64(1)<<options.BoundaryBits - 1
	return size >= options.MaxChunkEntries || (size >= options.MinChunkEntries && rolling&mask == 0)
}

func chunk[T any](length int, options Options, fingerprint func(int) uint64, slice func(int, int) []T) [][]T {
	var groups [][]T
	start := 0
	var rolling uint64
	mask := uint64(1)<<options.BoundaryBits - 1
	for i := 0; i < length; i++ {
		rolling = bits.RotateLeft64(rolling, 1) ^ fingerprint(i)
		size := i - start + 1
		if size >= options.MaxChunkEntries || (size >= options.MinChunkEntries && rolling&mask == 0) {
			groups = append(groups, slice(start, i+1))
			start = i + 1
			rolling = 0
		}
	}
	if start < length {
		groups = append(groups, slice(start, length))
	}
	return groups
}

func writeNode(ctx context.Context, store storage.Store, n node) (storage.Hash, error) {
	data, err := json.Marshal(n)
	if err != nil {
		return "", err
	}
	return store.Put(ctx, data)
}

func readNode(ctx context.Context, store storage.Store, hash storage.Hash) (node, error) {
	data, err := store.Get(ctx, hash)
	if err != nil {
		return node{}, err
	}
	var n node
	if err := json.Unmarshal(data, &n); err != nil {
		return node{}, fmt.Errorf("decode node %s: %w", hash, err)
	}
	return n, nil
}

func cloneEntries(entries []Entry) []Entry {
	cloned := make([]Entry, len(entries))
	for i, entry := range entries {
		cloned[i] = Entry{Key: clone(entry.Key), Value: clone(entry.Value)}
	}
	return cloned
}

func clone(data []byte) []byte { return append([]byte(nil), data...) }
