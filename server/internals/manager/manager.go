package manager

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/docker"
	"sparkdb/scaletozero/internals/engine"
	"sparkdb/scaletozero/internals/store"
)

// initTimeout bounds the eager first boot, which runs initdb and is much
// slower than an ordinary wake.
const initTimeout = 3 * time.Minute

// stopGrace is how many seconds Docker gives a container to shut down cleanly.
const stopGrace = 10

// validName guards names that end up in container names, volume names and
// URLs.
var validName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,62}$`)

// ErrInvalidName is returned for names that fail validName.
var ErrInvalidName = errors.New("manager: invalid database name")

// ErrUnknownEngine is returned when no engine is registered under a name.
var ErrUnknownEngine = errors.New("manager: unknown engine")

// dbState is the in-memory hot state for one database. The proxy consults it
// on every connection, so it never touches SQLite on the hot path.
type dbState struct {
	mu     sync.Mutex
	record store.Database

	// activeConns is the number of client connections currently proxied. The
	// reaper refuses to scale down while this is above zero.
	activeConns atomic.Int64

	// lastActive is a Unix nano timestamp of the last connection activity.
	lastActive atomic.Int64

	// waking is non-nil while a wake is in flight, so concurrent connections
	// coalesce onto one Docker start instead of racing.
	waking *wakeCall
}

// wakeCall lets queued connections block on an in-flight wake.
type wakeCall struct {
	done chan struct{}
	err  error
}

// Manager owns the database lifecycle: provisioning, waking, scaling to zero.
type Manager struct {
	cfg     *config.Config
	docker  *docker.Client
	engines engine.Registry
	store   *store.Store
	mu      sync.RWMutex
	states  map[string]*dbState
	logger  *log.Logger

	// nowFunc and addrFunc are seams for tests; both are set by New.
	nowFunc  func() time.Time
	addrFunc func(store.Database) (string, error)

	// networkMu guards networkReady, which records that the shared Docker
	// network has been confirmed to exist. A failed check is not cached, so a
	// transient Docker error is retried on the next Create.
	networkMu    sync.Mutex
	networkReady bool
}

// New builds a Manager and hydrates in-memory state from the store.
func New(cfg *config.Config, dc *docker.Client, engines engine.Registry, st *store.Store, logger *log.Logger) (*Manager, error) {
	m := &Manager{
		cfg:     cfg,
		docker:  dc,
		engines: engines,
		store:   st,
		states:  make(map[string]*dbState),
		nowFunc: time.Now,
		logger:  logger,
	}
	// Containers are reached by name on the shared Docker network.
	m.addrFunc = func(rec store.Database) (string, error) {
		eng, ok := m.engines.Get(rec.Engine)
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrUnknownEngine, rec.Engine)
		}
		return fmt.Sprintf("%s:%d", rec.ContainerName, eng.InternalPort()), nil
	}
	if err := m.hydrate(context.Background()); err != nil {
		return nil, err
	}
	return m, nil
}

// hydrate loads every persisted database into memory at boot.
func (m *Manager) hydrate(ctx context.Context) error {
	records, err := m.store.List(ctx)
	if err != nil {
		return fmt.Errorf("hydrate state: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		st := &dbState{record: r}
		last := r.LastActive
		if last.IsZero() {
			last = r.CreatedAt
		}
		st.lastActive.Store(last.UnixNano())
		m.states[r.Name] = st
	}
	return nil
}

// state returns the in-memory state for name.
func (m *Manager) state(name string) (*dbState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.states[name]
	return st, ok
}

// backendAddr is where the proxy dials this database, over the Docker network.
func (m *Manager) backendAddr(rec store.Database) (string, error) {
	return m.addrFunc(rec)
}

// SetBackendAddrFunc overrides how a database's backend address is derived.
// The default resolves the container by name on the Docker network, which is
// what production uses; overriding it is useful when the proxy runs somewhere
// that cannot resolve Docker DNS.
func (m *Manager) SetBackendAddrFunc(fn func(store.Database) (string, error)) {
	m.addrFunc = fn
}

// ensureNetwork confirms the shared Docker network exists, creating it once.
// The result is cached only on success so a transient failure is retried.
func (m *Manager) ensureNetwork(ctx context.Context) error {
	m.networkMu.Lock()
	defer m.networkMu.Unlock()
	if m.networkReady {
		return nil
	}
	if err := m.docker.EnsureNetwork(ctx, m.cfg.DOCKER_NETWORK); err != nil {
		return err
	}
	m.networkReady = true
	return nil
}

// Create provisions a database: named volume, container bound to it, an eager
// first boot so the data directory is initialised, then scale to zero.
func (m *Manager) Create(ctx context.Context, engineName, name, username, password string) (*store.Database, error) {
	if !validName.MatchString(name) {
		return nil, ErrInvalidName
	}
	eng, ok := m.engines.Get(engineName)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownEngine, engineName)
	}

	if err := m.ensureNetwork(ctx); err != nil {
		return nil, fmt.Errorf("ensure docker network %q: %w", m.cfg.DOCKER_NETWORK, err)
	}

	rec := store.Database{
		Name:          name,
		Engine:        eng.Name(),
		ContainerName: fmt.Sprintf("sparkdb-%s-%s", eng.Name(), name),
		VolumeName:    fmt.Sprintf("sparkdb-%s-%s-data", eng.Name(), name),
		Username:      username,
		Password:      password,
		Status:        store.StatusProvisioning,
		CreatedAt:     m.nowFunc(),
	}

	if err := m.store.Create(ctx, rec); err != nil {
		return nil, err
	}

	st := &dbState{record: rec}
	st.lastActive.Store(rec.CreatedAt.UnixNano())
	m.mu.Lock()
	m.states[name] = st
	m.mu.Unlock()

	labels := map[string]string{
		"sparkdb.managed":  "true",
		"sparkdb.database": name,
		"sparkdb.engine":   eng.Name(),
	}

	if err := m.docker.CreateVolume(ctx, rec.VolumeName, labels); err != nil {
		m.markError(ctx, st, name)
		return nil, fmt.Errorf("create volume: %w", err)
	}

	payload := docker.ContainerPayload{
		Image:  eng.Image(),
		Env:    eng.Env(username, password, name),
		Labels: labels,
		ExposedPorts: map[string]struct{}{
			fmt.Sprintf("%d/tcp", eng.InternalPort()): {},
		},
		HostConfig: docker.HostConfig{
			Binds:         []string{fmt.Sprintf("%s:%s", rec.VolumeName, eng.DataDir())},
			NetworkMode:   m.cfg.DOCKER_NETWORK,
			RestartPolicy: docker.RestartPolicy{Name: "no"},
		},
		NetworkingConfig: &docker.NetworkingConfig{
			EndpointsConfig: map[string]docker.EndpointConfig{
				m.cfg.DOCKER_NETWORK: {Aliases: []string{rec.ContainerName}},
			},
		},
	}

	containerID, err := m.docker.CreateContainer(ctx, rec.ContainerName, payload)
	if err != nil {
		m.markError(ctx, st, name)
		return nil, fmt.Errorf("create container: %w", err)
	}

	rec.ContainerID = containerID
	st.mu.Lock()
	st.record.ContainerID = containerID
	st.mu.Unlock()
	if err := m.store.SetContainer(ctx, name, containerID); err != nil {
		return nil, err
	}

	// Eager init: boot once so the engine initialises its data directory,
	// then scale to zero. Keeps the first real client connection fast.
	if err := m.bootAndWait(ctx, eng, rec, initTimeout); err != nil {
		m.markError(ctx, st, name)
		return nil, fmt.Errorf("initialise database: %w", err)
	}
	if err := m.docker.StopContainer(ctx, containerID, stopGrace); err != nil {
		m.markError(ctx, st, name)
		return nil, fmt.Errorf("stop after init: %w", err)
	}

	m.setStatus(ctx, st, name, store.StatusStopped)
	out := st.snapshot()
	return &out, nil
}

// bootAndWait starts the container and blocks until the engine accepts
// connections or timeout elapses.
func (m *Manager) bootAndWait(ctx context.Context, eng engine.Engine, rec store.Database, timeout time.Duration) error {
	if err := m.docker.StartContainer(ctx, rec.ContainerID); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	addr, err := m.backendAddr(rec)
	if err != nil {
		return err
	}
	return waitReady(ctx, eng, addr, timeout)
}

// waitReady polls the engine readiness probe until it succeeds or times out.
func waitReady(ctx context.Context, eng engine.Engine, addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	backoff := 25 * time.Millisecond
	var lastErr error

	for time.Now().Before(deadline) {
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		err := eng.Ready(probeCtx, addr)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		// Ramp the poll interval so a slow boot does not spin the CPU.
		if backoff < 250*time.Millisecond {
			backoff *= 2
		}
	}
	return fmt.Errorf("timed out waiting for %s after %s: %w", addr, timeout, lastErr)
}

// Acquire reserves a connection slot, waking the database if needed, and
// returns the backend address plus a release function.
//
// The slot is reserved BEFORE the wake so the reaper can never scale the
// database down between waking it and the caller dialling it.
func (m *Manager) Acquire(ctx context.Context, name string) (string, func(), error) {
	st, ok := m.state(name)
	if !ok {
		return "", nil, store.ErrNotFound
	}

	st.activeConns.Add(1)
	st.lastActive.Store(m.nowFunc().UnixNano())

	release := func() {
		st.lastActive.Store(m.nowFunc().UnixNano())
		st.activeConns.Add(-1)
	}

	addr, err := m.ensureRunning(ctx, st, name)
	if err != nil {
		release()
		return "", nil, err
	}
	return addr, release, nil
}

// ensureRunning brings the database up if it is not already serving,
// coalescing concurrent wakes onto a single Docker start.
func (m *Manager) ensureRunning(ctx context.Context, st *dbState, name string) (string, error) {
	st.mu.Lock()

	if st.record.Status == store.StatusRunning {
		rec := st.record
		st.mu.Unlock()
		return m.backendAddr(rec)
	}

	// A wake is already in flight: wait for it instead of starting another.
	if call := st.waking; call != nil {
		rec := st.record
		st.mu.Unlock()
		select {
		case <-call.done:
			if call.err != nil {
				return "", call.err
			}
			return m.backendAddr(rec)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	call := &wakeCall{done: make(chan struct{})}
	st.waking = call
	rec := st.record
	st.mu.Unlock()

	addr, err := m.wake(ctx, rec)

	st.mu.Lock()
	st.waking = nil
	if err == nil {
		st.record.Status = store.StatusRunning
	}
	st.mu.Unlock()

	call.err = err
	close(call.done)

	if err != nil {
		return "", err
	}
	// Persist the new status off the hot path; a failure here only costs
	// accuracy after a restart, so it is logged rather than fatal.
	if perr := m.store.SetStatus(context.WithoutCancel(ctx), rec.Name, store.StatusRunning); perr != nil {
		m.logf("persist running status for %s: %v", rec.Name, perr)
	}
	return addr, nil
}

// wake starts (or unpauses) the container and waits until it serves.
func (m *Manager) wake(ctx context.Context, rec store.Database) (string, error) {
	eng, ok := m.engines.Get(rec.Engine)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrUnknownEngine, rec.Engine)
	}
	addr, err := m.backendAddr(rec)
	if err != nil {
		return "", err
	}

	if rec.Status == store.StatusPaused {
		if err := m.docker.UnpauseContainer(ctx, rec.ContainerID); err != nil {
			return "", fmt.Errorf("unpause container: %w", err)
		}
	} else if err := m.docker.StartContainer(ctx, rec.ContainerID); err != nil {
		return "", fmt.Errorf("start container: %w", err)
	}

	if err := waitReady(ctx, eng, addr, m.cfg.WAKE_TIMEOUT); err != nil {
		return "", err
	}
	return addr, nil
}

// Touch records activity, keeping an in-use database out of the reaper's way.
func (m *Manager) Touch(name string) {
	if st, ok := m.state(name); ok {
		st.lastActive.Store(m.nowFunc().UnixNano())
	}
}

// Sleep scales a database to zero. It is a no-op when connections are live.
func (m *Manager) Sleep(ctx context.Context, name string) error {
	st, ok := m.state(name)
	if !ok {
		return store.ErrNotFound
	}
	st.mu.Lock()
	if st.waking != nil {
		st.mu.Unlock()
		return nil
	}
	rec := st.record
	st.mu.Unlock()

	if st.activeConns.Load() > 0 {
		return nil
	}
	if rec.Status != store.StatusRunning {
		return nil
	}

	if err := m.docker.StopContainer(ctx, rec.ContainerID, stopGrace); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	m.setStatus(ctx, st, name, store.StatusStopped)
	if err := m.store.Touch(ctx, name, time.Unix(0, st.lastActive.Load())); err != nil {
		m.logf("persist last_active for %s: %v", name, err)
	}
	return nil
}

// Reap scales down every database idle beyond the configured timeout.
func (m *Manager) Reap(ctx context.Context) {
	now := m.nowFunc()
	m.mu.RLock()
	names := make([]string, 0, len(m.states))
	for name := range m.states {
		names = append(names, name)
	}
	m.mu.RUnlock()

	for _, name := range names {
		st, ok := m.state(name)
		if !ok {
			continue
		}
		st.mu.Lock()
		status := st.record.Status
		st.mu.Unlock()

		if status != store.StatusRunning || st.activeConns.Load() > 0 {
			continue
		}
		idle := now.Sub(time.Unix(0, st.lastActive.Load()))
		if idle < m.cfg.IDLE_TIMEOUT {
			continue
		}
		if err := m.Sleep(ctx, name); err != nil {
			m.logf("reap %s: %v", name, err)
			continue
		}
		m.logf("scaled %s to zero after %s idle", name, idle.Round(time.Second))
	}
}

// RunReaper ticks the reaper until ctx is cancelled.
func (m *Manager) RunReaper(ctx context.Context) {
	ticker := time.NewTicker(m.cfg.REAP_INTERVAL)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.Reap(ctx)
		}
	}
}

// Start brings a database up on demand (admin action).
func (m *Manager) Start(ctx context.Context, name string) error {
	st, ok := m.state(name)
	if !ok {
		return store.ErrNotFound
	}
	_, err := m.ensureRunning(ctx, st, name)
	return err
}

// Stop scales a database to zero regardless of idle time (admin action).
func (m *Manager) Stop(ctx context.Context, name string) error {
	st, ok := m.state(name)
	if !ok {
		return store.ErrNotFound
	}
	rec := st.snapshot()
	if err := m.docker.StopContainer(ctx, rec.ContainerID, stopGrace); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	m.setStatus(ctx, st, name, store.StatusStopped)
	return nil
}

// Pause freezes a running database's processes (admin action).
func (m *Manager) Pause(ctx context.Context, name string) error {
	st, ok := m.state(name)
	if !ok {
		return store.ErrNotFound
	}
	rec := st.snapshot()
	if err := m.docker.PauseContainer(ctx, rec.ContainerID); err != nil {
		return fmt.Errorf("pause container: %w", err)
	}
	m.setStatus(ctx, st, name, store.StatusPaused)
	return nil
}

// Delete removes the container, its volume and the metadata row. This destroys
// the data.
func (m *Manager) Delete(ctx context.Context, name string) error {
	st, ok := m.state(name)
	if !ok {
		return store.ErrNotFound
	}
	rec := st.snapshot()

	if rec.ContainerID != "" {
		if err := m.docker.RemoveContainer(ctx, rec.ContainerID, false); err != nil && !errors.Is(err, docker.ErrNotFound) {
			return fmt.Errorf("remove container: %w", err)
		}
	}
	if err := m.docker.RemoveVolume(ctx, rec.VolumeName); err != nil && !errors.Is(err, docker.ErrNotFound) {
		return fmt.Errorf("remove volume: %w", err)
	}
	if err := m.store.Delete(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	m.mu.Lock()
	delete(m.states, name)
	m.mu.Unlock()
	return nil
}

// Get returns the current metadata for one database.
func (m *Manager) Get(name string) (store.Database, error) {
	st, ok := m.state(name)
	if !ok {
		return store.Database{}, store.ErrNotFound
	}
	return st.snapshot(), nil
}

// List returns metadata for every database.
func (m *Manager) List() []store.Database {
	m.mu.RLock()
	states := make([]*dbState, 0, len(m.states))
	for _, st := range m.states {
		states = append(states, st)
	}
	m.mu.RUnlock()

	out := make([]store.Database, 0, len(states))
	for _, st := range states {
		out = append(out, st.snapshot())
	}
	return out
}

// setStatus updates status in memory and persists it.
func (m *Manager) setStatus(ctx context.Context, st *dbState, name string, status store.Status) {
	st.mu.Lock()
	st.record.Status = status
	st.mu.Unlock()
	if err := m.store.SetStatus(ctx, name, status); err != nil {
		m.logf("persist status %s for %s: %v", status, name, err)
	}
}

// markError flags a database as failed after a provisioning error.
func (m *Manager) markError(ctx context.Context, st *dbState, name string) {
	m.setStatus(context.WithoutCancel(ctx), st, name, store.StatusError)
}

func (m *Manager) logf(format string, args ...any) {
	if m.logger != nil {
		m.logger.Printf(format, args...)
	}
}

// snapshot returns a copy of the record with the live last-active timestamp.
func (st *dbState) snapshot() store.Database {
	st.mu.Lock()
	defer st.mu.Unlock()
	rec := st.record
	rec.LastActive = time.Unix(0, st.lastActive.Load())
	return rec
}
