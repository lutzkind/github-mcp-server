package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	ghErrors "github.com/github/github-mcp-server/pkg/errors"
	"github.com/github/github-mcp-server/pkg/ifc"
	"github.com/github/github-mcp-server/pkg/inventory"
	"github.com/github/github-mcp-server/pkg/octicons"
	"github.com/github/github-mcp-server/pkg/scopes"
	"github.com/github/github-mcp-server/pkg/translations"
	"github.com/github/github-mcp-server/pkg/utils"
	"github.com/google/go-github/v87/github"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/shurcooL/githubv4"
)

func GetCommit(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "get_commit",
			Description: t("TOOL_GET_COMMITS_DESCRIPTION", "Get details for a commit from a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_GET_COMMITS_USER_TITLE", "Get commit details"),
				ReadOnlyHint: true,
			},
			InputSchema: WithPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"sha": {
						Type:        "string",
						Description: "Commit SHA, branch name, or tag name",
					},
					"detail": {
						Type:        "string",
						Enum:        []any{"none", "stats", "full_patch"},
						Description: "Level of detail to include for changed files. \"none\" omits stats and files entirely. \"stats\" (default) includes per-file metadata: filename, status, and lines-of-code counts (additions, deletions, changes), with no patch content. \"full_patch\" additionally includes the unified diff content for each file and can be very large.",
						Default:     json.RawMessage(`"stats"`),
					},
				},
				Required: []string{"owner", "repo", "sha"},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sha, err := RequiredParam[string](args, "sha")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			detailRaw, err := OptionalParam[string](args, "detail")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			detail, err := parseCommitDetail(detailRaw)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			opts := &github.ListOptions{
				Page:    pagination.Page,
				PerPage: pagination.PerPage,
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}
			commit, resp, err := client.Repositories.GetCommit(ctx, owner, repo, sha, opts)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					fmt.Sprintf("failed to get commit: %s", sha),
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get commit", resp, body), nil, nil
			}

			// Convert to minimal commit
			minimalCommit := convertToMinimalCommit(commit, detail)

			r, err := json.Marshal(minimalCommit)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Commit content is reachable from the repo's history; in public
			// repos anyone can land it via a PR (untrusted), in private repos
			// only collaborators can (trusted). Confidentiality follows repo
			// visibility.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result, ifc.LabelCommitContents)
			return result, nil, nil
		},
	)
}

// ListCommits creates a tool to get commits of a branch in a repository.
func ListCommits(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "list_commits",
			Description: t("TOOL_LIST_COMMITS_DESCRIPTION", "Get list of commits of a branch in a GitHub repository. Returns at least 30 results per page by default, but can return more if specified using the perPage parameter (up to 100)."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_LIST_COMMITS_USER_TITLE", "List commits"),
				ReadOnlyHint: true,
			},
			InputSchema: WithPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"sha": {
						Type:        "string",
						Description: "Commit SHA, branch or tag name to list commits of. If not provided, uses the default branch of the repository. If a commit SHA is provided, will list commits up to that SHA.",
					},
					"author": {
						Type:        "string",
						Description: "Author username or email address to filter commits by",
					},
					"path": {
						Type:        "string",
						Description: "Only commits containing this file path will be returned",
					},
					"since": {
						Type:        "string",
						Description: "Only commits after this date will be returned (ISO 8601 format: YYYY-MM-DDTHH:MM:SSZ or YYYY-MM-DD)",
					},
					"until": {
						Type:        "string",
						Description: "Only commits before this date will be returned (ISO 8601 format: YYYY-MM-DDTHH:MM:SSZ or YYYY-MM-DD)",
					},
				},
				Required: []string{"owner", "repo"},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sha, err := OptionalParam[string](args, "sha")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			author, err := OptionalParam[string](args, "author")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path, err := OptionalParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sinceStr, err := OptionalParam[string](args, "since")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			untilStr, err := OptionalParam[string](args, "until")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			// Set default perPage to 30 if not provided
			perPage := pagination.PerPage
			if perPage == 0 {
				perPage = 30
			}
			opts := &github.CommitsListOptions{
				SHA:    sha,
				Path:   path,
				Author: author,
				ListOptions: github.ListOptions{
					Page:    pagination.Page,
					PerPage: perPage,
				},
			}
			if sinceStr != "" {
				sinceTime, err := parseISOTimestamp(sinceStr)
				if err != nil {
					return utils.NewToolResultError(fmt.Sprintf("invalid since timestamp: %s", err)), nil, nil
				}
				opts.Since = sinceTime
			}
			if untilStr != "" {
				untilTime, err := parseISOTimestamp(untilStr)
				if err != nil {
					return utils.NewToolResultError(fmt.Sprintf("invalid until timestamp: %s", err)), nil, nil
				}
				opts.Until = untilTime
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}
			commits, resp, err := retryGitHubCall(ctx, deps, "list_commits", func(callCtx context.Context) ([]*github.RepositoryCommit, *github.Response, error) {
				return client.Repositories.ListCommits(callCtx, owner, repo, opts)
			})
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					fmt.Sprintf("failed to list commits: %s", sha),
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to list commits", resp, body), nil, nil
			}

			// Convert to minimal commits
			minimalCommits := make([]MinimalCommit, len(commits))
			for i, commit := range commits {
				minimalCommits[i] = convertToMinimalCommit(commit, commitDetailNone)
			}

			r, err := json.Marshal(minimalCommits)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Commit content is reachable from the repo's history; integrity
			// follows the same public-untrusted / private-trusted rule as file
			// contents. Confidentiality follows repo visibility.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result, ifc.LabelCommitContents)
			return result, nil, nil
		},
	)
}

// ListBranches creates a tool to list branches in a GitHub repository.
func ListBranches(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "list_branches",
			Description: t("TOOL_LIST_BRANCHES_DESCRIPTION", "List branches in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_LIST_BRANCHES_USER_TITLE", "List branches"),
				ReadOnlyHint: true,
			},
			InputSchema: WithPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
				},
				Required: []string{"owner", "repo"},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			opts := &github.BranchListOptions{
				ListOptions: github.ListOptions{
					Page:    pagination.Page,
					PerPage: pagination.PerPage,
				},
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			branches, resp, err := client.Repositories.ListBranches(ctx, owner, repo, opts)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to list branches",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to list branches", resp, body), nil, nil
			}

			// Convert to minimal branches
			minimalBranches := make([]MinimalBranch, 0, len(branches))
			for _, branch := range branches {
				minimalBranches = append(minimalBranches, convertToMinimalBranch(branch))
			}

			r, err := json.Marshal(minimalBranches)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Branches are structural repo metadata that only collaborators
			// with push access can create, so integrity is trusted.
			// Confidentiality follows repo visibility.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result, ifc.LabelRepoMetadata)
			return result, nil, nil
		},
	)
}

// CreateOrUpdateFile creates a tool to create or update a file in a GitHub repository.
func CreateOrUpdateFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name: "create_or_update_file",
			Description: t("TOOL_CREATE_OR_UPDATE_FILE_DESCRIPTION", `Create or update a single file in a GitHub repository. 
If updating, you should provide the SHA of the file you want to update. Use this tool to create or update a file in a GitHub repository remotely; do not use it for local file operations.

In order to obtain the SHA of original file version before updating, use the following git command:
git rev-parse <branch>:<path to file>

SHA MUST be provided for existing file updates.
`),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_CREATE_OR_UPDATE_FILE_USER_TITLE", "Create or update file"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner (username or organization)",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"path": {
						Type:        "string",
						Description: "Path where to create/update the file",
					},
					"content": {
						Type:        "string",
						Description: "Content of the file",
					},
					"operation": {
						Type:        "string",
						Enum:        []any{"replace", "patch_text", "patch_range", "unified_diff"},
						Description: "Mutation mode. Omitting this keeps the legacy replace behavior.",
					},
					"expected_blob_sha": {
						Type:        "string",
						Description: "Current Git blob SHA. Required when modifying an existing file.",
					},
					"search":               {Type: "string", Description: "Exact text to find for patch_text."},
					"replace":              {Type: "string", Description: "Replacement text for patch_text."},
					"expected_occurrences": {Type: "integer", Description: "Exact number of search matches required."},
					"start_line":           {Type: "integer", Description: "One-based inclusive start line for patch_range."},
					"end_line":             {Type: "integer", Description: "One-based inclusive end line for patch_range."},
					"replacement":          {Type: "string", Description: "Replacement text for patch_range."},
					"patch":                {Type: "string", Description: "Single-file unified diff for unified_diff."},
					"message": {
						Type:        "string",
						Description: "Commit message",
					},
					"branch": {
						Type:        "string",
						Description: "Branch to create/update the file in",
					},
					"sha": {
						Type:        "string",
						Description: "The blob SHA of the file being replaced. Required if the file already exists.",
					},
					"dry_run": {
						Type:        "boolean",
						Description: "Validate the request and report what would happen without writing to GitHub",
					},
				},
				Required: []string{"owner", "repo", "path", "message", "branch"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			content, err := OptionalParam[string](args, "content")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			message, err := RequiredParam[string](args, "message")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := RequiredParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sha, err := OptionalParam[string](args, "sha")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			dryRun, err := OptionalParam[bool](args, "dry_run")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			_, operationProvided := args["operation"]
			operation, err := OptionalParam[string](args, "operation")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			expectedBlobSHA, err := OptionalParam[string](args, "expected_blob_sha")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if expectedBlobSHA == "" {
				expectedBlobSHA = sha
			}
			expectedOccurrences, err := optionalIntArgument(args, "expected_occurrences")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			startLine, err := optionalIntArgument(args, "start_line")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			endLine, err := optionalIntArgument(args, "end_line")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			search, err := OptionalParam[string](args, "search")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			replace, err := OptionalParam[string](args, "replace")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			replacement, err := OptionalParam[string](args, "replacement")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			patch, err := OptionalParam[string](args, "patch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if isSecretLikeRepoPath(path) {
				return newBlockedFileToolResult(path, "secret_like_file"), nil, nil
			}
			if operation == "" {
				operation = "replace"
			}
			if operation == "replace" && content == "" {
				return utils.NewToolResultError("content is required for replace"), nil, nil
			}
			if reason := detectSecretLikeContent(path, content); reason != "" {
				return newBlockedFileToolResult(path, reason), nil, nil
			}
			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}
			if !operationProvided && args["expected_blob_sha"] == nil {
				return createOrUpdateFileResult(ctx, deps, client, owner, repo, path, content, message, branch, sha)
			}
			return createOrUpdateFileOperation(ctx, deps, client, owner, repo, path, message, branch, expectedBlobSHA, dryRun, filePatchInput{
				Operation: operation, Content: content, Search: search, Replace: replace, ExpectedOccurrences: expectedOccurrences,
				StartLine: startLine, EndLine: endLine, Replacement: replacement, Patch: patch,
			})
		},
	)
}

func githubSharedRoot() string {
	if root := strings.TrimSpace(os.Getenv("GITHUB_MCP_SHARED_ROOT")); root != "" {
		return root
	}
	return "/shared"
}

const githubSharedMaxBytes = 2 * 1024 * 1024

