# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

Git Feed is a Go CLI for monitoring:
- GitHub pull requests and issues
- GitLab merge requests and issues

The README uses the branding name "GitAI", but the binary is `git-feed`.

This repo is the unified (GitHub + GitLab) version. Avoid adding documentation that assumes GitHub-only behavior.

## Build & Run

```bash
go build -o git-feed .

# Default: GitHub platform, online mode
./git-feed

# Select platform
./git-feed --platform github
./git-feed --platform gitlab

# Time window (default: 1m)
./git-feed --time 3h
./git-feed --time 2d
./git-feed --time 3w
./git-feed --time 6m
./git-feed --time 1y

# Debug output (verbose logging)
./git-feed --debug

# Offline mode: read from local cache DB only
./git-feed --local

# Show hyperlinks under each MR/PR/issue
./git-feed --links

# Shortcut: --local --links
./git-feed --ll

# Delete and recreate the cache DB for the selected platform
./git-feed --clean

# Restrict to a bounded set of repos/projects
./git-feed --allowed-repos "owner/repo,owner/other"
./git-feed --platform gitlab --allowed-repos "group/repo,group/subgroup/repo"
```

## Configuration

Select the platform via `--platform github|gitlab` (default: `github`).

Online mode requirements depend on platform:

- GitHub: `GITHUB_TOKEN`, `GITHUB_USERNAME` (and optionally `GITHUB_ALLOWED_REPOS`)
- GitLab: `GITLAB_TOKEN` (or `GITLAB_ACTIVITY_TOKEN`) and `GITLAB_ALLOWED_REPOS`

Precedence order:
1) CLI flags
2) Environment variables
3) Shared `.env` file
4) Built-in defaults

The `.env` file is auto-created on first run at:
- `~/.git-feed/.env`

Important: `.env` loading does not override already-set environment variables.

Environment variables:
- GitHub
  - `GITHUB_TOKEN` (required online)
  - `GITHUB_USERNAME` (required online)
  - `GITHUB_ALLOWED_REPOS` (optional; comma-separated `owner/repo`)

- GitLab
  - `GITLAB_TOKEN` or `GITLAB_ACTIVITY_TOKEN` (required online)
  - `GITLAB_HOST` (optional host override; takes precedence over `GITLAB_BASE_URL`)
  - `GITLAB_BASE_URL` (optional; default: `https://gitlab.com`)
  - `GITLAB_ALLOWED_REPOS` (required online; comma-separated `group[/subgroup]/repo`)
  - `ALLOWED_REPOS` (legacy fallback for either platform when platform-specific vars are unset)
  - `GITLAB_USERNAME` or `GITLAB_USER` (documented in help/template, but the current code resolves the GitLab user via API and does not read these vars)

Token scopes:
- `read_api` (recommended)
- `api` only if your self-managed instance requires broader scope

Reference: https://docs.gitlab.com/user/profile/personal_access_tokens/

Database cache:
- GitHub: `~/.git-feed/github.db` (BBolt)
- GitLab: `~/.git-feed/gitlab.db` (BBolt)

Notes:
- The tool uses a platform-specific database file (based on `--platform`). Both files share the same on-disk schema.
- You will only see both `github.db` and `gitlab.db` after running the tool at least once for each platform.

## First Run Behavior

On first run the app creates `~/.git-feed/` (permissions: 0755) and ensures:
- `~/.git-feed/.env` exists (permissions: 0600)
- The platform database exists (permissions: 0666)

The `.env` file contains a template with both GitHub and GitLab variables.

## Architecture & Key Components

This repo is organized as a single CLI entrypoint (`main.go`) that dispatches to one of two platform implementations:
- `platform_github.go` (GitHub)
- `platform_gitlab.go` (GitLab)

Both platforms share:
- common models (`MergeRequestModel`, `IssueModel`) used for display
- label priority logic (`shouldUpdateLabel`) and its tests
- a BBolt cache (`db.go`) used for offline mode and persistence

### Data Flow

#### Platform Selection
`main.go` parses flags, sets up `~/.git-feed/.env` and the cache database file, loads environment variables, validates online requirements, then calls `fetchAndDisplayActivity(platform)`.

