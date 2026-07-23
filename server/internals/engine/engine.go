package engine

import (
	"context"
	"errors"
	"io"
)

// RoutingMode describes how the proxy figures out which database a client
// connection is meant for.
type RoutingMode int

const (
	// SharedPortStartupRouting means all databases of this engine share one
	// listener port and the target is read out of the client's first packet.
	// Only works for client-speaks-first protocols (Postgres).
	SharedPortStartupRouting RoutingMode = iota

	// PortPerDatabase means each database gets its own listener port, because
	// the protocol is server-speaks-first (MySQL, MongoDB) and the target
	// cannot be learned before the server would have to greet the client.
	PortPerDatabase
)

// ErrCancelRequest is returned by Resolve when the client sent a Postgres
// CancelRequest, which carries no database name and cannot be routed.
var ErrCancelRequest = errors.New("engine: cancel request carries no database")

// ErrNoDatabase is returned when the client's handshake named no database.
var ErrNoDatabase = errors.New("engine: client named no database")

// Engine abstracts one database technology. The scale-to-zero core is engine
// agnostic; adding a new database means implementing this interface.
type Engine interface {
	// Name is the engine identifier stored in metadata, e.g. "postgres".
	Name() string

	// Image is the container image used to provision a database.
	Image() string

	// InternalPort is the port the database listens on inside the container.
	InternalPort() int

	// DataDir is the in-container path that must be backed by the volume.
	DataDir() string

	// Env builds the container environment that seeds user/password/database.
	Env(user, password, database string) []string

	// Routing reports how connections for this engine are matched to a database.
	Routing() RoutingMode

	// Resolve reads from the client connection just enough to learn which
	// database it wants. It returns the database name plus the bytes that must
	// be replayed to the backend once it is awake (the handshake we consumed).
	//
	// It takes an io.ReadWriter because some protocols require answering the
	// client mid-handshake before the target is known, e.g. Postgres SSLRequest
	// negotiation.
	Resolve(rw io.ReadWriter) (database string, replay []byte, err error)

	// Ready reports whether the backend at addr is accepting client
	// connections. Used to decide when a woken container can be forwarded to.
	Ready(ctx context.Context, addr string) error
}

// ErrorReporter is implemented by engines that can express an error in their
// own wire protocol, so a client sees a real message instead of a dropped
// connection. Optional: the proxy simply closes the socket without it.
type ErrorReporter interface {
	// WriteError emits a protocol-native error. code is engine specific
	// (a SQLSTATE for Postgres).
	WriteError(w io.Writer, code, message string) error
}

// Registry maps engine names to implementations.
type Registry map[string]Engine

// NewRegistry builds a registry containing the given engines, keyed by name.
func NewRegistry(engines ...Engine) Registry {
	r := make(Registry, len(engines))
	for _, e := range engines {
		r[e.Name()] = e
	}
	return r
}

// Get returns the engine registered under name.
func (r Registry) Get(name string) (Engine, bool) {
	e, ok := r[name]
	return e, ok
}
