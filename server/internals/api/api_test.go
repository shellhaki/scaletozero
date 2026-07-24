package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/docker"
	"sparkdb/scaletozero/internals/engine"
	"sparkdb/scaletozero/internals/manager"
	"sparkdb/scaletozero/internals/store"
)

// stubEngine is an always-ready engine so lifecycle calls resolve instantly.
type stubEngine struct{}

func (stubEngine) Name() string                        { return "postgres" }
func (stubEngine) Image() string                       { return "postgres:latest" }
func (stubEngine) InternalPort() int                   { return 5432 }
func (stubEngine) DataDir() string                     { return "/var/lib/postgresql/data" }
func (stubEngine) Routing() engine.RoutingMode         { return engine.SharedPortStartupRouting }
func (stubEngine) Env(u, p, d string) []string         { return nil }
func (stubEngine) Ready(context.Context, string) error { return nil }
func (stubEngine) Resolve(io.ReadWriter) (string, []byte, error) {
	return "", nil, nil
}

// newTestAPI wires the router over a manager backed by a fake Docker API.
func newTestAPI(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/networks/"):
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"Name":"sparkdb-network"}`))
		case strings.HasSuffix(r.URL.Path, "/containers/create"):
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]string{"Id": "container-abc"})
		case strings.HasSuffix(r.URL.Path, "/volumes/create"):
			w.WriteHeader(http.StatusCreated)
			w.Write([]byte(`{"Name":"vol"}`))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	t.Cleanup(srv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	cfg := &config.Config{
		DOCKER_NETWORK: "sparkdb-network",
		IDLE_TIMEOUT:   time.Minute,
		REAP_INTERVAL:  time.Second,
		WAKE_TIMEOUT:   time.Second,
	}
	dc := &docker.Client{BaseURL: srv.URL, Username: "u", Password: "p", HTTP: srv.Client()}
	engines := engine.NewRegistry(stubEngine{})

	mgr, err := manager.New(cfg, dc, engines, st, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("manager.New: %v", err)
	}
	mgr.SetBackendAddrFunc(func(store.Database) (string, error) { return "127.0.0.1:1", nil })

	r := gin.New()
	New(mgr, engines).Register(r)
	return r
}

// do issues a request and returns the recorder.
func do(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(encoded)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func createDB(t *testing.T, r *gin.Engine, name string) {
	t.Helper()
	w := do(t, r, http.MethodPost, "/api/postgres/create", gin.H{
		"name": name, "username": "haki", "password": "secret",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("create %s: status %d, body %s", name, w.Code, w.Body)
	}
}

func TestCreateReturnsProvisionedDatabase(t *testing.T) {
	r := newTestAPI(t)
	w := do(t, r, http.MethodPost, "/api/postgres/create", gin.H{
		"name": "mydb", "username": "haki", "password": "secret",
	})

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body %s", w.Code, w.Body)
	}

	var got struct {
		Database DatabaseResponse `json:"database"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Database.Name != "mydb" || got.Database.Engine != "postgres" {
		t.Errorf("database = %+v", got.Database)
	}
	// Provisioning ends scaled to zero.
	if got.Database.Status != string(store.StatusStopped) {
		t.Errorf("Status = %q, want %q", got.Database.Status, store.StatusStopped)
	}
}

func TestCreateNeverLeaksPassword(t *testing.T) {
	r := newTestAPI(t)
	w := do(t, r, http.MethodPost, "/api/postgres/create", gin.H{
		"name": "mydb", "username": "haki", "password": "supersecret",
	})
	if strings.Contains(w.Body.String(), "supersecret") {
		t.Errorf("response leaks the password: %s", w.Body)
	}
}

func TestCreateValidatesBody(t *testing.T) {
	r := newTestAPI(t)

	tests := []struct {
		name string
		body gin.H
	}{
		{"missing name", gin.H{"username": "u", "password": "p"}},
		{"missing username", gin.H{"name": "mydb", "password": "p"}},
		{"missing password", gin.H{"name": "mydb", "username": "u"}},
		{"empty body", gin.H{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := do(t, r, http.MethodPost, "/api/postgres/create", tt.body)
			if w.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", w.Code)
			}
		})
	}
}

func TestCreateRejectsMalformedJSON(t *testing.T) {
	r := newTestAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/postgres/create", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestCreateRejectsInvalidName(t *testing.T) {
	r := newTestAPI(t)
	w := do(t, r, http.MethodPost, "/api/postgres/create", gin.H{
		"name": "bad name;drop", "username": "u", "password": "p",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body %s", w.Code, w.Body)
	}
}

func TestCreateRejectsUnknownEngine(t *testing.T) {
	r := newTestAPI(t)
	w := do(t, r, http.MethodPost, "/api/cassandra/create", gin.H{
		"name": "mydb", "username": "u", "password": "p",
	})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400, body %s", w.Code, w.Body)
	}
}

func TestCreateDuplicateConflicts(t *testing.T) {
	r := newTestAPI(t)
	createDB(t, r, "mydb")

	w := do(t, r, http.MethodPost, "/api/postgres/create", gin.H{
		"name": "mydb", "username": "u", "password": "p",
	})
	if w.Code != http.StatusConflict {
		t.Errorf("status = %d, want 409, body %s", w.Code, w.Body)
	}
}

