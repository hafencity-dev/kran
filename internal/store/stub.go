package store

import "errors"

// Replaced by the real implementation (owner C).
func open(path string) (Store, error) { return nil, errors.New("store: not implemented") }
