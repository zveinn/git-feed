//go:build ignore
// +build ignore

// explore_queries.go
//
// Standalone exploration tool to test GitHub search query optimization theories
// for github-feed before modifying the real code.
//
// Usage:
//   go run explore_queries.go
//   go run explore_queries.go 7d
//   GITHUB_TOKEN=... GITHUB_USERNAME=... go run explore_queries.go 30d
//
// It does NOT touch the database. Purely for research.

package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-github/v57/github"
)

func main() {
	timeStr := "30d"
	if len(os.Args) > 1 {
		timeStr = os.Args[1]
	}

	dur, err := parseTimeRange(timeStr)
	if err != nil {
		fmt.Printf("Invalid time range %q: %v. Using 30d.\n", timeStr, err)
		dur = 30 * 24 * time.Hour
	}

	home, _ := os.UserHomeDir()
	envPath := filepath.Join(home, ".github-feed", ".env")
	_ = loadEnvFile(envPath)

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		token = os.Getenv("GITHUB_ACTIVITY_TOKEN")
	}
	username := os.Getenv("GITHUB_USERNAME")
	if username == "" {
		username = os.Getenv("GITHUB_USER")
	}

	if token == "" || username == "" {
		fmt.Println("ERROR: Need GITHUB_TOKEN and GITHUB_USERNAME (or via ~/.github-feed/.env)")
		os.Exit(1)
	}

	client := github.NewClient(nil).WithAuthToken(token)
	ctx := context.Background()

	dateAgo := time.Now().Add(-dur).Format("2006-01-02")
	dateFilter := fmt.Sprintf("updated:>=%s", dateAgo)

	build := func(base string) string {
		return fmt.Sprintf("%s %s", base, dateFilter)
	}

	fmt.Printf("=== Query Explorer for user=%s, time range=%v (since %s) ===\n\n", username, dur, dateAgo)

	// Define the strategies we want to compare

	// Strategy Current: exact current code (10 queries)
	currentPR := []struct{ q, label string }{
		{build(fmt.Sprintf("is:pr reviewed-by:%s", username)), "Reviewed"},
		{build(fmt.Sprintf("is:pr review-requested:%s", username)), "Review Requested"},
		{build(fmt.Sprintf("is:pr author:%s", username)), "Authored"},
		{build(fmt.Sprintf("is:pr assignee:%s", username)), "Assigned"},
		{build(fmt.Sprintf("is:pr commenter:%s", username)), "Commented"},
		{build(fmt.Sprintf("is:pr mentions:%s", username)), "Mentioned"},
	}
	currentIssue := []struct{ q, label string }{
		{build(fmt.Sprintf("is:issue author:%s", username)), "Authored"},
		{build(fmt.Sprintf("is:issue mentions:%s", username)), "Mentioned"},
		{build(fmt.Sprintf("is:issue assignee:%s", username)), "Assigned"},
		{build(fmt.Sprintf("is:issue commenter:%s", username)), "Commented"},
	}

	// Strategy Optimized A (recommended hybrid): preserve high value labels + broad involves
	// PR:  author + assignee + reviewed + review-requested + involves (5)
	// Issue: author + assignee + involves (3)
	// Total 8 instead of 10. Good labels for important roles.
	optA_PR := []struct{ q, label string }{
		{build(fmt.Sprintf("is:pr reviewed-by:%s", username)), "Reviewed"},
		{build(fmt.Sprintf("is:pr review-requested:%s", username)), "Review Requested"},
		{build(fmt.Sprintf("is:pr author:%s", username)), "Authored"},
		{build(fmt.Sprintf("is:pr assignee:%s", username)), "Assigned"},
		{build(fmt.Sprintf("is:pr involves:%s", username)), "Involved"},
	}
	optA_Issue := []struct{ q, label string }{
		{build(fmt.Sprintf("is:issue author:%s", username)), "Authored"},
		{build(fmt.Sprintf("is:issue assignee:%s", username)), "Assigned"},
		{build(fmt.Sprintf("is:issue involves:%s", username)), "Involved"},
	}

	// Strategy Optimized B (more aggressive): fewer calls, more "Involved"
	// PR: reviewed + review-req + author + involves (4)
	// Issue: author + involves (2)
	// Total 6 queries. Assigned falls back to "Involved" unless author.
	optB_PR := []struct{ q, label string }{
		{build(fmt.Sprintf("is:pr reviewed-by:%s", username)), "Reviewed"},
		{build(fmt.Sprintf("is:pr review-requested:%s", username)), "Review Requested"},
		{build(fmt.Sprintf("is:pr author:%s", username)), "Authored"},
		{build(fmt.Sprintf("is:pr involves:%s", username)), "Involved"},
	}
	optB_Issue := []struct{ q, label string }{
		{build(fmt.Sprintf("is:issue author:%s", username)), "Authored"},
		{build(fmt.Sprintf("is:issue involves:%s", username)), "Involved"},
	}

	// Strategy C (pure minimal):  reviewed + review-req + involves (PR) + involves (issues) = 3+1?
	// We keep author for visibility of "Authored".
	optC_PR := []struct{ q, label string }{
		{build(fmt.Sprintf("is:pr reviewed-by:%s", username)), "Reviewed"},
		{build(fmt.Sprintf("is:pr review-requested:%s", username)), "Review Requested"},
		{build(fmt.Sprintf("is:pr involves:%s", username)), "Involved"},
	}
	optC_Issue := []struct{ q, label string }{
		{build(fmt.Sprintf("is:issue involves:%s", username)), "Involved"},
	}

	fmt.Println("--- Running CURRENT strategy (10 queries) ---")
	currPRs, currIssues, currCalls := runStrategy(ctx, client, currentPR, currentIssue, username)

	fmt.Println("\n--- Running OPT-A hybrid (preserve Authored/Assigned/Reviewed/ReviewReq + involves) (8 queries) ---")
	optAPR, optAIss, optACalls := runStrategy(ctx, client, optA_PR, optA_Issue, username)

	fmt.Println("\n--- Running OPT-B aggressive (6 queries) ---")
	optBPR, optBIss, optBCalls := runStrategy(ctx, client, optB_PR, optB_Issue, username)

	fmt.Println("\n--- Running OPT-C minimal ( ~3-4 queries) ---")
	optCPR, optCIss, optCCalls := runStrategy(ctx, client, optC_PR, optC_Issue, username)

	// Compare coverage
	fmt.Println("\n\n========== COVERAGE COMPARISON ==========")
	fmt.Printf("Current: %d PRs + %d Issues | %d search calls\n", len(currPRs), len(currIssues), currCalls)
	fmt.Printf("Opt-A  : %d PRs + %d Issues | %d search calls\n", len(optAPR), len(optAIss), optACalls)
	fmt.Printf("Opt-B  : %d PRs + %d Issues | %d search calls\n", len(optBPR), len(optBIss), optBCalls)
	fmt.Printf("Opt-C  : %d PRs + %d Issues | %d search calls\n", len(optCPR), len(optCIss), optCCalls)

	// Check overlap
	missingInA := diffKeys(currPRs, optAPR)
	missingInB := diffKeys(currPRs, optBPR)
	fmt.Printf("\nItems in CURRENT but missing in Opt-A: %d PRs\n", len(missingInA))
	fmt.Printf("Items in CURRENT but missing in Opt-B: %d PRs\n", len(missingInB))

	if len(missingInA) > 0 && len(missingInA) < 6 {
		fmt.Println("Sample missing in Opt-A:")
		for i, k := range missingInA {
			if i > 3 {
				break
			}
			fmt.Printf("  %s\n", k)
		}
	}

	fmt.Println("\nDone. Use the data above to decide final strategy.")
	fmt.Println("Recommendation (from design): Prefer Opt-A or Opt-B to keep 'Authored' and review labels while cutting queries.")
}

