package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

type PRActivity struct {
	Label      string
	Owner      string
	Repo       string
	MR         MergeRequestModel
	UpdatedAt  time.Time
	HasUpdates bool
	Issues     []IssueActivity
}

type IssueActivity struct {
	Label      string
	Owner      string
	Repo       string
	Issue      IssueModel
	UpdatedAt  time.Time
	HasUpdates bool
}

type MergeRequestModel struct {
	Number    int
	Title     string
	Body      string
	State     string
	UpdatedAt time.Time
	WebURL    string
	UserLogin string
	Merged    bool
}

type IssueModel struct {
	Number    int
	Title     string
	Body      string
	State     string
	UpdatedAt time.Time
	WebURL    string
	UserLogin string
}

type CommentModel struct {
	Body string
}

type Progress struct {
	current atomic.Int32
	total   atomic.Int32
}

type Config struct {
	debugMode      bool
	localMode      bool
	gitlabUserID   int64
	githubToken    string
	githubUsername string
	showLinks      bool
	timeRange      time.Duration
	gitlabUsername string
	allowedRepos   map[string]bool
	gitlabClient   *gitlab.Client
	db             *Database
	progress       *Progress
	ctx            context.Context
	dbErrorCount   atomic.Int32
}

var config Config

func (p *Progress) increment() {
	p.current.Add(1)
}

func (p *Progress) addToTotal(n int) {
	p.total.Add(int32(n))
}

func (p *Progress) buildBar(current, total int32) (string, *color.Color, float64) {
	percentage := float64(current) / float64(total) * 100
	filled := int(percentage / 2)
	var barContent string
	for i := range 50 {
		if i < filled {
			barContent += "="
		} else if i == filled {
			barContent += ">"
		} else {
			barContent += " "
		}
	}
	var barColor *color.Color
	if percentage < 33 {
		barColor = color.New(color.FgRed)
	} else if percentage < 66 {
		barColor = color.New(color.FgYellow)
	} else {
		barColor = color.New(color.FgGreen)
	}
	return barContent, barColor, percentage
}

func (p *Progress) display() {
	current := p.current.Load()
	total := p.total.Load()
	barContent, barColor, percentage := p.buildBar(current, total)
	fmt.Printf("\r[%s] %s/%s (%s) ",
		barColor.Sprint(barContent),
		color.New(color.FgCyan).Sprint(current),
		color.New(color.FgCyan).Sprint(total),
		barColor.Sprintf("%.0f%%", percentage))
}

func (p *Progress) displayWithWarning(message string) {
	current := p.current.Load()
	total := p.total.Load()
	barContent, barColor, percentage := p.buildBar(current, total)
	fmt.Printf("\r[%s] %s/%s (%s) %s ",
		barColor.Sprint(barContent),
		color.New(color.FgCyan).Sprint(current),
		color.New(color.FgCyan).Sprint(total),
		barColor.Sprintf("%.0f%%", percentage),
		color.New(color.FgYellow).Sprint("! "+message))
}

func getLabelColor(label string) *color.Color {
	labelColors := map[string]*color.Color{
		"Authored":         color.New(color.FgCyan),
		"Mentioned":        color.New(color.FgYellow),
		"Assigned":         color.New(color.FgMagenta),
		"Commented":        color.New(color.FgBlue),
		"Reviewed":         color.New(color.FgGreen),
		"Review Requested": color.New(color.FgRed),
		"Involved":         color.New(color.FgHiBlack),
		"Recent Activity":  color.New(color.FgHiCyan),
	}

	if c, ok := labelColors[label]; ok {
		return c
	}
	return color.New(color.FgWhite)
}

func getUserColor(username string) *color.Color {
	h := fnv.New32a()
	h.Write([]byte(username))
	hash := h.Sum32()

	colors := []*color.Color{
		color.New(color.FgHiGreen),
		color.New(color.FgHiYellow),
		color.New(color.FgHiBlue),
		color.New(color.FgHiMagenta),
		color.New(color.FgHiCyan),
		color.New(color.FgHiRed),
		color.New(color.FgGreen),
		color.New(color.FgYellow),
		color.New(color.FgBlue),
		color.New(color.FgMagenta),
		color.New(color.FgCyan),
	}

	return colors[hash%uint32(len(colors))]
}

