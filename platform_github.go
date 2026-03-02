package main

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/google/go-github/v57/github"
	"golang.org/x/oauth2"
)

var (
	githubCrossRefKeywordPattern = regexp.MustCompile(`(?i)\b(?:fix(?:e[sd])?|close[sd]?|resolve[sd]?)\b`)
	githubCrossRefSameRefPattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_])#([0-9]+)\b`)
	githubCrossRefQualifiedRef   = regexp.MustCompile(`(?i)\b([a-z0-9_.-]+/[a-z0-9_.-]+)#([0-9]+)\b`)
	githubCrossRefURLPattern     = regexp.MustCompile(`(?i)https?://github\.com/([a-z0-9_.-]+)/([a-z0-9_.-]+)/(?:issues|pull)/([0-9]+)\b`)
)

func fetchAndDisplayGitHubActivity() {
	startTime := time.Now()

	if config.debugMode {
		fmt.Println("Fetching data from GitHub...")
	} else {
		fmt.Print("Fetching data from GitHub... ")
	}

	cutoffTime := time.Now().Add(-config.timeRange)
	var (
		activities      []PRActivity
		issueActivities []IssueActivity
		err             error
	)

	if config.localMode {
		activities, issueActivities, err = loadGitHubCachedActivities(cutoffTime)
	} else {
		ctx := config.ctx
		if ctx == nil {
			ctx = context.Background()
		}
		activities, issueActivities, err = fetchGitHubActivitiesOnline(ctx, cutoffTime)
	}
	if err != nil {
		fmt.Printf("Error fetching GitHub activity: %v\n", err)
		return
	}

	if config.debugMode {
		fmt.Println()
		fmt.Printf("Total fetch time: %v\n", time.Since(startTime).Round(time.Millisecond))
		fmt.Printf("Found %d unique pull requests and %d unique issues\n", len(activities), len(issueActivities))
		fmt.Println()
	} else {
		fmt.Print("\r" + strings.Repeat(" ", 80) + "\r")
	}

	if len(activities) == 0 && len(issueActivities) == 0 {
		fmt.Println("No open activity found")
		return
	}

	sort.Slice(activities, func(i, j int) bool {
		return activities[i].UpdatedAt.After(activities[j].UpdatedAt)
	})
	sort.Slice(issueActivities, func(i, j int) bool {
		return issueActivities[i].UpdatedAt.After(issueActivities[j].UpdatedAt)
	})

	var openPRs, closedPRs, mergedPRs []PRActivity
	for _, activity := range activities {
		if activity.MR.State == "closed" {
			if activity.MR.Merged {
				mergedPRs = append(mergedPRs, activity)
			} else {
				closedPRs = append(closedPRs, activity)
			}
		} else {
			openPRs = append(openPRs, activity)
		}
	}

	var openIssues, closedIssues []IssueActivity
	for _, issue := range issueActivities {
		if issue.Issue.State == "closed" {
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
			displayMergeRequest(activity.Label, activity.Owner, activity.Repo, activity.MR, activity.HasUpdates)
			for _, issue := range activity.Issues {
				displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
			}
		}
	}

	if len(closedPRs) > 0 || len(mergedPRs) > 0 {
		fmt.Println()
		titleColor := color.New(color.FgHiRed, color.Bold)
		fmt.Println(titleColor.Sprint("CLOSED/MERGED PULL REQUESTS:"))
		fmt.Println("------------------------------------------")
		for _, activity := range mergedPRs {
			displayMergeRequest(activity.Label, activity.Owner, activity.Repo, activity.MR, activity.HasUpdates)
			for _, issue := range activity.Issues {
				displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
			}
		}
		for _, activity := range closedPRs {
			displayMergeRequest(activity.Label, activity.Owner, activity.Repo, activity.MR, activity.HasUpdates)
			for _, issue := range activity.Issues {
				displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
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
}

func fetchGitHubActivitiesOnline(ctx context.Context, cutoff time.Time) ([]PRActivity, []IssueActivity, error) {
	client := newGitHubClient(config.githubToken)
	dateFilter := cutoff.Format("2006-01-02")

	prActivities, prReviewComments, err := collectGitHubPRSearchResults(ctx, client, config.githubUsername, dateFilter, cutoff)
	if err != nil {
		return nil, nil, err
	}

	issueActivities, err := collectGitHubIssueSearchResults(ctx, client, config.githubUsername, dateFilter, cutoff)
	if err != nil {
		return nil, nil, err
	}

	nestedPRs := nestGitHubIssues(prActivities, issueActivities, prReviewComments)
	standaloneIssues := filterStandaloneGitHubIssues(nestedPRs, issueActivities)
	return nestedPRs, standaloneIssues, nil
}

func collectGitHubPRSearchResults(
	ctx context.Context,
	client *github.Client,
	username, dateFilter string,
	cutoff time.Time,
) ([]PRActivity, map[string][]GitHubPRReviewCommentRecord, error) {
	queries := []struct {
		Label string
		Query string
	}{
		{Label: "Reviewed", Query: fmt.Sprintf("is:pr reviewed-by:%s updated:>=%s", username, dateFilter)},
		{Label: "Review Requested", Query: fmt.Sprintf("is:pr review-requested:%s updated:>=%s", username, dateFilter)},
		{Label: "Authored", Query: fmt.Sprintf("is:pr author:%s updated:>=%s", username, dateFilter)},
		{Label: "Assigned", Query: fmt.Sprintf("is:pr assignee:%s updated:>=%s", username, dateFilter)},
		{Label: "Commented", Query: fmt.Sprintf("is:pr commenter:%s updated:>=%s", username, dateFilter)},
		{Label: "Mentioned", Query: fmt.Sprintf("is:pr mentions:%s updated:>=%s", username, dateFilter)},
	}

	byKey := make(map[string]PRActivity)
	prReviewComments := make(map[string][]GitHubPRReviewCommentRecord)

	for _, q := range queries {
		items, err := searchGitHubIssues(ctx, client, q.Query)
		if err != nil {
			return nil, nil, fmt.Errorf("search pull requests for %s: %w", q.Label, err)
		}

		for _, item := range items {
			if item == nil || item.GetPullRequestLinks() == nil {
				continue
			}
			owner, repo, ok := parseGitHubRepoFromSearchItem(item)
			if !ok || !isGitHubRepoAllowed(owner, repo) {
				continue
			}

			pr, err := getGitHubPullRequest(ctx, client, owner, repo, item.GetNumber())
			if err != nil {
				return nil, nil, err
			}
			model := toMergeRequestModelFromGitHubPR(pr)
			if model.UpdatedAt.IsZero() || model.UpdatedAt.Before(cutoff) {
				continue
			}

			key := buildGitHubItemKey(owner, repo, model.Number)
			activity, exists := byKey[key]
			if !exists {
				activity = PRActivity{Owner: owner, Repo: repo, MR: model, UpdatedAt: model.UpdatedAt}
			} else {
				if model.UpdatedAt.After(activity.UpdatedAt) {
					activity.UpdatedAt = model.UpdatedAt
				}
				activity.MR = model
			}
			if shouldUpdateLabel(activity.Label, q.Label, true) {
				activity.Label = q.Label
			}

			if config.db != nil {
				if err := config.db.SaveGitHubPullRequestWithLabel(owner, repo, model, activity.Label, config.debugMode); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save GitHub PR %s/%s#%d: %v\n", owner, repo, model.Number, err)
					}
				}
			}

			reviewComments, err := listGitHubPRReviewComments(ctx, client, owner, repo, model.Number)
			if err != nil {
				return nil, nil, err
			}
			records := make([]GitHubPRReviewCommentRecord, 0, len(reviewComments))
			for _, comment := range reviewComments {
				record := toGitHubPRReviewCommentRecord(owner, repo, model.Number, comment)
				records = append(records, record)
				if config.db != nil {
					if err := config.db.SaveGitHubPRReviewComment(record, config.debugMode); err != nil {
						config.dbErrorCount.Add(1)
						if config.debugMode {
							fmt.Printf("  [DB] Warning: Failed to save GitHub PR review comment %s/%s#%d/%d: %v\n", owner, repo, model.Number, record.CommentID, err)
						}
					}
				}
			}
			prReviewComments[key] = records

			byKey[key] = activity
		}
	}

	activities := make([]PRActivity, 0, len(byKey))
	for _, activity := range byKey {
		activities = append(activities, activity)
	}
	return activities, prReviewComments, nil
}

func collectGitHubIssueSearchResults(
	ctx context.Context,
	client *github.Client,
	username, dateFilter string,
	cutoff time.Time,
) ([]IssueActivity, error) {
	queries := []struct {
		Label string
		Query string
	}{
		{Label: "Authored", Query: fmt.Sprintf("is:issue author:%s updated:>=%s", username, dateFilter)},
		{Label: "Mentioned", Query: fmt.Sprintf("is:issue mentions:%s updated:>=%s", username, dateFilter)},
		{Label: "Assigned", Query: fmt.Sprintf("is:issue assignee:%s updated:>=%s", username, dateFilter)},
		{Label: "Commented", Query: fmt.Sprintf("is:issue commenter:%s updated:>=%s", username, dateFilter)},
	}

	byKey := make(map[string]IssueActivity)

	for _, q := range queries {
		items, err := searchGitHubIssues(ctx, client, q.Query)
		if err != nil {
			return nil, fmt.Errorf("search issues for %s: %w", q.Label, err)
		}

		for _, item := range items {
			if item == nil || item.GetPullRequestLinks() != nil {
				continue
			}
			owner, repo, ok := parseGitHubRepoFromSearchItem(item)
			if !ok || !isGitHubRepoAllowed(owner, repo) {
				continue
			}

			issue, err := getGitHubIssue(ctx, client, owner, repo, item.GetNumber())
			if err != nil {
				return nil, err
			}
			model := toIssueModelFromGitHubIssue(issue)
			if model.UpdatedAt.IsZero() || model.UpdatedAt.Before(cutoff) {
				continue
			}

			key := buildGitHubItemKey(owner, repo, model.Number)
			activity, exists := byKey[key]
			if !exists {
				activity = IssueActivity{Owner: owner, Repo: repo, Issue: model, UpdatedAt: model.UpdatedAt}
			} else {
				if model.UpdatedAt.After(activity.UpdatedAt) {
					activity.UpdatedAt = model.UpdatedAt
				}
				activity.Issue = model
			}
			if shouldUpdateLabel(activity.Label, q.Label, false) {
				activity.Label = q.Label
			}

			if config.db != nil {
				if err := config.db.SaveGitHubIssueWithLabel(owner, repo, model, activity.Label, config.debugMode); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save GitHub issue %s/%s#%d: %v\n", owner, repo, model.Number, err)
					}
				}
			}

			byKey[key] = activity
		}
	}

	activities := make([]IssueActivity, 0, len(byKey))
	for _, activity := range byKey {
		activities = append(activities, activity)
	}
	return activities, nil
}