// runStrategy executes a set of PR and Issue queries, applies simple label priority,
// and returns the final unique sets + total search API calls made.
func runStrategy(ctx context.Context, client *github.Client, prQueries, issueQueries []struct{ q, label string }, username string) (map[string]string, map[string]string, int) {
	prResults := make(map[string]string)  // key -> best label
	issueResults := make(map[string]string)
	calls := 0

	// Helper to run one role query
	runOne := func(query, label string, isPR bool) {
		opts := &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100}}
		page := 1
		for {
			result, resp, err := client.Search.Issues(ctx, query, opts)
			calls++
			if err != nil {
				fmt.Printf("  [ERROR on %s page %d]: %v\n", label, page, err)
				if resp != nil && resp.Rate.Remaining < 5 {
					fmt.Println("  Rate low, stopping this query.")
				}
				return
			}

			for _, item := range result.Issues {
				key := buildKey(item)
				if key == "" {
					continue
				}

				// PR vs Issue filter
				if isPR && item.PullRequestLinks == nil {
					continue
				}
				if !isPR && item.PullRequestLinks != nil {
					continue
				}

				// Apply "priority": only keep if better (or first)
				current := ""
				if isPR {
					current = prResults[key]
				} else {
					current = issueResults[key]
				}

				if current == "" || shouldTakeNewLabel(current, label, isPR) {
					if isPR {
						prResults[key] = label
					} else {
						issueResults[key] = label
					}
				}
			}

			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
			page++
			// small politeness
			time.Sleep(150 * time.Millisecond)
		}
	}

	for _, pq := range prQueries {
		fmt.Printf("  PR query [%s]: %s\n", pq.label, pq.q)
		runOne(pq.q, pq.label, true)
	}

	for _, iq := range issueQueries {
		fmt.Printf("  Issue query [%s]: %s\n", iq.label, iq.q)
		runOne(iq.q, iq.label, false)
	}

	// Count label distribution
	prLabels := countLabels(prResults)
	issLabels := countLabels(issueResults)
	fmt.Printf("  -> PRs: %d  (labels: %v)\n", len(prResults), prLabels)
	fmt.Printf("  -> Issues: %d (labels: %v)\n", len(issueResults), issLabels)

	return prResults, issueResults, calls
}

