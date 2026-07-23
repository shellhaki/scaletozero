package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sparkdb/scaletozero/config"
	"time"
)

// apiVersion pins the Docker Engine API version we target.
const apiVersion = "v1.44"

const userAgent = "SparkDB-Engine/1.0"

// ErrNotFound is returned when Docker replies 404 for a container or volume.
var ErrNotFound = errors.New("docker: resource not found")

// Client talks to the Docker Engine API through the authenticated socket
// proxy. Safe for concurrent use.
type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client
}

// New builds a Client from config.
func New(cfg *config.Config) *Client {
	return &Client{
		BaseURL:  cfg.DOCKER_API_URL,
		Username: cfg.DOCKER_API_USERNAME,
		Password: cfg.DOCKER_API_PASSWORD,
		HTTP:     &http.Client{Timeout: 30 * time.Second},
	}
}

// do performs an authenticated request. body, when non-nil, is JSON-encoded.
// The response is accepted only when its status is in okStatuses.
func (c *Client) do(ctx context.Context, method, path string, body any, okStatuses ...int) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode request body: %w", err)
		}
		reader = bytes.NewBuffer(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/"+apiVersion+path, reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// No credentials means the socket proxy is reached over the internal
	// Docker network, where it is not publicly exposed.
	if c.Username != "" {
		req.SetBasicAuth(c.Username, c.Password)
	}
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.HTTP.Do(req)
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
	if resp.StatusCode == http.StatusNotFound {
		return respBody, fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	return respBody, fmt.Errorf("docker API %s %s: unexpected status %s: %s", method, path, resp.Status, respBody)
}

// CreateVolume creates a named volume. Docker returns 201 on create and is
// idempotent for an existing name.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	_, err := c.do(ctx, http.MethodPost, "/volumes/create", VolumePayload{Name: name, Labels: labels}, http.StatusCreated)
	return err
}

// RemoveVolume deletes a named volume.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	path := fmt.Sprintf("/volumes/%s?force=true", url.PathEscape(name))
	_, err := c.do(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	return err
}

// CreateContainer creates a container with the given name and returns its ID.
func (c *Client) CreateContainer(ctx context.Context, name string, payload ContainerPayload) (string, error) {
	path := fmt.Sprintf("/containers/create?name=%s", url.QueryEscape(name))
	raw, err := c.do(ctx, http.MethodPost, path, payload, http.StatusCreated)
	if err != nil {
		return "", err
	}
	var out CreateContainerResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("decode create response: %w", err)
	}
	return out.ID, nil
}

// StartContainer starts a container. 304 (already started) is treated as success.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	path := fmt.Sprintf("/containers/%s/start", url.PathEscape(id))
	_, err := c.do(ctx, http.MethodPost, path, nil, http.StatusNoContent, http.StatusNotModified)
	return err
}

// StopContainer stops a container, allowing timeout seconds for clean
// shutdown. 304 (already stopped) is treated as success.
func (c *Client) StopContainer(ctx context.Context, id string, timeout int) error {
	path := fmt.Sprintf("/containers/%s/stop?t=%d", url.PathEscape(id), timeout)
	_, err := c.do(ctx, http.MethodPost, path, nil, http.StatusNoContent, http.StatusNotModified)
	return err
}

// PauseContainer freezes all processes in a running container.
func (c *Client) PauseContainer(ctx context.Context, id string) error {
	path := fmt.Sprintf("/containers/%s/pause", url.PathEscape(id))
	_, err := c.do(ctx, http.MethodPost, path, nil, http.StatusNoContent)
	return err
}

// UnpauseContainer resumes a paused container.
func (c *Client) UnpauseContainer(ctx context.Context, id string) error {
	path := fmt.Sprintf("/containers/%s/unpause", url.PathEscape(id))
	_, err := c.do(ctx, http.MethodPost, path, nil, http.StatusNoContent)
	return err
}

// RemoveContainer force-removes a container. When withVolumes is true its
// anonymous volumes go too (named volumes are never touched by this).
func (c *Client) RemoveContainer(ctx context.Context, id string, withVolumes bool) error {
	path := fmt.Sprintf("/containers/%s?force=true&v=%t", url.PathEscape(id), withVolumes)
	_, err := c.do(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	return err
}

// InspectContainer returns the container's current state.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInspect, error) {
	path := fmt.Sprintf("/containers/%s/json", url.PathEscape(id))
	raw, err := c.do(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out ContainerInspect
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode inspect response: %w", err)
	}
	return &out, nil
}
