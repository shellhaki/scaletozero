package postgres

import (
	"fmt"
	"net/http"
	"sparkdb/scaletozero/internals/shared"
	"strconv"
)

// Create provisions a new Postgres container named name and returns the host
// port it was bound to. The container is created but not started.
func (cl *Client) Create(user, password, name string) (int, error) {
	port, err := cl.PM.GenerateUniquePort()
	if err != nil {
		return 0, fmt.Errorf("allocate port: %w", err)
	}

	payload := shared.ContainerPayLoad{
		Image: "postgres:latest",
		Env: []string{
			fmt.Sprintf("POSTGRES_USER=%s", user),
			fmt.Sprintf("POSTGRES_PASSWORD=%s", password),
			fmt.Sprintf("POSTGRES_DB=%s", name),
		},
		HostConfig: shared.HostConfig{
			PortBindings: map[string][]shared.PortBinding{
				"5432/tcp": {
					{HostPort: strconv.Itoa(port)},
				},
			},
		},
	}

	path := fmt.Sprintf("/containers/create?name=%s", name)
	if _, err := cl.do(http.MethodPost, path, payload, http.StatusCreated); err != nil {
		return 0, err
	}
	return port, nil
}

// Start starts an existing container.
func (cl *Client) Start(name string) error {
	path := fmt.Sprintf("/containers/%s/start", name)
	_, err := cl.do(http.MethodPost, path, nil, http.StatusNoContent, http.StatusNotModified)
	return err
}

// Stop stops a running container, giving it 10s to shut down cleanly.
func (cl *Client) Stop(name string) error {
	path := fmt.Sprintf("/containers/%s/stop?t=10", name)
	_, err := cl.do(http.MethodPost, path, nil, http.StatusNoContent, http.StatusNotModified)
	return err
}

// Pause freezes all processes in a running container (scale-to-zero "paused"
// state) without tearing it down.
func (cl *Client) Pause(name string) error {
	path := fmt.Sprintf("/containers/%s/pause", name)
	_, err := cl.do(http.MethodPost, path, nil, http.StatusNoContent)
	return err
}

// Delete force-removes a container and its anonymous volumes.
func (cl *Client) Delete(name string) error {
	path := fmt.Sprintf("/containers/%s?v=true&force=true", name)
	_, err := cl.do(http.MethodDelete, path, nil, http.StatusNoContent)
	return err
}