func resolveSharedPath(sharedPath string) (string, error) {
	if strings.TrimSpace(sharedPath) == "" {
		return "", fmt.Errorf("shared_path is required")
	}
	if filepath.IsAbs(sharedPath) {
		return "", fmt.Errorf("shared_path must be relative to the shared directory")
	}
	root := filepath.Clean(githubSharedRoot())
	rootEval, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("failed to resolve shared root: %w", err)
	}
	cleaned := filepath.Clean(sharedPath)
	if cleaned == "." || cleaned == "" {
		return "", fmt.Errorf("shared_path must point to a file inside the shared directory")
	}
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("shared_path must stay within the shared directory")
	}
	joined := filepath.Join(rootEval, cleaned)
	resolved, err := filepath.EvalSymlinks(joined)
	if err != nil {
		return "", fmt.Errorf("failed to resolve shared_path: %w", err)
	}
	rel, err := filepath.Rel(rootEval, resolved)
	if err != nil {
		return "", fmt.Errorf("failed to verify shared_path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("shared_path must stay within the shared directory")
	}
	return resolved, nil
}

func loadSharedTextFile(sharedPath string, maxBytes int) (string, int, error) {
	resolved, err := resolveSharedPath(sharedPath)
	if err != nil {
		return "", 0, err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", 0, fmt.Errorf("failed to stat shared file: %w", err)
	}
	if info.IsDir() {
		return "", 0, fmt.Errorf("shared_path points to a directory, not a file")
	}
	if maxBytes > 0 && info.Size() > int64(maxBytes) {
		return "", 0, fmt.Errorf("shared file is too large: %d bytes exceeds limit of %d bytes", info.Size(), maxBytes)
	}
	contentBytes, err := os.ReadFile(resolved)
	if err != nil {
		return "", 0, fmt.Errorf("failed to read shared file: %w", err)
	}
	if !utf8.Valid(contentBytes) {
		return "", 0, fmt.Errorf("shared file is not valid UTF-8 text")
	}
	return string(contentBytes), len(contentBytes), nil
}

type sharedFileSpec struct {
	RepoPath   string
	SharedPath string
	Content    string
	SizeBytes  int
}

type SharedPushFilesResponse struct {
	CommitSHA    string   `json:"commit_sha"`
	Branch       string   `json:"branch"`
	ChangedPaths []string `json:"changed_paths"`
	FileCount    int      `json:"file_count"`
}

func parseSharedFileSpecs(args map[string]any) ([]sharedFileSpec, error) {
	filesObj, ok := args["files"].([]any)
	if !ok {
		return nil, fmt.Errorf("files parameter must be an array of objects with path and shared_path")
	}
	files := make([]sharedFileSpec, 0, len(filesObj))
	for _, file := range filesObj {
		fileMap, ok := file.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("each file must be an object with path and shared_path")
		}
		repoPath, ok := fileMap["path"].(string)
		if !ok || strings.TrimSpace(repoPath) == "" {
			return nil, fmt.Errorf("each file must have a path")
		}
		sharedPath, ok := fileMap["shared_path"].(string)
		if !ok || strings.TrimSpace(sharedPath) == "" {
			return nil, fmt.Errorf("each file must have a shared_path")
		}
		content, sizeBytes, err := loadSharedTextFile(sharedPath, githubSharedMaxBytes)
		if err != nil {
			return nil, err
		}
		files = append(files, sharedFileSpec{
			RepoPath:   repoPath,
			SharedPath: sharedPath,
			Content:    content,
			SizeBytes:  sizeBytes,
		})
	}
	return files, nil
}

func prepareBranchBaseCommit(ctx context.Context, client *github.Client, owner, repo, branch string) (*github.Reference, *github.Commit, error) {
	var repositoryIsEmpty bool
	var branchNotFound bool
	ref, resp, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
	if err != nil {
		ghErr, isGhErr := err.(*github.ErrorResponse)
		if isGhErr {
			if ghErr.Response.StatusCode == http.StatusConflict && ghErr.Message == "Git Repository is empty." {
				repositoryIsEmpty = true
			} else if ghErr.Response.StatusCode == http.StatusNotFound {
				branchNotFound = true
			}
		}
		if !repositoryIsEmpty && !branchNotFound {
			return nil, nil, fmt.Errorf("failed to get branch reference: %w", err)
		}
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	var baseCommit *github.Commit
	if !repositoryIsEmpty {
		if branchNotFound {
			ref, err = createReferenceFromDefaultBranch(ctx, client, owner, repo, branch)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create branch from default: %w", err)
			}
		}
		baseCommit, resp, err = client.Git.GetCommit(ctx, owner, repo, *ref.Object.SHA)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get base commit: %w", err)
		}
		if resp != nil && resp.Body != nil {
			defer func() { _ = resp.Body.Close() }()
		}
	} else {
		var base *github.Commit
		ref, base, err = initializeRepository(ctx, client, owner, repo)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to initialize repository: %w", err)
		}
		defaultBranch := strings.TrimPrefix(*ref.Ref, "refs/heads/")
		if branch != defaultBranch {
			ref, err = createReferenceFromDefaultBranch(ctx, client, owner, repo, branch)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to create branch from default: %w", err)
			}
		}
		baseCommit = base
	}
	return ref, baseCommit, nil
}

func pushTreeEntries(ctx context.Context, client *github.Client, owner, repo, branch, message string, entries []*github.TreeEntry, changedPaths []string) (*mcp.CallToolResult, any, error) {
	ref, baseCommit, err := prepareBranchBaseCommit(ctx, client, owner, repo, branch)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}

	newTree, resp, err := client.Git.CreateTree(ctx, owner, repo, *baseCommit.Tree.SHA, entries)
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to create tree",
			resp,
			err,
		), nil, nil
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	commit := github.Commit{
		Message: github.Ptr(message),
		Tree:    newTree,
		Parents: []*github.Commit{{SHA: baseCommit.SHA}},
	}
	newCommit, resp, err := client.Git.CreateCommit(ctx, owner, repo, commit, nil)
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to create commit",
			resp,
			err,
		), nil, nil
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	_, resp, err = client.Git.UpdateRef(ctx, owner, repo, *ref.Ref, github.UpdateRef{
		SHA:   *newCommit.SHA,
		Force: github.Ptr(false),
	})
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to update reference",
			resp,
			err,
		), nil, nil
	}
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}

	return MarshalledTextResult(SharedPushFilesResponse{
		CommitSHA:    newCommit.GetSHA(),
		Branch:       branch,
		ChangedPaths: changedPaths,
		FileCount:    len(changedPaths),
	}), nil, nil
}

func firstTextResult(result *mcp.CallToolResult) *mcp.TextContent {
	if result == nil {
		return nil
	}
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			return text
		}
	}
	return nil
}

type createOrUpdateFilePreview struct {
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	Path           string `json:"path"`
	Branch         string `json:"branch"`
	Action         string `json:"action"`
	RequiresSHA    bool   `json:"requires_sha"`
	CurrentSHA     string `json:"current_sha,omitempty"`
	DryRun         bool   `json:"dry_run"`
	SecretsChecked bool   `json:"secrets_checked"`
}

func preflightCreateOrUpdateFile(ctx context.Context, client *github.Client, owner, repo, path, branch, sha string) (*createOrUpdateFilePreview, *mcp.CallToolResult, error) {
	path = strings.TrimPrefix(path, "/")
	getOpts := &github.RepositoryContentGetOptions{Ref: branch}

	preview := &createOrUpdateFilePreview{
		Owner:          owner,
		Repo:           repo,
		Path:           path,
		Branch:         branch,
		DryRun:         true,
		SecretsChecked: true,
	}

	existingFile, dirContent, respCheck, getErr := client.Repositories.GetContents(ctx, owner, repo, path, getOpts)
	if respCheck != nil {
		defer func() { _ = respCheck.Body.Close() }()
	}

	switch {
	case getErr != nil:
		if respCheck == nil || respCheck.StatusCode != http.StatusNotFound {
			return nil, ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to check if file exists", respCheck, getErr), nil
		}
		preview.Action = "create"
		preview.RequiresSHA = false
		return preview, nil, nil
	case dirContent != nil:
		return nil, utils.NewToolResultError(fmt.Sprintf(
			"Path %s is a directory, not a file. This tool only works with files.",
			path,
		)), nil
	case existingFile != nil:
		currentSHA := existingFile.GetSHA()
		preview.Action = "update"
		preview.RequiresSHA = true
		preview.CurrentSHA = currentSHA
		if sha == "" {
			return nil, utils.NewToolResultError(fmt.Sprintf(
				"File already exists at %s. You must provide the current file's SHA when updating. Use git rev-parse %s:%s to get the blob SHA, then retry with the sha parameter.",
				path, branch, path,
			)), nil
		}
		if currentSHA != sha {
			return nil, utils.NewToolResultError(fmt.Sprintf(
				"SHA mismatch: provided SHA %s is stale. Current file SHA is %s. Pull the latest changes and use git rev-parse %s:%s to get the current SHA.",
				sha, currentSHA, branch, path,
			)), nil
		}
		return preview, nil, nil
	}

	return nil, utils.NewToolResultError("failed to prepare file update"), nil
}

func createOrUpdateFileResult(ctx context.Context, deps ToolDependencies, client *github.Client, owner, repo, path, content, message, branch, sha string) (*mcp.CallToolResult, any, error) {
	if isSecretLikeRepoPath(path) {
		return newBlockedFileToolResult(path, "secret_like_file"), nil, nil
	}
	if reason := detectSecretLikeContent(path, content); reason != "" {
		return newBlockedFileToolResult(path, reason), nil, nil
	}

	// json.Marshal encodes byte arrays with base64, which is required for the API.
	contentBytes := []byte(content)

	// Create the file options
	opts := &github.RepositoryContentFileOptions{
		Message: github.Ptr(message),
		Content: contentBytes,
		Branch:  github.Ptr(branch),
	}
	if sha != "" {
		opts.SHA = github.Ptr(sha)
	}

	path = strings.TrimPrefix(path, "/")
	if _, previewResult, previewErr := preflightCreateOrUpdateFile(ctx, client, owner, repo, path, branch, sha); previewResult != nil || previewErr != nil {
		return previewResult, nil, previewErr
	}

	fileContent, resp, err := client.Repositories.CreateFile(ctx, owner, repo, path, opts)
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx,
			"failed to create/update file",
			resp,
			err,
		), nil, nil
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to read response body: %w", err)
		}
		return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to create/update file", resp, body), nil, nil
	}

	minimalResponse := convertToMinimalFileContentResponse(fileContent)
	return MarshalledTextResult(minimalResponse), nil, nil
}

func decodeGitHubFileContent(file *github.RepositoryContent) (string, error) {
	if file == nil || file.Content == nil {
		return "", fmt.Errorf("GitHub did not return complete file content")
	}
	encodedContent, err := file.GetContent()
	if err != nil {
		return "", fmt.Errorf("failed to read current GitHub blob content: %w", err)
	}
	return encodedContent, nil
}

func boundedRedactedPreview(content string) string {
	redacted, _ := redactSecretLikeContent(content)
	if len(redacted) > 4000 {
		return redacted[:4000] + "\n...[truncated]"
	}
	return redacted
}

func optionalIntArgument(args map[string]any, key string) (int, error) {
	value, ok := args[key]
	if !ok || value == nil {
		return 0, nil
	}
	switch typed := value.(type) {
	case int:
		return typed, nil
	case int64:
		return int(typed), nil
	case float64:
		if typed != float64(int(typed)) {
			return 0, fmt.Errorf("parameter %s must be an integer", key)
		}
		return int(typed), nil
	default:
		return 0, fmt.Errorf("parameter %s is not of type int, is %T", key, value)
	}
}

