package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fatih/color"
	"github.com/google/go-github/v57/github"
)

type PRActivity struct {
	Label      string
	Owner      string
	Repo       string
	PR         *github.PullRequest
	UpdatedAt  time.Time
	HasUpdates bool
	Issues     []IssueActivity
}

type IssueActivity struct {
	Label      string
	Owner      string
	Repo       string
	Issue      *github.Issue
	UpdatedAt  time.Time
	HasUpdates bool
}

type Progress struct {
	current atomic.Int32
	total   atomic.Int32
}

type Config struct {
	debugMode     bool
	localMode     bool
	showLinks     bool
	timeRange     time.Duration
	username      string
	allowedRepos  map[string]bool
	client        *github.Client
	db            *Database
	progress      *Progress
	ctx           context.Context
	dbErrorCount  atomic.Int32
}

var config Config

func getPRLabelPriority(label string) int {
	priorities := map[string]int{
		"Authored":         1,
		"Assigned":         2,
		"Reviewed":         3,
		"Review Requested": 4,
		"Commented":        5,
		"Mentioned":        6,
	}
	if priority, ok := priorities[label]; ok {
		return priority
	}
	return 999 // Unknown labels get lowest priority
}

func getIssueLabelPriority(label string) int {
	priorities := map[string]int{
		"Authored":  1,
		"Assigned":  2,
		"Commented": 3,
		"Mentioned": 4,
	}
	if priority, ok := priorities[label]; ok {
		return priority
	}
	return 999 // Unknown labels get lowest priority
}

func shouldUpdateLabel(currentLabel, newLabel string, isPR bool) bool {
	if currentLabel == "" {
		return true
	}

	var currentPriority, newPriority int
	if isPR {
		currentPriority = getPRLabelPriority(currentLabel)
		newPriority = getPRLabelPriority(newLabel)
	} else {
		currentPriority = getIssueLabelPriority(currentLabel)
		newPriority = getIssueLabelPriority(newLabel)
	}

	return newPriority < currentPriority
}

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

func retryWithBackoff(operation func() error, operationName string) error {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
		backoffFactor  = 1.5
	)

	backoff := initialBackoff
	attempt := 1

	for {
		err := operation()
		if err == nil {
			return nil
		}

		// Check if this is a GitHub rate limit error with reset time
		var rateLimitErr *github.RateLimitError
		var abuseRateLimitErr *github.AbuseRateLimitError
		var waitTime time.Duration
		var isRateLimitError bool

		if errors.As(err, &rateLimitErr) {
			// Primary rate limit error - use the reset time from the error
			isRateLimitError = true
			resetTime := rateLimitErr.Rate.Reset.Time
			waitTime = time.Until(resetTime)

			// Add a small buffer to ensure the rate limit has definitely reset
			waitTime += 2 * time.Second

			// Cap at a reasonable maximum to avoid waiting forever if clock is wrong
			if waitTime > 1*time.Hour {
				waitTime = 1 * time.Hour
			}

			// Ensure we wait at least 1 second
			if waitTime < 1*time.Second {
				waitTime = 1 * time.Second
			}

			if config.debugMode {
				fmt.Printf("  [%s] Rate limit hit (attempt %d), reset at %v, waiting %v before retry...\n",
					operationName, attempt, resetTime.Format("15:04:05"), waitTime.Round(time.Second))
			}
		} else if errors.As(err, &abuseRateLimitErr) {
			// Secondary/abuse rate limit error - use RetryAfter if available
			isRateLimitError = true
			if abuseRateLimitErr.RetryAfter != nil {
				waitTime = *abuseRateLimitErr.RetryAfter
			} else {
				// If no RetryAfter specified, use a default wait time
				waitTime = 60 * time.Second
			}

			if config.debugMode {
				fmt.Printf("  [%s] Abuse rate limit hit (attempt %d), waiting %v before retry...\n",
					operationName, attempt, waitTime.Round(time.Second))
			}
		} else {
			// Check if error message suggests rate limiting (fallback for older errors)
			isRateLimitError = strings.Contains(err.Error(), "rate limit") ||
				strings.Contains(err.Error(), "API rate limit exceeded") ||
				strings.Contains(err.Error(), "403")

			if isRateLimitError {
				// Fallback to exponential backoff if we can't extract reset time
				waitTime = time.Duration(math.Min(float64(backoff), float64(maxBackoff)))
				if config.debugMode {
					fmt.Printf("  [%s] Rate limit hit (attempt %d), waiting %v before retry...\n",
						operationName, attempt, waitTime)
				}
			}
		}

		if isRateLimitError {
			if config.debugMode {
				select {
				case <-config.ctx.Done():
					return config.ctx.Err()
				case <-time.After(waitTime):
				}
			} else {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()

				remaining := int(waitTime.Seconds())
				for remaining > 0 {
					if config.progress != nil {
						config.progress.displayWithWarning(fmt.Sprintf("Rate limit hit, retrying in %ds", remaining))
					}

					select {
					case <-config.ctx.Done():
						return config.ctx.Err()
					case <-ticker.C:
						remaining--
					}
				}
			}

			backoff = time.Duration(float64(backoff) * backoffFactor)
		} else {
			// Non-rate-limit error - use exponential backoff
			waitTime := time.Duration(math.Min(float64(backoff)/2, float64(5*time.Second)))

			if config.debugMode {
				fmt.Printf("  [%s] Error (attempt %d): %v, waiting %v before retry...\n",
					operationName, attempt, err, waitTime)
				select {
				case <-config.ctx.Done():
					return config.ctx.Err()
				case <-time.After(waitTime):
				}
			} else {
				ticker := time.NewTicker(1 * time.Second)
				defer ticker.Stop()

				remaining := int(waitTime.Seconds())
				for remaining > 0 {
					if config.progress != nil {
						config.progress.displayWithWarning(fmt.Sprintf("API error, retrying in %ds", remaining))
					}

					select {
					case <-config.ctx.Done():
						return config.ctx.Err()
					case <-ticker.C:
						remaining--
					}
				}
			}

			backoff = time.Duration(float64(backoff) * backoffFactor)
		}

		attempt++
	}
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

