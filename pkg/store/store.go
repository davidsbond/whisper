// Package store provides types for persisting peer data.
package store

import (
	"errors"
)

var (
	// ErrPeerNotFound is the error given when querying a peer that does not exist.
	ErrPeerNotFound = errors.New("peer not found")
)
