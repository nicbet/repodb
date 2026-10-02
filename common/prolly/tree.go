// Package prolly implements a small immutable, content-addressed Prolly tree.
// It is an experimental storage core, not yet a production B-tree replacement.
package prolly

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"

	"github.com/nicbet/repodb/common/storage"
)

var ErrNotFound = errors.New("key not found")

// Entry is one key and value. Entries returned by a tree alias its stored
// node bytes, which are shared and must not be modified.
type Entry struct {
	Key   []byte
	Value []byte
}

// Edit replaces or deletes one key. Apply requires edits in strictly increasing
// key order. A non-delete edit inserts the key when it is absent.
type Edit struct {
	Key    []byte
	Value  []byte
	Delete bool
}

// link points at a child node: its largest key, the number of leaf entries
// below it and its hash.
type link struct {
	MaxKey []byte
	Count  uint64
	Hash   storage.Hash
}

type node struct {
	Level    uint8
	Entries  []Entry
	Children []link
}

type Options struct {
	MinChunkEntries int
	MaxChunkEntries int
	BoundaryBits    uint8
}

// DefaultOptions give leaves of about 124 entries on average. A chunk rarely
// reaches the 256-entry cap (about 4% of chunks), so nearly every boundary is
// content-defined.
var DefaultOptions = Options{MinChunkEntries: 64, MaxChunkEntries: 256, BoundaryBits: 6}

type Tree struct {
	store   storage.Store
	root    storage.Hash
	options Options
}

// Iterator lazily walks a tree in key order. At most one node per tree level is
// retained, so callers do not need to materialize the complete entry set. The
// current leaf is parsed in place, one entry per Next.
type Iterator struct {
	ctx   context.Context
	store storage.Store
	stack []iteratorFrame
	leaf  nodeReader
	done  bool
	// prefetch batches each internal node's children into one store read;
	// set for full-tree iteration, which visits them all.
	prefetch bool
}

type iteratorFrame struct {
	node node
	next int
}

// SortedBuilder incrementally constructs the same canonical tree as Build from
// strictly ordered entries. It buffers at most one chunk per tree level.
type SortedBuilder struct {
	ctx      context.Context
	store    storage.Store
	options  Options
	leaves   []Entry
	levels   []linkBuffer
	lastKey  []byte
	haveKey  bool
	finished bool
}

type linkBuffer struct {
	links []link
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
		links = append(links, link{MaxKey: clone(group[len(group)-1].Key), Count: uint64(len(group)), Hash: hash})
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
			next = append(next, link{MaxKey: clone(group[len(group)-1].MaxKey), Count: sumCounts(group), Hash: hash})
		}
		links = next
		level++
	}
	return &Tree{store: store, root: links[0].Hash, options: options}, nil
}