func main() {
	// Define flags
	var timeRangeStr string
	var debugMode bool
	var localMode bool
	var showLinks bool
	var llMode bool
	var allowedReposFlag string
	var cleanCache bool

	flag.StringVar(&timeRangeStr, "time", "1m", "Show items from last time range (1h, 2d, 3w, 4m, 1y)")
	flag.BoolVar(&debugMode, "debug", false, "Show detailed API logging")
	flag.BoolVar(&localMode, "local", false, "Use local database instead of GitHub API")
	flag.BoolVar(&showLinks, "links", false, "Show hyperlinks underneath each PR/issue")
	flag.BoolVar(&llMode, "ll", false, "Shortcut for --local --links (offline mode with links)")
	flag.BoolVar(&cleanCache, "clean", false, "Delete and recreate the database cache")
	flag.StringVar(&allowedReposFlag, "allowed-repos", "", "Comma-separated list of allowed repos (e.g., user/repo1,user/repo2)")

	// Custom usage message
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: %s [options]\n\n", os.Args[0])
		fmt.Fprintln(os.Stderr, "GitHub Feed - Monitor GitHub pull requests and issues across repositories")
		fmt.Fprintln(os.Stderr, "\nOptions:")
		flag.PrintDefaults()
		fmt.Fprintln(os.Stderr, "\nEnvironment Variables:")
		fmt.Fprintln(os.Stderr, "  GITHUB_TOKEN or GITHUB_ACTIVITY_TOKEN - GitHub Personal Access Token")
		fmt.Fprintln(os.Stderr, "  GITHUB_USERNAME or GITHUB_USER         - Your GitHub username")
		fmt.Fprintln(os.Stderr, "  ALLOWED_REPOS                          - Comma-separated list of allowed repos")
		fmt.Fprintln(os.Stderr, "\nConfiguration File:")
		fmt.Fprintln(os.Stderr, "  ~/.github-feed/.env                    - Configuration file (auto-created)")
	}

	flag.Parse()

	// Handle --ll shortcut
	if llMode {
		localMode = true
		showLinks = true
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

	configDir := filepath.Join(homeDir, ".github-feed")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		fmt.Printf("Error: Could not create config directory %s: %v\n", configDir, err)
		os.Exit(1)
	}

	envPath := filepath.Join(configDir, ".env")
	if _, err := os.Stat(envPath); os.IsNotExist(err) {
		envTemplate := `# GitHub Feed Configuration
# Add your GitHub credentials here

# Your GitHub Personal Access Token (required)
# Generate at: https://github.com/settings/tokens
# Required scopes: repo, read:org
GITHUB_TOKEN=

# Your GitHub username (required)
GITHUB_USERNAME=

# Optional: Comma-separated list of allowed repos (e.g., user/repo1,user/repo2)
# Leave empty to allow all repos
ALLOWED_REPOS=
`
		if err := os.WriteFile(envPath, []byte(envTemplate), 0o600); err != nil {
			fmt.Printf("Warning: Could not create .env file at %s: %v\n", envPath, err)
		}
	}

	_ = loadEnvFile(envPath)

	username := os.Getenv("GITHUB_USERNAME")
	if username == "" {
		username = os.Getenv("GITHUB_USER")
	}

	allowedReposStr := allowedReposFlag
	if allowedReposStr == "" {
		allowedReposStr = os.Getenv("ALLOWED_REPOS")
	}

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

	dbPath := filepath.Join(configDir, "github.db")

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

	token := os.Getenv("GITHUB_ACTIVITY_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_TOKEN")
	}

	// Validate configuration
	if err := validateConfig(username, token, localMode, envPath); err != nil {
		fmt.Printf("Configuration Error: %v\n\n", err)
		os.Exit(1)
	}

	if debugMode {
		fmt.Printf("Monitoring GitHub PR activity for user: %s\n", username)
		fmt.Printf("Showing items from the last %v\n", timeRange)
	}
	if debugMode {
		fmt.Println("Debug mode enabled")
	}

	config.debugMode = debugMode
	config.localMode = localMode
	config.showLinks = showLinks
	config.timeRange = timeRange
	config.username = username
	config.allowedRepos = allowedRepos
	config.db = db
	config.ctx = context.Background()
	config.client = github.NewClient(nil).WithAuthToken(token)

	fetchAndDisplayActivity()
}

func validateConfig(username, token string, localMode bool, envPath string) error {
	if localMode {
		return nil // No validation needed for offline mode
	}

	if username == "" {
		return fmt.Errorf("GitHub username is required.\n\nTo fix this:\n  - Set GITHUB_USERNAME environment variable\n  - Or add it to %s", envPath)
	}

	if token == "" {
		return fmt.Errorf("GitHub token is required.\n\nTo fix this:\n  1. Generate a token at https://github.com/settings/tokens\n  2. Click 'Generate new token' -> 'Generate new token (classic)'\n  3. Give it a name and select scopes: 'repo', 'read:org'\n  4. Generate and copy the token\n  5. Set GITHUB_TOKEN environment variable\n  6. Or add it to %s", envPath)
	}

	// Validate token format (GitHub PAT tokens start with ghp_, gho_, or github_pat_)
	if !strings.HasPrefix(token, "ghp_") &&
		!strings.HasPrefix(token, "gho_") &&
		!strings.HasPrefix(token, "github_pat_") {
		return fmt.Errorf("GitHub token format looks invalid.\n\nGitHub Personal Access Tokens should start with:\n  - 'ghp_' (classic PAT)\n  - 'gho_' (OAuth token)\n  - 'github_pat_' (fine-grained PAT)\n\nYour token starts with: '%s'\n\nPlease check your token at https://github.com/settings/tokens", token[:min(10, len(token))])
	}

	return nil
}

