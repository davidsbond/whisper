// Package bytepool provides a wrapper around sync.Pool for byte slices of a specified length.
package bytepool

import (
	"sync"
)

type (
	// The Pool type is a wrapper around sync.Pool.
	Pool struct {
		sync.Pool
	}
)

// New returns a new Pool that will manage byte slices of a specified length.
func New(length int) *Pool {
	return &Pool{
		Pool: sync.Pool{
			New: func() any {
				b := make([]byte, length)
				return &b
			},
		},
	}
}

// Get a byte slice.
func (p *Pool) Get() *[]byte {
	buf := p.Pool.Get().(*[]byte)

	return buf
}

// Put a byte slice back into the pool for later use.
func (p *Pool) Put(buf *[]byte) {
	p.Pool.Put(buf)
}
