package shared

import "sync"

type PortBinding struct {
	HostPort string `json:"HostPort"`
}

type HostConfig struct {
	PortBindings map[string][]PortBinding `json:"PortBindings"`
}

type ContainerPayLoad struct {
	Image      string     `json:"Image"`
	Env        []string   `json:"Env"`
	HostConfig HostConfig `json:"HostConfig"`
}

type PortManager struct {
	mu        sync.Mutex
	usedPorts []int
}