func isRepoAllowed(owner, repo string) bool {
	if config.allowedRepos == nil || len(config.allowedRepos) == 0 {
		return true
	}
	repoKey := fmt.Sprintf("%s/%s", owner, repo)
	return config.allowedRepos[repoKey]
}

func checkRateLimit() error {
	var rateLimits *github.RateLimits
	var err error

	retryErr := retryWithBackoff(func() error {
		rateLimits, _, err = config.client.RateLimit.Get(config.ctx)
		return err
	}, "RateLimitCheck")

	if retryErr != nil {
		return fmt.Errorf("failed to fetch rate limit: %w", retryErr)
	}

	core := rateLimits.Core
	search := rateLimits.Search

	if config.debugMode {
		fmt.Printf("Rate Limits - Core: %d/%d, Search: %d/%d\n",
			core.Remaining, core.Limit,
			search.Remaining, search.Limit)
	}

	if core.Remaining == 0 {
		resetTime := core.Reset.Time.Sub(time.Now())
		fmt.Printf("WARNING: Core API rate limit exceeded! Resets in %v\n", resetTime.Round(time.Second))
		return fmt.Errorf("rate limit exceeded, resets at %v", core.Reset.Time.Format("15:04:05"))
	}

	if search.Remaining == 0 {
		resetTime := search.Reset.Time.Sub(time.Now())
		fmt.Printf("WARNING: Search API rate limit exceeded! Resets in %v\n", resetTime.Round(time.Second))
		return fmt.Errorf("search rate limit exceeded, resets at %v", search.Reset.Time.Format("15:04:05"))
	}

	coreThreshold := core.Limit / 5
	if core.Remaining < coreThreshold && core.Remaining > 0 {
		fmt.Printf("WARNING: Core API rate limit running low (%d remaining)\n", core.Remaining)
	}

	if search.Remaining < 5 && search.Remaining > 0 {
		fmt.Printf("WARNING: Search API rate limit running low (%d remaining)\n", search.Remaining)
	}

	return nil
}

