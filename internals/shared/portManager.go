package shared

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"slices"
)

func (pm *PortManager) GenerateUniquePort() (int, error) {
	pm.mu.Lock()
	defer pm.mu.Unlock()
	min := 10000
	max := 65535
	rangeSize := big.NewInt(int64(max - min + 1))

	if len(pm.usedPorts) >= (max - min + 1) {
		return 0, fmt.Errorf("all ports in range exhausted")
	}

	for {
		n, err := rand.Int(rand.Reader, rangeSize)
		if err != nil {
			return 0, err
		}

		port := min + int(n.Int64())

		if !slices.Contains(pm.usedPorts, port) {
			pm.usedPorts = append(pm.usedPorts, port)
			return port, nil
		}
	}
}