// Apply incrementally applies sorted edits while preserving Build's canonical
// chunking. It reuses untouched leaf blobs before and after the affected range,
// and rebuilds internal nodes from leaf links without decoding unrelated rows.
func Apply(ctx context.Context, store storage.Store, tree *Tree, edits []Edit) (*Tree, error) {
	if store == nil || tree == nil {
		return nil, errors.New("store and tree are required")
	}
	if len(edits) == 0 {
		return &Tree{store: store, root: tree.root, options: tree.options}, nil
	}
	for i, edit := range edits {
		if len(edit.Key) == 0 {
			return nil, errors.New("edit key is required")
		}
		if i > 0 && bytes.Compare(edits[i-1].Key, edit.Key) >= 0 {
			return nil, errors.New("edits are not strictly ordered")
		}
	}
	leaves, err := leafLinks(ctx, tree.store, tree.root)
	if err != nil {
		return nil, err
	}
	start := sort.Search(len(leaves), func(i int) bool { return bytes.Compare(leaves[i].MaxKey, edits[0].Key) >= 0 })
	if start == len(leaves) {
		start = len(leaves) - 1
	}
	result := append([]link(nil), leaves[:start]...)
	chunker := &leafChunker{ctx: ctx, store: store, options: tree.options, links: &result}
	editIndex := 0
	for leafIndex := start; leafIndex < len(leaves); leafIndex++ {
		n, err := readNode(ctx, tree.store, leaves[leafIndex].Hash)
		if err != nil {
			return nil, err
		}
		if n.Level != 0 {
			return nil, errors.New("invalid leaf link")
		}
		for _, entry := range n.Entries {
			for editIndex < len(edits) && bytes.Compare(edits[editIndex].Key, entry.Key) < 0 {
				if !edits[editIndex].Delete {
					if err := chunker.add(Entry{Key: edits[editIndex].Key, Value: edits[editIndex].Value}); err != nil {
						return nil, err
					}
				}
				editIndex++
			}
			if editIndex < len(edits) && bytes.Equal(edits[editIndex].Key, entry.Key) {
				if !edits[editIndex].Delete {
					if err := chunker.add(Entry{Key: edits[editIndex].Key, Value: edits[editIndex].Value}); err != nil {
						return nil, err
					}
				}
				editIndex++
			} else if err := chunker.add(entry); err != nil {
				return nil, err
			}
		}
		if editIndex == len(edits) && len(chunker.entries) == 0 {
			result = append(result, leaves[leafIndex+1:]...)
			return buildFromLeafLinks(ctx, store, result, tree.options)
		}
	}
	for editIndex < len(edits) {
		if !edits[editIndex].Delete {
			if err := chunker.add(Entry{Key: edits[editIndex].Key, Value: edits[editIndex].Value}); err != nil {
				return nil, err
			}
		}
		editIndex++
	}
	if err := chunker.flush(); err != nil {
		return nil, err
	}
	if len(result) == 0 {
		hash, err := writeNode(ctx, store, node{Level: 0, Entries: []Entry{}})
		if err != nil {
			return nil, err
		}
		result = append(result, link{Hash: hash, Count: 0})
	}
	return buildFromLeafLinks(ctx, store, result, tree.options)
}

type leafChunker struct {
	ctx     context.Context
	store   storage.Store
	options Options
	entries []Entry
	links   *[]link
}

func (c *leafChunker) add(entry Entry) error {
	item := Entry{Key: clone(entry.Key), Value: clone(entry.Value)}
	c.entries = append(c.entries, item)
	if shouldCut(len(c.entries), entryBoundaryHash(item), c.options) {
		return c.flush()
	}
	return nil
}

func (c *leafChunker) flush() error {
	if len(c.entries) == 0 {
		return nil
	}
	group := c.entries
	c.entries = nil
	hash, err := writeNode(c.ctx, c.store, node{Level: 0, Entries: group})
	if err != nil {
		return err
	}
	*c.links = append(*c.links, link{MaxKey: clone(group[len(group)-1].Key), Count: uint64(len(group)), Hash: hash})
	return nil
}

func leafLinks(ctx context.Context, store storage.Store, root storage.Hash) ([]link, error) {
	n, err := readNode(ctx, store, root)
	if err != nil {
		return nil, err
	}
	if n.Level == 0 {
		var max []byte
		if len(n.Entries) != 0 {
			max = clone(n.Entries[len(n.Entries)-1].Key)
		}
		return []link{{MaxKey: max, Count: uint64(len(n.Entries)), Hash: root}}, nil
	}
	if n.Level == 1 {
		return append([]link(nil), n.Children...), nil
	}
	var leaves []link
	for _, child := range n.Children {
		items, err := leafLinks(ctx, store, child.Hash)
		if err != nil {
			return nil, err
		}
		leaves = append(leaves, items...)
	}
	return leaves, nil
}