func getCurrentGitHubFile(ctx context.Context, client *github.Client, owner, repo, path, branch string) (*github.RepositoryContent, string, *github.Response, error) {
	file, dirs, resp, err := client.Repositories.GetContents(ctx, owner, repo, strings.TrimPrefix(path, "/"), &github.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		return nil, "", resp, err
	}
	if dirs != nil {
		return nil, "", resp, fmt.Errorf("path %s is a directory, not a file", path)
	}
	content, err := decodeGitHubFileContent(file)
	if err != nil {
		return file, "", resp, err
	}
	return file, content, resp, nil
}

func createOrUpdateFileOperation(ctx context.Context, deps ToolDependencies, client *github.Client, owner, repo, path, message, branch, expectedSHA string, dryRun bool, patch filePatchInput) (*mcp.CallToolResult, any, error) {
	path = strings.TrimPrefix(path, "/")
	file, before, resp, err := getCurrentGitHubFile(ctx, client, owner, repo, path, branch)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	exists := err == nil && file != nil
	if err != nil && (resp == nil || resp.StatusCode != http.StatusNotFound) {
		return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to fetch current file before mutation", resp, err), nil, nil
	}
	if !exists && patch.Operation != "replace" {
		return utils.NewToolResultError("patch operations require an existing file"), nil, nil
	}
	if exists {
		if expectedSHA == "" {
			return utils.NewToolResultError(fmt.Sprintf("File already exists at %s. You must provide expected_blob_sha when modifying it.", path)), nil, nil
		}
		if expectedSHA != file.GetSHA() {
			return utils.NewToolResultError(fmt.Sprintf("SHA mismatch: provided SHA %s is stale. Current file SHA is %s.", expectedSHA, file.GetSHA())), nil, nil
		}
	}
	if patch.Operation == "replace" && !exists {
		before = ""
	}
	after, patchPreview, err := applyFilePatch(before, patch)
	if err != nil {
		return utils.NewToolResultError(err.Error()), nil, nil
	}
	if reason := detectSecretLikeContent(path, after); reason != "" {
		return newBlockedFileToolResult(path, reason), nil, nil
	}
	if !unrelatedFileContentPreserved(before, after, patch) {
		return utils.NewToolResultError("resulting file did not preserve unrelated content"), nil, nil
	}
	redactedBefore, _ := redactSecretLikeContent(before)
	redactedAfter, _ := redactSecretLikeContent(after)
	bounded := func(value string) string {
		if len(value) > 4000 {
			return value[:4000] + "\n...[truncated]"
		}
		return value
	}
	preview := map[string]any{
		"ok": true, "dry_run": dryRun, "operation": patchPreview.Operation, "owner": owner, "repo": repo, "path": path, "branch": branch,
		"action": map[bool]string{true: "update", false: "create"}[exists], "expected_blob_sha": expectedSHA, "matched_occurrences": patchPreview.MatchedOccurrences,
		"changed_line_count": patchPreview.ChangedLineCount, "hunks": patchPreview.Hunks, "before_preview": bounded(redactedBefore), "after_preview": bounded(redactedAfter), "redacted": redactedBefore != before || redactedAfter != after,
	}
	if dryRun {
		return MarshalledTextResult(preview), nil, nil
	}
	options := &github.RepositoryContentFileOptions{Message: github.Ptr(message), Content: []byte(after), Branch: github.Ptr(branch)}
	if exists {
		options.SHA = github.Ptr(expectedSHA)
	}
	committed, writeResp, err := client.Repositories.CreateFile(ctx, owner, repo, path, options)
	if writeResp != nil && writeResp.Body != nil {
		defer func() { _ = writeResp.Body.Close() }()
	}
	if err != nil {
		return ghErrors.NewGitHubAPIErrorResponse(ctx, "failed to create/update file", writeResp, err), nil, nil
	}
	verified, committedContent, verifyResp, err := getCurrentGitHubFile(ctx, client, owner, repo, path, branch)
	if verifyResp != nil && verifyResp.Body != nil {
		defer func() { _ = verifyResp.Body.Close() }()
	}
	if err != nil {
		return utils.NewToolResultError(fmt.Sprintf("post-commit verification failed: %v", err)), nil, nil
	}
	if committedContent != after {
		return utils.NewToolResultError("post-commit verification failed: committed content does not match the requested change"), nil, nil
	}
	if exists && verified.GetSHA() == expectedSHA {
		return utils.NewToolResultError("post-commit verification failed: blob SHA did not change"), nil, nil
	}
	return MarshalledTextResult(map[string]any{"ok": true, "applied": true, "verification": map[string]any{"ok": true, "exact_content": true, "unrelated_content_preserved": true}, "content": convertToMinimalFileContentResponse(committed)}), nil, nil
}

func CreateOrUpdateFileFromSharedPath(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name: "create_or_update_file_from_shared_path",
			Description: t("TOOL_CREATE_OR_UPDATE_FILE_FROM_SHARED_PATH_DESCRIPTION", `Create or update a single file in a GitHub repository using a UTF-8 text file that already exists on the MCP host under the shared directory.

Use this when the file content is already available on the server and is too large or too awkward to send inline through the MCP tool call.

Only files inside the configured shared directory are allowed. SHA MUST be provided for existing file updates.`),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_CREATE_OR_UPDATE_FILE_FROM_SHARED_PATH_USER_TITLE", "Create or update file from shared path"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner (username or organization)",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"path": {
						Type:        "string",
						Description: "Repository path where to create/update the file",
					},
					"shared_path": {
						Type:        "string",
						Description: "Relative path under the shared directory mounted into the MCP container",
					},
					"message": {
						Type:        "string",
						Description: "Commit message",
					},
					"branch": {
						Type:        "string",
						Description: "Branch to create/update the file in",
					},
					"sha": {
						Type:        "string",
						Description: "The blob SHA of the file being replaced. Required if the file already exists.",
					},
				},
				Required: []string{"owner", "repo", "path", "shared_path", "message", "branch"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sharedPath, err := RequiredParam[string](args, "shared_path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			message, err := RequiredParam[string](args, "message")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := RequiredParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sha, err := OptionalParam[string](args, "sha")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			content, sizeBytes, err := loadSharedTextFile(sharedPath, githubSharedMaxBytes)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			result, _, err := createOrUpdateFileResult(ctx, deps, client, owner, repo, path, content, message, branch, sha)
			if err != nil || result == nil || result.IsError {
				return result, nil, err
			}

			textResult := firstTextResult(result)
			if textResult == nil {
				return result, nil, nil
			}

			var structured map[string]any
			if unmarshalErr := json.Unmarshal([]byte(textResult.Text), &structured); unmarshalErr == nil {
				structured["source"] = map[string]any{
					"type":        "shared_path",
					"shared_path": sharedPath,
					"size_bytes":  sizeBytes,
				}
				result.StructuredContent = structured
			}

			return result, nil, nil
		},
	)
}

func PushFilesFromSharedPaths(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "push_files_from_shared_paths",
			Description: t("TOOL_PUSH_FILES_FROM_SHARED_PATHS_DESCRIPTION", "Push multiple UTF-8 text files from the shared directory to a GitHub repository in a single commit."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_PUSH_FILES_FROM_SHARED_PATHS_USER_TITLE", "Push files from shared paths"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"branch": {
						Type:        "string",
						Description: "Branch to push to",
					},
					"message": {
						Type:        "string",
						Description: "Commit message",
					},
					"files": {
						Type:        "array",
						Description: "Array of file objects to push, each object with path (repository path) and shared_path (relative path under the shared directory)",
						Items: &jsonschema.Schema{
							Type:                 "object",
							AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
							Properties: map[string]*jsonschema.Schema{
								"path": {
									Type:        "string",
									Description: "Repository path for the file",
								},
								"shared_path": {
									Type:        "string",
									Description: "Relative path under the shared directory mounted into the MCP container",
								},
							},
							Required: []string{"path", "shared_path"},
						},
					},
				},
				Required: []string{"owner", "repo", "branch", "files", "message"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := RequiredParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			message, err := RequiredParam[string](args, "message")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			files, err := parseSharedFileSpecs(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			entries := make([]*github.TreeEntry, 0, len(files))
			changedPaths := make([]string, 0, len(files))
			for _, file := range files {
				entries = append(entries, &github.TreeEntry{
					Path:    github.Ptr(file.RepoPath),
					Mode:    github.Ptr("100644"),
					Type:    github.Ptr("blob"),
					Content: github.Ptr(file.Content),
				})
				changedPaths = append(changedPaths, file.RepoPath)
			}

			return pushTreeEntries(ctx, client, owner, repo, branch, message, entries, changedPaths)
		},
	)
}

// CreateRepository creates a tool to create a new GitHub repository.
func CreateRepository(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "create_repository",
			Description: t("TOOL_CREATE_REPOSITORY_DESCRIPTION", "Create a new GitHub repository in your account or specified organization"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_CREATE_REPOSITORY_USER_TITLE", "Create repository"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"name": {
						Type:        "string",
						Description: "Repository name",
					},
					"description": {
						Type:        "string",
						Description: "Repository description",
					},
					"organization": {
						Type:        "string",
						Description: "Organization to create the repository in (omit to create in your personal account)",
					},
					"private": {
						Type:        "boolean",
						Description: "Whether the repository should be private. Defaults to true (private) when omitted.",
						Default:     json.RawMessage("true"),
					},
					"autoInit": {
						Type:        "boolean",
						Description: "Initialize with README",
					},
				},
				Required: []string{"name"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			name, err := RequiredParam[string](args, "name")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			description, err := OptionalParam[string](args, "description")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			organization, err := OptionalParam[string](args, "organization")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			private, err := OptionalBoolParamWithDefault(args, "private", true)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			autoInit, err := OptionalParam[bool](args, "autoInit")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			repo := &github.Repository{
				Name:        github.Ptr(name),
				Description: github.Ptr(description),
				Private:     github.Ptr(private),
				AutoInit:    github.Ptr(autoInit),
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}
			createdRepo, resp, err := client.Repositories.Create(ctx, organization, repo)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create repository",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusCreated {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to create repository", resp, body), nil, nil
			}

			// Return minimal response with just essential information
			minimalResponse := MinimalResponse{
				ID:  fmt.Sprintf("%d", createdRepo.GetID()),
				URL: createdRepo.GetHTMLURL(),
			}

			r, err := json.Marshal(minimalResponse)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// FetchRepoIsPrivate returns whether a repository is private. It is a thin
// wrapper around the GitHub Repositories.Get endpoint provided as a shared
// helper for IFC label computation across tools.
func FetchRepoIsPrivate(ctx context.Context, client *github.Client, owner, repo string) (bool, error) {
	r, resp, err := client.Repositories.Get(ctx, owner, repo)
	if resp != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return false, err
	}
	return r.GetPrivate(), nil
}

func isSecretLikeRepoPath(path string) bool {
	lower := strings.ToLower(strings.TrimSpace(path))
	base := filepath.Base(lower)
	switch {
	case lower == ".env",
		strings.HasPrefix(base, ".env."),
		base == "id_rsa",
		base == "id_ed25519",
		strings.HasSuffix(base, ".pem"),
		strings.HasSuffix(base, ".key"),
		base == "credentials.json",
		strings.Contains(base, "service-account") && strings.HasSuffix(base, ".json"),
		strings.HasSuffix(base, ".sql"),
		strings.HasSuffix(base, ".dump"),
		strings.HasSuffix(base, ".bak"),
		strings.HasSuffix(base, ".sqlite"),
		strings.HasSuffix(base, ".sqlite3"):
		return true
	default:
		return false
	}
}

