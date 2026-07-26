package github

import (
	"fmt"
	"sync"
	"testing"
	"time"
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

func TestRepositoryMutationPreviewBindsIncidentRepositoriesAndIdenticalPaths(t *testing.T) {
	base := func(repo string, id int64, head string) repositoryMutationTarget {
		return repositoryMutationTarget{
			Owner: "lutzkind", Repository: repo, RepositoryID: id, Operation: "atomic_repository_changes",
			Branch: "main", Ref: "refs/heads/main", Paths: []string{"README.md"}, ExpectedHeadSHA: head,
		}
	}
	change := []RepositoryChange{{Path: "README.md", Operation: "upsert", Content: "fixture\n"}}
	lead := base("lead-website-enricher", 101, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	frappe := base("frappe-airwallex", 202, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	leadHash := repositoryMutationHash(lead, "isolated fixture mutation", change)
	frappeHash := repositoryMutationHash(frappe, "isolated fixture mutation", change)
	if leadHash == frappeHash {
		t.Fatal("identical branch/path payloads in different repositories must have different preview hashes")
	}
	if lead.RepositoryID == frappe.RepositoryID || lead.Ref != frappe.Ref || lead.Paths[0] != frappe.Paths[0] {
		t.Fatal("test fixture must exercise immutable repository IDs with identical ref/path names")
	}
	if leadHash == repositoryMutationHash(lead, "isolated fixture mutation", []RepositoryChange{{Path: "README.md", Operation: "upsert", Content: "changed fixture\n"}}) {
		t.Fatal("preview hash must bind expected content")
	}
}

func TestRepositoryMutationPreviewRejectsCrossRepositoryStaleAndChangedRefBindings(t *testing.T) {
	target := repositoryMutationTarget{Owner: "lutzkind", Repository: "lead-website-enricher", RepositoryID: 303, Operation: "atomic_repository_changes", Branch: "main", Ref: "refs/heads/main", Paths: []string{"same.txt"}, ExpectedHeadSHA: "cccccccccccccccccccccccccccccccccccccccc"}
	change := []RepositoryChange{{Path: "same.txt", Operation: "upsert", Content: "one\n"}}
	previewHash := repositoryMutationHash(target, "fixture", change)
	crossRepo := target
	crossRepo.Repository = "frappe-airwallex"
	crossRepo.RepositoryID = 404
	if repositoryMutationHash(crossRepo, "fixture", change) == previewHash {
		t.Fatal("apply against a different repository must not reuse the preview binding")
	}
	changedRef := target
	changedRef.ExpectedHeadSHA = "dddddddddddddddddddddddddddddddddddddddd"
	if repositoryMutationHash(changedRef, "fixture", change) == previewHash {
		t.Fatal("apply after a branch head change must not reuse the preview binding")
	}
	stale := repositoryMutationPreview{Target: target, PreviewHash: previewHash, ExpiresAt: time.Now().Add(-time.Minute)}
	if !stale.ExpiresAt.Before(time.Now()) {
		t.Fatal("expired previews must be rejected")
	}
	rename := target
	rename.Repository = "lead-website-enricher-renamed"
	if repositoryMutationHash(rename, "fixture", change) == previewHash {
		t.Fatal("repository rename between preview and apply must invalidate the binding")
	}
	deleted := target
	deleted.RepositoryID = 0
	if repositoryMutationHash(deleted, "fixture", change) == previewHash {
		t.Fatal("repository deletion between preview and apply must invalidate the binding")
	}
}

func TestRepositoryMutationResponseAttributionIsConcurrentAndTargetBound(t *testing.T) {
	const workers = 24
	results := make(chan ApplyRepositoryChangesResponse, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			repo := fmt.Sprintf("fixture-%d", index)
			results <- ApplyRepositoryChangesResponse{RequestID: fmt.Sprintf("request-%d", index), AuditID: fmt.Sprintf("audit-%d", index), ImmutableRepositoryID: int64(index + 1), RequestedTarget: map[string]any{"owner": "fixture", "repo": repo, "branch": "main", "paths": []string{"same.txt"}}}
		}(index)
	}
	group.Wait()
	close(results)
	seen := make(map[string]struct{}, workers)
	for response := range results {
		repo := response.RequestedTarget["repo"].(string)
		if response.RequestID == "" || response.AuditID == "" || response.ImmutableRepositoryID == 0 || repo == "" {
			t.Fatal("concurrent response is missing target-bound attestation fields")
		}
		seen[repo] = struct{}{}
	}
	if len(seen) != workers {
		t.Fatalf("response attribution collapsed %d concurrent targets into %d repositories", workers, len(seen))
	}
}
