package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/google/go-github/v87/github"
	"github.com/stretchr/testify/require"
)

func TestCreateOrUpdateFileOneLinePatchLargeFileDryRunAndVerification(t *testing.T) {
	lines := []string{"header"}
	for i := 0; i < 1000; i++ {
		lines = append(lines, "unrelated line")
	}
	lines = append(lines, "target = old")
	for i := 0; i < 1000; i++ {
		lines = append(lines, "unrelated tail")
	}
	lines = append(lines, "footer")
	before := strings.Join(lines, "\n")
	after := strings.Replace(before, "target = old", "target = new", 1)
	current := &github.RepositoryContent{Path: github.Ptr("fixture.txt"), SHA: github.Ptr("blob-old"), Type: github.Ptr("file"), Content: github.Ptr(base64.StdEncoding.EncodeToString([]byte(before))), Encoding: github.Ptr("base64")}
	updated := &github.RepositoryContent{Path: github.Ptr("fixture.txt"), SHA: github.Ptr("blob-new"), Type: github.Ptr("file"), Content: github.Ptr(base64.StdEncoding.EncodeToString([]byte(after))), Encoding: github.Ptr("base64")}
	commit := &github.RepositoryContentResponse{Content: updated}
	getCount := 0
	clientHTTP := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposContentsByOwnerByRepoByPath: func(w http.ResponseWriter, _ *http.Request) {
			getCount++
			payload := current
			if getCount > 1 {
				payload = updated
			}
			mockResponse(t, http.StatusOK, payload)(w, nil)
		},
		PutReposContentsByOwnerByRepoByPath: mockResponse(t, http.StatusOK, commit),
	})
	client := mustNewGHClient(t, clientHTTP)
	deps := BaseDeps{Client: client}
	serverTool := CreateOrUpdateFile(translations.NullTranslationHelper)
	handler := serverTool.Handler(deps)
	request := createMCPRequest(map[string]any{
		"owner": "owner", "repo": "repo", "path": "fixture.txt", "message": "patch fixture", "branch": "main",
		"operation": "patch_text", "expected_blob_sha": "blob-old", "search": "target = old", "replace": "target = new", "expected_occurrences": 1,
	})
	result, err := handler(ContextWithDeps(context.Background(), deps), &request)
	require.NoError(t, err)
	require.False(t, result.IsError)
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(getTextResult(t, result).Text), &payload))
	require.Equal(t, true, payload["ok"])
	require.Equal(t, true, payload["applied"])
	require.Equal(t, true, payload["verification"].(map[string]any)["exact_content"])

	dryGetCount := 0
	dryHTTP := MockHTTPClientWithHandlers(map[string]http.HandlerFunc{
		GetReposContentsByOwnerByRepoByPath: func(w http.ResponseWriter, _ *http.Request) {
			dryGetCount++
			mockResponse(t, http.StatusOK, current)(w, nil)
		},
	})
	dryDeps := BaseDeps{Client: mustNewGHClient(t, dryHTTP)}
	dryHandler := serverTool.Handler(dryDeps)
	dryRequest := createMCPRequest(map[string]any{
		"owner": "owner", "repo": "repo", "path": "fixture.txt", "message": "preview", "branch": "main", "operation": "patch_text",
		"expected_blob_sha": "blob-old", "search": "target = old", "replace": "target = new", "expected_occurrences": 1, "dry_run": true,
	})
	dryResult, err := dryHandler(ContextWithDeps(context.Background(), dryDeps), &dryRequest)
	require.NoError(t, err)
	require.False(t, dryResult.IsError)
	var dryPayload map[string]any
	require.NoError(t, json.Unmarshal([]byte(getTextResult(t, dryResult).Text), &dryPayload))
	require.Equal(t, true, dryPayload["dry_run"])
	require.Equal(t, 1, dryGetCount)
}