var secretLikeContentPatterns = []struct {
	reason string
	regex  *regexp.Regexp
}{
	{reason: "private_key_material", regex: regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
	{reason: "github_token", regex: regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`)},
	{reason: "aws_access_key", regex: regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
	{reason: "openai_api_key", regex: regexp.MustCompile(`\bsk-(?:live|proj|ant)-[A-Za-z0-9]{16,}\b`)},
	{reason: "slack_token", regex: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`)},
}

var secretRedactionPatterns = []struct {
	regex       *regexp.Regexp
	replacement string
}{
	{
		regex:       regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`),
		replacement: "***REDACTED PRIVATE KEY***",
	},
	{
		regex:       regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
		replacement: "***REDACTED GITHUB TOKEN***",
	},
	{
		regex:       regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
		replacement: "***REDACTED AWS KEY***",
	},
	{
		regex:       regexp.MustCompile(`\bsk-(?:live|proj|ant)-[A-Za-z0-9]{16,}\b`),
		replacement: "***REDACTED API KEY***",
	},
	{
		regex:       regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),
		replacement: "***REDACTED SLACK TOKEN***",
	},
	{
		regex:       regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._\-+/=]{12,}\b`),
		replacement: "Bearer ***REDACTED***",
	},
	{
		regex:       regexp.MustCompile(`(?im)^([A-Z][A-Z0-9_]*(?:TOKEN|SECRET|PASSWORD|API_KEY|ACCESS_KEY|PRIVATE_KEY)[A-Z0-9_]*\s*[:=]\s*).+$`),
		replacement: "${1}***REDACTED***",
	},
	{
		regex:       regexp.MustCompile(`(?i)\b((?:access[_-]?token|refresh[_-]?token|api[_-]?key|client[_-]?secret|webhook[_-]?secret|password|private[_-]?key)\b\s*[:=]\s*)(?:"[^"]*"|'[^']*'|[^'",\s]+)`),
		replacement: "${1}***REDACTED***",
	},
}

func detectSecretLikeContent(path, content string) string {
	if isSecretLikeRepoPath(path) {
		return "secret_like_file"
	}
	for _, pattern := range secretLikeContentPatterns {
		if pattern.regex.MatchString(content) {
			return pattern.reason
		}
	}
	return ""
}

func redactSecretLikeContent(content string) (string, bool) {
	redacted := content
	changed := false
	for _, pattern := range secretRedactionPatterns {
		next := pattern.regex.ReplaceAllString(redacted, pattern.replacement)
		if next != redacted {
			changed = true
			redacted = next
		}
	}
	return redacted, changed
}

func detectFileMimeType(path, fallback string) string {
	if strings.EqualFold(filepath.Base(path), "Dockerfile") {
		return "text/x-dockerfile"
	}
	if ext := filepath.Ext(path); ext != "" {
		if byExt := mime.TypeByExtension(ext); byExt != "" {
			return byExt
		}
	}
	if fallback != "" {
		return fallback
	}
	return "text/plain; charset=utf-8"
}

func detectFenceLanguage(path string) string {
	if strings.EqualFold(filepath.Base(path), "Dockerfile") {
		return "dockerfile"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".py":
		return "py"
	case ".ts":
		return "ts"
	case ".tsx":
		return "tsx"
	case ".js":
		return "js"
	case ".jsx":
		return "jsx"
	case ".json":
		return "json"
	case ".md":
		return "md"
	case ".yml", ".yaml":
		return "yaml"
	case ".toml":
		return "toml"
	case ".css":
		return "css"
	case ".html":
		return "html"
	case ".sh":
		return "sh"
	default:
		return "text"
	}
}

func trimTextByBytes(content string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(content) <= maxBytes {
		return content, false
	}
	return content[:maxBytes], true
}

func optionalIntArg(args map[string]any, key string) int {
	value, ok := args[key]
	if !ok || value == nil {
		return 0
	}
	switch v := value.(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func sliceTextByLines(content string, startLine, endLine int) (string, int, int, bool) {
	lines := strings.Split(content, "\n")
	if startLine < 1 {
		startLine = 1
	}
	if endLine < startLine || endLine == 0 {
		endLine = len(lines)
	}
	if startLine > len(lines) {
		return "", startLine, startLine, true
	}
	if endLine > len(lines) {
		endLine = len(lines)
	}
	return strings.Join(lines[startLine-1:endLine], "\n"), startLine, endLine, endLine < len(lines)
}

func newInlineFileToolResult(owner, repo, filePath, ref, sha, mimeType, content string, startLine, endLine int, truncated bool) *mcp.CallToolResult {
	structured := map[string]any{
		"owner":      owner,
		"repo":       repo,
		"path":       filePath,
		"ref":        ref,
		"sha":        sha,
		"mime_type":  mimeType,
		"encoding":   "utf-8",
		"truncated":  truncated,
		"start_line": startLine,
		"end_line":   endLine,
		"content":    content,
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: fmt.Sprintf("File: %s\nRepo: %s/%s\nRef: %s\nSHA: %s\n\n```%s\n%s\n```",
					filePath,
					owner,
					repo,
					ref,
					sha,
					detectFenceLanguage(filePath),
					content,
				),
			},
		},
		StructuredContent: structured,
	}
}

func newBlockedFileToolResult(path, reason string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{Text: fmt.Sprintf(`{"blocked":true,"reason":"%s","path":"%s"}`, reason, path)},
		},
		StructuredContent: map[string]any{
			"blocked": true,
			"reason":  reason,
			"path":    path,
		},
		IsError: true,
	}
}

func newBinaryMetadataToolResult(owner, repo, filePath, ref, sha, mimeType string, size int) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{
			&mcp.TextContent{
				Text: fmt.Sprintf("Binary file not inlined.\nPath: %s\nRepo: %s/%s\nRef: %s\nSHA: %s\nMIME: %s\nSize: %d",
					filePath, owner, repo, ref, sha, mimeType, size),
			},
		},
		StructuredContent: map[string]any{
			"owner":      owner,
			"repo":       repo,
			"path":       filePath,
			"ref":        ref,
			"sha":        sha,
			"mime_type":  mimeType,
			"encoding":   "binary",
			"size":       size,
			"truncated":  true,
			"start_line": 0,
			"end_line":   0,
			"content":    "",
			"blocked":    true,
			"reason":     "binary_file",
		},
	}
}

// GetFileContents creates a tool to get the contents of a file or directory from a GitHub repository.
func GetFileContents(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "get_file_contents",
			Description: t("TOOL_GET_FILE_CONTENTS_DESCRIPTION", "Get the contents of a file or directory from a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_GET_FILE_CONTENTS_USER_TITLE", "Get file or directory contents"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner (username or organization)",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"path": {
						Type:        "string",
						Description: "Path to file/directory",
						Default:     json.RawMessage(`"/"`),
					},
					"ref": {
						Type:        "string",
						Description: "Accepts optional git refs such as `refs/tags/{tag}`, `refs/heads/{branch}` or `refs/pull/{pr_number}/head`",
					},
					"sha": {
						Type:        "string",
						Description: "Accepts optional commit SHA. If specified, it will be used instead of ref",
					},
					"start_line": {
						Type:        "integer",
						Description: "Optional start line for text file reads",
					},
					"end_line": {
						Type:        "integer",
						Description: "Optional end line for text file reads",
					},
					"max_bytes": {
						Type:        "integer",
						Description: "Optional maximum UTF-8 bytes to return for text file reads",
					},
				},
				Required: []string{"owner", "repo"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			path, err := OptionalParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path = strings.TrimPrefix(path, "/")
			if isSecretLikeRepoPath(path) {
				return newBlockedFileToolResult(path, "secret_like_file"), nil, nil
			}

			ref, err := OptionalParam[string](args, "ref")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			originalRef := ref

			sha, err := OptionalParam[string](args, "sha")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return utils.NewToolResultError("failed to get GitHub client"), nil, nil
			}

			// attachIFC adds the IFC label to a successful tool result when
			// IFC labels are enabled. The visibility lookup is performed
			// lazily on first use and cached because GetFileContents has
			// many possible return paths and would otherwise re-fetch on
			// each. If the visibility lookup fails we skip the label rather
			// than misclassify the result; the failure is not cached so a
			// later return path can retry.
			attachIFC := newRepoVisibilityIFCLabeler(ctx, deps, client, owner, repo, ifc.LabelGetFileContents)

			rawOpts, fallbackUsed, err := resolveGitReference(ctx, client, owner, repo, ref, sha)
			if err != nil {
				return utils.NewToolResultError(fmt.Sprintf("failed to resolve git reference: %s", err)), nil, nil
			}
			startLine := optionalIntArg(args, "start_line")
			endLine := optionalIntArg(args, "end_line")
			maxBytes := optionalIntArg(args, "max_bytes")

			if rawOpts.SHA != "" {
				ref = rawOpts.SHA
			}

			var fileSHA string
			opts := &github.RepositoryContentGetOptions{Ref: ref}

			// Always call GitHub Contents API first to get metadata including SHA and determine if it's a file or directory
			type contentsResult struct {
				fileContent *github.RepositoryContent
				dirContent  []*github.RepositoryContent
			}
			contents, respContents, err := retryGitHubCall(ctx, deps, "get_file_contents:GetContents", func(callCtx context.Context) (contentsResult, *github.Response, error) {
				fileContent, dirContent, respContents, err := client.Repositories.GetContents(callCtx, owner, repo, path, opts)
				return contentsResult{fileContent: fileContent, dirContent: dirContent}, respContents, err
			})
			if respContents != nil {
				defer func() { _ = respContents.Body.Close() }()
			}
			fileContent := contents.fileContent
			dirContent := contents.dirContent

			// The path does not point to a file or directory.
			// Instead let's try to find it in the Git Tree by matching the end of the path.
			if err != nil || (fileContent == nil && dirContent == nil) {
				res, data, err := matchFiles(ctx, client, owner, repo, ref, path, rawOpts, 0)
				return attachIFC(res), data, err
			}

			if fileContent != nil && fileContent.SHA != nil {
				fileSHA = *fileContent.SHA
				fileSize := fileContent.GetSize()

				// main branch ref passed in ref parameter but it doesn't exist - default branch was used
				var successNote string
				if fallbackUsed {
					successNote = fmt.Sprintf(" Note: the provided ref '%s' does not exist, default branch '%s' was used instead.", originalRef, rawOpts.Ref)
				}
				if fileSize == 0 {
					result := newInlineFileToolResult(owner, repo, path, rawOpts.Ref, fileSHA, "text/plain", "", 1, 1, false)
					if successNote != "" {
						text := result.Content[0].(*mcp.TextContent)
						text.Text = strings.Replace(text.Text, "\n\n```", fmt.Sprintf("%s\n\n```", successNote), 1)
					}
					return attachIFC(result), nil, nil
				}

				var contentBytes []byte
				var contentType string
				const maxContentSize = 1024 * 1024 // 1MB
				useRaw := fileSize >= maxContentSize
				if !useRaw {
					content, decodeErr := fileContent.GetContent()
					if decodeErr != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to decode file content: %s", decodeErr)), nil, nil
					}
					contentBytes = []byte(content)
					contentType = detectFileMimeType(path, http.DetectContentType(contentBytes))
				} else {
					rawClient, rawErr := deps.GetRawClient(ctx)
					if rawErr != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to get GitHub raw content client: %s", rawErr)), nil, nil
					}
					rawResp, rawErr := retryGitHubHTTPCall(ctx, deps, "get_file_contents:GetRawContent", func(callCtx context.Context) (*http.Response, error) {
						return rawClient.GetRawContent(callCtx, owner, repo, path, rawOpts)
					})
					if rawErr != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to fetch raw file content: %s", rawErr)), nil, nil
					}
					defer func() { _ = rawResp.Body.Close() }()
					if rawResp.StatusCode != http.StatusOK {
						body, readErr := io.ReadAll(rawResp.Body)
						if readErr == nil {
							_ = body
						}
						return attachIFC(newBinaryMetadataToolResult(owner, repo, path, rawOpts.Ref, fileSHA, detectFileMimeType(path, "application/octet-stream"), fileSize)), nil, nil
					}
					contentBytes, err = io.ReadAll(rawResp.Body)
					if err != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to read file content: %s", err)), nil, nil
					}
					contentType = detectFileMimeType(path, rawResp.Header.Get("Content-Type"))
				}
				isTextContent := strings.HasPrefix(contentType, "text/") ||
					contentType == "application/json" ||
					contentType == "application/xml" ||
					strings.HasSuffix(contentType, "+json") ||
					strings.HasSuffix(contentType, "+xml")
				if !isTextContent {
					msg := fmt.Sprintf("Binary file not inlined (SHA: %s)%s", fileSHA, successNote)
					return attachIFC(newBinaryMetadataToolResult(owner, repo, path, rawOpts.Ref, fileSHA, contentType, fileSize)), map[string]any{"message": msg}, nil
				}

				content := string(contentBytes)
				if startLine > 0 || endLine > 0 {
					var truncated bool
					content, startLine, endLine, truncated = sliceTextByLines(content, startLine, endLine)
					content, redacted := redactSecretLikeContent(content)
					result := newInlineFileToolResult(owner, repo, path, rawOpts.Ref, fileSHA, contentType, content, startLine, endLine, truncated)
					if redacted {
						if structured, ok := result.StructuredContent.(map[string]any); ok {
							structured["redacted"] = true
						}
					}
					if successNote != "" {
						text := result.Content[0].(*mcp.TextContent)
						text.Text = strings.Replace(text.Text, "\n\n```", fmt.Sprintf("%s\n\n```", successNote), 1)
					}
					return attachIFC(result), nil, nil
				}

				if maxBytes == 0 {
					maxBytes = 128 * 1024
				}
				content, truncated := trimTextByBytes(content, maxBytes)
				content, redacted := redactSecretLikeContent(content)
				result := newInlineFileToolResult(owner, repo, path, rawOpts.Ref, fileSHA, contentType, content, 1, strings.Count(content, "\n")+1, truncated)
				if redacted {
					if structured, ok := result.StructuredContent.(map[string]any); ok {
						structured["redacted"] = true
					}
				}
				if successNote != "" {
					text := result.Content[0].(*mcp.TextContent)
					text.Text = strings.Replace(text.Text, "\n\n```", fmt.Sprintf("%s\n\n```", successNote), 1)
				}
				return attachIFC(result), nil, nil
			} else if dirContent != nil {
				// file content or file SHA is nil which means it's a directory
				r, err := json.Marshal(dirContent)
				if err != nil {
					return utils.NewToolResultError("failed to marshal response"), nil, nil
				}
				return attachIFC(utils.NewToolResultText(string(r))), nil, nil
			}

			return utils.NewToolResultError("failed to get file contents"), nil, nil
		},
	)
}