func buildKey(issue *github.Issue) string {
	if issue == nil || issue.RepositoryURL == nil || issue.Number == nil {
		return ""
	}
	repoURL := *issue.RepositoryURL
	parts := strings.Split(repoURL, "/")
	if len(parts) < 2 {
		return ""
	}
	owner := parts[len(parts)-2]
	repo := parts[len(parts)-1]
	return fmt.Sprintf("%s/%s#%d", owner, repo, *issue.Number)
}

func countLabels(m map[string]string) map[string]int {
	out := map[string]int{}
	for _, l := range m {
		out[l]++
	}
	return out
}

func diffKeys(a, b map[string]string) []string {
	var res []string
	for k := range a {
		if _, ok := b[k]; !ok {
			res = append(res, k)
		}
	}
	sort.Strings(res)
	return res
}

// Local copy of priority logic for the explorer
func getPRLabelPriority(label string) int {
	priorities := map[string]int{
		"Authored":         1,
		"Assigned":         2,
		"Reviewed":         3,
		"Review Requested": 4,
		"Commented":        5,
		"Mentioned":        6,
		"Involved":         7,
	}
	if p, ok := priorities[label]; ok {
		return p
	}
	return 999
}

func getIssueLabelPriority(label string) int {
	priorities := map[string]int{
		"Authored":  1,
		"Assigned":  2,
		"Commented": 3,
		"Mentioned": 4,
		"Involved":  5,
	}
	if p, ok := priorities[label]; ok {
		return p
	}
	return 999
}

func shouldTakeNewLabel(current, newLabel string, isPR bool) bool {
	if current == "" {
		return true
	}
	var cp, np int
	if isPR {
		cp = getPRLabelPriority(current)
		np = getPRLabelPriority(newLabel)
	} else {
		cp = getIssueLabelPriority(current)
		np = getIssueLabelPriority(newLabel)
	}
	return np < cp
}

// --- minimal helpers copied/adapted from main.go ---

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
		return 0, fmt.Errorf("invalid time range format: %s", timeStr)
	}

	numStr := timeStr[:len(timeStr)-1]
	unit := strings.ToLower(timeStr[len(timeStr)-1:])

	num, err := strconv.Atoi(numStr)
	if err != nil || num < 1 {
		return 0, fmt.Errorf("invalid time range number: %s", numStr)
	}

	switch unit {
	case "h":
		return time.Duration(num) * time.Hour, nil
	case "d":
		return time.Duration(num) * 24 * time.Hour, nil
	case "w":
		return time.Duration(num) * 7 * 24 * time.Hour, nil
	case "m":
		return time.Duration(num) * 30 * 24 * time.Hour, nil
	case "y":
		return time.Duration(num) * 365 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("invalid time unit: %s", unit)
	}
}