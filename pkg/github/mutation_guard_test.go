package github

import (
	"sync"
	"testing"
)

func TestMutationRequestIDIsIsolatedAcrossConcurrentRequests(t *testing.T) {
	const workers = 32
	const requestsPerWorker = 16
	ids := make(chan string, workers*requestsPerWorker)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for request := 0; request < requestsPerWorker; request++ {
				ids <- mutationRequestID()
			}
		}()
	}
	group.Wait()
	close(ids)

	seen := make(map[string]struct{}, workers*requestsPerWorker)
	for id := range ids {
		if id == "" {
			t.Fatal("mutation request ID must not be empty")
		}
		if _, exists := seen[id]; exists {
			t.Fatalf("mutation request ID was reused concurrently: %s", id)
		}
		seen[id] = struct{}{}
	}
	if got, want := len(seen), workers*requestsPerWorker; got != want {
		t.Fatalf("generated %d unique request IDs, want %d", got, want)
	}
}
