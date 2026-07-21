# Repository Hygiene Tools

The GitHub MCP Server includes a narrow repository hygiene surface for agents that need clean branches and atomic file changes without creating temporary GitHub Actions workflows.

The public tools are:

- `manage_branch`: create or delete a branch with source/head SHA guards.
- `apply_repository_changes`: apply file additions, updates, and deletions through one Git tree, one commit, and one non-force branch ref update.
- `update_pull_request`: now accepts `state` with `open` or `closed` to close or reopen pull requests.

Low-level Git Data APIs for blobs, trees, commits, and refs remain internal implementation details. They are intentionally not exposed as public MCP tools to avoid tool bloat and generic GitHub API escape hatches.

## Concurrency

Use `expected_head_sha` on `apply_repository_changes` when the caller has observed the branch head. The tool resolves the branch head once and refuses to proceed if it does not match exactly.

Use `expected_blob_sha` when updating a file only if you require that exact blob. Use it for every delete; deletes require the current blob SHA so stale deletions fail before any commit is created.

Branch create and delete use `expected_sha`:

- For `action=create`, it must match the resolved source commit.
- For `action=delete`, it must match the current branch head.

All branch ref updates are non-force updates.

## Create A Clean Branch From `main`

```json
{
  "owner": "OWNER",
  "repo": "REPO",
  "action": "create",
  "branch": "agent/clean-change",
  "source_ref": "main",
  "expected_sha": "1111111111111111111111111111111111111111"
}
```

The response includes the created branch name, resolved source SHA, and created branch SHA.

## Copy Selected Files In One Atomic Commit

```json
{
  "owner": "OWNER",
  "repo": "REPO",
  "branch": "agent/clean-change",
  "message": "Copy selected files",
  "expected_head_sha": "1111111111111111111111111111111111111111",
  "changes": [
    {
      "path": "docs/example.md",
      "operation": "upsert",
      "content": "# Example\n"
    },
    {
      "path": "src/config.json",
      "operation": "upsert",
      "content": "{\"enabled\":true}\n",
      "expected_blob_sha": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
    }
  ]
}
```

The tool rejects duplicate paths, absolute paths, traversal paths, directory paths, missing delete targets, stale blob SHAs, and empty change sets before writing anything. The response includes the previous branch head, new commit SHA, new tree SHA, and added, updated, and deleted path lists.

## Delete A Temporary Workflow

```json
{
  "owner": "OWNER",
  "repo": "REPO",
  "branch": "agent/clean-change",
  "message": "Remove temporary workflow",
  "expected_head_sha": "2222222222222222222222222222222222222222",
  "changes": [
    {
      "path": ".github/workflows/temp-agent.yml",
      "operation": "delete",
      "expected_blob_sha": "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    }
  ]
}
```

Deletes never accept `content` and always require `expected_blob_sha`.

## Close An Obsolete PR

```json
{
  "owner": "OWNER",
  "repo": "REPO",
  "pullNumber": 42,
  "state": "closed"
}
```

`update_pull_request` preserves existing title, body, draft, maintainer-edit, base, and reviewer behavior. This extension only adds state close/reopen support; it does not merge pull requests.

## Delete The Obsolete Branch

```json
{
  "owner": "OWNER",
  "repo": "REPO",
  "action": "delete",
  "branch": "agent/clean-change",
  "expected_sha": "3333333333333333333333333333333333333333"
}
```

Branch deletion refuses the repository default branch and refuses branches with open pull requests. The response includes the deleted branch name and prior head SHA.