func getStateColor(state string) *color.Color {
	switch state {
	case "open":
		return color.New(color.FgGreen)
	case "closed":
		return color.New(color.FgRed)
	case "merged":
		return color.New(color.FgMagenta)
	default:
		return color.New(color.FgWhite)
	}
}

func loadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.TrimSpace(parts[1])
			if _, exists := os.LookupEnv(key); exists {
				continue
			}
			os.Setenv(key, value)
		}
	}

	return scanner.Err()
}

func parseTimeRange(timeStr string) (time.Duration, error) {
	if len(timeStr) < 2 {
		return 0, fmt.Errorf("invalid time range format: %s (expected format like 1h, 2d, 3w, 4m, 1y)", timeStr)
	}

	numStr := timeStr[:len(timeStr)-1]
	unit := timeStr[len(timeStr)-1:]

	num, err := strconv.Atoi(numStr)
	if err != nil || num < 1 {
		return 0, fmt.Errorf("invalid time range number: %s (must be a positive integer)", numStr)
	}

	var duration time.Duration
	switch unit {
	case "h":
		duration = time.Duration(num) * time.Hour
	case "d":
		duration = time.Duration(num) * 24 * time.Hour
	case "w":
		duration = time.Duration(num) * 7 * 24 * time.Hour
	case "m":
		duration = time.Duration(num) * 30 * 24 * time.Hour
	case "y":
		duration = time.Duration(num) * 365 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid time unit: %s (use h=hours, d=days, w=weeks, m=months, y=years)", unit)
	}

	return duration, nil
}

func resolveAllowedRepos(platform, allowedReposFlag string) string {
	if value := strings.TrimSpace(allowedReposFlag); value != "" {
		return value
	}

	platformVar := "GITHUB_ALLOWED_REPOS"
	if platform == "gitlab" {
		platformVar = "GITLAB_ALLOWED_REPOS"
	}

	if value := strings.TrimSpace(os.Getenv(platformVar)); value != "" {
		return value
	}

	return strings.TrimSpace(os.Getenv("ALLOWED_REPOS"))
}