// ForkRepository creates a tool to fork a repository.
func ForkRepository(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "fork_repository",
			Description: t("TOOL_FORK_REPOSITORY_DESCRIPTION", "Fork a GitHub repository to your account or specified organization"),
			Icons:       octicons.Icons("repo-forked"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_FORK_REPOSITORY_USER_TITLE", "Fork repository"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"organization": {
						Type:        "string",
						Description: "Organization to fork to",
					},
				},
				Required: []string{"owner", "repo"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			org, err := OptionalParam[string](args, "organization")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			opts := &github.RepositoryCreateForkOptions{}
			if org != "" {
				opts.Organization = org
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}
			forkedRepo, resp, err := client.Repositories.CreateFork(ctx, owner, repo, opts)
			if err != nil {
				// Check if it's an acceptedError. An acceptedError indicates that the update is in progress,
				// and it's not a real error.
				if resp != nil && resp.StatusCode == http.StatusAccepted && isAcceptedError(err) {
					return utils.NewToolResultText("Fork is in progress"), nil, nil
				}
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to fork repository",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusAccepted {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to fork repository", resp, body), nil, nil
			}

			// Return minimal response with just essential information
			minimalResponse := MinimalResponse{
				ID:  fmt.Sprintf("%d", forkedRepo.GetID()),
				URL: forkedRepo.GetHTMLURL(),
			}

			r, err := json.Marshal(minimalResponse)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// DeleteFile creates a tool to delete a file in a GitHub repository.
// This tool uses a more roundabout way of deleting a file than just using the client.Repositories.DeleteFile.
// This is because REST file deletion endpoint (and client.Repositories.DeleteFile) don't add commit signing to the deletion commit,
// unlike how the endpoint backing the create_or_update_files tool does. This appears to be a quirk of the API.
// The approach implemented here gets automatic commit signing when used with either the github-actions user or as an app,
// both of which suit an LLM well.
func DeleteFile(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "delete_file",
			Description: t("TOOL_DELETE_FILE_DESCRIPTION", "Delete a file from a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:           t("TOOL_DELETE_FILE_USER_TITLE", "Delete file"),
				ReadOnlyHint:    false,
				DestructiveHint: github.Ptr(true),
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner (username or organization)",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"path": {
						Type:        "string",
						Description: "Path to the file to delete",
					},
					"message": {
						Type:        "string",
						Description: "Commit message",
					},
					"branch": {
						Type:        "string",
						Description: "Branch to delete the file from",
					},
				},
				Required: []string{"owner", "repo", "path", "message", "branch"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			message, err := RequiredParam[string](args, "message")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := RequiredParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			// Get the reference for the branch
			ref, resp, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get branch reference: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()

			// Get the commit object that the branch points to
			baseCommit, resp, err := client.Git.GetCommit(ctx, owner, repo, *ref.Object.SHA)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to get base commit",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get commit", resp, body), nil, nil
			}

			// Create a tree entry for the file deletion by setting SHA to nil
			treeEntries := []*github.TreeEntry{
				{
					Path: github.Ptr(path),
					Mode: github.Ptr("100644"), // Regular file mode
					Type: github.Ptr("blob"),
					SHA:  nil, // Setting SHA to nil deletes the file
				},
			}

			// Create a new tree with the deletion
			newTree, resp, err := client.Git.CreateTree(ctx, owner, repo, *baseCommit.Tree.SHA, treeEntries)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create tree",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusCreated {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to create tree", resp, body), nil, nil
			}

			// Create a new commit with the new tree
			commit := github.Commit{
				Message: github.Ptr(message),
				Tree:    newTree,
				Parents: []*github.Commit{{SHA: baseCommit.SHA}},
			}
			newCommit, resp, err := client.Git.CreateCommit(ctx, owner, repo, commit, nil)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create commit",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusCreated {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to create commit", resp, body), nil, nil
			}

			// Update the branch reference to point to the new commit
			ref.Object.SHA = newCommit.SHA
			_, resp, err = client.Git.UpdateRef(ctx, owner, repo, *ref.Ref, github.UpdateRef{
				SHA:   *newCommit.SHA,
				Force: github.Ptr(false),
			})
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to update reference",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to update reference", resp, body), nil, nil
			}

			// Create a response similar to what the DeleteFile API would return
			response := map[string]any{
				"commit":  newCommit,
				"content": nil,
			}

			r, err := json.Marshal(response)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// CreateBranch creates a tool to create a new branch.
