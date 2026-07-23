package postgres

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"sparkdb/scaletozero/internals/shared"
)

// newTestClient returns a Client wired to the given test server, plus the
// creds it will send.
func newTestClient(baseURL string) *Client {
	return &Client{
		BaseURL:  baseURL,
		Username: "haki",
		Password: "secret",
		HTTP:     &http.Client{Timeout: 5 * time.Second},
		PM:       &shared.PortManager{},
	}
}

// capture records the last request the mock Docker API received.
type capture struct {
	method string
	path   string
	rawURL string
	user   string
	pass   string
	body   []byte
}

// mockDocker spins up an httptest server that records the request and replies
// with the given status code.
func mockDocker(t *testing.T, status int, cap *capture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.rawURL = r.URL.RequestURI()
		cap.user, cap.pass, _ = r.BasicAuth()
		cap.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCreate_Success(t *testing.T) {
	var cap capture
	srv := mockDocker(t, http.StatusCreated, &cap)
	cl := newTestClient(srv.URL)

	port, err := cl.Create("u", "p", "mydb")
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if port < 10000 || port > 65535 {
		t.Errorf("port %d out of range", port)
	}

	if cap.method != http.MethodPost {
		t.Errorf("method = %s, want POST", cap.method)
	}
	if cap.path != "/v1.44/containers/create" {
		t.Errorf("path = %s, want /v1.44/containers/create", cap.path)
	}
	if !strings.Contains(cap.rawURL, "name=mydb") {
		t.Errorf("rawURL %q missing name=mydb", cap.rawURL)
	}
	if cap.user != "haki" || cap.pass != "secret" {
		t.Errorf("basic auth = %s:%s, want haki:secret", cap.user, cap.pass)
	}

	// Body must carry the image, env, and the allocated port binding.
	var payload shared.ContainerPayLoad
	if err := json.Unmarshal(cap.body, &payload); err != nil {
		t.Fatalf("bad request body: %v", err)
	}
	if payload.Image != "postgres:latest" {
		t.Errorf("image = %s, want postgres:latest", payload.Image)
	}
	bindings := payload.HostConfig.PortBindings["5432/tcp"]
	if len(bindings) != 1 || bindings[0].HostPort != strconv.Itoa(port) {
		t.Errorf("port binding = %+v, want host port %d", bindings, port)
	}
}

func TestCreate_DockerError(t *testing.T) {
	var cap capture
	srv := mockDocker(t, http.StatusInternalServerError, &cap)
	cl := newTestClient(srv.URL)

	if _, err := cl.Create("u", "p", "mydb"); err == nil {
		t.Fatal("expected error on 500 response, got nil")
	}
}

func TestLifecycleOps(t *testing.T) {
	cases := []struct {
		name       string
		call       func(cl *Client) error
		okStatus   int
		wantMethod string
		wantPath   string
	}{
		{"Start", func(cl *Client) error { return cl.Start("db") }, http.StatusNoContent, http.MethodPost, "/v1.44/containers/db/start"},
		{"Stop", func(cl *Client) error { return cl.Stop("db") }, http.StatusNoContent, http.MethodPost, "/v1.44/containers/db/stop"},
		{"Pause", func(cl *Client) error { return cl.Pause("db") }, http.StatusNoContent, http.MethodPost, "/v1.44/containers/db/pause"},
		{"Delete", func(cl *Client) error { return cl.Delete("db") }, http.StatusNoContent, http.MethodDelete, "/v1.44/containers/db"},
	}

	for _, tc := range cases {
		t.Run(tc.name+"_success", func(t *testing.T) {
			var cap capture
			srv := mockDocker(t, tc.okStatus, &cap)
			cl := newTestClient(srv.URL)

			if err := tc.call(cl); err != nil {
				t.Fatalf("%s returned error: %v", tc.name, err)
			}
			if cap.method != tc.wantMethod {
				t.Errorf("method = %s, want %s", cap.method, tc.wantMethod)
			}
			if cap.path != tc.wantPath {
				t.Errorf("path = %s, want %s", cap.path, tc.wantPath)
			}
			if cap.user != "haki" || cap.pass != "secret" {
				t.Errorf("basic auth = %s:%s, want haki:secret", cap.user, cap.pass)
			}
		})

		t.Run(tc.name+"_error", func(t *testing.T) {
			var cap capture
			srv := mockDocker(t, http.StatusInternalServerError, &cap)
			cl := newTestClient(srv.URL)

			if err := tc.call(cl); err == nil {
				t.Fatalf("%s: expected error on 500, got nil", tc.name)
			}
		})
	}
}