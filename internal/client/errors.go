package client

import (
	"errors"
	"fmt"
)

var errNoDaemon = errors.New("kran daemon is not running")

// ErrPermission: the socket exists but this user may not open it (the
// daemon runs as someone else, or the runtime dir has wrong permissions).
var ErrPermission = errors.New("permission denied on the kran daemon socket")

// ErrIncompatible: the daemon answered but speaks another API/protocol major.
var ErrIncompatible = errors.New("kran daemon runs an incompatible version")

// ErrSessionTimeout: the daemon stopped sending anything (not even
// heartbeats) on a run session.
var ErrSessionTimeout = errors.New("kran daemon stopped responding")

// APIError is a non-2xx answer of the local API.
type APIError struct {
	Status  int
	Message string
	Hint    string
}

func (e *APIError) Error() string {
	if e.Hint != "" {
		return fmt.Sprintf("%s (%s)", e.Message, e.Hint)
	}
	return e.Message
}

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == 404
}