func fetchAndDisplayActivity() {
	startTime := time.Now()

	if !config.localMode {
		if err := checkRateLimit(); err != nil {
			fmt.Printf("Skipping this cycle due to rate limit: %v\n", err)
			return
		}
		if config.debugMode {
			fmt.Println()
		}
	}

	var seenPRs sync.Map        // Maps prKey -> label
	activitiesMap := sync.Map{} // Maps prKey -> *PRActivity

	// 6 PR queries + 4 issue queries = 10 total
	initialTotal := 10
	if !config.localMode {
		initialTotal += 3 // Add 3 for event pages
	}
	config.progress = &Progress{}
	config.progress.current.Store(0)
	config.progress.total.Store(int32(initialTotal))

	if config.debugMode {
		fmt.Println("Running optimized search queries...")
	} else {
		fmt.Print("Fetching data from GitHub... ")
		config.progress.display()
	}

	dateAgo := time.Now().Add(-config.timeRange).Format("2006-01-02")
	dateFilter := fmt.Sprintf("updated:>=%s", dateAgo)

	buildQuery := func(base string) string {
		return fmt.Sprintf("%s %s", base, dateFilter)
	}

	var prWg sync.WaitGroup

	prQueries := []struct {
		query string
		label string
	}{
		{buildQuery(fmt.Sprintf("is:pr reviewed-by:%s", config.username)), "Reviewed"},
		{buildQuery(fmt.Sprintf("is:pr review-requested:%s", config.username)), "Review Requested"},
		{buildQuery(fmt.Sprintf("is:pr author:%s", config.username)), "Authored"},
		{buildQuery(fmt.Sprintf("is:pr assignee:%s", config.username)), "Assigned"},
		{buildQuery(fmt.Sprintf("is:pr commenter:%s", config.username)), "Commented"},
		{buildQuery(fmt.Sprintf("is:pr mentions:%s", config.username)), "Mentioned"},
	}

	for _, pq := range prQueries {
		query := pq.query
		label := pq.label
		prWg.Go(func() {
			collectSearchResults(query, label, &seenPRs, &activitiesMap)
		})
	}

	prWg.Wait()

	if config.debugMode {
		fmt.Println()
		fmt.Println("Running issue search queries...")
	}
	var seenIssues sync.Map          // Maps issueKey -> label
	issueActivitiesMap := sync.Map{} // Maps issueKey -> *IssueActivity

	var issueWg sync.WaitGroup

	issueQueries := []struct {
		query string
		label string
	}{
		{buildQuery(fmt.Sprintf("is:issue author:%s", config.username)), "Authored"},
		{buildQuery(fmt.Sprintf("is:issue mentions:%s", config.username)), "Mentioned"},
		{buildQuery(fmt.Sprintf("is:issue assignee:%s", config.username)), "Assigned"},
		{buildQuery(fmt.Sprintf("is:issue commenter:%s", config.username)), "Commented"},
	}

	for _, iq := range issueQueries {
		query := iq.query
		label := iq.label
		issueWg.Go(func() {
			collectIssueSearchResults(query, label, &seenIssues, &issueActivitiesMap)
		})
	}

	issueWg.Wait()

	// Convert activitiesMap to slice
	activities := []PRActivity{}
	activitiesMap.Range(func(key, value interface{}) bool {
		if activity, ok := value.(*PRActivity); ok {
			activities = append(activities, *activity)
		}
		return true
	})

	// Convert issueActivitiesMap to slice
	issueActivities := []IssueActivity{}
	issueActivitiesMap.Range(func(key, value interface{}) bool {
		if activity, ok := value.(*IssueActivity); ok {
			issueActivities = append(issueActivities, *activity)
		}
		return true
	})

	if config.debugMode {
		fmt.Println("Checking cross-references between PRs and issues...")
	}

	// Channel-based approach to avoid race conditions
	type crossRefResult struct {
		prIndex   int
		issue     IssueActivity
		issueKey  string
		debugInfo string
	}
	resultsChan := make(chan crossRefResult, 100)

	var wg sync.WaitGroup

	// Launch collector goroutine to handle all appends and map writes
	collectorDone := make(chan struct{})
	go func() {
		for result := range resultsChan {
			// Safe to modify since only this goroutine accesses these
			activities[result.prIndex].Issues = append(activities[result.prIndex].Issues, result.issue)
			if config.debugMode {
				fmt.Println(result.debugInfo)
			}
		}
		close(collectorDone)
	}()

	for j := range issueActivities {
		issue := &issueActivities[j]
		issueKey := buildItemKey(issue.Owner, issue.Repo, issue.Issue.GetNumber())

		for i := range activities {
			pr := &activities[i]
			if pr.Owner == issue.Owner && pr.Repo == issue.Repo {
				// Capture loop variables
				prIndex := i
				issueCopy := *issue
				issueKeyCopy := issueKey
				prCopy := pr
				wg.Go(func() {
					if areCrossReferenced(prCopy, &issueCopy) {
						debugInfo := ""
						if config.debugMode {
							debugInfo = fmt.Sprintf("  Linked %s/%s#%d <-> %s/%s#%d",
								prCopy.Owner, prCopy.Repo, prCopy.PR.GetNumber(),
								issueCopy.Owner, issueCopy.Repo, issueCopy.Issue.GetNumber())
						}
						resultsChan <- crossRefResult{
							prIndex:   prIndex,
							issue:     issueCopy,
							issueKey:  issueKeyCopy,
							debugInfo: debugInfo,
						}
					}
				})
			}
		}
	}

	wg.Wait()
	close(resultsChan)
	<-collectorDone

	// Every issue we found was discovered via an involvement search query
	// (authored, assigned, commented, or mentioned), so it is linked to the
	// user in some way and always belongs in the OPEN/CLOSED ISSUES sections.
	// Issues that also cross-reference a PR are *additionally* shown nested
	// under that PR above; they are no longer removed from this list.
	standaloneIssues := make([]IssueActivity, len(issueActivities))
	copy(standaloneIssues, issueActivities)

	duration := time.Since(startTime)
	if config.debugMode {
		fmt.Println()
		fmt.Printf("Total fetch time: %v\n", duration.Round(time.Millisecond))
		fmt.Printf("Found %d unique PRs and %d unique issues\n", len(activities), len(issueActivities))

		if config.db != nil {
			prCount, issueCount, commentCount, err := config.db.Stats()
			if err == nil {
				fmt.Printf("Database stats: %d PRs, %d issues, %d comments\n", prCount, issueCount, commentCount)
			}
		}
		fmt.Println()
	} else {
		fmt.Print("\r" + strings.Repeat(" ", 80) + "\r")
	}

	if len(activities) == 0 && len(standaloneIssues) == 0 {
		fmt.Println("No open activity found")
		return
	}

	sort.Slice(activities, func(i, j int) bool {
		return activities[i].UpdatedAt.After(activities[j].UpdatedAt)
	})
	sort.Slice(standaloneIssues, func(i, j int) bool {
		return standaloneIssues[i].UpdatedAt.After(standaloneIssues[j].UpdatedAt)
	})

	var openPRs, closedPRs, mergedPRs []PRActivity
	for _, activity := range activities {
		if activity.PR.State != nil && *activity.PR.State == "closed" {
			if activity.PR.Merged != nil && *activity.PR.Merged {
				mergedPRs = append(mergedPRs, activity)
			} else {
				closedPRs = append(closedPRs, activity)
			}
		} else {
			openPRs = append(openPRs, activity)
		}
	}

	var openIssues, closedIssues []IssueActivity
	for _, issue := range standaloneIssues {
		if issue.Issue.State != nil && *issue.Issue.State == "closed" {
			closedIssues = append(closedIssues, issue)
		} else {
			openIssues = append(openIssues, issue)
		}
	}

	if len(openPRs) > 0 {
		titleColor := color.New(color.FgHiGreen, color.Bold)
		fmt.Println(titleColor.Sprint("OPEN PULL REQUESTS:"))
		fmt.Println("------------------------------------------")
		for _, activity := range openPRs {
			displayPR(activity.Label, activity.Owner, activity.Repo, activity.PR, activity.HasUpdates)
			if len(activity.Issues) > 0 {
				for _, issue := range activity.Issues {
					displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
				}
			}
		}
	}

	if len(closedPRs) > 0 {
		fmt.Println()
		titleColor := color.New(color.FgHiRed, color.Bold)
		fmt.Println(titleColor.Sprint("CLOSED/MERGED PULL REQUESTS:"))
		fmt.Println("------------------------------------------")
		for _, activity := range closedPRs {
			displayPR(activity.Label, activity.Owner, activity.Repo, activity.PR, activity.HasUpdates)
			if len(activity.Issues) > 0 {
				for _, issue := range activity.Issues {
					displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
				}
			}
		}
	}

	if len(openIssues) > 0 {
		fmt.Println()
		titleColor := color.New(color.FgHiGreen, color.Bold)
		fmt.Println(titleColor.Sprint("OPEN ISSUES:"))
		fmt.Println("------------------------------------------")
		for _, issue := range openIssues {
			displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, false, issue.HasUpdates)
		}
	}

	if len(closedIssues) > 0 {
		fmt.Println()
		titleColor := color.New(color.FgHiRed, color.Bold)
		fmt.Println(titleColor.Sprint("CLOSED ISSUES:"))
		fmt.Println("------------------------------------------")
		for _, issue := range closedIssues {
			displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, false, issue.HasUpdates)
		}
	}

	// Warn about database errors if any occurred
	if dbErrors := config.dbErrorCount.Load(); dbErrors > 0 {
		fmt.Printf("\n")
		warningColor := color.New(color.FgYellow, color.Bold)
		fmt.Printf("%s %d database write error(s) occurred. Offline mode may be incomplete.\n",
			warningColor.Sprint("Warning:"), dbErrors)
		if !config.debugMode {
			fmt.Println("Run with --debug to see detailed error messages.")
		}
	}
}

