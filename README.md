# GitAI - GitHub Activity Monitor

A fast, colorful CLI tool for monitoring GitHub pull requests/issues and GitLab merge requests/issues across repositories. Track your contributions, reviews, and assignments with real-time progress visualization.

## Features

- 🚀 **Parallel API Calls** - Fetches data concurrently for maximum speed
- 🎨 **Colorized Output** - Easy-to-read color-coded labels, states, and progress
- 📊 **Smart Cross-Referencing** - Automatically links related PRs and issues
- ⚡ **Real-Time Progress Bar** - Visual feedback with color-coded completion status
- 🔍 **Comprehensive Search** - Tracks authored, mentioned, assigned, commented, and reviewed items
- 📅 **Time Filtering** - View items from the last month by default (configurable with `--time`)
- 🎯 **Organized Display** - Separates open, merged, and closed items into clear sections

## Installation

### Pre-built Binaries (Recommended)

Download the latest release for your platform from the [releases page](https://github.com/zveinn/git-feed/releases):

**macOS**
```bash
# Intel Mac
curl -L https://github.com/zveinn/git-feed/releases/latest/download/git-feed_<VERSION>_Darwin_x86_64.tar.gz | tar xz
chmod +x git-feed
sudo mv git-feed /usr/local/bin/

# Apple Silicon Mac
curl -L https://github.com/zveinn/git-feed/releases/latest/download/git-feed_<VERSION>_Darwin_arm64.tar.gz | tar xz
chmod +x git-feed
sudo mv git-feed /usr/local/bin/
```

**Linux**
```bash
# x86_64
curl -L https://github.com/zveinn/git-feed/releases/latest/download/git-feed_<VERSION>_Linux_x86_64.tar.gz | tar xz
chmod +x git-feed
sudo mv git-feed /usr/local/bin/

# ARM64
curl -L https://github.com/zveinn/git-feed/releases/latest/download/git-feed_<VERSION>_Linux_arm64.tar.gz | tar xz
chmod +x git-feed
sudo mv git-feed /usr/local/bin/
```

**Windows**

Download the appropriate `.zip` file from the releases page, extract it, and add `git-feed.exe` to your PATH.

### Build from Source

```bash
go build -o git-feed .
```

### Release Management

Releases are automatically built and published via GitHub Actions using GoReleaser:

```bash
# Create a new release
git tag -a v1.0.0 -m "Release v1.0.0"
git push origin v1.0.0
```

This will automatically:
- Build binaries for Linux (amd64, arm64), macOS (Intel, Apple Silicon), and Windows (amd64)
- Generate checksums for all releases
- Create a GitHub release with installation instructions
- Publish all artifacts to the releases page

## Configuration

### First Run Setup

On first run, GitAI automatically creates a configuration directory at `~/.git-feed/` with:
- `.env` - Shared configuration file for GitHub and GitLab
- `github.db` - Local database for caching GitHub data
- `gitlab.db` - Local database for caching GitLab data

### GitHub Token Setup

Create a GitHub Personal Access Token with the following scopes:
- `repo` - Access to repositories
- `read:org` - Read organization data

**Generate token:** https://github.com/settings/tokens

### Environment Setup

You can provide credentials via a shared env file or environment variables. Select platform with `--platform github|gitlab` (default: `github`).

**Option 1: Configuration File (Recommended)**

Edit `~/.git-feed/.env` and add your credentials:
```bash
# GitHub (`--platform github`)
# Required in GitHub online mode
GITHUB_TOKEN=your_token_here
GITHUB_USERNAME=your_username

# Optional in GitHub online mode
GITHUB_ALLOWED_REPOS=user/repo1,user/repo2

# GitLab (`--platform gitlab`)
# Required in GitLab online mode
GITLAB_TOKEN=your_token_here
# Optional alternative token variable
GITLAB_ACTIVITY_TOKEN=

# Optional host/base URL settings
GITLAB_HOST=
GITLAB_BASE_URL=https://gitlab.com

# Required in GitLab online mode
GITLAB_ALLOWED_REPOS=group/repo1,group/subgroup/repo2

# Legacy fallback used only when platform-specific vars are unset
ALLOWED_REPOS=
```

**Option 2: Environment Variables**
```bash
export GITHUB_TOKEN="your_token_here"
export GITHUB_USERNAME="your_username"
export GITHUB_ALLOWED_REPOS="user/repo1,user/repo2"  # Optional in GitHub mode

export GITLAB_TOKEN="your_token_here"
export GITLAB_ALLOWED_REPOS="group/repo1,group/subgroup/repo2"  # Required in GitLab mode
```

**Note:** Environment variables take precedence over the `.env` file.

## Usage

### Basic Usage

```bash
# Monitor items from the last month (default, platform=github)
git-feed

# Explicit platform
git-feed --platform github
git-feed --platform gitlab

# Show items from the last 3 hours
git-feed --time 3h

# Show items from the last 2 days
git-feed --time 2d

# Show items from the last 3 weeks
git-feed --time 3w

# Show items from the last 6 months
git-feed --time 6m

# Show items from the last year
git-feed --time 1y

# Show detailed logging output
git-feed --debug

# Use local database instead of GitHub API (offline mode)
git-feed --local

# Show hyperlinks underneath each PR/issue
git-feed --links

# Delete and recreate the database cache (start fresh)
git-feed --clean

# Filter to specific repositories only
git-feed --allowed-repos="user/repo1,user/repo2"

# Quick offline mode with links (combines --local and --links)
git-feed --ll

# Combine flags
git-feed --local --time 2w --debug --links --allowed-repos="miniohq/ec,tunnels-is/tunnels"
```

### Command Line Options

| Flag | Description |
|------|-------------|
| `--time RANGE` | Show items from the last time range (default: `1m`)<br>Examples: `1h` (hour), `2d` (days), `3w` (weeks), `4m` (months), `1y` (year) |
| `--platform PLATFORM` | Activity source platform: `github` or `gitlab` (default: `github`) |
| `--debug` | Show detailed API call progress instead of progress bar |
| `--local` | Use local database instead of platform API (offline mode, no token required) |
| `--links` | Show hyperlinks (with 🔗 icon) underneath each PR and issue |
| `--ll` | Shortcut for `--local --links` (offline mode with links) |
| `--clean` | Delete and recreate the database cache (useful for starting fresh or fixing corrupted cache) |
| `--allowed-repos REPOS` | Filter to specific repositories (GitHub: `owner/repo1`; GitLab: `group[/subgroup]/repo`) |

### Color Coding

**Labels:**
- `AUTHORED` - Cyan
- `MENTIONED` - Yellow
- `ASSIGNED` - Magenta
- `COMMENTED` - Blue
- `REVIEWED` - Green
- `REVIEW REQUESTED` - Red
- `INVOLVED` - Gray

**States:**
- `OPEN` - Green
- `CLOSED` - Red
- `MERGED` - Magenta

**Usernames:** Each user gets a consistent color based on hash

## How It Works

### Online Mode (Default)

1. **Parallel Fetching** - Simultaneously searches for activity on the selected platform:
   - PRs you authored
   - PRs where you're mentioned
   - PRs assigned to you
   - PRs you commented on
   - PRs you reviewed
   - PRs requesting your review
   - PRs involving you
   - Your recent activity events
   - Issues you authored/mentioned/assigned/commented

2. **Local Caching** - All fetched data is automatically saved to a local BBolt database (`~/.git-feed/github.db` for GitHub or `~/.git-feed/gitlab.db` for GitLab)
   - MRs/PRs, issues, and comments/notes are cached for offline access
   - Each item is stored/updated with a unique key
   - Database grows as you fetch more data

3. **Cross-Reference Detection** - Automatically finds connections between PRs and issues by:
   - Checking PR body and comments for issue references (`#123`, `fixes #123`, full URLs)
   - Checking issue body and comments for PR references
   - Displaying linked issues directly under their related PRs

4. **Smart Filtering**:
   - Shows both open and closed items from the specified time period
   - **Default**: Items updated in last month (`1m`)
   - **Custom**: Use `--time` with values like `1h`, `2d`, `3w`, `6m`, `1y`

### Offline Mode (`--local`)

- Reads all data from the selected local database instead of platform APIs
- No internet connection or API token required
- Displays all cached PR/MR and issue activity
- Useful for:
  - Working offline
  - Faster lookups when you don't need fresh data
  - Reviewing previously fetched data

## API Rate Limits

GitAI monitors GitHub API rate limits and will warn you when running low:
- **Search API**: 30 requests per minute
- **Core API**: 5000 requests per hour

Rate limit status is displayed in debug mode.

### Automatic Retry & Backoff

When rate limits are hit, GitAI automatically retries with exponential backoff:
- Detects rate limit errors (429, 403 responses)
- Waits progressively longer between retries (1s → 2s → 4s → ... up to 30s max)
- Continues indefinitely until the request succeeds
- Shows clear warnings: `⚠ Rate limit hit, waiting [duration] before retry...`
- No manual intervention required - the tool handles rate limits gracefully

## Troubleshooting

### "GITHUB_TOKEN environment variable is required"
Set up your GitHub token (`GITHUB_TOKEN`) for `--platform github`, or GitLab token (`GITLAB_TOKEN` / `GITLAB_ACTIVITY_TOKEN`) plus `GITLAB_ALLOWED_REPOS` for `--platform gitlab`.

### "Rate limit exceeded"
Wait for the rate limit to reset. Use `--debug` to see current rate limits.

### Progress bar looks garbled
Your terminal may not support ANSI colors properly. Use `--debug` mode for plain text output.

## Development

### Project Structure
```
git-feed/
├── main.go                      # Main application code
├── platform_github.go           # GitHub platform implementation
├── platform_gitlab.go           # GitLab platform implementation
├── db.go                        # Database operations for caching GitHub data
├── README.md                    # This file
├── CLAUDE.md                    # Instructions for Claude Code AI assistant
├── .goreleaser.yml              # GoReleaser configuration for builds
├── .github/
│   └── workflows/
│       └── release.yml          # GitHub Actions workflow for releases

~/.git-feed/                  # Config directory (auto-created)
 ├── .env                     # Shared configuration file
 ├── github.db                # BBolt database for GitHub cache
 └── gitlab.db                # BBolt database for GitLab cache
```

### Testing Releases Locally

You can test the GoReleaser build locally before pushing a tag:

```bash
# Install goreleaser
go install github.com/goreleaser/goreleaser/v2@latest

# Test the build (creates snapshot without publishing)
goreleaser release --snapshot --clean

# Check the dist/ folder for built binaries
ls -la dist/
```

## License

MIT License - Feel free to use and modify as needed.
