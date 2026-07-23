package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/docker"
	"sparkdb/scaletozero/internals/engine"
	"sparkdb/scaletozero/internals/manager"
	"sparkdb/scaletozero/internals/store"
)

// lineEngine is a stand-in protocol: the client's handshake is one line
// holding the database name. Keeps the proxy test independent of Postgres
// framing, which is covered by the engine package's own tests.
type lineEngine struct {
	errorsWritten atomic.Int64
}

func (l *lineEngine) Name() string                { return "postgres" }
func (l *lineEngine) Image() string               { return "postgres:latest" }
func (l *lineEngine) InternalPort() int           { return 5432 }
func (l *lineEngine) DataDir() string             { return "/data" }
func (l *lineEngine) Routing() engine.RoutingMode { return engine.SharedPortStartupRouting }

func (l *lineEngine) Env(user, password, database string) []string { return nil }

func (l *lineEngine) Resolve(rw io.ReadWriter) (string, []byte, error) {
	// Read exactly the handshake, one byte at a time. Buffering ahead would
	// swallow payload bytes that belong to the backend — a real protocol
	// parser reads only the framed handshake for the same reason.
	var line []byte
	buf := make([]byte, 1)
	for len(line) < 256 {
		if _, err := io.ReadFull(rw, buf); err != nil {
			return "", nil, err
		}
		line = append(line, buf[0])
		if buf[0] == '\n' {
			break
		}
	}
	name := strings.TrimSpace(string(line))
	if name == "" {
		return "", nil, engine.ErrNoDatabase
	}
	if name == "cancel" {
		return "", nil, engine.ErrCancelRequest
	}
	return name, line, nil
}

func (l *lineEngine) Ready(ctx context.Context, addr string) error {
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (l *lineEngine) WriteError(w io.Writer, code, message string) error {
	l.errorsWritten.Add(1)
	_, err := w.Write([]byte("ERR " + code + " " + message + "\n"))
	return err
}

// fakeBackend is a stand-in database container: it records the handshake it
// receives and then echoes everything back uppercased.
type fakeBackend struct {
	ln        net.Listener
	mu        sync.Mutex
	handshake []byte
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	b := &fakeBackend{ln: ln}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go b.serve(conn)
		}
	}()
	return b
}

func (b *fakeBackend) serve(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	line, err := reader.ReadBytes('\n')
	if err != nil {
		return
	}
	b.mu.Lock()
	b.handshake = append([]byte(nil), line...)
	b.mu.Unlock()

	// Echo the rest back uppercased so the test can prove both directions.
	for {
		payload, err := reader.ReadBytes('\n')
		if len(payload) > 0 {
			conn.Write([]byte(strings.ToUpper(string(payload))))
		}
		if err != nil {
			return
		}
	}
}

func (b *fakeBackend) gotHandshake() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.handshake)
}

func (b *fakeBackend) addr() string { return b.ln.Addr().String() }

// dockerCounts tracks the Docker calls the manager issues during a test.
type dockerCounts struct {
	mu     sync.Mutex
	counts map[string]int
}

func (d *dockerCounts) inc(k string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counts[k]++
}

func (d *dockerCounts) get(k string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.counts[k]
}

// newTestStack wires a manager plus proxy over a fake Docker API, pointing all
// backends at the given address. idleTimeout controls how quickly the reaper
// considers a database cold.
func newTestStack(t *testing.T, backendAddr string, idleTimeout time.Duration) (*Proxy, *manager.Manager, *lineEngine, *dockerCounts) {
	t.Helper()

	dc := &dockerCounts{counts: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case strings.HasSuffix(path, "/volumes/create"):
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Name":"vol"}`))
		case strings.HasSuffix(path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"Id": "container-abc"})
		case strings.HasSuffix(path, "/start"):
			dc.inc("start")
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/stop"):
			dc.inc("stop")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "proxy.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	eng := &lineEngine{}
	cfg := &config.Config{
		DOCKER_NETWORK: "sparkdb-network",
		IDLE_TIMEOUT:   idleTimeout,
		REAP_INTERVAL:  time.Second,
		WAKE_TIMEOUT:   3 * time.Second,
	}

	dockerClient := &docker.Client{BaseURL: srv.URL, Username: "u", Password: "p", HTTP: srv.Client()}
	logger := log.New(io.Discard, "", 0)

	mgr, err := manager.New(cfg, dockerClient, engine.NewRegistry(eng), st, logger)
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	mgr.SetBackendAddrFunc(func(store.Database) (string, error) { return backendAddr, nil })

	return New(eng, mgr, logger), mgr, eng, dc
}

// startProxy runs the proxy on an ephemeral port and returns its address.
func startProxy(t *testing.T, p *Proxy) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	go p.Serve(ctx, ln)
	return ln.Addr().String()
}