#### GitHub Online Mode (Default when `--platform github` and not `--local`)
1. **Search**: runs several GitHub Search API queries to find PRs and issues the user is involved in.
2. **Hydrate details**: fetches full PR/issue objects by number (not just search items).
3. **Review comment collection**: fetches PR review comments for cross-reference detection.
4. **Caching**: stores PRs, issues, and PR review comments to `~/.git-feed/github.db`.
5. **Cross-reference nesting**: nests issues under PRs when references are detected in bodies or review comments.
6. **Rendering**: prints grouped sections (open PRs, closed/merged PRs, open issues, closed issues), optionally with links.

#### GitHub Offline Mode (`--local`)
1. **Database loading**: reads PRs, issues, and PR review comments from `~/.git-feed/github.db`.
2. **Filtering**: applies the cutoff time (`--time`) and optional allowed repos.
3. **Cross-reference nesting**: uses the same cross-reference logic as online mode.
4. **Rendering**: same output layout as online mode.

#### GitLab Online Mode (Default when `--platform gitlab` and not `--local`)
GitLab online mode is intentionally bounded: `GITLAB_ALLOWED_REPOS` (or `ALLOWED_REPOS`) must be set.

1. **Project resolution**: resolves each allowed `group[/subgroup]/repo` path to a project ID via the Projects API.
2. **Per-project scans**: lists merge requests and issues updated after the cutoff using project-scoped list endpoints.
3. **Label derivation**:
   - Uses MR/issue author and assignees first.
   - Uses approval state for "Reviewed" on merge requests.
   - Uses reviewers list for "Review Requested".
   - Uses notes (comments) to detect "Commented" and "Mentioned".
4. **Caching**: stores merge requests, issues, and relevant notes to `~/.git-feed/gitlab.db`.
5. **Cross-reference nesting**:
   - Preferred: uses GitLab's "issues closed on merge request" endpoint.
   - Fallback: parses MR bodies/notes for issue references (same-project refs, qualified refs, and issue URLs).
6. **Rendering**: same section layout as GitHub mode, using the unified models.

#### GitLab Offline Mode (`--local`)
1. **Database loading**: reads cached MRs, issues, and notes from `~/.git-feed/gitlab.db`.
2. **Filtering**: applies cutoff time and allowed projects.
3. **Cross-reference nesting**: parses MR bodies and cached notes for issue references.
4. **Rendering**: same output layout.

### Core Data Structures

**Config** (`main.go`): runtime configuration and shared references.
- Controls mode flags (`debugMode`, `localMode`, `showLinks`), platform credentials, allowed repos, and cache handle.

**PRActivity** (`main.go`): a unified "merge request" activity record.
- Used for both GitHub pull requests and GitLab merge requests.
- Fields: involvement label, owner/repo, `MergeRequestModel`, update time, nested issues.

**IssueActivity** (`main.go`): a unified issue activity record.

**MergeRequestModel** / **IssueModel** (`main.go`): simplified, platform-neutral view models.
- These are the types stored in BBolt for both platforms.

### Label Priority System

When the same item matches multiple involvement sources, the display shows the most important label.

**PR/MR Label Priorities** (highest to lowest):
1. Authored
2. Assigned
3. Reviewed
4. Review Requested
5. Commented
6. Mentioned

**Issue Label Priorities** (highest to lowest):
1. Authored
2. Assigned
3. Commented
4. Mentioned

The shared helper `shouldUpdateLabel(current, candidate, isPR)` implements this rule.

### Cross-Reference Detection

**GitHub** (`platform_github.go`):
- Only nests issues under PRs within the same repository.
- Detects references in:
  - PR body and issue body
  - PR review comments
- Supported patterns include:
  - `#123`
  - `owner/repo#123`
  - `https://github.com/owner/repo/issues/123` (and `/pull/`)

**GitLab** (`platform_gitlab.go`):
- Preferred nesting via API endpoint: issues closed on merge request.
- Fallback parsing from MR bodies and notes:
  - same-project `#123`
  - qualified `group/subgroup/repo#123`
  - issue URLs (including relative `/-/issues/123`)

## GitHub API Integration

Uses `google/go-github/v57`.

Key API patterns in `platform_github.go`:
- Search: `client.Search.Issues()` to discover candidate items.
- Details: `client.PullRequests.Get()` and `client.Issues.Get()` to fetch canonical fields.
- Comments: `client.PullRequests.ListComments()` for review comment bodies (used for cross-reference detection).

## GitLab API Integration

Uses `gitlab.com/gitlab-org/api/client-go`.

Base URL handling:
- `GITLAB_HOST` or `GITLAB_BASE_URL` is normalized to include `/api/v4` (and supports path prefixes).