func CreateBranch(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "create_branch",
			Description: t("TOOL_CREATE_BRANCH_DESCRIPTION", "Create a new branch in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_CREATE_BRANCH_USER_TITLE", "Create branch"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"branch": {
						Type:        "string",
						Description: "Name for new branch",
					},
					"from_branch": {
						Type:        "string",
						Description: "Source branch (defaults to repo default)",
					},
				},
				Required: []string{"owner", "repo", "branch"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := RequiredParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			fromBranch, err := OptionalParam[string](args, "from_branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			// Get the source branch SHA
			var ref *github.Reference

			if fromBranch == "" {
				// Get default branch if from_branch not specified
				repository, resp, err := client.Repositories.Get(ctx, owner, repo)
				if err != nil {
					return ghErrors.NewGitHubAPIErrorResponse(ctx,
						"failed to get repository",
						resp,
						err,
					), nil, nil
				}
				defer func() { _ = resp.Body.Close() }()

				fromBranch = *repository.DefaultBranch
			}

			// Get SHA of source branch
			ref, resp, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+fromBranch)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to get reference",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			// Create new branch
			newRef := github.CreateRef{
				Ref: "refs/heads/" + branch,
				SHA: *ref.Object.SHA,
			}

			createdRef, resp, err := client.Git.CreateRef(ctx, owner, repo, newRef)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create branch",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			r, err := json.Marshal(createdRef)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// PushFiles creates a tool to push multiple files in a single commit to a GitHub repository.
func PushFiles(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "push_files",
			Description: t("TOOL_PUSH_FILES_DESCRIPTION", "Push multiple files to a GitHub repository in a single commit"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_PUSH_FILES_USER_TITLE", "Push files to repository"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"branch": {
						Type:        "string",
						Description: "Branch to push to",
					},
					"files": {
						Type:        "array",
						Description: "Array of file objects to push, each object with path (string) and content (string)",
						Items: &jsonschema.Schema{
							Type:                 "object",
							AdditionalProperties: &jsonschema.Schema{Not: &jsonschema.Schema{}},
							Properties: map[string]*jsonschema.Schema{
								"path": {
									Type:        "string",
									Description: "path to the file",
								},
								"content": {
									Type:        "string",
									Description: "file content",
								},
								"operation":            {Type: "string", Enum: []any{"replace", "patch_text", "patch_range", "unified_diff"}},
								"expected_blob_sha":    {Type: "string"},
								"search":               {Type: "string"},
								"replace":              {Type: "string"},
								"expected_occurrences": {Type: "integer"},
								"start_line":           {Type: "integer"},
								"end_line":             {Type: "integer"},
								"replacement":          {Type: "string"},
								"patch":                {Type: "string"},
							},
							Required: []string{"path"},
						},
					},
					"message": {
						Type:        "string",
						Description: "Commit message",
					},
					"dry_run": {
						Type:        "boolean",
						Description: "Validate the request and report what would happen without writing to GitHub",
					},
				},
				Required: []string{"owner", "repo", "branch", "files", "message"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			branch, err := RequiredParam[string](args, "branch")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			message, err := RequiredParam[string](args, "message")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			dryRun, err := OptionalParam[bool](args, "dry_run")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			// Parse files parameter - this should be an array of objects with path and content
			filesObj, ok := args["files"].([]any)
			if !ok {
				return utils.NewToolResultError("files parameter must be an array of objects with path and content"), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			// Get the reference for the branch
			var repositoryIsEmpty bool
			var branchNotFound bool
			ref, resp, err := client.Git.GetRef(ctx, owner, repo, "refs/heads/"+branch)
			if err != nil {
				ghErr, isGhErr := err.(*github.ErrorResponse)
				if isGhErr {
					if ghErr.Response.StatusCode == http.StatusConflict && ghErr.Message == "Git Repository is empty." {
						repositoryIsEmpty = true
					} else if ghErr.Response.StatusCode == http.StatusNotFound {
						branchNotFound = true
					}
				}

				if !repositoryIsEmpty && !branchNotFound {
					return ghErrors.NewGitHubAPIErrorResponse(ctx,
						"failed to get branch reference",
						resp,
						err,
					), nil, nil
				}
			}
			// Only close resp if it's not nil and not an error case where resp might be nil
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			var baseCommit *github.Commit
			if !repositoryIsEmpty {
				if branchNotFound {
					ref, err = createReferenceFromDefaultBranch(ctx, client, owner, repo, branch)
					if err != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to create branch from default: %v", err)), nil, nil
					}
				}

				// Get the commit object that the branch points to
				baseCommit, resp, err = client.Git.GetCommit(ctx, owner, repo, *ref.Object.SHA)
				if err != nil {
					return ghErrors.NewGitHubAPIErrorResponse(ctx,
						"failed to get base commit",
						resp,
						err,
					), nil, nil
				}
				if resp != nil && resp.Body != nil {
					defer func() { _ = resp.Body.Close() }()
				}
			} else {
				var base *github.Commit
				// Repository is empty, need to initialize it first
				ref, base, err = initializeRepository(ctx, client, owner, repo)
				if err != nil {
					return utils.NewToolResultError(fmt.Sprintf("failed to initialize repository: %v", err)), nil, nil
				}

				defaultBranch := strings.TrimPrefix(*ref.Ref, "refs/heads/")
				if branch != defaultBranch {
					// Create the requested branch from the default branch
					ref, err = createReferenceFromDefaultBranch(ctx, client, owner, repo, branch)
					if err != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to create branch from default: %v", err)), nil, nil
					}
				}

				baseCommit = base
			}

			// Create tree entries for all files (or remaining files if empty repo)
			var entries []*github.TreeEntry

			changedPaths := make([]string, 0, len(filesObj))
			patchPreviews := make([]any, 0)
			for _, file := range filesObj {
				fileMap, ok := file.(map[string]any)
				if !ok {
					return utils.NewToolResultError("each file must be an object with path and content"), nil, nil
				}

				path, ok := fileMap["path"].(string)
				if !ok || path == "" {
					return utils.NewToolResultError("each file must have a path"), nil, nil
				}

				content, hasContent := fileMap["content"].(string)
				rawOperation, operationProvided := fileMap["operation"]
				operation, _ := rawOperation.(string)
				if operation == "" {
					operation = "replace"
				}
				if !hasContent && operation == "replace" {
					return utils.NewToolResultError("each file must have content"), nil, nil
				}
				expectedSHA, expectedBlobProvided := fileMap["expected_blob_sha"].(string)
				if expectedSHA == "" {
					expectedSHA, _ = fileMap["sha"].(string)
				}
				if operationProvided || expectedBlobProvided {
					current, currentContent, currentResp, currentErr := getCurrentGitHubFile(ctx, client, owner, repo, path, branch)
					if currentResp != nil && currentResp.Body != nil {
						_ = currentResp.Body.Close()
					}
					if currentErr != nil {
						return utils.NewToolResultError(fmt.Sprintf("failed to fetch %s before patch: %v", path, currentErr)), nil, nil
					}
					if expectedSHA == "" {
						return utils.NewToolResultError(fmt.Sprintf("expected_blob_sha is required for existing file %s", path)), nil, nil
					}
					if current.GetSHA() != expectedSHA {
						return utils.NewToolResultError(fmt.Sprintf("SHA mismatch for %s: provided %s, current %s", path, expectedSHA, current.GetSHA())), nil, nil
					}
					occurrences, _ := optionalIntArgument(fileMap, "expected_occurrences")
					startLine, _ := optionalIntArgument(fileMap, "start_line")
					endLine, _ := optionalIntArgument(fileMap, "end_line")
					search, _ := fileMap["search"].(string)
					replace, _ := fileMap["replace"].(string)
					replacement, _ := fileMap["replacement"].(string)
					patchText, _ := fileMap["patch"].(string)
					patchInput := filePatchInput{Operation: operation, Content: content, Search: search, Replace: replace, ExpectedOccurrences: occurrences, StartLine: startLine, EndLine: endLine, Replacement: replacement, Patch: patchText}
					nextContent, patchPreview, patchErr := applyFilePatch(currentContent, patchInput)
					if patchErr != nil {
						return utils.NewToolResultError(fmt.Sprintf("%s: %v", path, patchErr)), nil, nil
					}
					if !unrelatedFileContentPreserved(currentContent, nextContent, patchInput) {
						return utils.NewToolResultError(fmt.Sprintf("%s: unrelated content was not preserved", path)), nil, nil
					}
					content = nextContent
					patchPreviews = append(patchPreviews, map[string]any{"path": path, "operation": operation, "expected_blob_sha": expectedSHA, "matched_occurrences": patchPreview.MatchedOccurrences, "hunks": patchPreview.Hunks, "before_preview": boundedRedactedPreview(currentContent), "after_preview": boundedRedactedPreview(nextContent)})
				}
				if isSecretLikeRepoPath(path) {
					return newBlockedFileToolResult(path, "secret_like_file"), nil, nil
				}
				if reason := detectSecretLikeContent(path, content); reason != "" {
					return newBlockedFileToolResult(path, reason), nil, nil
				}

				// Create a tree entry for the file
				entries = append(entries, &github.TreeEntry{
					Path:    github.Ptr(path),
					Mode:    github.Ptr("100644"), // Regular file mode
					Type:    github.Ptr("blob"),
					Content: github.Ptr(content),
				})
				changedPaths = append(changedPaths, path)
			}

			if dryRun {
				return MarshalledTextResult(map[string]any{
					"branch":         branch,
					"changed_paths":  changedPaths,
					"file_count":     len(changedPaths),
					"patch_previews": patchPreviews,
				}), nil, nil
			}

			// Create a new tree with the file entries (baseCommit is now guaranteed to exist)
			newTree, resp, err := client.Git.CreateTree(ctx, owner, repo, *baseCommit.Tree.SHA, entries)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create tree",
					resp,
					err,
				), nil, nil
			}
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			// Create a new commit (baseCommit always has a value now)
			commit := github.Commit{
				Message: github.Ptr(message),
				Tree:    newTree,
				Parents: []*github.Commit{{SHA: baseCommit.SHA}},
			}
			newCommit, resp, err := client.Git.CreateCommit(ctx, owner, repo, commit, nil)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to create commit",
					resp,
					err,
				), nil, nil
			}
			if resp != nil && resp.Body != nil {
				defer func() { _ = resp.Body.Close() }()
			}

			// Update the reference to point to the new commit
			ref.Object.SHA = newCommit.SHA
			updatedRef, resp, err := client.Git.UpdateRef(ctx, owner, repo, *ref.Ref, github.UpdateRef{
				SHA:   *newCommit.SHA,
				Force: github.Ptr(false),
			})
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to update reference",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			r, err := json.Marshal(updatedRef)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(r)), nil, nil
		},
	)
}

// ListTags creates a tool to list tags in a GitHub repository.
func ListTags(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "list_tags",
			Description: t("TOOL_LIST_TAGS_DESCRIPTION", "List git tags in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_LIST_TAGS_USER_TITLE", "List tags"),
				ReadOnlyHint: true,
			},
			InputSchema: WithPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
				},
				Required: []string{"owner", "repo"},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			opts := &github.ListOptions{
				Page:    pagination.Page,
				PerPage: pagination.PerPage,
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			tags, resp, err := client.Repositories.ListTags(ctx, owner, repo, opts)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to list tags",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to list tags", resp, body), nil, nil
			}

			minimalTags := make([]MinimalTag, 0, len(tags))
			for _, tag := range tags {
				if tag != nil {
					minimalTags = append(minimalTags, convertToMinimalTag(tag))
				}
			}

			r, err := json.Marshal(minimalTags)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Tags are structural repo metadata created by collaborators with
			// push access, so integrity is trusted. Confidentiality follows
			// repo visibility.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result, ifc.LabelRepoMetadata)
			return result, nil, nil
		},
	)
}

// GetTag creates a tool to get details about a specific tag in a GitHub repository.
func GetTag(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "get_tag",
			Description: t("TOOL_GET_TAG_DESCRIPTION", "Get details about a specific git tag in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_GET_TAG_USER_TITLE", "Get tag details"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"tag": {
						Type:        "string",
						Description: "Tag name",
					},
				},
				Required: []string{"owner", "repo", "tag"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			tag, err := RequiredParam[string](args, "tag")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			// First get the tag reference
			ref, resp, err := client.Git.GetRef(ctx, owner, repo, "refs/tags/"+tag)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to get tag reference",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get tag reference", resp, body), nil, nil
			}

			// Differentiate between lightweight and annotated tags since lightweight ones don't have a fetchable object
			if ref.Object.GetType() == "commit" {
				r, err := json.Marshal(ref)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
				}
				result := utils.NewToolResultText(string(r))
				result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result, ifc.LabelRepoMetadata)
				return result, nil, nil
			}

			tagObj, resp, err := client.Git.GetTag(ctx, owner, repo, *ref.Object.SHA)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to get tag object",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get tag object", resp, body), nil, nil
			}

			r, err := json.Marshal(tagObj)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// An annotated tag object is structural repo metadata created by a
			// collaborator with push access. Confidentiality follows repo
			// visibility.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result, ifc.LabelRepoMetadata)
			return result, nil, nil
		},
	)
}

// ListReleases creates a tool to list releases in a GitHub repository.
func ListReleases(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "list_releases",
			Description: t("TOOL_LIST_RELEASES_DESCRIPTION", "List releases in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_LIST_RELEASES_USER_TITLE", "List releases"),
				ReadOnlyHint: true,
			},
			InputSchema: WithPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
				},
				Required: []string{"owner", "repo"},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			opts := &github.ListOptions{
				Page:    pagination.Page,
				PerPage: pagination.PerPage,
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			releases, resp, err := client.Repositories.ListReleases(ctx, owner, repo, opts)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to list releases: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to list releases", resp, body), nil, nil
			}

			minimalReleases := make([]MinimalRelease, 0, len(releases))
			for _, release := range releases {
				if release != nil {
					minimalReleases = append(minimalReleases, convertToMinimalRelease(release))
				}
			}

			r, err := json.Marshal(minimalReleases)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Releases are published by collaborators with push access, so
			// integrity is trusted. Confidentiality follows repo visibility,
			// but draft releases are visible only to push-access users and are
			// not world-readable even on a public repo, so the result is only
			// public when no returned release is a draft.
			hasDraft := false
			for _, mr := range minimalReleases {
				if mr.Draft {
					hasDraft = true
					break
				}
			}
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result,
				func(isPrivate bool) ifc.SecurityLabel {
					return ifc.LabelRelease(isPrivate, hasDraft)
				})
			return result, nil, nil
		},
	)
}

// GetLatestRelease creates a tool to get the latest release in a GitHub repository.
func GetLatestRelease(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "get_latest_release",
			Description: t("TOOL_GET_LATEST_RELEASE_DESCRIPTION", "Get the latest release in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_GET_LATEST_RELEASE_USER_TITLE", "Get latest release"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
				},
				Required: []string{"owner", "repo"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			release, resp, err := client.Repositories.GetLatestRelease(ctx, owner, repo)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get latest release: %w", err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get latest release", resp, body), nil, nil
			}

			r, err := json.Marshal(release)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Releases are published by collaborators with push access, so
			// integrity is trusted. The "latest release" endpoint never returns
			// a draft, but the draft flag is honored defensively: a draft is
			// not world-readable even on a public repo.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result,
				func(isPrivate bool) ifc.SecurityLabel {
					return ifc.LabelRelease(isPrivate, release.GetDraft())
				})
			return result, nil, nil
		},
	)
}

