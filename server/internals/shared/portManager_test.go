package shared

import (
	"sync"
	"testing"
)

func TestGenerateUniquePort_InRange(t *testing.T) {
	pm := &PortManager{}
	for i := 0; i < 100; i++ {
		port, err := pm.GenerateUniquePort()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if port < 10000 || port > 65535 {
			t.Fatalf("port %d out of range [10000,65535]", port)
		}
	}
}

func TestGenerateUniquePort_NoDuplicates(t *testing.T) {
	pm := &PortManager{}
	seen := make(map[int]bool)
	for i := 0; i < 500; i++ {
		port, err := pm.GenerateUniquePort()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if seen[port] {
			t.Fatalf("duplicate port %d issued", port)
		}
		seen[port] = true
	}
}

func TestGenerateUniquePort_Concurrent(t *testing.T) {
	pm := &PortManager{}
	const n = 1000

	var mu sync.Mutex
	seen := make(map[int]bool)
	var wg sync.WaitGroup

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			port, err := pm.GenerateUniquePort()
			if err != nil {
				t.Errorf("unexpected error: %v", err)
				return
			}
			mu.Lock()
			if seen[port] {
				t.Errorf("duplicate port %d under concurrency", port)
			}
			seen[port] = true
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("got %d unique ports, want %d", len(seen), n)
	}
}