package engine

import (
	"github.com/zeebo/xxh3"
)

// hash128 returns the 128 bit xxh3 hash of b.  xxh3 is used purely as a fast
// in-process mixdown of the canonical row bytes; equality of the aggregate is
// always cross checked by an exact row merge before anything is reported, so a
// hash collision cannot produce a wrong difference list.
func hash128(b []byte) [16]byte {
	return xxh3.Hash128(b).Bytes()
}