func areCrossReferenced(pr *PRActivity, issue *IssueActivity) bool {
	prNumber := pr.PR.GetNumber()
	issueNumber := issue.Issue.GetNumber()

	if config.debugMode {
		fmt.Printf("  Checking cross-reference: PR %s/%s#%d <-> Issue %s/%s#%d\n",
			pr.Owner, pr.Repo, prNumber,
			issue.Owner, issue.Repo, issueNumber)
	}

	prBody := pr.PR.GetBody()
	if mentionsNumber(prBody, issueNumber, pr.Owner, pr.Repo) {
		return true
	}

	issueBody := issue.Issue.GetBody()
	if mentionsNumber(issueBody, prNumber, issue.Owner, issue.Repo) {
		return true
	}

	var prComments []*github.PullRequestComment
	var err error

	if config.localMode {
		if config.db != nil {
			prComments, err = config.db.GetPRComments(pr.Owner, pr.Repo, prNumber)
			if err != nil && config.debugMode {
				fmt.Printf("  Warning: Could not fetch comments from database for %s/%s#%d: %v\n",
					pr.Owner, pr.Repo, prNumber, err)
			}
		}
	} else {
		config.progress.addToTotal(1)
		if !config.debugMode {
			config.progress.display()
		}

		retryErr := retryWithBackoff(func() error {
			prComments, _, err = config.client.PullRequests.ListComments(config.ctx, pr.Owner, pr.Repo, prNumber, &github.PullRequestListCommentsOptions{
				ListOptions: github.ListOptions{PerPage: 100},
			})
			return err
		}, fmt.Sprintf("Comments-PR#%d", prNumber))

		config.progress.increment()
		if !config.debugMode {
			config.progress.display()
		}

		if retryErr != nil {
			err = retryErr
		}

		if err == nil && config.db != nil {
			for _, comment := range prComments {
				if err := config.db.SavePRComment(pr.Owner, pr.Repo, prNumber, comment, config.debugMode); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save PR comment for %s/%s#%d: %v\n", pr.Owner, pr.Repo, prNumber, err)
					}
				}
			}
		}
	}

	if err == nil {
		for _, comment := range prComments {
			if mentionsNumber(comment.GetBody(), issueNumber, pr.Owner, pr.Repo) {
				return true
			}
		}
	}

	return false
}

func mentionsNumber(text string, number int, owner string, repo string) bool {
	if text == "" {
		return false
	}

	lowerText := strings.ToLower(text)

	urlPatterns := []string{
		fmt.Sprintf("github.com/%s/%s/issues/%d", strings.ToLower(owner), strings.ToLower(repo), number),
		fmt.Sprintf("github.com/%s/%s/pull/%d", strings.ToLower(owner), strings.ToLower(repo), number),
	}
	for _, pattern := range urlPatterns {
		if strings.Contains(lowerText, pattern) {
			return true
		}
	}

	patterns := []string{
		fmt.Sprintf("#%d", number),
		fmt.Sprintf("fixes #%d", number),
		fmt.Sprintf("closes #%d", number),
		fmt.Sprintf("resolves #%d", number),
		fmt.Sprintf("fixed #%d", number),
		fmt.Sprintf("closed #%d", number),
		fmt.Sprintf("resolved #%d", number),
		fmt.Sprintf("fix #%d", number),
		fmt.Sprintf("close #%d", number),
		fmt.Sprintf("resolve #%d", number),
	}

	for _, pattern := range patterns {
		if strings.Contains(lowerText, pattern) {
			return true
		}
	}

	return false
}

