package proxy

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"sparkdb/scaletozero/internals/engine"
	"sparkdb/scaletozero/internals/manager"
	"sparkdb/scaletozero/internals/store"
)

// handshakeTimeout bounds how long a client may take to send its handshake
// before we give up on it.
const handshakeTimeout = 15 * time.Second

// dialTimeout bounds connecting to an awake backend.
const dialTimeout = 10 * time.Second

// Proxy fronts one engine: it accepts client connections, resolves which
// database they want, wakes it if it is scaled to zero, then splices the
// connection through to the backend.
type Proxy struct {
	Engine  engine.Engine
	Manager *manager.Manager
	Logger  *log.Logger
}

// New builds a Proxy for an engine.
func New(eng engine.Engine, mgr *manager.Manager, logger *log.Logger) *Proxy {
	return &Proxy{Engine: eng, Manager: mgr, Logger: logger}
}

// ListenAndServe accepts connections on addr until ctx is cancelled.
func (p *Proxy) ListenAndServe(ctx context.Context, addr string) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	p.logf("%s proxy listening on %s", p.Engine.Name(), addr)
	return p.Serve(ctx, ln)
}

// Serve accepts connections from ln until ctx is cancelled.
func (p *Proxy) Serve(ctx context.Context, ln net.Listener) error {
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A single bad accept should not kill the listener.
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.handle(ctx, conn)
		}()
	}
}

// handle drives one client connection end to end.
func (p *Proxy) handle(ctx context.Context, client net.Conn) {
	defer client.Close()

	// Read the handshake under a deadline so a silent client cannot pin a
	// goroutine forever.
	if err := client.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}

	name, replay, err := p.Engine.Resolve(client)
	if err != nil {
		if !errors.Is(err, engine.ErrCancelRequest) {
			p.logf("resolve from %s: %v", client.RemoteAddr(), err)
		}
		return
	}

	// Clear the handshake deadline: from here the connection is long-lived.
	if err := client.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	// Acquire reserves a connection slot before waking, so the reaper cannot
	// scale this database down underneath us.
	addr, release, err := p.Manager.Acquire(ctx, name)
	if err != nil {
		p.reportError(client, name, err)
		return
	}
	defer release()

	backend, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		p.logf("dial backend %s for %s: %v", addr, name, err)
		p.writeError(client, "08006", "could not connect to database backend")
		return
	}
	defer backend.Close()

	// Replay the handshake bytes we consumed to identify the database.
	if _, err := backend.Write(replay); err != nil {
		p.logf("replay handshake to %s: %v", addr, err)
		return
	}

	splice(client, backend)
}

// reportError turns a lookup or wake failure into a protocol-level error the
// client can actually display.
func (p *Proxy) reportError(client net.Conn, name string, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		p.writeError(client, "3D000", "database \""+name+"\" does not exist")
	case errors.Is(err, context.DeadlineExceeded):
		p.logf("wake %s timed out: %v", name, err)
		p.writeError(client, "57P03", "database \""+name+"\" is starting up, retry shortly")
	default:
		p.logf("acquire %s: %v", name, err)
		p.writeError(client, "08006", "could not start database \""+name+"\"")
	}
}

// writeError sends an engine-native error packet when the engine supports it.
func (p *Proxy) writeError(client net.Conn, code, message string) {
	reporter, ok := p.Engine.(engine.ErrorReporter)
	if !ok {
		return
	}
	_ = client.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if err := reporter.WriteError(client, code, message); err != nil {
		p.logf("write error packet: %v", err)
	}
}

// splice copies bytes in both directions until either side closes.
//
// The connections are passed to io.Copy unwrapped on purpose: for TCP pairs
// this lets the runtime use splice(2) on Linux, keeping payload bytes out of
// user space. Idle tracking does not need per-byte accounting because an open
// connection already blocks the reaper via the manager's active-connection
// count.
func splice(client, backend net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		io.Copy(backend, client)
		closeWrite(backend)
	}()
	go func() {
		defer wg.Done()
		io.Copy(client, backend)
		closeWrite(client)
	}()

	wg.Wait()
}

// closeWrite propagates EOF to the peer without tearing down the whole socket,
// so the other direction can drain.
func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}

func (p *Proxy) logf(format string, args ...any) {
	if p.Logger != nil {
		p.Logger.Printf(format, args...)
	}
}