func TestLifecycleEndpoints(t *testing.T) {
	for _, path := range []string{"start", "stop", "pause", "delete"} {
		t.Run(path, func(t *testing.T) {
			r := newTestAPI(t)
			createDB(t, r, "mydb")

			w := do(t, r, http.MethodPost, "/api/postgres/"+path, gin.H{"name": "mydb"})
			if w.Code != http.StatusOK {
				t.Errorf("status = %d, want 200, body %s", w.Code, w.Body)
			}
		})
	}
}

func TestLifecycleOnUnknownDatabaseIs404(t *testing.T) {
	for _, path := range []string{"start", "stop", "pause", "delete"} {
		t.Run(path, func(t *testing.T) {
			r := newTestAPI(t)
			w := do(t, r, http.MethodPost, "/api/postgres/"+path, gin.H{"name": "ghost"})
			if w.Code != http.StatusNotFound {
				t.Errorf("status = %d, want 404, body %s", w.Code, w.Body)
			}
		})
	}
}

func TestLifecycleRequiresName(t *testing.T) {
	r := newTestAPI(t)
	for _, path := range []string{"start", "stop", "pause", "delete"} {
		w := do(t, r, http.MethodPost, "/api/postgres/"+path, gin.H{})
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", path, w.Code)
		}
	}
}

func TestStatusTransitions(t *testing.T) {
	r := newTestAPI(t)
	createDB(t, r, "mydb")

	if got := statusOf(t, r, "mydb"); got != string(store.StatusStopped) {
		t.Fatalf("after create status = %q, want stopped", got)
	}

	if w := do(t, r, http.MethodPost, "/api/postgres/start", gin.H{"name": "mydb"}); w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	if got := statusOf(t, r, "mydb"); got != string(store.StatusRunning) {
		t.Errorf("after start status = %q, want running", got)
	}

	if w := do(t, r, http.MethodPost, "/api/postgres/pause", gin.H{"name": "mydb"}); w.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", w.Code, w.Body)
	}
	if got := statusOf(t, r, "mydb"); got != string(store.StatusPaused) {
		t.Errorf("after pause status = %q, want paused", got)
	}

	if w := do(t, r, http.MethodPost, "/api/postgres/stop", gin.H{"name": "mydb"}); w.Code != http.StatusOK {
		t.Fatalf("stop: %d %s", w.Code, w.Body)
	}
	if got := statusOf(t, r, "mydb"); got != string(store.StatusStopped) {
		t.Errorf("after stop status = %q, want stopped", got)
	}
}

func TestDeleteRemovesFromListing(t *testing.T) {
	r := newTestAPI(t)
	createDB(t, r, "mydb")

	if w := do(t, r, http.MethodPost, "/api/postgres/delete", gin.H{"name": "mydb"}); w.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if n := len(listDatabases(t, r, "/api/databases")); n != 0 {
		t.Errorf("listing has %d entries after delete, want 0", n)
	}
}

func TestListReturnsAllDatabases(t *testing.T) {
	r := newTestAPI(t)
	createDB(t, r, "one")
	createDB(t, r, "two")

	list := listDatabases(t, r, "/api/databases")
	if len(list) != 2 {
		t.Fatalf("len = %d, want 2", len(list))
	}
	for _, d := range list {
		if d.Password() != "" {
			t.Errorf("listing leaks credentials")
		}
	}
}

func TestEngineScopedListFilters(t *testing.T) {
	r := newTestAPI(t)
	createDB(t, r, "one")

	if n := len(listDatabases(t, r, "/api/postgres/list")); n != 1 {
		t.Errorf("postgres list = %d, want 1", n)
	}
	if n := len(listDatabases(t, r, "/api/mysql/list")); n != 0 {
		t.Errorf("mysql list = %d, want 0 (no mysql databases exist)", n)
	}
}

func TestRootAndHealth(t *testing.T) {
	r := newTestAPI(t)

	w := do(t, r, http.MethodGet, "/health", nil)
	if w.Code != http.StatusOK {
		t.Errorf("health status = %d", w.Code)
	}

	w = do(t, r, http.MethodGet, "/", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("root status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "postgres") {
		t.Errorf("root must advertise registered engines, got %s", w.Body)
	}
}

// dbView adds a helper so tests can assert credentials never ship.
type dbView struct {
	DatabaseResponse
	Raw map[string]any
}

func (d dbView) Password() string {
	if v, ok := d.Raw["password"].(string); ok {
		return v
	}
	return ""
}

func listDatabases(t *testing.T, r *gin.Engine, path string) []dbView {
	t.Helper()
	w := do(t, r, http.MethodGet, path, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s: status %d", path, w.Code)
	}

	var raw struct {
		Databases []map[string]any `json:"databases"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode listing: %v", err)
	}

	out := make([]dbView, 0, len(raw.Databases))
	for _, entry := range raw.Databases {
		var view DatabaseResponse
		encoded, _ := json.Marshal(entry)
		json.Unmarshal(encoded, &view)
		out = append(out, dbView{DatabaseResponse: view, Raw: entry})
	}
	return out
}

func statusOf(t *testing.T, r *gin.Engine, name string) string {
	t.Helper()
	for _, d := range listDatabases(t, r, "/api/databases") {
		if d.Name == name {
			return d.Status
		}
	}
	t.Fatalf("database %q not found in listing", name)
	return ""
}