func main() {
	// Define flags
	var timeRangeStr string
	var platform string
	var debugMode bool
	var localMode bool
	var showLinks bool
	var llMode bool
	var allowedReposFlag string
	var cleanCache bool

	flag.StringVar(&timeRangeStr, "time", "1m", "Show items from last time range (1h, 2d, 3w, 4m, 1y)")
	flag.StringVar(&platform, "platform", "github", "Platform to use (gitlab|github)")
	flag.BoolVar(&debugMode, "debug", false, "Show detailed API logging")
	flag.BoolVar(&localMode, "local", false, "Use local database instead of platform API")
	flag.BoolVar(&showLinks, "links", false, "Show hyperlinks underneath each PR/issue")
	flag.BoolVar(&llMode, "ll", false, "Shortcut for --local --links (offline mode with links)")
	flag.BoolVar(&cleanCache, "clean", false, "Delete and recreate the database cache")
	flag.StringVar(&allowedReposFlag, "allowed-repos", "", "Comma-separated list of allowed repos (GitHub: owner/repo; GitLab: group[/subgroup]/repo)")

	// Custom usage message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "Git Feed - Monitor pull requests and issues across repositories")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nEnvironment Variables:")
		fmt.Fprintln(os.Stderr, "  GITLAB_TOKEN or GITLAB_ACTIVITY_TOKEN  - GitLab Personal Access Token")
		fmt.Fprintln(os.Stderr, "  GITLAB_USERNAME or GITLAB_USER         - Optional GitLab username")
		fmt.Fprintln(os.Stderr, "  GITLAB_HOST                            - Optional GitLab host (overrides GITLAB_BASE_URL when set)")
		fmt.Fprintln(os.Stderr, "  GITLAB_BASE_URL                        - Optional GitLab base URL (default: https://gitlab.com)")
		fmt.Fprintln(os.Stderr, "  GITHUB_TOKEN                           - GitHub Personal Access Token")
		fmt.Fprintln(os.Stderr, "  GITHUB_USERNAME                        - Required in GitHub online mode")
		fmt.Fprintln(os.Stderr, "  GITHUB_ALLOWED_REPOS                   - Optional in GitHub online mode (owner/repo)")
		fmt.Fprintln(os.Stderr, "  GITLAB_ALLOWED_REPOS                   - Required in GitLab online mode (group[/subgroup]/repo)")
		fmt.Fprintln(os.Stderr, "  ALLOWED_REPOS                          - Legacy fallback when platform-specific vars are unset")
		fmt.Fprintln(os.Stderr, "\nConfiguration File:")
		fmt.Fprintln(os.Stderr, "  ~/.git-feed/.env                       - Shared configuration file (auto-created)")
		fmt.Fprintln(os.Stderr, "  ~/.git-feed/github.db|gitlab.db        - Platform-specific cache databases")
	}

	flag.Parse()

	// Handle --ll shortcut
	if llMode {
		localMode = true
		showLinks = true
	}

	platform = strings.ToLower(strings.TrimSpace(platform))
	if platform != "gitlab" && platform != "github" {
		fmt.Printf("Error: invalid --platform value %q (allowed: gitlab|github)\n", platform)
		os.Exit(1)
	}

	// Parse time range
	timeRange, err := parseTimeRange(timeRangeStr)
	if err != nil {
		fmt.Printf("Error: %v\n", err)
		fmt.Println("Examples: --time 1h (1 hour), --time 2d (2 days), --time 3w (3 weeks), --time 4m (4 months), --time 1y (1 year)")
		os.Exit(1)
	}

	homeDir, err := os.UserHomeDir()
	if err != nil {
		fmt.Printf("Error: Could not determine home directory: %v\n", err)
		os.Exit(1)
	}

	configDir := filepath.Join(homeDir, ".git-feed")
	dbFileName := "github.db"
	if platform == "gitlab" {
		dbFileName = "gitlab.db"
	}

	envTemplate := `# Activity Feed Configuration
# Shared environment file for both platforms

# =========================
# GitHub (--platform github)
# =========================

# Required in GitHub online mode
GITHUB_TOKEN=
GITHUB_USERNAME=

	# Optional in GitHub online mode
	# Comma-separated owner/repo values
	# Example: owner/repo,owner/another-repo
	GITHUB_ALLOWED_REPOS=

# =========================
# GitLab (--platform gitlab)
# =========================

# Required in GitLab online mode
# Recommended scope: read_api (or api on some self-managed instances)
GITLAB_TOKEN=
# Optional alternative token variable supported by the app
GITLAB_ACTIVITY_TOKEN=

# Optional username (the app can also resolve current user via API)
GITLAB_USERNAME=

# Optional: GitLab host for self-managed/cloud instances
# If set, this overrides GITLAB_BASE_URL.
GITLAB_HOST=

# Optional: full GitLab base URL (supports path prefixes)
# Default: https://gitlab.com
GITLAB_BASE_URL=https://gitlab.com

	# Required in GitLab online mode
	# Comma-separated group[/subgroup]/repo values
	# Example: team/repo,platform/backend/git-feed
	GITLAB_ALLOWED_REPOS=

	# Legacy fallback when platform-specific vars are unset
	ALLOWED_REPOS=
	`

	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Printf("Error: Could not create config directory %s: %v\n", configDir, err)
		os.Exit(1)
	}

	envPath := filepath.Join(configDir, ".env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		if err := os.WriteFile(envPath, []byte(envTemplate), 0o600); err != nil {
			fmt.Printf("Warning: Could not create .env file at %s: %v\n", envPath, err)
		}
	}

	_ = loadEnvFile(envPath)

	allowedReposStr := resolveAllowedRepos(platform, allowedReposFlag)

	var allowedRepos map[string]bool
	if allowedReposStr != "" {
		allowedRepos = make(map[string]bool)
		repos := strings.Split(allowedReposStr, ",")
		for _, repo := range repos {
			repo = strings.TrimSpace(repo)
			if repo != "" {
				allowedRepos[repo] = true
			}
		}
		if debugMode && len(allowedRepos) > 0 {
			fmt.Printf("Filtering to allowed repositories: %v\n", allowedRepos)
		}
	}

	dbPath := filepath.Join(configDir, dbFileName)

	if cleanCache {
		fmt.Println("Cleaning database cache...")
		if _, err := os.Stat(dbPath); err == nil {
			if err := os.Remove(dbPath); err != nil {
				fmt.Printf("Warning: Failed to delete database file: %v\n", err)
			} else {
				fmt.Println("Database cache cleaned successfully")
			}
		} else {
			fmt.Println("No existing database cache to clean")
		}
	}

	db, err := OpenDatabase(dbPath)
	if err != nil {
		fmt.Printf("Warning: Failed to open database: %v\n", err)
		fmt.Println("Continuing without database caching...")
		db = nil
	} else {
		defer db.Close()
	}

	var token string
	if platform == "gitlab" {
		token = os.Getenv("GITLAB_ACTIVITY_TOKEN")
		if token == "" {
			token = os.Getenv("GITLAB_TOKEN")
		}
	} else {
		token = os.Getenv("GITHUB_TOKEN")
	}

	githubUsername := strings.TrimSpace(os.Getenv("GITHUB_USERNAME"))

	normalizedGitLabBaseURL := ""
	if platform == "gitlab" {
		rawGitLabHost := os.Getenv("GITLAB_HOST")
		rawGitLabBaseURL := os.Getenv("GITLAB_BASE_URL")
		selectedGitLabBaseURL := rawGitLabBaseURL
		if strings.TrimSpace(rawGitLabHost) != "" {
			selectedGitLabBaseURL = rawGitLabHost
		}

		normalizedGitLabBaseURL, err = normalizeGitLabBaseURL(selectedGitLabBaseURL)
		if err != nil {
			if strings.TrimSpace(selectedGitLabBaseURL) != "" {
				fmt.Printf("Configuration Error: %v\n", err)
				os.Exit(1)
			}

			normalizedGitLabBaseURL, _ = normalizeGitLabBaseURL("")
		}
	}

	var gitlabClient *gitlab.Client
	gitlabUsername := ""
	var gitlabUserID int64
	if platform == "gitlab" && !localMode && token != "" {
		rawGitLabHost := os.Getenv("GITLAB_HOST")
		rawGitLabBaseURL := os.Getenv("GITLAB_BASE_URL")
		selectedGitLabBaseURL := rawGitLabBaseURL
		if strings.TrimSpace(rawGitLabHost) != "" {
			selectedGitLabBaseURL = rawGitLabHost
		}

		client, _, err := newGitLabClient(token, selectedGitLabBaseURL)
		if err != nil {
			fmt.Printf("Configuration Error: %v\n", err)
			os.Exit(1)
		}
		gitlabClient = client

		currentUser, _, err := gitlabClient.Users.CurrentUser(gitlab.WithContext(context.Background()))
		if err != nil {
			fmt.Printf("Configuration Error: failed to fetch GitLab current user: %v\n", err)
			os.Exit(1)
		}
		gitlabUsername = strings.TrimSpace(currentUser.Username)
		gitlabUserID = currentUser.ID
		if gitlabUsername == "" {
			fmt.Println("Configuration Error: GitLab current user has empty username")
			os.Exit(1)
		}
	}

	// Validate configuration
	if err := validateConfig(platform, token, githubUsername, localMode, envPath, allowedRepos); err != nil {
		fmt.Printf("Configuration Error: %v\n\n", err)
		os.Exit(1)
	}

	if debugMode {
		if platform == "gitlab" {
			fmt.Println("Monitoring GitLab merge request and issue activity")
			fmt.Printf("GitLab API base URL: %s\n", normalizedGitLabBaseURL)
		} else {
			fmt.Println("Monitoring GitHub pull request and issue activity")
		}
		fmt.Printf("Showing items from the last %v\n", timeRange)
	}
	if debugMode {
		fmt.Println("Debug mode enabled")
	}

	config.debugMode = debugMode
	config.localMode = localMode
	config.gitlabUserID = gitlabUserID
	config.githubToken = token
	config.githubUsername = githubUsername
	config.showLinks = showLinks
	config.timeRange = timeRange
	config.gitlabUsername = gitlabUsername
	config.allowedRepos = allowedRepos
	config.db = db
	config.ctx = context.Background()
	config.gitlabClient = gitlabClient

	fetchAndDisplayActivity(platform)
}

