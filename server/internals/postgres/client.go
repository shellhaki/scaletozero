package postgres

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sparkdb/scaletozero/config"
	"sparkdb/scaletozero/internals/shared"
	"time"
)

// dockerAPIVersion pins the Docker Engine API version we target.
const dockerAPIVersion = "v1.44"

// userAgent is sent on every request to the socket proxy.
const userAgent = "Mozilla/5.0 (X11; Linux x86_64) SparkDB-Engine/1.0"

// Client talks to the Docker Engine API (through the socket proxy) to manage
// Postgres containers. It is safe for concurrent use: the PortManager guards
// its own state and the http.Client is reused.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client
	PM       *shared.PortManager
}

// NewClient builds a Client from loaded config with sane defaults.
func NewClient(cfg *config.Config) *Client {
	return &Client{
		BaseURL:  cfg.DOCKER_API_URL,
		Username: cfg.DOCKER_API_USERNAME,
		Password: cfg.DOCKER_API_PASSWORD,
		HTTP:     &http.Client{Timeout: 20 * time.Second},
		PM:       &shared.PortManager{},
	}
}

// do performs an authenticated request against the Docker API. If body is
// non-nil it is JSON-encoded. The response is accepted only when its status
// code is one of okStatuses; otherwise an error carrying the body is returned.
func (cl *Client) do(method, path string, body any, okStatuses ...int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewBuffer(encoded)
	}

	url := cl.BaseURL + "/" + dockerAPIVersion + path
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.SetBasicAuth(cl.Username, cl.Password)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := cl.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	for _, ok := range okStatuses {
		if resp.StatusCode == ok {
			return respBody, nil
		}
	}
	return respBody, fmt.Errorf("docker API %s %s: unexpected status %s: %s", method, path, resp.Status, respBody)
}