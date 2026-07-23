package postgres

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// newTestServer builds a gin engine whose Postgres handlers talk to a mock
// Docker API replying with dockerStatus.
func newTestServer(t *testing.T, dockerStatus int) *gin.Engine {
	t.Helper()
	var cap capture
	srv := mockDocker(t, dockerStatus, &cap)
	h := NewHandler(newTestClient(srv.URL))
	r := gin.New()
	h.Register(r)
	return r
}

func doJSON(t *testing.T, r *gin.Engine, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestCreateHandler_Success(t *testing.T) {
	r := newTestServer(t, http.StatusCreated)
	w := doJSON(t, r, "/api/postgres/create", `{"username":"u","password":"p","name":"mydb"}`)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad response json: %v", err)
	}
	if _, ok := resp["port"]; !ok {
		t.Errorf("response missing port field: %v", resp)
	}
}

func TestCreateHandler_MissingField(t *testing.T) {
	r := newTestServer(t, http.StatusCreated)
	// no password -> binding:"required" should reject with 400
	w := doJSON(t, r, "/api/postgres/create", `{"username":"u","name":"mydb"}`)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}

func TestCreateHandler_DockerError(t *testing.T) {
	r := newTestServer(t, http.StatusInternalServerError)
	w := doJSON(t, r, "/api/postgres/create", `{"username":"u","password":"p","name":"mydb"}`)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body: %s", w.Code, w.Body.String())
	}
}

func TestLifecycleHandlers_Success(t *testing.T) {
	paths := []string{
		"/api/postgres/start",
		"/api/postgres/stop",
		"/api/postgres/pause",
		"/api/postgres/delete",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			r := newTestServer(t, http.StatusNoContent)
			w := doJSON(t, r, p, `{"name":"mydb"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLifecycleHandlers_MissingName(t *testing.T) {
	r := newTestServer(t, http.StatusNoContent)
	w := doJSON(t, r, "/api/postgres/stop", `{}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", w.Code, w.Body.String())
	}
}