func validateConfig(platform, token, githubUsername string, localMode bool, envPath string, allowedRepos map[string]bool) error {
	if localMode {
		return nil // No validation needed for offline mode
	}

	switch platform {
	case "gitlab":
		if token == "" {
			return fmt.Errorf("token is required for GitLab API mode.\n\nTo fix this:\n  - Set GITLAB_TOKEN or GITLAB_ACTIVITY_TOKEN\n  - Or add it to %s", envPath)
		}
		if len(allowedRepos) == 0 {
			return fmt.Errorf("GITLAB_ALLOWED_REPOS is required for GitLab API mode to keep API usage bounded.\n\nTo fix this:\n  - Set GITLAB_ALLOWED_REPOS with group[/subgroup]/repo paths\n  - Example: GITLAB_ALLOWED_REPOS=team/service,platform/backend/git-feed\n  - Or use legacy fallback ALLOWED_REPOS\n  - Or add it to %s", envPath)
		}
	case "github":
		if token == "" {
			return fmt.Errorf("token is required for GitHub API mode.\n\nTo fix this:\n  - Set GITHUB_TOKEN\n  - Or add it to %s", envPath)
		}
		if githubUsername == "" {
			return fmt.Errorf("username is required for GitHub API mode.\n\nTo fix this:\n  - Set GITHUB_USERNAME\n  - Or add it to %s", envPath)
		}
	default:
		return fmt.Errorf("unsupported platform %q", platform)
	}
	return nil
}

