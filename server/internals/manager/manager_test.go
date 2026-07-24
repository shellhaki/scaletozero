package manager

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
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
	"sparkdb/scaletozero/internals/store"
)

// fakeEngine is a minimal Engine whose readiness is controlled by the test.
type fakeEngine struct {
	readyErr atomic.Pointer[error]
	readyN   atomic.Int64
}

func (f *fakeEngine) Name() string                { return "postgres" }
func (f *fakeEngine) Image() string               { return "postgres:latest" }
func (f *fakeEngine) InternalPort() int           { return 5432 }
func (f *fakeEngine) DataDir() string             { return "/var/lib/postgresql/data" }
func (f *fakeEngine) Routing() engine.RoutingMode { return engine.SharedPortStartupRouting }

func (f *fakeEngine) Env(user, password, database string) []string {
	return []string{"POSTGRES_USER=" + user, "POSTGRES_PASSWORD=" + password, "POSTGRES_DB=" + database}
}

func (f *fakeEngine) Resolve(rw io.ReadWriter) (string, []byte, error) {
	return "", nil, errors.New("not used")
}

func (f *fakeEngine) Ready(ctx context.Context, addr string) error {
	f.readyN.Add(1)
	if p := f.readyErr.Load(); p != nil {
		return *p
	}
	return nil
}

func (f *fakeEngine) setReadyErr(err error) {
	if err == nil {
		f.readyErr.Store(nil)
		return
	}
	f.readyErr.Store(&err)
}

// fakeDocker counts the Docker API calls the manager makes.
type fakeDocker struct {
	mu     sync.Mutex
	counts map[string]int

	// startDelay simulates a slow container boot so wake coalescing is
	// observable.
	startDelay time.Duration
}

func (f *fakeDocker) count(action string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.counts[action]++
}

func (f *fakeDocker) get(action string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.counts[action]
}

func newFakeDocker(t *testing.T) (*docker.Client, *fakeDocker) {
	t.Helper()
	fd := &fakeDocker{counts: map[string]int{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.Contains(path, "/networks/"):
			// The shared network already exists in tests.
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"Name":"sparkdb-network"}`))
		case strings.HasSuffix(path, "/volumes/create"):
			fd.count("volume_create")
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Name":"vol"}`))
		case strings.HasSuffix(path, "/containers/create"):
			fd.count("container_create")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"Id": "container-abc"})
		case strings.HasSuffix(path, "/start"):
			fd.count("start")
			if fd.startDelay > 0 {
				time.Sleep(fd.startDelay)
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/stop"):
			fd.count("stop")
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/pause"):
			fd.count("pause")
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(path, "/unpause"):
			fd.count("unpause")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.Contains(path, "/containers/"):
			fd.count("container_remove")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.Contains(path, "/volumes/"):
			fd.count("volume_remove")
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	return &docker.Client{BaseURL: srv.URL, Username: "u", Password: "p", HTTP: srv.Client()}, fd
}

// newTestManager wires a manager over a fake Docker API and a temp SQLite file.
func newTestManager(t *testing.T) (*Manager, *fakeDocker, *fakeEngine) {
	t.Helper()

	dc, fd := newFakeDocker(t)
	eng := &fakeEngine{}

	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		DOCKER_NETWORK: "sparkdb-network",
		IDLE_TIMEOUT:   2 * time.Minute,
		REAP_INTERVAL:  10 * time.Millisecond,
		WAKE_TIMEOUT:   2 * time.Second,
	}

	mgr, err := New(cfg, dc, engine.NewRegistry(eng), st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	// Container DNS names do not resolve in tests.
	mgr.addrFunc = func(rec store.Database) (string, error) {
		return "127.0.0.1:1", nil
	}
	return mgr, fd, eng
}

func mustCreate(t *testing.T, mgr *Manager, name string) *store.Database {
	t.Helper()
	db, err := mgr.Create(context.Background(), "postgres", name, "haki", "secret")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return db
}

func TestCreateProvisionsThenScalesToZero(t *testing.T) {
	mgr, fd, _ := newTestManager(t)

	db := mustCreate(t, mgr, "mydb")

	if fd.get("volume_create") != 1 {
		t.Errorf("volume_create = %d, want 1", fd.get("volume_create"))
	}
	if fd.get("container_create") != 1 {
		t.Errorf("container_create = %d, want 1", fd.get("container_create"))
	}
	// Eager init: boot once so the data dir is initialised, then scale down.
	if fd.get("start") != 1 {
		t.Errorf("start = %d, want 1 (eager init boot)", fd.get("start"))
	}
	if fd.get("stop") != 1 {
		t.Errorf("stop = %d, want 1 (scaled to zero after init)", fd.get("stop"))
	}
	if db.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", db.Status, store.StatusStopped)
	}
	if db.ContainerID != "container-abc" {
		t.Errorf("ContainerID = %q", db.ContainerID)
	}
	if db.ContainerName != "sparkdb-postgres-mydb" || db.VolumeName != "sparkdb-postgres-mydb-data" {
		t.Errorf("naming = %s / %s", db.ContainerName, db.VolumeName)
	}
}