func loadGitHubCachedActivities(cutoff time.Time) ([]PRActivity, []IssueActivity, error) {
	if config.db == nil {
		return []PRActivity{}, []IssueActivity{}, nil
	}

	allPRs, prLabels, err := config.db.GetAllGitHubPullRequestsWithLabels(config.debugMode)
	if err != nil {
		return nil, nil, err
	}

	activities := make([]PRActivity, 0, len(allPRs))
	prReviewComments := make(map[string][]GitHubPRReviewCommentRecord)
	for key, pr := range allPRs {
		if pr.UpdatedAt.IsZero() || pr.UpdatedAt.Before(cutoff) {
			continue
		}

		owner, repo, _, ok := parseGitHubItemKey(key)
		if !ok || !isGitHubRepoAllowed(owner, repo) {
			continue
		}

		activities = append(activities, PRActivity{
			Label:     prLabels[key],
			Owner:     owner,
			Repo:      repo,
			MR:        pr,
			UpdatedAt: pr.UpdatedAt,
		})

		comments, err := config.db.GetGitHubPRReviewComments(owner, repo, pr.Number)
		if err != nil {
			return nil, nil, err
		}
		prReviewComments[key] = comments
	}

	allIssues, issueLabels, err := config.db.GetAllGitHubIssuesWithLabels(config.debugMode)
	if err != nil {
		return nil, nil, err
	}

	issueActivities := make([]IssueActivity, 0, len(allIssues))
	for key, issue := range allIssues {
		if issue.UpdatedAt.IsZero() || issue.UpdatedAt.Before(cutoff) {
			continue
		}

		owner, repo, _, ok := parseGitHubItemKey(key)
		if !ok || !isGitHubRepoAllowed(owner, repo) {
			continue
		}

		issueActivities = append(issueActivities, IssueActivity{
			Label:     issueLabels[key],
			Owner:     owner,
			Repo:      repo,
			Issue:     issue,
			UpdatedAt: issue.UpdatedAt,
		})
	}

	nestedPRs := nestGitHubIssues(activities, issueActivities, prReviewComments)
	standaloneIssues := filterStandaloneGitHubIssues(nestedPRs, issueActivities)
	return nestedPRs, standaloneIssues, nil
}