func fetchAndDisplayActivity(platform string) {
	switch platform {
	case "gitlab":
		fetchAndDisplayGitLabActivity()
	case "github":
		fetchAndDisplayGitHubActivity()
	default:
		fmt.Printf("Unsupported platform: %s\n", platform)
	}
}

type DisplayConfig struct {
	Owner      string
	Repo       string
	Number     int
	Title      string
	User       string
	UpdatedAt  time.Time
	WebURL     string
	Label      string
	HasUpdates bool
	IsIndented bool
	State      string
}

func displayItem(cfg DisplayConfig) {
	dateStr := "          "
	if !cfg.UpdatedAt.IsZero() {
		dateStr = cfg.UpdatedAt.Format("2006/01/02")
	}

	indent := ""
	linkIndent := "   "
	if cfg.IsIndented && cfg.State != "" {
		state := strings.ToUpper(cfg.State)
		stateColor := getStateColor(cfg.State)
		indent = fmt.Sprintf("-- %s ", stateColor.Sprint(state))
		linkIndent = "      "
	}

	labelColor := getLabelColor(cfg.Label)
	userColor := getUserColor(cfg.User)

	updateIcon := ""
	if cfg.HasUpdates {
		updateIcon = color.New(color.FgYellow, color.Bold).Sprint("● ")
	}

	repoDisplay := ""
	if cfg.Repo == "" {
		repoDisplay = fmt.Sprintf("%s#%d", cfg.Owner, cfg.Number)
	} else {
		repoDisplay = fmt.Sprintf("%s/%s#%d", cfg.Owner, cfg.Repo, cfg.Number)
	}

	fmt.Printf("%s%s%s %s %s %s - %s\n",
		updateIcon,
		indent,
		dateStr,
		labelColor.Sprint(strings.ToUpper(cfg.Label)),
		userColor.Sprint(cfg.User),
		repoDisplay,
		cfg.Title,
	)

	if config.showLinks && cfg.WebURL != "" {
		fmt.Printf("%s🔗 %s\n", linkIndent, cfg.WebURL)
	}
}

func displayMergeRequest(label, owner, repo string, mr MergeRequestModel, hasUpdates bool) {
	displayItem(DisplayConfig{
		Owner:      owner,
		Repo:       repo,
		Number:     mr.Number,
		Title:      mr.Title,
		User:       mr.UserLogin,
		UpdatedAt:  mr.UpdatedAt,
		WebURL:     mr.WebURL,
		Label:      label,
		HasUpdates: hasUpdates,
		IsIndented: false,
	})
}

func displayIssue(label, owner, repo string, issue IssueModel, indented bool, hasUpdates bool) {
	displayItem(DisplayConfig{
		Owner:      owner,
		Repo:       repo,
		Number:     issue.Number,
		Title:      issue.Title,
		User:       issue.UserLogin,
		UpdatedAt:  issue.UpdatedAt,
		WebURL:     issue.WebURL,
		Label:      label,
		HasUpdates: hasUpdates,
		IsIndented: indented,
		State:      issue.State,
	})
}
