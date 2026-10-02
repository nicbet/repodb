package prolly

import (
	"bytes"
	"sort"
	"sync"

	"github.com/nicbet/repodb/common/storage"
)

// Nodes are content-addressed and immutable, so the byte offset of each item
// in a node is a pure function of its hash. Caching the offsets lets point
// reads binary-search a node instead of parsing every item before the key: a
// leaf holds up to MaxChunkEntries rows, and a linear scan touches each one.
//
// The cache is process-wide and bounded. Entries are small (one uint32 per
// item); eviction drops an arbitrary entry from the full shard.
const (
	offsetCacheShards          = 16
	offsetCacheEntriesPerShard = 4096
)

type offsetCacheShard struct {
	mu      sync.RWMutex
	entries map[storage.Hash][]uint32
}

var offsetCache [offsetCacheShards]offsetCacheShard

func offsetShard(hash storage.Hash) *offsetCacheShard {
	var h uint32
	for i := 0; i < len(hash) && i < 8; i++ {
		h = h*31 + uint32(hash[i])
	}
	return &offsetCache[h%offsetCacheShards]
}

// itemOffsets returns the start offset of every item of r's node, parsing and
// validating the node once per hash.
func itemOffsets(r nodeReader) ([]uint32, error) {
	shard := offsetShard(r.hash)
	shard.mu.RLock()
	offsets, ok := shard.entries[r.hash]
	shard.mu.RUnlock()
	if ok {
		return offsets, nil
	}
	offsets = make([]uint32, 0, r.remaining)
	for r.remaining > 0 {
		offsets = append(offsets, uint32(r.pos))
		var err error
		if r.level == 0 {
			_, err = r.nextEntry()
		} else {
			_, _, _, err = r.nextLinkRaw()
		}
		if err != nil {
			return nil, err
		}
	}
	if err := r.finish(); err != nil {
		return nil, err
	}
	shard.mu.Lock()
	if shard.entries == nil {
		shard.entries = make(map[storage.Hash][]uint32)
	}
	if len(shard.entries) >= offsetCacheEntriesPerShard {
		for evict := range shard.entries {
			delete(shard.entries, evict)
			break
		}
	}
	shard.entries[r.hash] = offsets
	shard.mu.Unlock()
	return offsets, nil
}

// at returns a reader positioned at the item starting at offset.
func (r nodeReader) at(offset uint32) nodeReader {
	r.pos = int(offset)
	r.remaining = 1
	return r
}

// search returns the index of the first item whose key (a leaf entry's key or
// an interior link's max key) is >= key, and a reader positioned at it.
func (r nodeReader) search(key []byte) (int, nodeReader, error) {
	offsets, err := itemOffsets(r)
	if err != nil {
		return 0, r, err
	}
	var searchErr error
	i := sort.Search(len(offsets), func(i int) bool {
		item := r.at(offsets[i])
		itemKey, err := item.field()
		if err != nil {
			searchErr = err
			return true
		}
		return bytes.Compare(itemKey, key) >= 0
	})
	if searchErr != nil {
		return 0, r, searchErr
	}
	if i == len(offsets) {
		return i, r, nil
	}
	return i, r.at(offsets[i]), nil
}