func searchGitHubIssues(ctx context.Context, client *github.Client, query string) ([]*github.Issue, error) {
	allIssues := make([]*github.Issue, 0)
	options := &github.SearchOptions{ListOptions: github.ListOptions{PerPage: 100, Page: 1}}

	for {
		result, resp, err := client.Search.Issues(ctx, query, options)
		if err != nil {
			return nil, err
		}
		allIssues = append(allIssues, result.Issues...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		options.Page = resp.NextPage
	}

	return allIssues, nil
}

func newGitHubClient(token string) *github.Client {
	tokenSource := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: strings.TrimSpace(token)})
	httpClient := oauth2.NewClient(context.Background(), tokenSource)
	return github.NewClient(httpClient)
}

func getGitHubPullRequest(ctx context.Context, client *github.Client, owner, repo string, number int) (*github.PullRequest, error) {
	pr, _, err := client.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get pull request %s/%s#%d: %w", owner, repo, number, err)
	}
	return pr, nil
}

func getGitHubIssue(ctx context.Context, client *github.Client, owner, repo string, number int) (*github.Issue, error) {
	issue, _, err := client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, fmt.Errorf("get issue %s/%s#%d: %w", owner, repo, number, err)
	}
	return issue, nil
}

func listGitHubPRReviewComments(ctx context.Context, client *github.Client, owner, repo string, number int) ([]*github.PullRequestComment, error) {
	allComments := make([]*github.PullRequestComment, 0)
	options := &github.PullRequestListCommentsOptions{ListOptions: github.ListOptions{PerPage: 100, Page: 1}}

	for {
		comments, resp, err := client.PullRequests.ListComments(ctx, owner, repo, number, options)
		if err != nil {
			return nil, fmt.Errorf("list PR review comments for %s/%s#%d: %w", owner, repo, number, err)
		}
		allComments = append(allComments, comments...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		options.Page = resp.NextPage
	}

	return allComments, nil
}

func parseGitHubRepoFromSearchItem(item *github.Issue) (string, string, bool) {
	if item == nil {
		return "", "", false
	}

	repoURL := strings.TrimSpace(item.GetRepositoryURL())
	if repoURL == "" {
		htmlURL := strings.TrimSpace(item.GetHTMLURL())
		if htmlURL == "" {
			return "", "", false
		}
		parsed, err := url.Parse(htmlURL)
		if err != nil {
			return "", "", false
		}
		parts := splitPathParts(parsed.Path)
		if len(parts) < 2 {
			return "", "", false
		}
		return parts[0], parts[1], true
	}

	parsed, err := url.Parse(repoURL)
	if err != nil {
		return "", "", false
	}
	parts := splitPathParts(parsed.Path)
	if len(parts) < 3 {
		return "", "", false
	}
	if !strings.EqualFold(parts[0], "repos") {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func splitPathParts(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	parts := strings.Split(trimmed, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value := strings.TrimSpace(part)
		if value == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func toMergeRequestModelFromGitHubPR(pr *github.PullRequest) MergeRequestModel {
	if pr == nil {
		return MergeRequestModel{}
	}

	updatedAt := time.Time{}
	if pr.UpdatedAt != nil {
		updatedAt = pr.UpdatedAt.Time
	}

	state := strings.ToLower(pr.GetState())
	if state == "" {
		state = "open"
	}

	userLogin := ""
	if pr.User != nil {
		userLogin = pr.User.GetLogin()
	}

	return MergeRequestModel{
		Number:    pr.GetNumber(),
		Title:     pr.GetTitle(),
		Body:      pr.GetBody(),
		State:     state,
		UpdatedAt: updatedAt,
		WebURL:    pr.GetHTMLURL(),
		UserLogin: userLogin,
		Merged:    pr.GetMerged(),
	}
}

func toIssueModelFromGitHubIssue(issue *github.Issue) IssueModel {
	if issue == nil {
		return IssueModel{}
	}

	updatedAt := time.Time{}
	if issue.UpdatedAt != nil {
		updatedAt = issue.UpdatedAt.Time
	}

	state := strings.ToLower(issue.GetState())
	if state == "" {
		state = "open"
	}

	userLogin := ""
	if issue.User != nil {
		userLogin = issue.User.GetLogin()
	}

	return IssueModel{
		Number:    issue.GetNumber(),
		Title:     issue.GetTitle(),
		Body:      issue.GetBody(),
		State:     state,
		UpdatedAt: updatedAt,
		WebURL:    issue.GetHTMLURL(),
		UserLogin: userLogin,
	}
}

func toGitHubPRReviewCommentRecord(owner, repo string, prNumber int, comment *github.PullRequestComment) GitHubPRReviewCommentRecord {
	record := GitHubPRReviewCommentRecord{Owner: owner, Repo: repo, PRNumber: prNumber}
	if comment == nil {
		return record
	}

	record.CommentID = comment.GetID()
	record.Body = comment.GetBody()
	if comment.User != nil {
		record.AuthorUsername = comment.User.GetLogin()
		record.AuthorID = comment.User.GetID()
	}

	return record
}

func parseGitHubItemKey(key string) (string, string, int, bool) {
	parts := strings.SplitN(key, "#", 2)
	if len(parts) != 2 {
		return "", "", 0, false
	}

	repoParts := strings.SplitN(parts[0], "/", 2)
	if len(repoParts) != 2 {
		return "", "", 0, false
	}

	number, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || number <= 0 {
		return "", "", 0, false
	}

	owner := strings.TrimSpace(repoParts[0])
	repo := strings.TrimSpace(repoParts[1])
	if owner == "" || repo == "" {
		return "", "", 0, false
	}

	return owner, repo, number, true
}

func isGitHubRepoAllowed(owner, repo string) bool {
	if len(config.allowedRepos) == 0 {
		return true
	}

	target := strings.ToLower(strings.TrimSpace(owner + "/" + repo))
	for allowed := range config.allowedRepos {
		if strings.ToLower(strings.TrimSpace(allowed)) == target {
			return true
		}
	}
	return false
}

func nestGitHubIssues(
	activities []PRActivity,
	issueActivities []IssueActivity,
	prReviewComments map[string][]GitHubPRReviewCommentRecord,
) []PRActivity {
	issueByKey := make(map[string]IssueActivity, len(issueActivities))
	for _, issue := range issueActivities {
		issueByKey[buildGitHubItemKey(issue.Owner, issue.Repo, issue.Issue.Number)] = issue
	}

	for i := range activities {
		activities[i].Issues = nil
		for _, issue := range issueActivities {
			key := buildGitHubItemKey(activities[i].Owner, activities[i].Repo, activities[i].MR.Number)
			if areGitHubCrossReferenced(activities[i], issue, prReviewComments[key]) {
				issueKey := buildGitHubItemKey(issue.Owner, issue.Repo, issue.Issue.Number)
				if nestedIssue, ok := issueByKey[issueKey]; ok {
					activities[i].Issues = append(activities[i].Issues, nestedIssue)
				}
			}
		}
		sort.Slice(activities[i].Issues, func(a, b int) bool {
			return activities[i].Issues[a].UpdatedAt.After(activities[i].Issues[b].UpdatedAt)
		})
	}

	return activities
}

func filterStandaloneGitHubIssues(activities []PRActivity, issueActivities []IssueActivity) []IssueActivity {
	nestedIssueKeys := make(map[string]struct{})
	for _, activity := range activities {
		for _, issue := range activity.Issues {
			nestedIssueKeys[buildGitHubItemKey(issue.Owner, issue.Repo, issue.Issue.Number)] = struct{}{}
		}
	}

	standalone := make([]IssueActivity, 0, len(issueActivities))
	for _, issue := range issueActivities {
		key := buildGitHubItemKey(issue.Owner, issue.Repo, issue.Issue.Number)
		if _, nested := nestedIssueKeys[key]; nested {
			continue
		}
		standalone = append(standalone, issue)
	}

	return standalone
}

func areGitHubCrossReferenced(prActivity PRActivity, issueActivity IssueActivity, reviewComments []GitHubPRReviewCommentRecord) bool {
	if !strings.EqualFold(strings.TrimSpace(prActivity.Owner), strings.TrimSpace(issueActivity.Owner)) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(prActivity.Repo), strings.TrimSpace(issueActivity.Repo)) {
		return false
	}

	if mentionsNumber(prActivity.MR.Body, issueActivity.Issue.Number, prActivity.Owner, prActivity.Repo) {
		return true
	}
	if mentionsNumber(issueActivity.Issue.Body, prActivity.MR.Number, prActivity.Owner, prActivity.Repo) {
		return true
	}
	for _, comment := range reviewComments {
		if mentionsNumber(comment.Body, issueActivity.Issue.Number, prActivity.Owner, prActivity.Repo) {
			return true
		}
	}

	return false
}

func mentionsNumber(text string, number int, owner, repo string) bool {
	if strings.TrimSpace(text) == "" || number <= 0 {
		return false
	}

	targetRepo := strings.ToLower(strings.TrimSpace(owner + "/" + repo))
	targetNumber := strconv.Itoa(number)
	keywordOnly := githubCrossRefKeywordPattern.MatchString(text)

	for _, match := range githubCrossRefURLPattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 4 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(match[1]), strings.TrimSpace(owner)) &&
			strings.EqualFold(strings.TrimSpace(match[2]), strings.TrimSpace(repo)) &&
			strings.TrimSpace(match[3]) == targetNumber {
			return true
		}
	}

	for _, match := range githubCrossRefQualifiedRef.FindAllStringSubmatch(text, -1) {
		if len(match) < 3 {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(match[1]), targetRepo) && strings.TrimSpace(match[2]) == targetNumber {
			return true
		}
	}

	for _, match := range githubCrossRefSameRefPattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 2 {
			continue
		}
		if strings.TrimSpace(match[1]) == targetNumber {
			return true
		}
	}

	if keywordOnly {
		return false
	}

	return false
}