func collectSearchResults(query, label string, seenPRs *sync.Map, activitiesMap *sync.Map) {
	if config.localMode {
		if config.db == nil {
			return
		}

		allPRs, prLabels, err := config.db.GetAllPullRequestsWithLabels(config.debugMode)
		if err != nil {
			if config.debugMode {
				fmt.Printf("  [%s] Error loading from database: %v\n", label, err)
			}
			return
		}

		if config.debugMode {
			fmt.Printf("  [%s] Loading from database...\n", label)
		}

		totalFound := 0
		cutoffTime := time.Now().Add(-config.timeRange)
		for key, pr := range allPRs {
			storedLabel := prLabels[key]

			if storedLabel != label {
				continue
			}

			if pr.GetUpdatedAt().Time.Before(cutoffTime) {
				continue
			}

			parts := strings.Split(key, "/")
			if len(parts) < 2 {
				continue
			}
			owner := parts[0]
			repoAndNum := parts[1]
			repoParts := strings.Split(repoAndNum, "#")
			if len(repoParts) < 2 {
				continue
			}
			repo := repoParts[0]

			if !isRepoAllowed(owner, repo) {
				continue
			}

			prKey := key

			// Check if we've already processed this PR in activitiesMap
			existingActivity, alreadyProcessed := activitiesMap.Load(prKey)
			shouldProcess := true

			if alreadyProcessed {
				existingPR := existingActivity.(*PRActivity)
				if shouldUpdateLabel(existingPR.Label, label, true) {
					// New label has higher priority, we'll update it
					if config.debugMode {
						fmt.Printf("  [%s] Updating label for %s from %s to %s (higher priority)\n", label, prKey, existingPR.Label, label)
					}
				} else {
					// Existing label has higher or equal priority, skip
					shouldProcess = false
				}
			}

			if shouldProcess {
				activity := PRActivity{
					Label:     label,
					Owner:     owner,
					Repo:      repo,
					PR:        pr,
					UpdatedAt: pr.GetUpdatedAt().Time,
				}
				activitiesMap.Store(prKey, &activity)
				totalFound++
			}
		}

		if config.debugMode && totalFound > 0 {
			fmt.Printf("  [%s] Complete: %d PRs found\n", label, totalFound)
		}

		return
	}

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	totalFound := 0

	page := 1
	for {
		if config.debugMode {
			fmt.Printf("  [%s] Searching page %d with query: %s\n", label, page, query)
		}

		var result *github.IssuesSearchResult
		var resp *github.Response
		var err error

		retryErr := retryWithBackoff(func() error {
			result, resp, err = config.client.Search.Issues(config.ctx, query, opts)
			return err
		}, fmt.Sprintf("%s-page%d", label, page))

		config.progress.increment()
		if !config.debugMode {
			config.progress.display()
		}

		if page == 1 && resp != nil && resp.NextPage != 0 {
			lastPage := resp.LastPage
			if lastPage > 1 {
				additionalPages := lastPage - 1
				config.progress.addToTotal(additionalPages)
				if !config.debugMode {
					config.progress.display()
				}
			}
		}

		if retryErr != nil {
			fmt.Printf("  [%s] Error searching after retries: %v\n", label, retryErr)
			if resp != nil {
				fmt.Printf("  [%s] Rate limit remaining: %d/%d\n", label, resp.Rate.Remaining, resp.Rate.Limit)
			}
			return
		}

		if config.debugMode && resp != nil {
			fmt.Printf("  [%s] API Response: %d results, Rate: %d/%d\n", label, len(result.Issues), resp.Rate.Remaining, resp.Rate.Limit)
		}

		pageResults := 0
		for _, issue := range result.Issues {
			if issue.PullRequestLinks == nil {
				continue
			}

			repoURL := *issue.RepositoryURL
			parts := strings.Split(repoURL, "/")
			if len(parts) < 2 {
				fmt.Printf("  [%s] Error: Invalid repository URL format: %s\n", label, repoURL)
				continue
			}
			owner := parts[len(parts)-2]
			repo := parts[len(parts)-1]

			if !isRepoAllowed(owner, repo) {
				continue
			}

			prKey := buildItemKey(owner, repo, *issue.Number)

			// Check if we've already processed this PR in activitiesMap
			existingActivity, alreadyProcessed := activitiesMap.Load(prKey)
			shouldProcess := true

			if alreadyProcessed {
				// PR is already in activitiesMap, check if we need to update the label
				existingPR := existingActivity.(*PRActivity)
				if shouldUpdateLabel(existingPR.Label, label, true) {
					// New label has higher priority, we'll update it
					if config.debugMode {
						fmt.Printf("  [%s] Updating label for %s from %s to %s (higher priority)\n", label, prKey, existingPR.Label, label)
					}
				} else {
					// Existing label has higher or equal priority, skip fetching again
					shouldProcess = false
				}
			}

			if shouldProcess {
				// Store in seenPRs to prevent other goroutines from fetching the same PR
				seenPRs.Store(prKey, label)
				config.progress.addToTotal(1)
				if !config.debugMode {
					config.progress.display()
				}

				var pr *github.PullRequest
				// var prErr error

				config.progress.increment()
				if !config.debugMode {
					config.progress.display()
				}

				pr = &github.PullRequest{
					Number:    issue.Number,
					Title:     issue.Title,
					Body:      issue.Body,
					State:     issue.State,
					UpdatedAt: issue.UpdatedAt,
					User:      issue.User,
					HTMLURL:   issue.HTMLURL,
				}
				// }

				hasUpdates := false

				if config.db != nil {
					cachedPR, err := config.db.GetPullRequest(owner, repo, *issue.Number)
					if err == nil {
						if pr.GetUpdatedAt().After(cachedPR.GetUpdatedAt().Time) {
							hasUpdates = true
							if config.debugMode {
								fmt.Printf("  [%s] Update detected: %s/%s#%d (API: %s > DB: %s)\n",
									label, owner, repo, *issue.Number,
									pr.GetUpdatedAt().Format("2006-01-02 15:04:05"),
									cachedPR.GetUpdatedAt().Time.Format("2006-01-02 15:04:05"))
							}
						} else if config.debugMode {
							fmt.Printf("  [%s] No update: %s/%s#%d (API: %s == DB: %s)\n",
								label, owner, repo, *issue.Number,
								pr.GetUpdatedAt().Format("2006-01-02 15:04:05"),
								cachedPR.GetUpdatedAt().Time.Format("2006-01-02 15:04:05"))
						}
					} else {
						// If there's no cached version, this is a new PR, so it has "updates"
						hasUpdates = true
						if config.debugMode {
							fmt.Printf("  [%s] New PR (not in DB): %s/%s#%d\n",
								label, owner, repo, *issue.Number)
						}
					}
				}

				// Determine the final label to use
				finalLabel := label
				if alreadyProcessed {
					existingPR := existingActivity.(*PRActivity)
					if !shouldUpdateLabel(existingPR.Label, label, true) {
						// Keep the existing higher-priority label
						finalLabel = existingPR.Label
						if config.debugMode {
							fmt.Printf("  [%s] Keeping existing label %s for %s (higher priority)\n", label, finalLabel, prKey)
						}
					}
				}

				if config.db != nil {
					if err := config.db.SavePullRequestWithLabel(owner, repo, pr, finalLabel, config.debugMode); err != nil {
						config.dbErrorCount.Add(1)
						if config.debugMode {
							fmt.Printf("  [DB] Warning: Failed to save PR %s/%s#%d: %v\n", owner, repo, pr.GetNumber(), err)
						}
					}
				}

				activity := PRActivity{
					Label:      finalLabel,
					Owner:      owner,
					Repo:       repo,
					PR:         pr,
					UpdatedAt:  pr.GetUpdatedAt().Time,
					HasUpdates: hasUpdates,
				}
				activitiesMap.Store(prKey, &activity)
				pageResults++
				totalFound++
			}
		}

		if config.debugMode {
			fmt.Printf("  [%s] Page %d: found %d new PRs (total: %d)\n", label, page, pageResults, totalFound)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
		page++
	}

	if config.debugMode && totalFound > 0 {
		fmt.Printf("  [%s] Complete: %d PRs found\n", label, totalFound)
	}
}

// DisplayConfig holds all the information needed to display a PR or issue
type DisplayConfig struct {
	Owner      string
	Repo       string
	Number     int
	Title      string
	User       string
	UpdatedAt  *github.Timestamp
	HTMLURL    *string
	Label      string
	HasUpdates bool
	IsIndented bool   // for nested display under PRs
	State      *string // for issues nested under PRs (OPEN/CLOSED)
}

// displayItem is the unified display function for both PRs and issues
func displayItem(cfg DisplayConfig) {
	dateStr := "          "
	if cfg.UpdatedAt != nil {
		dateStr = cfg.UpdatedAt.Format("2006/01/02")
	}

	indent := ""
	linkIndent := "   "
	if cfg.IsIndented && cfg.State != nil {
		state := strings.ToUpper(*cfg.State)
		stateColor := getStateColor(*cfg.State)
		indent = fmt.Sprintf("-- %s ", stateColor.Sprint(state))
		linkIndent = "      "
	}

	labelColor := getLabelColor(cfg.Label)
	userColor := getUserColor(cfg.User)

	updateIcon := ""
	if cfg.HasUpdates {
		updateIcon = color.New(color.FgYellow, color.Bold).Sprint("● ")
	}

	fmt.Printf("%s%s%s %s %s %s/%s#%d - %s\n",
		updateIcon,
		indent,
		dateStr,
		labelColor.Sprint(strings.ToUpper(cfg.Label)),
		userColor.Sprint(cfg.User),
		cfg.Owner, cfg.Repo, cfg.Number,
		cfg.Title,
	)

	if config.showLinks && cfg.HTMLURL != nil {
		fmt.Printf("%s🔗 %s\n", linkIndent, *cfg.HTMLURL)
	}
}

func displayPR(label, owner, repo string, pr *github.PullRequest, hasUpdates bool) {
	displayItem(DisplayConfig{
		Owner:      owner,
		Repo:       repo,
		Number:     pr.GetNumber(),
		Title:      pr.GetTitle(),
		User:       pr.User.GetLogin(),
		UpdatedAt:  pr.UpdatedAt,
		HTMLURL:    pr.HTMLURL,
		Label:      label,
		HasUpdates: hasUpdates,
		IsIndented: false,
	})
}

func displayIssue(label, owner, repo string, issue *github.Issue, indented bool, hasUpdates bool) {
	displayItem(DisplayConfig{
		Owner:      owner,
		Repo:       repo,
		Number:     issue.GetNumber(),
		Title:      issue.GetTitle(),
		User:       issue.User.GetLogin(),
		UpdatedAt:  issue.UpdatedAt,
		HTMLURL:    issue.HTMLURL,
		Label:      label,
		HasUpdates: hasUpdates,
		IsIndented: indented,
		State:      issue.State,
	})
}

func collectIssueSearchResults(query, label string, seenIssues *sync.Map, issueActivitiesMap *sync.Map) {
	if config.localMode {
		if config.db == nil {
			return
		}

		allIssues, issueLabels, err := config.db.GetAllIssuesWithLabels(config.debugMode)
		if err != nil {
			if config.debugMode {
				fmt.Printf("  [%s] Error loading from database: %v\n", label, err)
			}
			return
		}

		if config.debugMode {
			fmt.Printf("  [%s] Loading from database...\n", label)
		}

		totalFound := 0
		cutoffTime := time.Now().Add(-config.timeRange)
		for key, issue := range allIssues {
			storedLabel := issueLabels[key]

			if storedLabel != label {
				continue
			}

			if issue.GetUpdatedAt().Time.Before(cutoffTime) {
				continue
			}

			parts := strings.Split(key, "/")
			if len(parts) < 2 {
				continue
			}
			owner := parts[0]
			repoAndNum := parts[1]
			repoParts := strings.Split(repoAndNum, "#")
			if len(repoParts) < 2 {
				continue
			}
			repo := repoParts[0]

			if !isRepoAllowed(owner, repo) {
				continue
			}

			issueKey := key

			// Check if we've already processed this issue in issueActivitiesMap
			existingActivity, alreadyProcessed := issueActivitiesMap.Load(issueKey)
			shouldProcess := true

			if alreadyProcessed {
				// Issue is already in issueActivitiesMap, check if we need to update the label
				existingIssue := existingActivity.(*IssueActivity)
				if shouldUpdateLabel(existingIssue.Label, label, false) {
					// New label has higher priority, we'll update it
					if config.debugMode {
						fmt.Printf("  [%s] Updating label for %s from %s to %s (higher priority)\n", label, issueKey, existingIssue.Label, label)
					}
				} else {
					// Existing label has higher or equal priority, skip
					shouldProcess = false
				}
			}

			if shouldProcess {
				activity := IssueActivity{
					Label:     label,
					Owner:     owner,
					Repo:      repo,
					Issue:     issue,
					UpdatedAt: issue.GetUpdatedAt().Time,
				}
				issueActivitiesMap.Store(issueKey, &activity)
				totalFound++
			}
		}

		if config.debugMode && totalFound > 0 {
			fmt.Printf("  [%s] Complete: %d issues found\n", label, totalFound)
		}

		return
	}

	opts := &github.SearchOptions{
		ListOptions: github.ListOptions{PerPage: 100},
	}

	totalFound := 0

	page := 1
	for {
		if config.debugMode {
			fmt.Printf("  [%s] Searching page %d with query: %s\n", label, page, query)
		}

		var result *github.IssuesSearchResult
		var resp *github.Response
		var err error

		retryErr := retryWithBackoff(func() error {
			result, resp, err = config.client.Search.Issues(config.ctx, query, opts)
			return err
		}, fmt.Sprintf("%s-issues-page%d", label, page))

		config.progress.increment()
		if !config.debugMode {
			config.progress.display()
		}

		if page == 1 && resp != nil && resp.NextPage != 0 {
			lastPage := resp.LastPage
			if lastPage > 1 {
				additionalPages := lastPage - 1
				config.progress.addToTotal(additionalPages)
				if !config.debugMode {
					config.progress.display()
				}
			}
		}

		if retryErr != nil {
			fmt.Printf("  [%s] Error searching after retries: %v\n", label, retryErr)
			if resp != nil {
				fmt.Printf("  [%s] Rate limit remaining: %d/%d\n", label, resp.Rate.Remaining, resp.Rate.Limit)
			}
			return
		}

		if config.debugMode && resp != nil {
			fmt.Printf("  [%s] API Response: %d results, Rate: %d/%d\n", label, len(result.Issues), resp.Rate.Remaining, resp.Rate.Limit)
		}

		pageResults := 0
		for _, issue := range result.Issues {
			if issue.PullRequestLinks != nil {
				continue
			}

			repoURL := *issue.RepositoryURL
			parts := strings.Split(repoURL, "/")
			if len(parts) < 2 {
				fmt.Printf("  [%s] Error: Invalid repository URL format: %s\n", label, repoURL)
				continue
			}
			owner := parts[len(parts)-2]
			repo := parts[len(parts)-1]

			if !isRepoAllowed(owner, repo) {
				continue
			}

			issueKey := buildItemKey(owner, repo, *issue.Number)

			// Check if we've already processed this issue in issueActivitiesMap
			existingActivity, alreadyProcessed := issueActivitiesMap.Load(issueKey)
			shouldProcess := true

			if alreadyProcessed {
				// Issue is already in issueActivitiesMap, check if we need to update the label
				existingIssue := existingActivity.(*IssueActivity)
				if shouldUpdateLabel(existingIssue.Label, label, false) {
					// New label has higher priority, we'll update it
					if config.debugMode {
						fmt.Printf("  [%s] Updating label for %s from %s to %s (higher priority)\n", label, issueKey, existingIssue.Label, label)
					}
				} else {
					// Existing label has higher or equal priority, skip
					shouldProcess = false
				}
			}

			if shouldProcess {
				hasUpdates := false

				if config.db != nil {
					cachedIssue, err := config.db.GetIssue(owner, repo, *issue.Number)
					if err == nil {
						if issue.GetUpdatedAt().After(cachedIssue.GetUpdatedAt().Time) {
							hasUpdates = true
						}
					}
					if err := config.db.SaveIssueWithLabel(owner, repo, issue, label, config.debugMode); err != nil {
						config.dbErrorCount.Add(1)
						if config.debugMode {
							fmt.Printf("  [DB] Warning: Failed to save issue %s/%s#%d: %v\n", owner, repo, *issue.Number, err)
						}
					}
				}

				activity := IssueActivity{
					Label:      label,
					Owner:      owner,
					Repo:       repo,
					Issue:      issue,
					UpdatedAt:  issue.GetUpdatedAt().Time,
					HasUpdates: hasUpdates,
				}
				issueActivitiesMap.Store(issueKey, &activity)
				pageResults++
				totalFound++
			}
		}

		if config.debugMode {
			fmt.Printf("  [%s] Page %d: found %d new issues (total: %d)\n", label, page, pageResults, totalFound)
		}

		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
		page++
	}

	if config.debugMode && totalFound > 0 {
		fmt.Printf("  [%s] Complete: %d issues found\n", label, totalFound)
	}
}
