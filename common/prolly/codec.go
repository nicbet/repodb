package prolly

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/nicbet/repodb/common/storage"
)

// Node encoding. A node is
//
//	'P' | codec version | level (u8) | uvarint item count | items
//
// A leaf item is a uvarint key length, the key, a uvarint value length and the
// value. An interior item is a uvarint max-key length, the max key, a uvarint
// count of the leaf entries below the child, and the child's 32-byte SHA-256.
//
// Nodes are read in place: decoded keys and values alias the stored bytes.
const (
	nodeMagic        = 'P'
	nodeCodecVersion = 1
	hashSize         = 32
)

var errTruncatedNode = errors.New("truncated node")

func encodeNode(n node) ([]byte, error) {
	size := 3 + binary.MaxVarintLen64
	for _, e := range n.Entries {
		size += 2*binary.MaxVarintLen64 + len(e.Key) + len(e.Value)
	}
	for _, c := range n.Children {
		size += 2*binary.MaxVarintLen64 + len(c.MaxKey) + hashSize
	}
	b := make([]byte, 0, size)
	b = append(b, nodeMagic, nodeCodecVersion, n.Level)
	if n.Level == 0 {
		b = binary.AppendUvarint(b, uint64(len(n.Entries)))
		for _, e := range n.Entries {
			b = binary.AppendUvarint(b, uint64(len(e.Key)))
			b = append(b, e.Key...)
			b = binary.AppendUvarint(b, uint64(len(e.Value)))
			b = append(b, e.Value...)
		}
		return b, nil
	}
	b = binary.AppendUvarint(b, uint64(len(n.Children)))
	for _, c := range n.Children {
		raw, err := hex.DecodeString(string(c.Hash))
		if err != nil || len(raw) != hashSize {
			return nil, fmt.Errorf("invalid child hash %q", c.Hash)
		}
		b = binary.AppendUvarint(b, uint64(len(c.MaxKey)))
		b = append(b, c.MaxKey...)
		b = binary.AppendUvarint(b, c.Count)
		b = append(b, raw...)
	}
	return b, nil
}

// nodeReader parses an encoded node in place, one item at a time.
type nodeReader struct {
	hash      storage.Hash
	data      []byte
	pos       int
	level     uint8
	remaining int
}

func newNodeReader(hash storage.Hash, data []byte) (nodeReader, error) {
	r := nodeReader{hash: hash, data: data}
	if len(data) < 4 || data[0] != nodeMagic {
		return r, r.errorf(errors.New("not a Prolly node"))
	}
	if data[1] != nodeCodecVersion {
		return r, r.errorf(fmt.Errorf("unsupported node codec version %d", data[1]))
	}
	r.level = data[2]
	r.pos = 3
	count, err := r.uvarint()
	if err != nil {
		return r, err
	}
	// Every item takes at least two bytes, which bounds count by the input.
	if count > uint64(len(data)-r.pos)/2 {
		return r, r.errorf(errTruncatedNode)
	}
	r.remaining = int(count)
	return r, nil
}

func (r *nodeReader) errorf(err error) error {
	return fmt.Errorf("decode node %s: %w", r.hash, err)
}

func (r *nodeReader) uvarint() (uint64, error) {
	v, n := binary.Uvarint(r.data[r.pos:])
	if n <= 0 {
		return 0, r.errorf(errTruncatedNode)
	}
	r.pos += n
	return v, nil
}

// field returns a length-prefixed byte string, aliasing the input with its
// capacity clipped so an append cannot overwrite the following bytes.
func (r *nodeReader) field() ([]byte, error) {
	length, err := r.uvarint()
	if err != nil {
		return nil, err
	}
	if length > uint64(len(r.data)-r.pos) {
		return nil, r.errorf(errTruncatedNode)
	}
	end := r.pos + int(length)
	f := r.data[r.pos:end:end]
	r.pos = end
	return f, nil
}

// nextEntry returns the next leaf entry. The caller checks remaining.
func (r *nodeReader) nextEntry() (Entry, error) {
	key, err := r.field()
	if err != nil {
		return Entry{}, err
	}
	value, err := r.field()
	if err != nil {
		return Entry{}, err
	}
	r.remaining--
	return Entry{Key: key, Value: value}, nil
}

// nextLinkRaw returns the next interior item with its hash as raw bytes, so a
// search can skip links without converting their hashes.
func (r *nodeReader) nextLinkRaw() (maxKey []byte, count uint64, raw []byte, err error) {
	if maxKey, err = r.field(); err != nil {
		return nil, 0, nil, err
	}
	if count, err = r.uvarint(); err != nil {
		return nil, 0, nil, err
	}
	if len(r.data)-r.pos < hashSize {
		return nil, 0, nil, r.errorf(errTruncatedNode)
	}
	raw = r.data[r.pos : r.pos+hashSize]
	r.pos += hashSize
	r.remaining--
	return maxKey, count, raw, nil
}

func (r *nodeReader) nextLink() (link, error) {
	maxKey, count, raw, err := r.nextLinkRaw()
	if err != nil {
		return link{}, err
	}
	return link{MaxKey: maxKey, Count: count, Hash: storage.Hash(hex.EncodeToString(raw))}, nil
}

// finish reports an error unless every item was read and no bytes remain.
func (r *nodeReader) finish() error {
	if r.remaining != 0 || r.pos != len(r.data) {
		return r.errorf(errors.New("trailing bytes"))
	}
	return nil
}

// decode reads the remaining items into a node.
func (r *nodeReader) decode() (node, error) {
	n := node{Level: r.level}
	if r.level == 0 {
		n.Entries = make([]Entry, 0, r.remaining)
		for r.remaining > 0 {
			entry, err := r.nextEntry()
			if err != nil {
				return node{}, err
			}
			n.Entries = append(n.Entries, entry)
		}
	} else {
		n.Children = make([]link, 0, r.remaining)
		for r.remaining > 0 {
			child, err := r.nextLink()
			if err != nil {
				return node{}, err
			}
			n.Children = append(n.Children, child)
		}
	}
	return n, r.finish()
}

func decodeNode(hash storage.Hash, data []byte) (node, error) {
	r, err := newNodeReader(hash, data)
	if err != nil {
		return node{}, err
	}
	return r.decode()
}
