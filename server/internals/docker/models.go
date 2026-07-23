package docker

// EndpointConfig is a container's attachment to one network.
type EndpointConfig struct {
	Aliases []string `json:"Aliases,omitempty"`
}

// NetworkingConfig attaches a container to networks at creation time.
type NetworkingConfig struct {
	EndpointsConfig map[string]EndpointConfig `json:"EndpointsConfig"`
}

// RestartPolicy controls container restart behaviour. We deliberately use
// "no": the reaper owns the lifecycle, Docker must not resurrect containers.
type RestartPolicy struct {
	Name string `json:"Name"`
}

// HostConfig is the host-side container configuration. Note there are no
// PortBindings: containers are reached over the Docker network by name, so
// nothing is published to the host.
type HostConfig struct {
	Binds         []string      `json:"Binds,omitempty"`
	NetworkMode   string        `json:"NetworkMode,omitempty"`
	RestartPolicy RestartPolicy `json:"RestartPolicy"`
}

// ContainerPayload is the body of POST /containers/create.
type ContainerPayload struct {
	Image            string              `json:"Image"`
	Env              []string            `json:"Env,omitempty"`
	Labels           map[string]string   `json:"Labels,omitempty"`
	ExposedPorts     map[string]struct{} `json:"ExposedPorts,omitempty"`
	HostConfig       HostConfig          `json:"HostConfig"`
	NetworkingConfig *NetworkingConfig   `json:"NetworkingConfig,omitempty"`
}

// CreateContainerResponse is the reply to POST /containers/create.
type CreateContainerResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// VolumePayload is the body of POST /volumes/create.
type VolumePayload struct {
	Name   string            `json:"Name"`
	Labels map[string]string `json:"Labels,omitempty"`
}

// ContainerState is the runtime state reported by container inspect.
type ContainerState struct {
	// Status is one of: created, running, paused, restarting, removing,
	// exited, dead.
	Status   string `json:"Status"`
	Running  bool   `json:"Running"`
	Paused   bool   `json:"Paused"`
	ExitCode int    `json:"ExitCode"`
	Error    string `json:"Error"`
}

// ContainerInspect is the subset of GET /containers/{id}/json we care about.
type ContainerInspect struct {
	ID    string         `json:"Id"`
	Name  string         `json:"Name"`
	State ContainerState `json:"State"`
}