func buildFromLeafLinks(ctx context.Context, store storage.Store, leaves []link, options Options) (*Tree, error) {
	if len(leaves) == 1 {
		return &Tree{store: store, root: leaves[0].Hash, options: options}, nil
	}
	links := leaves
	level := uint8(1)
	for len(links) > 1 {
		groups := chunkLinks(links, options)
		next := make([]link, 0, len(groups))
		for _, group := range groups {
			hash, err := writeNode(ctx, store, node{Level: level, Children: group})
			if err != nil {
				return nil, err
			}
			next = append(next, link{MaxKey: clone(group[len(group)-1].MaxKey), Count: sumCounts(group), Hash: hash})
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
	if shouldCut(len(b.leaves), entryBoundaryHash(item), b.options) {
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
	hash, err := writeNode(b.ctx, b.store, node{Level: 0, Entries: group})
	if err != nil {
		return err
	}
	return b.addLink(0, link{MaxKey: clone(group[len(group)-1].Key), Count: uint64(len(group)), Hash: hash})
}

func (b *SortedBuilder) addLink(level int, item link) error {
	for len(b.levels) <= level {
		b.levels = append(b.levels, linkBuffer{})
	}
	buffer := &b.levels[level]
	buffer.links = append(buffer.links, item)
	if shouldCut(len(buffer.links), linkBoundaryHash(item), b.options) {
		return b.flushLinks(level)
	}
	return nil
}

func (b *SortedBuilder) flushLinks(level int) error {
	buffer := &b.levels[level]
	group := buffer.links
	buffer.links = nil
	hash, err := writeNode(b.ctx, b.store, node{Level: uint8(level + 1), Children: group})
	if err != nil {
		return err
	}
	return b.addLink(level+1, link{MaxKey: clone(group[len(group)-1].MaxKey), Count: sumCounts(group), Hash: hash})
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
	it := &Iterator{ctx: ctx, store: t.store, prefetch: true}
	if err := it.descend(t.root); err != nil {
		return nil, err
	}
	return it, nil
}

// IteratorFrom returns a lazy ordered iterator positioned at the first entry
// whose key is >= startKey. If no such entry exists the iterator is immediately
// done.
func (t *Tree) IteratorFrom(ctx context.Context, startKey []byte) (*Iterator, error) {
	it := &Iterator{ctx: ctx, store: t.store}
	if err := it.seekTo(t.root, startKey); err != nil {
		return nil, err
	}
	return it, nil
}

// Next returns the next entry, or ok=false at end of input. The returned bytes
// are shared with the store: they stay valid after later calls but must not be
// modified.
func (it *Iterator) Next() (entry Entry, ok bool, err error) {
	if it.done {
		return Entry{}, false, nil
	}
	if err := it.ctx.Err(); err != nil {
		it.Close()
		return Entry{}, false, err
	}
	for {
		if it.leaf.remaining > 0 {
			entry, err := it.leaf.nextEntry()
			if err != nil {
				it.Close()
				return Entry{}, false, err
			}
			return entry, true, nil
		}
		if it.leaf.data != nil {
			if err := it.leaf.finish(); err != nil {
				it.Close()
				return Entry{}, false, err
			}
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
	it.leaf = nodeReader{}
	it.done = true
	return nil
}

// openNode reads a node and positions a reader at its first item.
func openNode(ctx context.Context, store storage.Store, hash storage.Hash) (nodeReader, error) {
	data, err := store.Get(ctx, hash)
	if err != nil {
		return nodeReader{}, err
	}
	return newNodeReader(hash, data)
}

func (it *Iterator) descend(hash storage.Hash) error {
	for {
		r, err := openNode(it.ctx, it.store, hash)
		if err != nil {
			return err
		}
		if r.level == 0 {
			it.leaf = r
			return nil
		}
		n, err := r.decode()
		if err != nil {
			return err
		}
		if len(n.Children) == 0 {
			return fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		if it.prefetch {
			prefetchChildren(it.ctx, it.store, n, nil)
		}
		it.stack = append(it.stack, iteratorFrame{node: n, next: 1})
		hash = n.Children[0].Hash
	}
}

func (it *Iterator) seekTo(hash storage.Hash, startKey []byte) error {
	for {
		r, err := openNode(it.ctx, it.store, hash)
		if err != nil {
			return err
		}
		if r.level == 0 {
			for r.remaining > 0 {
				before := r
				entry, err := r.nextEntry()
				if err != nil {
					return err
				}
				if bytes.Compare(entry.Key, startKey) >= 0 {
					it.leaf = before
					return nil
				}
			}
			if err := r.finish(); err != nil {
				return err
			}
			return it.advance()
		}
		n, err := r.decode()
		if err != nil {
			return err
		}
		if len(n.Children) == 0 {
			return fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		i := sort.Search(len(n.Children), func(i int) bool {
			return bytes.Compare(n.Children[i].MaxKey, startKey) >= 0
		})
		if i == len(n.Children) {
			it.done = true
			return nil
		}
		it.stack = append(it.stack, iteratorFrame{node: n, next: i + 1})
		hash = n.Children[i].Hash
	}
}

func (it *Iterator) advance() error {
	it.leaf = nodeReader{}
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
	return nil
}

// Entries returns every entry in key order.
func (t *Tree) Entries(ctx context.Context) ([]Entry, error) {
	var entries []Entry
	if _, err := walk(ctx, t.store, t.root, nil, func(n node) {
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
	seen := make(map[storage.Hash]uint64)
	if _, err := walk(ctx, store, root, seen, nil); err != nil {
		return nil, err
	}
	hashes := make([]storage.Hash, 0, len(seen))
	for hash := range seen {
		hashes = append(hashes, hash)
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i] < hashes[j] })
	return hashes, nil
}

// walk validates the subtree at hash and returns its number of leaf entries.
// seen, when set, records the count of each visited node so shared subtrees
// are walked once.
func walk(ctx context.Context, store storage.Store, hash storage.Hash, seen map[storage.Hash]uint64, visit func(node)) (uint64, error) {
	if seen != nil {
		if count, ok := seen[hash]; ok {
			return count, nil
		}
	}
	n, err := readNode(ctx, store, hash)
	if err != nil {
		return 0, fmt.Errorf("read Prolly node %s: %w", hash, err)
	}
	var count uint64
	if n.Level == 0 {
		if len(n.Children) != 0 {
			return 0, fmt.Errorf("leaf Prolly node %s has children", hash)
		}
		for i := 1; i < len(n.Entries); i++ {
			if bytes.Compare(n.Entries[i-1].Key, n.Entries[i].Key) >= 0 {
				return 0, fmt.Errorf("prolly node %s entries are not strictly ordered", hash)
			}
		}
		count = uint64(len(n.Entries))
	} else {
		if len(n.Entries) != 0 || len(n.Children) == 0 {
			return 0, fmt.Errorf("invalid internal Prolly node %s", hash)
		}
		prefetchChildren(ctx, store, n, seen)
		for i, child := range n.Children {
			if !child.Hash.Valid() || (i > 0 && bytes.Compare(n.Children[i-1].MaxKey, child.MaxKey) >= 0) {
				return 0, fmt.Errorf("invalid child link in Prolly node %s", hash)
			}
			childCount, err := walk(ctx, store, child.Hash, seen, visit)
			if err != nil {
				return 0, err
			}
			if childCount != child.Count {
				return 0, fmt.Errorf("child link in Prolly node %s counts %d entries, child has %d", hash, child.Count, childCount)
			}
			count += childCount
		}
	}
	if seen != nil {
		seen[hash] = count
	}
	if visit != nil {
		visit(n)
	}
	return count, nil
}

// Get returns a copy of the value stored under key. Each node on the path is
// binary-searched through its cached item offsets.
func (t *Tree) Get(ctx context.Context, key []byte) ([]byte, error) {
	hash := t.root
	for {
		r, err := openNode(ctx, t.store, hash)
		if err != nil {
			return nil, err
		}
		i, item, err := r.search(key)
		if err != nil {
			return nil, err
		}
		// A fresh reader's remaining is the node's item count.
		if i == r.remaining {
			return nil, ErrNotFound
		}
		if r.level == 0 {
			entry, err := item.nextEntry()
			if err != nil {
				return nil, err
			}
			if !bytes.Equal(entry.Key, key) {
				return nil, ErrNotFound
			}
			return clone(entry.Value), nil
		}
		_, _, raw, err := item.nextLinkRaw()
		if err != nil {
			return nil, err
		}
		hash = storage.Hash(hex.EncodeToString(raw))
	}
}

// Count returns the number of entries in the tree. It reads only the root.
func (t *Tree) Count(ctx context.Context) (uint64, error) {
	r, err := openNode(ctx, t.store, t.root)
	if err != nil {
		return 0, err
	}
	if r.level == 0 {
		return uint64(r.remaining), nil
	}
	var total uint64
	for r.remaining > 0 {
		_, count, _, err := r.nextLinkRaw()
		if err != nil {
			return 0, err
		}
		total += count
	}
	return total, r.finish()
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
		return entryBoundaryHash(entries[i])
	}, func(start, end int) []Entry { return entries[start:end] })
}

func chunkLinks(links []link, options Options) [][]link {
	return chunk(len(links), options, func(i int) uint64 {
		return linkBoundaryHash(links[i])
	}, func(start, end int) []link { return links[start:end] })
}

// Chunk boundaries are content-local: whether a chunk may end after an item
// depends only on that item's key (a leaf entry's key, or a link's MaxKey)
// and on the chunk's size. Hashing values or child hashes, or accumulating a
// hash across items, would let one edit move every later boundary, and Apply
// would then rewrite chunks until the boundaries happened to realign.
func entryBoundaryHash(entry Entry) uint64 { return keyBoundaryHash(entry.Key) }

func linkBoundaryHash(item link) uint64 { return keyBoundaryHash(item.MaxKey) }

// keyBoundaryHash is FNV-64a of the key passed through the splitmix64
// finalizer. shouldCut tests the low bits, and FNV-64a's low bits depend only
// on the low bits of the key's last byte: sequential integer keys would cut
// exactly every 2^BoundaryBits keys. The finalizer makes every output bit
// depend on every input bit.
func keyBoundaryHash(key []byte) uint64 {
	h := fnv.New64a()
	h.Write(key)
	x := h.Sum64()
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	x ^= x >> 31
	return x
}

// shouldCut reports whether a chunk of size entries ends at an item whose
// boundary hash is hash.
func shouldCut(size int, hash uint64, options Options) bool {
	mask := uint64(1)<<options.BoundaryBits - 1
	return size >= options.MaxChunkEntries || (size >= options.MinChunkEntries && hash&mask == 0)
}

func chunk[T any](length int, options Options, boundaryHash func(int) uint64, slice func(int, int) []T) [][]T {
	var groups [][]T
	start := 0
	for i := 0; i < length; i++ {
		if shouldCut(i-start+1, boundaryHash(i), options) {
			groups = append(groups, slice(start, i+1))
			start = i + 1
		}
	}
	if start < length {
		groups = append(groups, slice(start, length))
	}
	return groups
}

func writeNode(ctx context.Context, store storage.Store, n node) (storage.Hash, error) {
	data, err := encodeNode(n)
	if err != nil {
		return "", err
	}
	return store.Put(ctx, data)
}

// prefetchChildren hints store to read n's children not yet in seen in one
// batch.
func prefetchChildren(ctx context.Context, store storage.Store, n node, seen map[storage.Hash]uint64) {
	if _, ok := store.(storage.Prefetcher); !ok {
		return
	}
	hashes := make([]storage.Hash, 0, len(n.Children))
	for _, child := range n.Children {
		if _, visited := seen[child.Hash]; !visited {
			hashes = append(hashes, child.Hash)
		}
	}
	storage.Prefetch(ctx, store, hashes)
}

func readNode(ctx context.Context, store storage.Store, hash storage.Hash) (node, error) {
	data, err := store.Get(ctx, hash)
	if err != nil {
		return node{}, err
	}
	return decodeNode(hash, data)
}

func sumCounts(links []link) uint64 {
	var total uint64
	for _, l := range links {
		total += l.Count
	}
	return total
}

func cloneEntries(entries []Entry) []Entry {
	cloned := make([]Entry, len(entries))
	for i, entry := range entries {
		cloned[i] = Entry{Key: clone(entry.Key), Value: clone(entry.Value)}
	}
	return cloned
}

func clone(data []byte) []byte { return append([]byte(nil), data...) }