func GetReleaseByTag(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "get_release_by_tag",
			Description: t("TOOL_GET_RELEASE_BY_TAG_DESCRIPTION", "Get a specific release by its tag name in a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_GET_RELEASE_BY_TAG_USER_TITLE", "Get a release by tag name"),
				ReadOnlyHint: true,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"tag": {
						Type:        "string",
						Description: "Tag name (e.g., 'v1.0.0')",
					},
				},
				Required: []string{"owner", "repo", "tag"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			tag, err := RequiredParam[string](args, "tag")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			release, resp, err := client.Repositories.GetReleaseByTag(ctx, owner, repo, tag)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					fmt.Sprintf("failed to get release by tag: %s", tag),
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to get release by tag", resp, body), nil, nil
			}

			r, err := json.Marshal(release)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// Releases are published by collaborators with push access, so
			// integrity is trusted. A release fetched by tag may be a draft,
			// which is visible only to push-access users and not world-readable
			// even on a public repo, so a draft forces private confidentiality.
			result = attachRepoVisibilityIFCLabel(ctx, deps, client, owner, repo, result,
				func(isPrivate bool) ifc.SecurityLabel {
					return ifc.LabelRelease(isPrivate, release.GetDraft())
				})
			return result, nil, nil
		},
	)
}

// ListStarredRepositories creates a tool to list starred repositories for the authenticated user or a specified user.
func ListStarredRepositories(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataStargazers,
		mcp.Tool{
			Name:        "list_starred_repositories",
			Description: t("TOOL_LIST_STARRED_REPOSITORIES_DESCRIPTION", "List starred repositories"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_LIST_STARRED_REPOSITORIES_USER_TITLE", "List starred repositories"),
				ReadOnlyHint: true,
			},
			InputSchema: WithPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"username": {
						Type:        "string",
						Description: "Username to list starred repositories for. Defaults to the authenticated user.",
					},
					"sort": {
						Type:        "string",
						Description: "How to sort the results. Can be either 'created' (when the repository was starred) or 'updated' (when the repository was last pushed to).",
						Enum:        []any{"created", "updated"},
					},
					"direction": {
						Type:        "string",
						Description: "The direction to sort the results by.",
						Enum:        []any{"asc", "desc"},
					},
				},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			username, err := OptionalParam[string](args, "username")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			sort, err := OptionalParam[string](args, "sort")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			direction, err := OptionalParam[string](args, "direction")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			opts := &github.ActivityListStarredOptions{
				ListOptions: github.ListOptions{
					Page:    pagination.Page,
					PerPage: pagination.PerPage,
				},
			}
			if sort != "" {
				opts.Sort = sort
			}
			if direction != "" {
				opts.Direction = direction
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			var repos []*github.StarredRepository
			var resp *github.Response
			if username == "" {
				// List starred repositories for the authenticated user
				repos, resp, err = client.Activity.ListStarred(ctx, "", opts)
			} else {
				// List starred repositories for a specific user
				repos, resp, err = client.Activity.ListStarred(ctx, username, opts)
			}

			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					fmt.Sprintf("failed to list starred repositories for user '%s'", username),
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 200 {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to list starred repositories", resp, body), nil, nil
			}

			// Convert to minimal format
			minimalRepos := make([]MinimalRepository, 0, len(repos))
			for _, starredRepo := range repos {
				repo := starredRepo.Repository
				minimalRepo := MinimalRepository{
					ID:            repo.GetID(),
					Name:          repo.GetName(),
					FullName:      repo.GetFullName(),
					Description:   repo.GetDescription(),
					HTMLURL:       repo.GetHTMLURL(),
					Language:      repo.GetLanguage(),
					Stars:         repo.GetStargazersCount(),
					Forks:         repo.GetForksCount(),
					OpenIssues:    repo.GetOpenIssuesCount(),
					Private:       repo.GetPrivate(),
					Fork:          repo.GetFork(),
					Archived:      repo.GetArchived(),
					DefaultBranch: repo.GetDefaultBranch(),
				}

				if repo.UpdatedAt != nil {
					minimalRepo.UpdatedAt = repo.UpdatedAt.Format("2006-01-02T15:04:05Z")
				}

				minimalRepos = append(minimalRepos, minimalRepo)
			}

			r, err := json.Marshal(minimalRepos)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal starred repositories: %w", err)
			}

			result := utils.NewToolResultText(string(r))
			// A starred-repository listing exposes repository data across many
			// repos; reuse the multi-repo join shared with search_repositories
			// (untrusted integrity; confidentiality private if any matched repo
			// is private). Visibility is read directly from the response, so no
			// extra API call is needed.
			visibilities := make([]bool, 0, len(minimalRepos))
			for _, mr := range minimalRepos {
				visibilities = append(visibilities, mr.Private)
			}
			result = attachJoinedIFCLabel(ctx, deps, result, visibilities, ifc.LabelSearchIssues)
			return result, nil, nil
		},
	)
}

// StarRepository creates a tool to star a repository.
func StarRepository(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataStargazers,
		mcp.Tool{
			Name:        "star_repository",
			Description: t("TOOL_STAR_REPOSITORY_DESCRIPTION", "Star a GitHub repository"),
			Icons:       octicons.Icons("star-fill"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_STAR_REPOSITORY_USER_TITLE", "Star repository"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
				},
				Required: []string{"owner", "repo"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			resp, err := client.Activity.Star(ctx, owner, repo)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					fmt.Sprintf("failed to star repository %s/%s", owner, repo),
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 204 {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to star repository", resp, body), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Successfully starred repository %s/%s", owner, repo)), nil, nil
		},
	)
}

// UnstarRepository creates a tool to unstar a repository.
func UnstarRepository(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataStargazers,
		mcp.Tool{
			Name:        "unstar_repository",
			Description: t("TOOL_UNSTAR_REPOSITORY_DESCRIPTION", "Unstar a GitHub repository"),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_UNSTAR_REPOSITORY_USER_TITLE", "Unstar repository"),
				ReadOnlyHint: false,
			},
			InputSchema: &jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
				},
				Required: []string{"owner", "repo"},
			},
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			resp, err := client.Activity.Unstar(ctx, owner, repo)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					fmt.Sprintf("failed to unstar repository %s/%s", owner, repo),
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != 204 {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to unstar repository", resp, body), nil, nil
			}

			return utils.NewToolResultText(fmt.Sprintf("Successfully unstarred repository %s/%s", owner, repo)), nil, nil
		},
	)
}

// maxBlameRanges caps the number of matching blame ranges considered for one response.
const maxBlameRanges = 1000

const blameCursorPrefix = "blame-range:"

func encodeBlameCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString(fmt.Appendf(nil, "%s%d", blameCursorPrefix, offset))
}

func decodeBlameCursor(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}

	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, fmt.Errorf("after cursor is invalid")
	}

	value := string(decoded)
	if !strings.HasPrefix(value, blameCursorPrefix) {
		return 0, fmt.Errorf("after cursor is invalid")
	}

	offset, err := strconv.Atoi(strings.TrimPrefix(value, blameCursorPrefix))
	if err != nil || offset < 0 {
		return 0, fmt.Errorf("after cursor is invalid")
	}

	return offset, nil
}

// BlameAuthor describes the author of a commit referenced by a BlameRange.
type BlameAuthor struct {
	Name  string  `json:"name"`
	Email string  `json:"email"`
	Login *string `json:"login,omitempty"`
	URL   *string `json:"url,omitempty"`
}

// BlameCommit holds commit metadata shared by one or more blame ranges.
type BlameCommit struct {
	SHA             string      `json:"sha"`
	MessageHeadline string      `json:"message_headline"`
	CommittedDate   string      `json:"committed_date"`
	Author          BlameAuthor `json:"author"`
}

// BlameRange is a contiguous run of lines attributed to a single commit.
//
// Age is the relative position of this range's commit among distinct commits
// touching the file (0 = newest), not an absolute time delta. See:
// https://docs.github.com/en/graphql/reference/objects#blamerange
type BlameRange struct {
	StartingLine int    `json:"starting_line"`
	EndingLine   int    `json:"ending_line"`
	Age          int    `json:"age"`
	CommitSHA    string `json:"commit_sha"`
}

// BlameResult is the response payload returned by the get_file_blame tool.
//
// Commits is keyed by SHA. TotalRanges counts matching ranges before cursor
// pagination or truncation. Truncated reports whether maxBlameRanges was hit.
type BlameResult struct {
	Repository  string                 `json:"repository"`
	Path        string                 `json:"path"`
	Ref         string                 `json:"ref"`
	Ranges      []BlameRange           `json:"ranges"`
	Commits     map[string]BlameCommit `json:"commits"`
	PageInfo    MinimalPageInfo        `json:"pageInfo"`
	TotalRanges int                    `json:"total_ranges"`
	Truncated   bool                   `json:"truncated,omitempty"`
}

// blameCommitFragment is the GraphQL selection for a Commit's blame data.
type blameCommitFragment struct {
	Blame struct {
		Ranges []struct {
			StartingLine githubv4.Int
			EndingLine   githubv4.Int
			Age          githubv4.Int
			Commit       struct {
				OID           githubv4.String
				Message       githubv4.String
				CommittedDate githubv4.DateTime
				Author        struct {
					Name  githubv4.String
					Email githubv4.String
					User  *struct {
						Login githubv4.String
						URL   githubv4.String
					}
				}
			}
		}
	} `graphql:"blame(path: $path)"`
}

// validateBlamePath rejects empty, leading-slash, traversal-laden, or
// control-character paths before any network call is made.
func validateBlamePath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("path must not be empty")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path must be relative to the repository root (no leading '/')")
	}
	if slices.Contains(strings.Split(p, "/"), "..") {
		return fmt.Errorf("path must not contain '..' segments")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("path must not contain control characters")
		}
	}
	return nil
}