func TestCreateRejectsInvalidNames(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	for _, name := range []string{"", "1leading-digit", "has space", "has/slash", "has;semi", strings.Repeat("a", 64)} {
		_, err := mgr.Create(context.Background(), "postgres", name, "u", "p")
		if !errors.Is(err, ErrInvalidName) {
			t.Errorf("Create(%q) err = %v, want ErrInvalidName", name, err)
		}
	}
}

func TestCreateRejectsUnknownEngine(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, err := mgr.Create(context.Background(), "cassandra", "mydb", "u", "p")
	if !errors.Is(err, ErrUnknownEngine) {
		t.Errorf("err = %v, want ErrUnknownEngine", err)
	}
}

func TestCreateRejectsDuplicateName(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	_, err := mgr.Create(context.Background(), "postgres", "mydb", "u", "p")
	if !errors.Is(err, store.ErrExists) {
		t.Errorf("err = %v, want store.ErrExists", err)
	}
}

func TestCreateMarksErrorWhenInitNeverBecomesReady(t *testing.T) {
	mgr, _, eng := newTestManager(t)
	eng.setReadyErr(errors.New("connection refused"))
	mgr.cfg.WAKE_TIMEOUT = 100 * time.Millisecond

	// Shrink the eager-init wait by cancelling the context instead of waiting
	// out initTimeout.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	_, err := mgr.Create(ctx, "postgres", "mydb", "u", "p")
	if err == nil {
		t.Fatal("expected create to fail when the engine never becomes ready")
	}

	rec, gerr := mgr.Get("mydb")
	if gerr != nil {
		t.Fatalf("Get: %v", gerr)
	}
	if rec.Status != store.StatusError {
		t.Errorf("Status = %q, want %q after a failed init", rec.Status, store.StatusError)
	}
}

func TestAcquireWakesStoppedDatabase(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")
	startsAfterCreate := fd.get("start")

	addr, release, err := mgr.Acquire(context.Background(), "mydb")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	if addr == "" {
		t.Error("Acquire must return a backend address")
	}
	if got := fd.get("start") - startsAfterCreate; got != 1 {
		t.Errorf("start calls during wake = %d, want 1", got)
	}
	rec, _ := mgr.Get("mydb")
	if rec.Status != store.StatusRunning {
		t.Errorf("Status = %q, want %q", rec.Status, store.StatusRunning)
	}
}

func TestAcquireOnRunningDatabaseSkipsDockerStart(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	_, release1, err := mgr.Acquire(context.Background(), "mydb")
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer release1()
	startsAfterWake := fd.get("start")

	_, release2, err := mgr.Acquire(context.Background(), "mydb")
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer release2()

	if fd.get("start") != startsAfterWake {
		t.Errorf("an already-running database must not be started again")
	}
}

func TestConcurrentAcquireStartsContainerOnce(t *testing.T) {
	// This is the singleflight guarantee: a cold database hit by a burst of
	// connections must issue exactly one Docker start.
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")
	startsAfterCreate := fd.get("start")

	fd.mu.Lock()
	fd.startDelay = 50 * time.Millisecond
	fd.mu.Unlock()

	const clients = 25
	var wg sync.WaitGroup
	errs := make(chan error, clients)

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := mgr.Acquire(context.Background(), "mydb")
			if err != nil {
				errs <- err
				return
			}
			release()
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("Acquire: %v", err)
	}
	if got := fd.get("start") - startsAfterCreate; got != 1 {
		t.Errorf("start calls = %d, want exactly 1 for %d concurrent waking connections", got, clients)
	}
}

