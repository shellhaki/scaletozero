package docker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// capture records what the fake Docker API received.
type capture struct {
	method string
	path   string
	query  string
	body   []byte
	user   string
	pass   string
	agent  string
}

// newFakeDocker serves one canned reply and records the request.
func newFakeDocker(t *testing.T, status int, reply string) (*Client, *capture) {
	t.Helper()
	got := &capture{}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method = r.Method
		got.path = r.URL.Path
		got.query = r.URL.RawQuery
		got.agent = r.Header.Get("User-Agent")
		got.user, got.pass, _ = r.BasicAuth()
		buf := make([]byte, r.ContentLength)
		if r.ContentLength > 0 {
			r.Body.Read(buf)
			got.body = buf
		}
		w.WriteHeader(status)
		w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)

	return &Client{
		BaseURL:  srv.URL,
		Username: "haki",
		Password: "hunter2",
		HTTP:     srv.Client(),
	}, got
}

func TestCreateContainerSendsPayloadAndReturnsID(t *testing.T) {
	client, got := newFakeDocker(t, http.StatusCreated, `{"Id":"abc123","Warnings":[]}`)

	payload := ContainerPayload{
		Image: "postgres:latest",
		Env:   []string{"POSTGRES_DB=mydb"},
		HostConfig: HostConfig{
			Binds:         []string{"vol:/var/lib/postgresql/data"},
			NetworkMode:   "sparkdb-network",
			RestartPolicy: RestartPolicy{Name: "no"},
		},
		NetworkingConfig: &NetworkingConfig{
			EndpointsConfig: map[string]EndpointConfig{"sparkdb-network": {}},
		},
	}

	id, err := client.CreateContainer(context.Background(), "sparkdb-postgres-mydb", payload)
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if id != "abc123" {
		t.Errorf("id = %q, want abc123", id)
	}
	if got.method != http.MethodPost {
		t.Errorf("method = %s, want POST", got.method)
	}
	if got.path != "/v1.44/containers/create" {
		t.Errorf("path = %q", got.path)
	}
	if !strings.Contains(got.query, "name=sparkdb-postgres-mydb") {
		t.Errorf("query = %q, want the container name", got.query)
	}

	var sent ContainerPayload
	if err := json.Unmarshal(got.body, &sent); err != nil {
		t.Fatalf("decode sent payload: %v", err)
	}
	if sent.Image != "postgres:latest" {
		t.Errorf("Image = %q", sent.Image)
	}
	if len(sent.HostConfig.Binds) != 1 || sent.HostConfig.Binds[0] != "vol:/var/lib/postgresql/data" {
		t.Errorf("Binds = %v, the volume must be mounted at the data dir", sent.HostConfig.Binds)
	}
	if sent.HostConfig.NetworkMode != "sparkdb-network" {
		t.Errorf("NetworkMode = %q, the proxy reaches containers over this network", sent.HostConfig.NetworkMode)
	}
	if sent.HostConfig.RestartPolicy.Name != "no" {
		t.Errorf("RestartPolicy = %q, want \"no\" so Docker never resurrects a reaped container", sent.HostConfig.RestartPolicy.Name)
	}
}

func TestCreateContainerPublishesNoHostPorts(t *testing.T) {
	// Containers are reached by name on the Docker network. A published port
	// would leak the database to the host and exhaust the port range.
	client, got := newFakeDocker(t, http.StatusCreated, `{"Id":"abc123"}`)

	_, err := client.CreateContainer(context.Background(), "c", ContainerPayload{Image: "postgres:latest"})
	if err != nil {
		t.Fatalf("CreateContainer: %v", err)
	}
	if strings.Contains(string(got.body), "PortBindings") {
		t.Errorf("payload must not contain PortBindings, got %s", got.body)
	}
}

func TestRequestsCarryBasicAuth(t *testing.T) {
	client, got := newFakeDocker(t, http.StatusNoContent, "")

	if err := client.StartContainer(context.Background(), "abc123"); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if got.user != "haki" || got.pass != "hunter2" {
		t.Errorf("basic auth = %q/%q, want haki/hunter2", got.user, got.pass)
	}
	if got.agent != userAgent {
		t.Errorf("User-Agent = %q, want %q", got.agent, userAgent)
	}
}

func TestNoCredentialsSendsNoAuthHeader(t *testing.T) {
	// Over the internal Docker network the socket proxy needs no basic auth.
	client, got := newFakeDocker(t, http.StatusNoContent, "")
	client.Username = ""
	client.Password = ""

	if err := client.StartContainer(context.Background(), "abc"); err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	if got.user != "" {
		t.Errorf("basic auth user = %q, want none sent", got.user)
	}
}

