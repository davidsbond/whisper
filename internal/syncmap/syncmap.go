// Package syncmap provides a concurrent map implementation using paramaterized types.
package syncmap

import (
	"maps"
	"slices"
	"sync"
)

type (
	// The Map type wraps a map within a sync.RWMutex.
	Map[K comparable, V any] struct {
		mux   sync.RWMutex
		stuff map[K]V
	}
)

// New returns a new Map.
func New[K comparable, V any]() *Map[K, V] {
	return &Map[K, V]{
		stuff: make(map[K]V),
	}
}

// Put a value into the Map.
func (m *Map[K, V]) Put(k K, v V) {
	m.mux.Lock()
	m.stuff[k] = v
	m.mux.Unlock()
}

// Get a value from the Map. The boolean return value indicates if a value was found.
func (m *Map[K, V]) Get(k K) (V, bool) {
	m.mux.RLock()
	defer m.mux.RUnlock()

	v, ok := m.stuff[k]
	return v, ok
}

// Values returns a slice of all values within the Map.
func (m *Map[K, V]) Values() []V {
	m.mux.RLock()
	defer m.mux.RUnlock()

	return slices.Collect(maps.Values(m.stuff))
}

// Remove an entry from the Map.
func (m *Map[K, V]) Remove(k K) {
	m.mux.Lock()
	defer m.mux.Unlock()

	delete(m.stuff, k)
}