func TestAcquireUnknownDatabase(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	_, _, err := mgr.Acquire(context.Background(), "ghost")
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

func TestAcquireReleasesSlotWhenWakeFails(t *testing.T) {
	mgr, _, eng := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	eng.setReadyErr(errors.New("refused"))
	mgr.cfg.WAKE_TIMEOUT = 50 * time.Millisecond

	if _, _, err := mgr.Acquire(context.Background(), "mydb"); err == nil {
		t.Fatal("expected Acquire to fail when the backend never becomes ready")
	}

	st, _ := mgr.state("mydb")
	if n := st.activeConns.Load(); n != 0 {
		t.Errorf("activeConns = %d, want 0 (a failed acquire must not leak a slot)", n)
	}
}

func TestReaperScalesDownIdleDatabase(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	_, release, err := mgr.Acquire(context.Background(), "mydb")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	stopsBefore := fd.get("stop")

	// Pretend the last activity was well past the idle timeout.
	st, _ := mgr.state("mydb")
	st.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	mgr.Reap(context.Background())

	if got := fd.get("stop") - stopsBefore; got != 1 {
		t.Errorf("stop calls = %d, want 1", got)
	}
	rec, _ := mgr.Get("mydb")
	if rec.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", rec.Status, store.StatusStopped)
	}
}

func TestReaperLeavesActiveConnectionAlone(t *testing.T) {
	// An open client connection must never be cut, no matter how long the
	// connection has been idle.
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	_, release, err := mgr.Acquire(context.Background(), "mydb")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()
	stopsBefore := fd.get("stop")

	st, _ := mgr.state("mydb")
	st.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	mgr.Reap(context.Background())

	if got := fd.get("stop") - stopsBefore; got != 0 {
		t.Errorf("stop calls = %d, want 0 while a connection is live", got)
	}
	rec, _ := mgr.Get("mydb")
	if rec.Status != store.StatusRunning {
		t.Errorf("Status = %q, want it to stay running", rec.Status)
	}
}

func TestReaperIgnoresRecentlyActiveDatabase(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	_, release, err := mgr.Acquire(context.Background(), "mydb")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	stopsBefore := fd.get("stop")

	mgr.Reap(context.Background())

	if got := fd.get("stop") - stopsBefore; got != 0 {
		t.Errorf("stop calls = %d, want 0 before the idle timeout elapses", got)
	}
}

func TestReaperIgnoresAlreadyStoppedDatabase(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")
	stopsBefore := fd.get("stop")

	st, _ := mgr.state("mydb")
	st.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	mgr.Reap(context.Background())

	if got := fd.get("stop") - stopsBefore; got != 0 {
		t.Errorf("stop calls = %d, want 0 for an already-stopped database", got)
	}
}

func TestRunReaperStopsOnContextCancel(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		mgr.RunReaper(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("RunReaper did not return after context cancellation")
	}
}

func TestTouchDefersReaping(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	_, release, _ := mgr.Acquire(context.Background(), "mydb")
	release()

	st, _ := mgr.state("mydb")
	st.lastActive.Store(time.Now().Add(-5 * time.Minute).UnixNano())

	mgr.Touch("mydb")
	stopsBefore := fd.get("stop")
	mgr.Reap(context.Background())

	if got := fd.get("stop") - stopsBefore; got != 0 {
		t.Errorf("stop calls = %d, want 0 after Touch reset the idle clock", got)
	}
}