func TestProxyForwardsHandshakeAndCopiesBothDirections(t *testing.T) {
	backend := newFakeBackend(t)
	p, mgr, _, _ := newTestStack(t, backend.addr(), time.Minute)

	if _, err := mgr.Create(context.Background(), "postgres", "mydb", "haki", "secret"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	addr := startProxy(t, p)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	if _, err := conn.Write([]byte("mydb\n")); err != nil {
		t.Fatalf("write handshake: %v", err)
	}
	if _, err := conn.Write([]byte("hello\n")); err != nil {
		t.Fatalf("write payload: %v", err)
	}

	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	if strings.TrimSpace(reply) != "HELLO" {
		t.Errorf("reply = %q, want %q", strings.TrimSpace(reply), "HELLO")
	}

	// The handshake bytes consumed for routing must reach the backend intact,
	// otherwise the database sees a truncated protocol stream.
	if got := backend.gotHandshake(); got != "mydb\n" {
		t.Errorf("backend handshake = %q, want %q", got, "mydb\n")
	}
}

func TestProxyWakesStoppedDatabase(t *testing.T) {
	backend := newFakeBackend(t)
	p, mgr, _, dc := newTestStack(t, backend.addr(), time.Minute)

	if _, err := mgr.Create(context.Background(), "postgres", "mydb", "haki", "secret"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	startsAfterCreate := dc.get("start")

	addr := startProxy(t, p)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("mydb\nping\n"))

	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatalf("read reply: %v", err)
	}

	if got := dc.get("start") - startsAfterCreate; got != 1 {
		t.Errorf("start calls = %d, want 1 (connection woke the database)", got)
	}
	rec, _ := mgr.Get("mydb")
	if rec.Status != store.StatusRunning {
		t.Errorf("Status = %q, want running", rec.Status)
	}
}

func TestProxyRejectsUnknownDatabaseWithProtocolError(t *testing.T) {
	backend := newFakeBackend(t)
	p, _, eng, _ := newTestStack(t, backend.addr(), time.Minute)
	addr := startProxy(t, p)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("ghost\n"))

	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read error reply: %v", err)
	}
	if !strings.Contains(reply, "3D000") {
		t.Errorf("reply = %q, want the invalid-catalog code 3D000", reply)
	}
	if !strings.Contains(reply, "ghost") {
		t.Errorf("reply = %q, want the database name echoed back", reply)
	}
	if eng.errorsWritten.Load() != 1 {
		t.Errorf("errorsWritten = %d, want 1", eng.errorsWritten.Load())
	}
}

func TestProxyClosesConnectionOnUnroutableHandshake(t *testing.T) {
	backend := newFakeBackend(t)
	p, _, _, _ := newTestStack(t, backend.addr(), time.Minute)
	addr := startProxy(t, p)

	// A cancel request carries no database and cannot be routed.
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("cancel\n"))

	if _, err := io.ReadAll(conn); err != nil {
		t.Fatalf("read: %v", err)
	}
	// Reaching EOF without a hang is the expected outcome.
}

func TestProxyReportsUnreachableBackend(t *testing.T) {
	// Provision against a live backend, then make the address dead so the
	// database still reads as running but cannot actually be dialled.
	backend := newFakeBackend(t)
	p, mgr, eng, _ := newTestStack(t, backend.addr(), time.Minute)

	if _, err := mgr.Create(context.Background(), "postgres", "mydb", "haki", "secret"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), "mydb"); err != nil {
		t.Fatalf("Start: %v", err)
	}

	dead, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	deadAddr := dead.Addr().String()
	dead.Close()
	mgr.SetBackendAddrFunc(func(store.Database) (string, error) { return deadAddr, nil })

	addr := startProxy(t, p)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("mydb\n"))

	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatalf("read error reply: %v", err)
	}
	if !strings.Contains(reply, "08006") {
		t.Errorf("reply = %q, want the connection-failure code 08006", reply)
	}
	if eng.errorsWritten.Load() != 1 {
		t.Errorf("errorsWritten = %d, want 1", eng.errorsWritten.Load())
	}
}

func TestProxyHandlesConcurrentConnections(t *testing.T) {
	backend := newFakeBackend(t)
	p, mgr, _, dc := newTestStack(t, backend.addr(), time.Minute)

	if _, err := mgr.Create(context.Background(), "postgres", "mydb", "haki", "secret"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	startsAfterCreate := dc.get("start")
	addr := startProxy(t, p)

	const clients = 15
	var wg sync.WaitGroup
	errs := make(chan error, clients)

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(10 * time.Second))

			if _, err := conn.Write([]byte("mydb\nping\n")); err != nil {
				errs <- err
				return
			}
			reply, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil {
				errs <- err
				return
			}
			if strings.TrimSpace(reply) != "PING" {
				errs <- errors.New("unexpected reply: " + reply)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("client: %v", err)
	}
	// All connections target one cold database: exactly one wake.
	if got := dc.get("start") - startsAfterCreate; got != 1 {
		t.Errorf("start calls = %d, want 1 across %d concurrent clients", got, clients)
	}
}

func TestServeReturnsOnContextCancel(t *testing.T) {
	backend := newFakeBackend(t)
	p, _, _, _ := newTestStack(t, backend.addr(), time.Minute)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- p.Serve(ctx, ln) }()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v, want nil on cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Serve did not return after context cancellation")
	}
}

func TestProxyReleasesConnectionSlotOnClose(t *testing.T) {
	// Once the client disconnects, the reaper must be free to scale down. A
	// zero idle timeout means any released database is immediately cold.
	backend := newFakeBackend(t)
	p, mgr, _, dc := newTestStack(t, backend.addr(), 0)

	if _, err := mgr.Create(context.Background(), "postgres", "mydb", "haki", "secret"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	addr := startProxy(t, p)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	conn.Write([]byte("mydb\nping\n"))
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatalf("read reply: %v", err)
	}
	conn.Close()

	// Poll: the slot is released as the proxy goroutine unwinds.
	stopsBefore := dc.get("stop")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mgr.Reap(context.Background())
		if dc.get("stop") > stopsBefore {
			rec, _ := mgr.Get("mydb")
			if rec.Status != store.StatusStopped {
				t.Errorf("Status = %q, want stopped", rec.Status)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Error("database was never scaled down after the client disconnected")
}