func GetFileBlame(t translations.TranslationHelperFunc) inventory.ServerTool {
	st := NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name: "get_file_blame",
			Description: t("TOOL_GET_FILE_BLAME_DESCRIPTION",
				"Get git blame information for a file, showing the commit that last modified each line. "+
					"Ranges share commit metadata via the top-level 'commits' map keyed by SHA. "+
					"Use 'start_line'/'end_line' to restrict the result to a window of the file, and "+
					"'perPage'/'after' to cursor-page through returned ranges. Matching ranges are capped at "+
					"1000; when the cap is hit 'truncated' is set to true and 'total_ranges' reports the pre-cap match count.",
			),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_GET_FILE_BLAME_USER_TITLE", "Get file blame information"),
				ReadOnlyHint: true,
			},
			InputSchema: WithCursorPagination(&jsonschema.Schema{
				Type: "object",
				Properties: map[string]*jsonschema.Schema{
					"owner": {
						Type:        "string",
						Description: "Repository owner (username or organization)",
					},
					"repo": {
						Type:        "string",
						Description: "Repository name",
					},
					"path": {
						Type:        "string",
						Description: "Path to the file in the repository, relative to the repository root",
					},
					"ref": {
						Type:        "string",
						Description: "Git reference (branch, tag, or commit SHA). Defaults to the repository's default branch (HEAD).",
					},
					"start_line": {
						Type:        "number",
						Description: "Optional 1-based starting line of the window of interest. Only ranges overlapping [start_line, end_line] are returned, clamped to the window.",
						Minimum:     jsonschema.Ptr(1.0),
					},
					"end_line": {
						Type:        "number",
						Description: "Optional 1-based ending line of the window of interest. Must be >= start_line when both are provided.",
						Minimum:     jsonschema.Ptr(1.0),
					},
				},
				Required: []string{"owner", "repo", "path"},
			}),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			path, err := RequiredParam[string](args, "path")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if err := validateBlamePath(path); err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			ref, err := OptionalParam[string](args, "ref")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			_, hasStartLine := args["start_line"]
			startLine, err := OptionalIntParam(args, "start_line")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if hasStartLine && startLine < 1 {
				return utils.NewToolResultError("start_line must be omitted or >= 1"), nil, nil
			}
			_, hasEndLine := args["end_line"]
			endLine, err := OptionalIntParam(args, "end_line")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if hasEndLine && endLine < 1 {
				return utils.NewToolResultError("end_line must be omitted or >= 1"), nil, nil
			}
			if hasStartLine && hasEndLine && endLine < startLine {
				return utils.NewToolResultError("end_line must be >= start_line when both are provided"), nil, nil
			}
			if _, hasPage := args["page"]; hasPage {
				return utils.NewToolResultError("This tool uses cursor-based pagination. Use the 'after' parameter with the 'endCursor' value from the previous response instead of 'page'."), nil, nil
			}
			pagination, err := OptionalCursorPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			if _, hasPerPage := args["perPage"]; hasPerPage {
				perPage, err := OptionalIntParam(args, "perPage")
				if err != nil {
					return utils.NewToolResultError(err.Error()), nil, nil
				}
				if perPage < 1 || perPage > 100 {
					return utils.NewToolResultError("perPage must be between 1 and 100 when provided"), nil, nil
				}
				pagination.PerPage = perPage
			}
			afterOffset, err := decodeBlameCursor(pagination.After)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetGQLClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub GraphQL client: %w", err)
			}

			// Default to HEAD and fetch defaultBranchRef.name in the same query
			// so the response can echo a readable ref.
			refExpression := ref
			if refExpression == "" {
				refExpression = "HEAD"
			}

			var blameQuery struct {
				Repository struct {
					DefaultBranchRef struct {
						Name githubv4.String
					}
					Object struct {
						Typename githubv4.String     `graphql:"__typename"`
						Commit   blameCommitFragment `graphql:"... on Commit"`
						// Annotated tag targets are followed one level. Tag-of-tag
						// chains are not followed and will return an error.
						Tag struct {
							Target struct {
								Typename githubv4.String     `graphql:"__typename"`
								Commit   blameCommitFragment `graphql:"... on Commit"`
							}
						} `graphql:"... on Tag"`
					} `graphql:"object(expression: $ref)"`
				} `graphql:"repository(owner: $owner, name: $repo)"`
			}

			vars := map[string]any{
				"owner": githubv4.String(owner),
				"repo":  githubv4.String(repo),
				"ref":   githubv4.String(refExpression),
				"path":  githubv4.String(path),
			}

			if err := client.Query(ctx, &blameQuery, vars); err != nil {
				return ghErrors.NewGitHubGraphQLErrorResponse(ctx,
					fmt.Sprintf("failed to get blame for file: %s", path),
					err,
				), nil, nil
			}

			// GitHub's Commit.blame field accepts only path, and Blame.ranges is
			// not a connection, so cursor pagination is applied locally below.
			// The ref must resolve to a commit, either directly or via an annotated tag.
			objectTypename := string(blameQuery.Repository.Object.Typename)
			if objectTypename == "" {
				return utils.NewToolResultError(
					fmt.Sprintf("ref %q was not found in %s/%s", refExpression, owner, repo),
				), nil, nil
			}
			blameCommit := &blameQuery.Repository.Object.Commit
			if objectTypename == "Tag" {
				targetTypename := string(blameQuery.Repository.Object.Tag.Target.Typename)
				if targetTypename != "Commit" {
					if targetTypename == "" {
						targetTypename = "unknown"
					}
					return utils.NewToolResultError(
						fmt.Sprintf("ref %q resolved to a tag in %s/%s, but the tag target did not resolve to a commit (resolved to %s)",
							refExpression, owner, repo, targetTypename),
					), nil, nil
				}
				blameCommit = &blameQuery.Repository.Object.Tag.Target.Commit
			} else if objectTypename != "Commit" {
				return utils.NewToolResultError(
					fmt.Sprintf("ref %q did not resolve to a commit in %s/%s (resolved to %s)",
						refExpression, owner, repo, objectTypename),
				), nil, nil
			}

			// Echo the caller's ref, otherwise prefer the default branch name.
			responseRef := ref
			if responseRef == "" {
				if name := string(blameQuery.Repository.DefaultBranchRef.Name); name != "" {
					responseRef = name
				} else {
					responseRef = refExpression
				}
			}

			rawRanges := blameCommit.Blame.Ranges
			pageRanges := make([]BlameRange, 0, pagination.PerPage)
			commits := make(map[string]BlameCommit)
			totalRanges := 0
			truncated := false

			for _, r := range rawRanges {
				start := int(r.StartingLine)
				end := int(r.EndingLine)
				if startLine > 0 && end < startLine {
					continue
				}
				if endLine > 0 && start > endLine {
					continue
				}
				if startLine > 0 && start < startLine {
					start = startLine
				}
				if endLine > 0 && end > endLine {
					end = endLine
				}

				matchIndex := totalRanges
				totalRanges++
				if matchIndex >= maxBlameRanges {
					truncated = true
					continue
				}
				if matchIndex < afterOffset || len(pageRanges) >= pagination.PerPage {
					continue
				}

				blameRange := BlameRange{
					StartingLine: start,
					EndingLine:   end,
					Age:          int(r.Age),
					CommitSHA:    string(r.Commit.OID),
				}
				pageRanges = append(pageRanges, blameRange)

				sha := string(r.Commit.OID)
				if _, seen := commits[sha]; seen {
					continue
				}
				headline := string(r.Commit.Message)
				if idx := strings.IndexByte(headline, '\n'); idx >= 0 {
					headline = headline[:idx]
				}
				headline = strings.TrimRight(headline, " \t\r")
				bc := BlameCommit{
					SHA:             sha,
					MessageHeadline: headline,
					CommittedDate:   r.Commit.CommittedDate.Format("2006-01-02T15:04:05Z"),
					Author: BlameAuthor{
						Name:  string(r.Commit.Author.Name),
						Email: string(r.Commit.Author.Email),
					},
				}
				if r.Commit.Author.User != nil {
					login := string(r.Commit.Author.User.Login)
					url := string(r.Commit.Author.User.URL)
					bc.Author.Login = &login
					bc.Author.URL = &url
				}
				commits[sha] = bc
			}

			cappedRanges := min(totalRanges, maxBlameRanges)
			consumedRanges := min(afterOffset+len(pageRanges), cappedRanges)
			pageInfo := MinimalPageInfo{
				HasNextPage:     consumedRanges < cappedRanges,
				HasPreviousPage: afterOffset > 0,
			}
			if len(pageRanges) > 0 {
				pageInfo.StartCursor = encodeBlameCursor(afterOffset)
				pageInfo.EndCursor = encodeBlameCursor(consumedRanges)
			}

			result := BlameResult{
				Repository:  fmt.Sprintf("%s/%s", owner, repo),
				Path:        path,
				Ref:         responseRef,
				Ranges:      pageRanges,
				Commits:     commits,
				PageInfo:    pageInfo,
				TotalRanges: totalRanges,
				Truncated:   truncated,
			}
			if result.Ranges == nil {
				result.Ranges = []BlameRange{}
			}

			payload, err := json.Marshal(result)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to marshal response: %w", err)
			}

			return utils.NewToolResultText(string(payload)), nil, nil
		},
	)
	st.FeatureFlagEnable = FeatureFlagFileBlame
	return st
}

// ListRepositoryCollaborators creates a tool to list collaborators of a GitHub repository.
func ListRepositoryCollaborators(t translations.TranslationHelperFunc) inventory.ServerTool {
	return NewTool(
		ToolsetMetadataRepos,
		mcp.Tool{
			Name:        "list_repository_collaborators",
			Description: t("TOOL_LIST_REPOSITORY_COLLABORATORS_DESCRIPTION", "List collaborators of a GitHub repository. Results are paginated; the response includes `nextPage`, `prevPage`, `firstPage`, and `lastPage` fields. To get the next page, use the `nextPage` value as the `page` parameter."),
			Annotations: &mcp.ToolAnnotations{
				Title:        t("TOOL_LIST_REPOSITORY_COLLABORATORS_USER_TITLE", "List repository collaborators"),
				ReadOnlyHint: true,
			},
			InputSchema: func() *jsonschema.Schema {
				schema := WithPagination(&jsonschema.Schema{
					Type: "object",
					Properties: map[string]*jsonschema.Schema{
						"owner": {
							Type:        "string",
							Description: "Repository owner",
						},
						"repo": {
							Type:        "string",
							Description: "Repository name",
						},
						"affiliation": {
							Type:        "string",
							Description: "Filter by affiliation. Can be one of: 'outside' (outside collaborators), 'direct' (all with permissions regardless of org membership), 'all' (all collaborators). Default: 'all'",
							Enum:        []any{"outside", "direct", "all"},
						},
					},
					Required: []string{"owner", "repo"},
				})
				schema.Properties["page"].Description = "Page number for pagination (default 1, min 1)"
				schema.Properties["perPage"].Description = "Results per page for pagination (default 30, min 1, max 100)"
				return schema
			}(),
		},
		[]scopes.Scope{scopes.Repo},
		func(ctx context.Context, deps ToolDependencies, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
			owner, err := RequiredParam[string](args, "owner")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			repo, err := RequiredParam[string](args, "repo")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			affiliation, err := OptionalParam[string](args, "affiliation")
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}
			pagination, err := OptionalPaginationParams(args)
			if err != nil {
				return utils.NewToolResultError(err.Error()), nil, nil
			}

			client, err := deps.GetClient(ctx)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to get GitHub client: %w", err)
			}

			opts := &github.ListCollaboratorsOptions{
				Affiliation: affiliation,
				ListOptions: github.ListOptions{
					Page:    pagination.Page,
					PerPage: pagination.PerPage,
				},
			}

			collaborators, resp, err := client.Repositories.ListCollaborators(ctx, owner, repo, opts)
			if err != nil {
				return ghErrors.NewGitHubAPIErrorResponse(ctx,
					"failed to list collaborators",
					resp,
					err,
				), nil, nil
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to read response body: %w", err)
				}
				return ghErrors.NewGitHubAPIStatusErrorResponse(ctx, "failed to list collaborators", resp, body), nil, nil
			}

			result := make([]MinimalCollaborator, 0, len(collaborators))
			for _, c := range collaborators {
				result = append(result, MinimalCollaborator{
					Login:    c.GetLogin(),
					ID:       c.GetID(),
					RoleName: c.GetRoleName(),
				})
			}

			response := map[string]any{
				"items":     result,
				"nextPage":  resp.NextPage,
				"prevPage":  resp.PrevPage,
				"firstPage": resp.FirstPage,
				"lastPage":  resp.LastPage,
			}

			callResult := MarshalledTextResult(response)
			// The collaborator roster is GitHub-maintained membership data
			// (trusted, not attacker-authored). Listing collaborators requires
			// push access, so the roster is never world-readable — not even on
			// a public repo — hence always private confidentiality.
			callResult = attachStaticIFCLabel(ctx, deps, callResult, ifc.LabelCollaboratorRoster())
			return callResult, nil, nil
		},
	)
}