Retry strategy:
- GitLab requests are wrapped via `retryWithBackoff()` for 429 rate limits and transient 5xx errors.
- For 429 responses the code respects `Retry-After` when present, otherwise uses `Ratelimit-Reset` when available.

## Database Module (db.go)

The cache uses BBolt and stores platform data as JSON.

Buckets:
- GitLab: `gitlab_merge_requests`, `gitlab_issues`, `gitlab_notes`
- GitHub: `pull_requests`, `issues`, `comments`

Key formats:
- GitLab MR key: `path_with_namespace#!IID`
- GitLab issue key: `path_with_namespace##IID`
- GitLab note key: `path|itemType|iid|noteID`
- GitHub item key: `owner/repo#number`
- GitHub PR review comment key: `owner/repo#number/pr_review_comment/commentID`

Data formats:
- GitHub and GitLab store their simplified models (`MergeRequestModel`, `IssueModel`) wrapped with a `Label` field.
- GitHub readers keep backwards compatibility by falling back to unmarshaling legacy (unwrapped) records.

## Command-Line Flags

Flags are parsed with the standard library `flag` package (`main.go`).

- `--platform github|gitlab` (default: `github`)
- `--time RANGE` (default: `1m`; supports `h`, `d`, `w`, `m`, `y`)
- `--debug` (verbose logging)
- `--local` (offline mode from cache)
- `--links` (print item URLs under each entry)
- `--ll` (shortcut for `--local --links`)
- `--clean` (delete and recreate the selected platform DB)
- `--allowed-repos` (comma-separated)
  - GitHub: `owner/repo`
  - GitLab: `group[/subgroup]/repo`

Allowed repo resolution order:
1. `--allowed-repos`
2. `GITHUB_ALLOWED_REPOS` or `GITLAB_ALLOWED_REPOS` (depending on `--platform`)
3. `ALLOWED_REPOS` (legacy fallback)

## Testing Considerations

When modifying this codebase:
- Validate both platforms (`--platform github` and `--platform gitlab`).
- Validate both modes (`--local` vs online).
- If touching GitLab behavior, ensure rate limit/backoff behavior still passes the table-driven tests.
- If changing reference parsing, add tests for both GitHub and GitLab patterns.
- Cache schema changes should preserve offline compatibility.

## Testing

Run the full test suite:

```bash
go test ./... -count=1
```

Notable tests (in `priority_test.go`):
- label priority and `shouldUpdateLabel()` behavior
- GitLab base URL normalization (`normalizeGitLabBaseURL`)
- retry/backoff behavior for GitLab rate limits and transient errors (`retryWithBackoff`)
- database round-trip and offline parity for GitLab cache
- end-to-end `go run . --platform gitlab --debug` against a mock GitLab server

## Known Issues & Discrepancies

These are documentation/behavior mismatches worth keeping in mind while working on the repo:

1. GitLab username env vars: `GITLAB_USERNAME` / `GITLAB_USER` appear in the usage text and `.env` template, but the current implementation resolves the current user via API and does not read those variables.
2. Progress bar wiring: a `Progress` type exists and is used for retry countdown messaging when set, but `config.progress` is not initialized in the main execution path (so progress rendering is effectively disabled).

## Refactoring Opportunities

Potential improvements that would reduce complexity or improve UX (not required for normal changes):
- Parallelize GitHub query passes and/or per-project GitLab scanning while keeping API usage bounded.
- Wire up `Progress` in non-debug mode (or remove it if not desired).
- Consolidate shared display and nesting logic further, while keeping platform-specific API details isolated.

## File Structure

```
git-feed/
├── main.go                      # CLI entrypoint, config, shared models, output rendering
├── platform_github.go           # GitHub API fetch + caching + nesting
├── platform_gitlab.go           # GitLab API fetch + caching + nesting + retry
├── db.go                        # BBolt schema and persistence helpers
├── priority_test.go             # Unit/integration tests
├── go.mod                       # Module: github.com/zveinn/git-feed
├── go.sum
├── README.md
├── CLAUDE.md
├── .goreleaser.yml
└── .github/
    └── workflows/
        └── release.yml

~/.git-feed/                     # Config directory (auto-created)
 ├── .env                        # Shared configuration file
 ├── github.db                   # GitHub cache DB (created after running with --platform github)
 └── gitlab.db                   # GitLab cache DB (created after running with --platform gitlab)
```

## Testing

```bash
go test ./... -count=1
```