func TestWakeUnpausesPausedDatabase(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	if err := mgr.Pause(context.Background(), "mydb"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	startsBefore := fd.get("start")

	if _, release, err := mgr.Acquire(context.Background(), "mydb"); err != nil {
		t.Fatalf("Acquire: %v", err)
	} else {
		defer release()
	}

	if fd.get("unpause") != 1 {
		t.Errorf("unpause = %d, want 1 (a paused container is resumed, not started)", fd.get("unpause"))
	}
	if fd.get("start") != startsBefore {
		t.Errorf("start must not be called to resume a paused container")
	}
}

func TestStopStatusTransition(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")
	stopsBefore := fd.get("stop")

	if err := mgr.Stop(context.Background(), "mydb"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := fd.get("stop") - stopsBefore; got != 1 {
		t.Errorf("stop calls = %d, want 1", got)
	}
	rec, _ := mgr.Get("mydb")
	if rec.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", rec.Status, store.StatusStopped)
	}
}

func TestPauseStatusTransition(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	if err := mgr.Pause(context.Background(), "mydb"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if fd.get("pause") != 1 {
		t.Errorf("pause = %d, want 1", fd.get("pause"))
	}
	rec, _ := mgr.Get("mydb")
	if rec.Status != store.StatusPaused {
		t.Errorf("Status = %q, want %q", rec.Status, store.StatusPaused)
	}
}

func TestDeleteRemovesContainerVolumeAndRow(t *testing.T) {
	mgr, fd, _ := newTestManager(t)
	mustCreate(t, mgr, "mydb")

	if err := mgr.Delete(context.Background(), "mydb"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if fd.get("container_remove") != 1 {
		t.Errorf("container_remove = %d, want 1", fd.get("container_remove"))
	}
	if fd.get("volume_remove") != 1 {
		t.Errorf("volume_remove = %d, want 1 (delete destroys the data)", fd.get("volume_remove"))
	}
	if _, err := mgr.Get("mydb"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get err = %v, want ErrNotFound", err)
	}
}

func TestLifecycleOnUnknownDatabase(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	ctx := context.Background()

	ops := map[string]error{
		"Start":  mgr.Start(ctx, "ghost"),
		"Stop":   mgr.Stop(ctx, "ghost"),
		"Pause":  mgr.Pause(ctx, "ghost"),
		"Delete": mgr.Delete(ctx, "ghost"),
		"Sleep":  mgr.Sleep(ctx, "ghost"),
	}
	for name, err := range ops {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s err = %v, want store.ErrNotFound", name, err)
		}
	}
}

func TestListReportsEveryDatabase(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	mustCreate(t, mgr, "one")
	mustCreate(t, mgr, "two")

	list := mgr.List()
	if len(list) != 2 {
		t.Fatalf("len(list) = %d, want 2", len(list))
	}
	names := map[string]bool{}
	for _, d := range list {
		names[d.Name] = true
	}
	if !names["one"] || !names["two"] {
		t.Errorf("List = %v, want both databases", names)
	}
}

func TestHydrateRestoresStateAfterRestart(t *testing.T) {
	// A proxy restart must not lose track of provisioned databases.
	dc, _ := newFakeDocker(t)
	eng := &fakeEngine{}
	path := filepath.Join(t.TempDir(), "persist.db")

	cfg := &config.Config{
		DOCKER_NETWORK: "sparkdb-network",
		IDLE_TIMEOUT:   time.Minute,
		REAP_INTERVAL:  time.Second,
		WAKE_TIMEOUT:   time.Second,
	}
	logger := log.New(io.Discard, "", 0)

	st1, err := store.Open(path)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	mgr1, err := New(cfg, dc, engine.NewRegistry(eng), st1, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mgr1.addrFunc = func(store.Database) (string, error) { return "127.0.0.1:1", nil }
	mustCreate(t, mgr1, "mydb")
	st1.Close()

	st2, err := store.Open(path)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	defer st2.Close()
	mgr2, err := New(cfg, dc, engine.NewRegistry(eng), st2, logger)
	if err != nil {
		t.Fatalf("New after restart: %v", err)
	}

	rec, err := mgr2.Get("mydb")
	if err != nil {
		t.Fatalf("Get after restart: %v", err)
	}
	if rec.Status != store.StatusStopped {
		t.Errorf("Status = %q, want %q", rec.Status, store.StatusStopped)
	}
	if rec.ContainerID != "container-abc" {
		t.Errorf("ContainerID = %q, want it restored from the store", rec.ContainerID)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	eng := &fakeEngine{}
	eng.setReadyErr(errors.New("refused"))

	start := time.Now()
	err := waitReady(context.Background(), eng, "127.0.0.1:1", 150*time.Millisecond)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, want a timeout message", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("waitReady overran its budget: %s", elapsed)
	}
	if eng.readyN.Load() < 2 {
		t.Errorf("readiness was probed %d times, want repeated polling", eng.readyN.Load())
	}
}

func TestWaitReadySucceedsOnceEngineAnswers(t *testing.T) {
	eng := &fakeEngine{}
	eng.setReadyErr(errors.New("still booting"))

	// Flip to ready shortly after polling begins.
	go func() {
		time.Sleep(60 * time.Millisecond)
		eng.setReadyErr(nil)
	}()

	if err := waitReady(context.Background(), eng, "127.0.0.1:1", 3*time.Second); err != nil {
		t.Errorf("waitReady: %v", err)
	}
}

func TestWaitReadyHonoursContextCancellation(t *testing.T) {
	eng := &fakeEngine{}
	eng.setReadyErr(errors.New("refused"))

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := waitReady(ctx, eng, "127.0.0.1:1", 30*time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestBackendAddrUsesContainerNameAndEnginePort(t *testing.T) {
	// The default addressing must target the container over the Docker
	// network, never a published host port.
	dc, _ := newFakeDocker(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "addr.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer st.Close()

	mgr, err := New(&config.Config{DOCKER_NETWORK: "sparkdb-network"}, dc,
		engine.NewRegistry(&fakeEngine{}), st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	addr, err := mgr.backendAddr(store.Database{Engine: "postgres", ContainerName: "sparkdb-postgres-mydb"})
	if err != nil {
		t.Fatalf("backendAddr: %v", err)
	}
	if want := fmt.Sprintf("sparkdb-postgres-mydb:%d", 5432); addr != want {
		t.Errorf("addr = %q, want %q", addr, want)
	}
}