func TestLifecycleEndpoints(t *testing.T) {
	tests := []struct {
		name       string
		call       func(*Client) error
		wantMethod string
		wantPath   string
		wantQuery  string
		status     int
	}{
		{
			name:       "start",
			call:       func(c *Client) error { return c.StartContainer(context.Background(), "abc") },
			wantMethod: http.MethodPost,
			wantPath:   "/v1.44/containers/abc/start",
			status:     http.StatusNoContent,
		},
		{
			name:       "stop passes grace period",
			call:       func(c *Client) error { return c.StopContainer(context.Background(), "abc", 10) },
			wantMethod: http.MethodPost,
			wantPath:   "/v1.44/containers/abc/stop",
			wantQuery:  "t=10",
			status:     http.StatusNoContent,
		},
		{
			name:       "pause",
			call:       func(c *Client) error { return c.PauseContainer(context.Background(), "abc") },
			wantMethod: http.MethodPost,
			wantPath:   "/v1.44/containers/abc/pause",
			status:     http.StatusNoContent,
		},
		{
			name:       "unpause",
			call:       func(c *Client) error { return c.UnpauseContainer(context.Background(), "abc") },
			wantMethod: http.MethodPost,
			wantPath:   "/v1.44/containers/abc/unpause",
			status:     http.StatusNoContent,
		},
		{
			name:       "remove container keeps named volumes",
			call:       func(c *Client) error { return c.RemoveContainer(context.Background(), "abc", false) },
			wantMethod: http.MethodDelete,
			wantPath:   "/v1.44/containers/abc",
			wantQuery:  "force=true&v=false",
			status:     http.StatusNoContent,
		},
		{
			name:       "remove volume",
			call:       func(c *Client) error { return c.RemoveVolume(context.Background(), "vol") },
			wantMethod: http.MethodDelete,
			wantPath:   "/v1.44/volumes/vol",
			status:     http.StatusNoContent,
		},
		{
			name:       "create volume",
			call:       func(c *Client) error { return c.CreateVolume(context.Background(), "vol", nil) },
			wantMethod: http.MethodPost,
			wantPath:   "/v1.44/volumes/create",
			status:     http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, got := newFakeDocker(t, tt.status, "")
			if err := tt.call(client); err != nil {
				t.Fatalf("call: %v", err)
			}
			if got.method != tt.wantMethod {
				t.Errorf("method = %s, want %s", got.method, tt.wantMethod)
			}
			if got.path != tt.wantPath {
				t.Errorf("path = %q, want %q", got.path, tt.wantPath)
			}
			if tt.wantQuery != "" && got.query != tt.wantQuery {
				t.Errorf("query = %q, want %q", got.query, tt.wantQuery)
			}
		})
	}
}

func TestStartTreatsAlreadyStartedAsSuccess(t *testing.T) {
	// Docker answers 304 when the container is already running. A concurrent
	// wake must not surface that as a failure.
	client, _ := newFakeDocker(t, http.StatusNotModified, "")
	if err := client.StartContainer(context.Background(), "abc"); err != nil {
		t.Errorf("StartContainer on 304: %v, want nil", err)
	}
}

func TestStopTreatsAlreadyStoppedAsSuccess(t *testing.T) {
	client, _ := newFakeDocker(t, http.StatusNotModified, "")
	if err := client.StopContainer(context.Background(), "abc", 10); err != nil {
		t.Errorf("StopContainer on 304: %v, want nil", err)
	}
}

func TestNotFoundIsTyped(t *testing.T) {
	// Delete unwraps this to stay idempotent when a container is already gone.
	client, _ := newFakeDocker(t, http.StatusNotFound, `{"message":"no such container"}`)
	err := client.StartContainer(context.Background(), "ghost")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestUnexpectedStatusIncludesBody(t *testing.T) {
	client, _ := newFakeDocker(t, http.StatusInternalServerError, `{"message":"boom"}`)
	err := client.StartContainer(context.Background(), "abc")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %q, want the Docker message included for debugging", err)
	}
}

func TestInspectContainerDecodesState(t *testing.T) {
	reply := `{"Id":"abc123","Name":"/sparkdb-postgres-mydb","State":{"Status":"running","Running":true,"Paused":false,"ExitCode":0}}`
	client, got := newFakeDocker(t, http.StatusOK, reply)

	info, err := client.InspectContainer(context.Background(), "abc123")
	if err != nil {
		t.Fatalf("InspectContainer: %v", err)
	}
	if got.method != http.MethodGet || got.path != "/v1.44/containers/abc123/json" {
		t.Errorf("request = %s %s", got.method, got.path)
	}
	if info.ID != "abc123" {
		t.Errorf("ID = %q", info.ID)
	}
	if !info.State.Running || info.State.Status != "running" {
		t.Errorf("State = %+v, want running", info.State)
	}
}

func TestContextCancellationIsHonoured(t *testing.T) {
	client, _ := newFakeDocker(t, http.StatusNoContent, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := client.StartContainer(ctx, "abc"); err == nil {
		t.Error("expected an error from a cancelled context")
	}
}
