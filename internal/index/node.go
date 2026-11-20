// Package index holds the B+ tree. A node is one page, laid out as fixed size
// arrays. This file has what leaves and internal nodes share
package index

import (
	"encoding/binary"
	"errors"

	"github.com/wilfex81/DB_Engine/internal/storage"
)

var ErrCorrupt = errors.New("index: corrupt node")

const (
	keySize   = 8 // int64, little endian
	ridSize   = 6 // page number (4), slot(2)
	childSize = 4 // page number

	body = storage.PageSize - storage.HeaderSize

	// LeafCapacity is the most entries a lead can hold.
	LeafCapacity = body / (keySize + ridSize) // 291
	// InternalCapacity is the most keys an internal node can hold. It always
	// has one more child than keys
	InternalCapacity = (body - childSize) / (keySize + childSize) //339

	// Leaf: header, then LeafCapacity keys, then LeafCapacity RIDs.
	// Internal: header, then InternalCapacity Keys, then InternalCapacity+1 children.
	keysAt     = storage.HeaderSize
	leafRIDsAt = keysAt + LeafCapacity*keySize     // 2344
	childrenAt = keysAt + InternalCapacity*keySize // 2728
)

// The compiler rejects the builds if a layout does not fit in a page.
var (
	_ [storage.PageSize - (leafRIDsAt + LeafCapacity*ridSize)]struct{}
	_ [storage.PageSize - (childrenAt + (InternalCapacity+1)*childSize)]struct{}
)

func count(p *storage.Page) int       { return int(binary.LittleEndian.Uint16(p[0:2])) }
func setCount(p *storage.Page, n int) { binary.LittleEndian.PutUint16(p[0:2], uint16(n)) }

func key(p *storage.Page, i int) int64 {
	return int64(binary.LittleEndian.Uint64(p[keysAt+i*keySize:]))
}

func setKey(p *storage.Page, i int, k int64) {
	binary.LittleEndian.PutUint64(p[keysAt+i*keySize:], uint64(k))
}

// shift moves the bytes of elements [from,to] of an array that starts at
// base, with elements of the given size, up by one element.
func shiftUp(p *storage.Page, base, size, from, to int) {
	copy(p[base+(from+1)*size:base+(to+1)*size], p[base+from*size:base+to*size])
}

// shiftDown moves elements [from+1, to] down by one, and clears the fred last element
func shiftDown(p *storage.Page, base, size, from, to int) {
	copy(p[base+from*size:base+(to-1)*size], p[base+(from+1)*size:base+to*size])
	clear(p[base+(to-1)*size : base+to*size])
}
