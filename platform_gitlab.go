package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fatih/color"
	gitlab "gitlab.com/gitlab-org/api/client-go"
)

var retryAfter = time.After

const defaultGitLabBaseURL = "https://gitlab.com"

func normalizeGitLabBaseURL(raw string) (string, error) {
	baseURL := strings.TrimSpace(raw)
	if baseURL == "" {
		baseURL = defaultGitLabBaseURL
	}

	parsed, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid GitLab base URL %q: %w", baseURL, err)
	}

	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid GitLab base URL %q: must include scheme and host", baseURL)
	}

	normalizedPath := strings.TrimSuffix(parsed.EscapedPath(), "/")
	if normalizedPath == "" {
		normalizedPath = "/api/v4"
	} else if !strings.HasSuffix(normalizedPath, "/api/v4") {
		normalizedPath += "/api/v4"
	}

	parsed.Path = normalizedPath
	parsed.RawPath = ""

	return parsed.String(), nil
}

func newGitLabClient(token, rawBaseURL string) (*gitlab.Client, string, error) {
	normalizedBaseURL, err := normalizeGitLabBaseURL(rawBaseURL)
	if err != nil {
		return nil, "", err
	}

	client, err := gitlab.NewClient(token, gitlab.WithBaseURL(normalizedBaseURL))
	if err != nil {
		return nil, "", fmt.Errorf("failed to create GitLab client: %w", err)
	}

	return client, normalizedBaseURL, nil
}

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
	return 999
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
	return 999
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

func retryWithBackoff(operation func() error, operationName string) error {
	const (
		initialBackoff = 1 * time.Second
		maxBackoff     = 30 * time.Second
		backoffFactor  = 1.5
	)

	backoff := initialBackoff
	attempt := 1
	retryCtx := config.ctx
	if retryCtx == nil {
		retryCtx = context.Background()
	}

	for {
		err := operation()
		if err == nil {
			return nil
		}

		var gitLabErr *gitlab.ErrorResponse
		var waitTime time.Duration
		var isRateLimitError bool
		var isTransientServerError bool
		shouldRetry := true

		if errors.As(err, &gitLabErr) && gitLabErr.Response != nil {
			statusCode := gitLabErr.Response.StatusCode

			if statusCode == http.StatusTooManyRequests {
				isRateLimitError = true
				retryAfterSeconds, parseErr := strconv.Atoi(strings.TrimSpace(gitLabErr.Response.Header.Get("Retry-After")))
				if parseErr == nil && retryAfterSeconds > 0 {
					waitTime = time.Duration(retryAfterSeconds) * time.Second
				} else if resetWait, ok := gitLabRateLimitResetWait(gitLabErr.Response.Header.Get("Ratelimit-Reset")); ok {
					waitTime = resetWait
				} else {
					waitTime = time.Duration(math.Min(float64(backoff), float64(maxBackoff)))
				}

				if config.debugMode {
					fmt.Printf("  [%s] GitLab rate limit hit (attempt %d), waiting %v before retry...\n",
						operationName, attempt, waitTime.Round(time.Second))
				}
			} else if statusCode >= http.StatusInternalServerError && statusCode <= 599 {
				isTransientServerError = true
				waitTime = time.Duration(math.Min(float64(backoff), float64(maxBackoff)))

				if config.debugMode {
					fmt.Printf("  [%s] GitLab server error %d (attempt %d), waiting %v before retry...\n",
						operationName, statusCode, attempt, waitTime)
				}
			} else {
				shouldRetry = false
			}
		} else {
			isRateLimitError = strings.Contains(err.Error(), "rate limit") ||
				strings.Contains(err.Error(), "API rate limit exceeded") ||
				strings.Contains(err.Error(), "403")

			if isRateLimitError {
				waitTime = time.Duration(math.Min(float64(backoff), float64(maxBackoff)))
				if config.debugMode {
					fmt.Printf("  [%s] Rate limit hit (attempt %d), waiting %v before retry...\n",
						operationName, attempt, waitTime)
				}
			}
		}

		if !shouldRetry {
			return err
		}

		if isRateLimitError {
			if config.debugMode {
				select {
				case <-retryCtx.Done():
					return retryCtx.Err()
				case <-retryAfter(waitTime):
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
					case <-retryCtx.Done():
						return retryCtx.Err()
					case <-ticker.C:
						remaining--
					}
				}
			}

			backoff = time.Duration(float64(backoff) * backoffFactor)
		} else if isTransientServerError {
			if config.debugMode {
				select {
				case <-retryCtx.Done():
					return retryCtx.Err()
				case <-retryAfter(waitTime):
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
					case <-retryCtx.Done():
						return retryCtx.Err()
					case <-ticker.C:
						remaining--
					}
				}
			}

			backoff = time.Duration(float64(backoff) * backoffFactor)
		} else {
			waitTime := time.Duration(math.Min(float64(backoff)/2, float64(5*time.Second)))

			if config.debugMode {
				fmt.Printf("  [%s] Error (attempt %d): %v, waiting %v before retry...\n",
					operationName, attempt, err, waitTime)
				select {
				case <-retryCtx.Done():
					return retryCtx.Err()
				case <-retryAfter(waitTime):
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
					case <-retryCtx.Done():
						return retryCtx.Err()
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

func gitLabRateLimitResetWait(rawHeader string) (time.Duration, bool) {
	resetAtUnix, err := strconv.ParseInt(strings.TrimSpace(rawHeader), 10, 64)
	if err != nil || resetAtUnix <= 0 {
		return 0, false
	}

	resetTime := time.Unix(resetAtUnix, 0)
	waitTime := time.Until(resetTime)
	if waitTime <= 0 {
		return 1 * time.Second, true
	}

	return waitTime, true
}

type gitLabProject struct {
	PathWithNamespace string
	ID                int64
}

func fetchAndDisplayGitLabActivity() {
	startTime := time.Now()

	if config.debugMode {
		fmt.Println("Fetching data from GitLab...")
	} else {
		fmt.Print("Fetching data from GitLab... ")
	}

	cutoffTime := time.Now().Add(-config.timeRange)
	var (
		activities      []PRActivity
		issueActivities []IssueActivity
		err             error
	)

	if config.localMode {
		activities, issueActivities, err = loadGitLabCachedActivities(cutoffTime)
	} else {
		activities, issueActivities, err = fetchGitLabProjectActivities(
			config.ctx,
			config.gitlabClient,
			config.allowedRepos,
			cutoffTime,
			config.gitlabUsername,
			config.gitlabUserID,
			config.db,
		)
	}
	if err != nil {
		fmt.Printf("Error fetching GitLab activity: %v\n", err)
		return
	}

	if config.debugMode {
		fmt.Println()
		fmt.Printf("Total fetch time: %v\n", time.Since(startTime).Round(time.Millisecond))
		fmt.Printf("Found %d unique merge requests and %d unique issues\n", len(activities), len(issueActivities))
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
			if len(activity.Issues) > 0 {
				for _, issue := range activity.Issues {
					displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
				}
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
			if len(activity.Issues) > 0 {
				for _, issue := range activity.Issues {
					displayIssue(issue.Label, issue.Owner, issue.Repo, issue.Issue, true, issue.HasUpdates)
				}
			}
		}
		for _, activity := range closedPRs {
			displayMergeRequest(activity.Label, activity.Owner, activity.Repo, activity.MR, activity.HasUpdates)
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
}

func fetchGitLabProjectActivities(
	ctx context.Context,
	client *gitlab.Client,
	allowedRepos map[string]bool,
	cutoff time.Time,
	currentUsername string,
	currentUserID int64,
	db *Database,
) ([]PRActivity, []IssueActivity, error) {
	projects, err := resolveAllowedGitLabProjects(ctx, client, allowedRepos)
	if err != nil {
		return nil, nil, err
	}

	currentUsername = strings.TrimSpace(currentUsername)
	if currentUsername == "" {
		return nil, nil, fmt.Errorf("gitlab current username is required")
	}

	if len(projects) == 0 {
		return []PRActivity{}, []IssueActivity{}, nil
	}

	activities := make([]PRActivity, 0)
	issueActivities := make([]IssueActivity, 0)
	seenMergeRequests := make(map[string]struct{})
	seenIssues := make(map[string]struct{})
	projectIDByPath := make(map[string]int64, len(projects))
	mrNotesByKey := make(map[string][]*gitlab.Note)

	for _, project := range projects {
		projectIDByPath[normalizeProjectPathWithNamespace(project.PathWithNamespace)] = project.ID
	}

	for _, project := range projects {
		projectMergeRequests, err := listGitLabProjectMergeRequests(ctx, client, project.ID, cutoff)
		if err != nil {
			return nil, nil, fmt.Errorf("list merge requests for %s: %w", project.PathWithNamespace, err)
		}

		for _, item := range projectMergeRequests {
			key := buildGitLabDedupKey(project.PathWithNamespace, "mr", item.IID)
			if _, exists := seenMergeRequests[key]; exists {
				continue
			}
			seenMergeRequests[key] = struct{}{}

			model := toMergeRequestModelFromGitLab(item)
			if model.UpdatedAt.IsZero() || model.UpdatedAt.Before(cutoff) {
				continue
			}

			label, notes, err := deriveGitLabMergeRequestLabel(ctx, client, project.ID, item, currentUsername, currentUserID)
			if err != nil {
				return nil, nil, fmt.Errorf("derive merge request label for %s!%d: %w", project.PathWithNamespace, item.IID, err)
			}

			if db != nil {
				if err := db.SaveGitLabMergeRequestWithLabel(project.PathWithNamespace, model, label, config.debugMode); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save GitLab MR %s!%d: %v\n", project.PathWithNamespace, item.IID, err)
					}
				}
				if err := persistGitLabNotes(db, project.PathWithNamespace, "mr", int(item.IID), notes); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save GitLab MR notes %s!%d: %v\n", project.PathWithNamespace, item.IID, err)
					}
				}
			}

			mrNotesByKey[buildGitLabMergeRequestKey(project.PathWithNamespace, model.Number)] = notes

			owner, repo, ok := splitGitLabPathWithNamespace(project.PathWithNamespace)
			if !ok {
				owner = project.PathWithNamespace
				repo = ""
			}

			activities = append(activities, PRActivity{
				Label:     label,
				Owner:     owner,
				Repo:      repo,
				MR:        model,
				UpdatedAt: model.UpdatedAt,
			})
		}

		projectIssues, err := listGitLabProjectIssues(ctx, client, project.ID, cutoff)
		if err != nil {
			return nil, nil, fmt.Errorf("list issues for %s: %w", project.PathWithNamespace, err)
		}

		for _, item := range projectIssues {
			key := buildGitLabDedupKey(project.PathWithNamespace, "issue", item.IID)
			if _, exists := seenIssues[key]; exists {
				continue
			}
			seenIssues[key] = struct{}{}

			model := toIssueModelFromGitLab(item)
			if model.UpdatedAt.IsZero() || model.UpdatedAt.Before(cutoff) {
				continue
			}

			label, notes, err := deriveGitLabIssueLabel(ctx, client, project.ID, item, currentUsername, currentUserID)
			if err != nil {
				return nil, nil, fmt.Errorf("derive issue label for %s#%d: %w", project.PathWithNamespace, item.IID, err)
			}

			if db != nil {
				if err := db.SaveGitLabIssueWithLabel(project.PathWithNamespace, model, label, config.debugMode); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save GitLab issue %s#%d: %v\n", project.PathWithNamespace, item.IID, err)
					}
				}
				if err := persistGitLabNotes(db, project.PathWithNamespace, "issue", int(item.IID), notes); err != nil {
					config.dbErrorCount.Add(1)
					if config.debugMode {
						fmt.Printf("  [DB] Warning: Failed to save GitLab issue notes %s#%d: %v\n", project.PathWithNamespace, item.IID, err)
					}
				}
			}

			owner, repo, ok := splitGitLabPathWithNamespace(project.PathWithNamespace)
			if !ok {
				owner = project.PathWithNamespace
				repo = ""
			}

			issueActivities = append(issueActivities, IssueActivity{
				Label:     label,
				Owner:     owner,
				Repo:      repo,
				Issue:     model,
				UpdatedAt: model.UpdatedAt,
			})
		}
	}

	activities, issueActivities, err = linkGitLabCrossReferencesOnline(ctx, client, activities, issueActivities, projectIDByPath, mrNotesByKey, db)
	if err != nil {
		return nil, nil, err
	}

	return activities, issueActivities, nil
}

func deriveGitLabMergeRequestLabel(
	ctx context.Context,
	client *gitlab.Client,
	projectID int64,
	item *gitlab.BasicMergeRequest,
	currentUsername string,
	currentUserID int64,
) (string, []*gitlab.Note, error) {
	if item == nil {
		return "Involved", nil, nil
	}

	currentLabel := ""
	if matchesGitLabBasicUser(item.Author, currentUsername, currentUserID) {
		currentLabel = mergeLabelWithPriority(currentLabel, "Authored", true)
	}
	if gitLabBasicUserListContains(item.Assignees, currentUsername, currentUserID) || matchesGitLabBasicUser(item.Assignee, currentUsername, currentUserID) {
		currentLabel = mergeLabelWithPriority(currentLabel, "Assigned", true)
	}

	if currentLabel == "Authored" || currentLabel == "Assigned" {
		return currentLabel, nil, nil
	}

	var approvalState *gitlab.MergeRequestApprovalState
	err := retryWithBackoff(func() error {
		var apiErr error
		approvalState, _, apiErr = client.MergeRequestApprovals.GetApprovalState(projectID, item.IID, gitlab.WithContext(ctx))
		return apiErr
	}, fmt.Sprintf("GitLabGetApprovalState %d!%d", projectID, item.IID))
	if err != nil {
		return "", nil, err
	}
	if gitLabApprovalStateReviewedByCurrentUser(approvalState, currentUsername, currentUserID) {
		currentLabel = mergeLabelWithPriority(currentLabel, "Reviewed", true)
	}

	if gitLabBasicUserListContains(item.Reviewers, currentUsername, currentUserID) {
		currentLabel = mergeLabelWithPriority(currentLabel, "Review Requested", true)
	}

	if !needsLowerPriorityPRChecks(currentLabel) {
		if currentLabel == "" {
			return "Involved", nil, nil
		}
		return currentLabel, nil, nil
	}

	notes, err := listAllGitLabMergeRequestNotes(ctx, client, projectID, item.IID)
	if err != nil {
		return "", nil, err
	}

	commented, mentioned := gitLabNotesInvolvement(notes, item.Description, currentUsername, currentUserID)
	if commented {
		currentLabel = mergeLabelWithPriority(currentLabel, "Commented", true)
	}
	if mentioned {
		currentLabel = mergeLabelWithPriority(currentLabel, "Mentioned", true)
	}

	if currentLabel == "" {
		return "Involved", notes, nil
	}
	return currentLabel, notes, nil
}

func deriveGitLabIssueLabel(
	ctx context.Context,
	client *gitlab.Client,
	projectID int64,
	item *gitlab.Issue,
	currentUsername string,
	currentUserID int64,
) (string, []*gitlab.Note, error) {
	if item == nil {
		return "Involved", nil, nil
	}

	currentLabel := ""
	if matchesGitLabIssueAuthor(item.Author, currentUsername, currentUserID) {
		currentLabel = mergeLabelWithPriority(currentLabel, "Authored", false)
	}
	if gitLabIssueAssigneeListContains(item.Assignees, currentUsername, currentUserID) || matchesGitLabIssueAssignee(item.Assignee, currentUsername, currentUserID) {
		currentLabel = mergeLabelWithPriority(currentLabel, "Assigned", false)
	}

	if currentLabel == "Authored" || currentLabel == "Assigned" {
		return currentLabel, nil, nil
	}

	notes, err := listAllGitLabIssueNotes(ctx, client, projectID, item.IID)
	if err != nil {
		return "", nil, err
	}

	commented, mentioned := gitLabNotesInvolvement(notes, item.Description, currentUsername, currentUserID)
	if commented {
		currentLabel = mergeLabelWithPriority(currentLabel, "Commented", false)
	}
	if mentioned {
		currentLabel = mergeLabelWithPriority(currentLabel, "Mentioned", false)
	}

	if currentLabel == "" {
		return "Involved", notes, nil
	}
	return currentLabel, notes, nil
}

func persistGitLabNotes(db *Database, projectPath, itemType string, itemIID int, notes []*gitlab.Note) error {
	if db == nil || len(notes) == 0 {
		return nil
	}

	for _, note := range notes {
		if note == nil {
			continue
		}

		authorUsername := ""
		authorID := int64(0)
		author := note.Author
		authorUsername = strings.TrimSpace(author.Username)
		authorID = author.ID

		record := GitLabNoteRecord{
			ProjectPath:    projectPath,
			ItemType:       itemType,
			ItemIID:        itemIID,
			NoteID:         int64(note.ID),
			Body:           note.Body,
			AuthorUsername: authorUsername,
			AuthorID:       authorID,
		}

		if err := db.SaveGitLabNote(record, config.debugMode); err != nil {
			return err
		}
	}

	return nil
}

func loadGitLabCachedActivities(cutoff time.Time) ([]PRActivity, []IssueActivity, error) {
	if config.db == nil {
		return []PRActivity{}, []IssueActivity{}, nil
	}

	allMRs, mrLabels, err := config.db.GetAllGitLabMergeRequestsWithLabels(config.debugMode)
	if err != nil {
		return nil, nil, err
	}

	activities := make([]PRActivity, 0, len(allMRs))
	for key, mr := range allMRs {
		if mr.UpdatedAt.IsZero() || mr.UpdatedAt.Before(cutoff) {
			continue
		}

		projectPath, ok := parseGitLabMRProjectPath(key)
		if !ok || !isGitLabProjectAllowed(projectPath) {
			continue
		}

		owner, repo, ok := splitGitLabPathWithNamespace(projectPath)
		if !ok {
			owner = projectPath
			repo = ""
		}

		activities = append(activities, PRActivity{
			Label:     mrLabels[key],
			Owner:     owner,
			Repo:      repo,
			MR:        mr,
			UpdatedAt: mr.UpdatedAt,
		})
	}

	allIssues, issueLabels, err := config.db.GetAllGitLabIssuesWithLabels(config.debugMode)
	if err != nil {
		return nil, nil, err
	}

	issueActivities := make([]IssueActivity, 0, len(allIssues))
	for key, issue := range allIssues {
		if issue.UpdatedAt.IsZero() || issue.UpdatedAt.Before(cutoff) {
			continue
		}

		projectPath, ok := parseGitLabIssueProjectPath(key)
		if !ok || !isGitLabProjectAllowed(projectPath) {
			continue
		}

		owner, repo, ok := splitGitLabPathWithNamespace(projectPath)
		if !ok {
			owner = projectPath
			repo = ""
		}

		issueActivities = append(issueActivities, IssueActivity{
			Label:     issueLabels[key],
			Owner:     owner,
			Repo:      repo,
			Issue:     issue,
			UpdatedAt: issue.UpdatedAt,
		})
	}

	activities, issueActivities, err = linkGitLabCrossReferencesOffline(config.db, activities, issueActivities)
	if err != nil {
		return nil, nil, err
	}

	return activities, issueActivities, nil
}

var (
	gitLabIssueSameProjectRefPattern = regexp.MustCompile(`(?i)(?:^|[^a-z0-9_])#([0-9]+)\b`)
	gitLabIssueQualifiedRefPattern   = regexp.MustCompile(`(?i)([a-z0-9_.-]+(?:/[a-z0-9_.-]+)+)#([0-9]+)\b`)
	gitLabIssueURLRefPattern         = regexp.MustCompile(`(?i)https?://[^\s]+/([a-z0-9_.-]+(?:/[a-z0-9_.-]+)+)/-/issues/([0-9]+)\b`)
	gitLabIssueRelativeURLRefPattern = regexp.MustCompile(`(?i)/-/issues/([0-9]+)\b`)
)

func linkGitLabCrossReferencesOnline(
	ctx context.Context,
	client *gitlab.Client,
	activities []PRActivity,
	issueActivities []IssueActivity,
	projectIDByPath map[string]int64,
	mrNotesByKey map[string][]*gitlab.Note,
	db *Database,
) ([]PRActivity, []IssueActivity, error) {
	mrToIssueKeys := make(map[string]map[string]struct{}, len(activities))

	for _, activity := range activities {
		projectPath := normalizeProjectPathWithNamespace(gitLabProjectPath(activity.Owner, activity.Repo))
		projectID, ok := projectIDByPath[projectPath]
		if !ok {
			continue
		}

		mrKey := buildGitLabMergeRequestKey(projectPath, activity.MR.Number)
		closedIssues, err := listGitLabIssuesClosedOnMergeRequest(ctx, client, projectID, int64(activity.MR.Number))
		if err == nil {
			resolvedKeys := make(map[string]struct{})
			for _, item := range closedIssues {
				issueKey, ok := gitLabIssueKeyFromIssue(item, projectPath)
				if !ok {
					continue
				}
				resolvedKeys[issueKey] = struct{}{}
			}
			if len(resolvedKeys) > 0 {
				mrToIssueKeys[mrKey] = resolvedKeys
			}
			continue
		}

		fallbackKeys := gitLabIssueReferenceKeysFromText(activity.MR.Body, projectPath)
		if len(fallbackKeys) == 0 {
			notes := mrNotesByKey[mrKey]
			if len(notes) == 0 {
				notes, err = listAllGitLabMergeRequestNotes(ctx, client, projectID, int64(activity.MR.Number))
				if err == nil {
					mrNotesByKey[mrKey] = notes
					if db != nil {
						if persistErr := persistGitLabNotes(db, projectPath, "mr", activity.MR.Number, notes); persistErr != nil {
							config.dbErrorCount.Add(1)
							if config.debugMode {
								fmt.Printf("  [DB] Warning: Failed to save GitLab MR notes %s!%d: %v\n", projectPath, activity.MR.Number, persistErr)
							}
						}
					}
				}
			}

			for _, note := range notes {
				if note == nil {
					continue
				}
				for issueKey := range gitLabIssueReferenceKeysFromText(note.Body, projectPath) {
					fallbackKeys[issueKey] = struct{}{}
				}
			}
		}

		if len(fallbackKeys) > 0 {
			mrToIssueKeys[mrKey] = fallbackKeys
		}
	}

	nestedActivities := nestGitLabIssues(activities, issueActivities, mrToIssueKeys)
	return nestedActivities, filterStandaloneGitLabIssues(nestedActivities, issueActivities), nil
}

func linkGitLabCrossReferencesOffline(db *Database, activities []PRActivity, issueActivities []IssueActivity) ([]PRActivity, []IssueActivity, error) {
	mrToIssueKeys := make(map[string]map[string]struct{}, len(activities))

	for _, activity := range activities {
		projectPath := normalizeProjectPathWithNamespace(gitLabProjectPath(activity.Owner, activity.Repo))
		mrKey := buildGitLabMergeRequestKey(projectPath, activity.MR.Number)
		linked := gitLabIssueReferenceKeysFromText(activity.MR.Body, projectPath)
		if len(linked) == 0 && db != nil {
			notes, err := db.GetGitLabNotes(projectPath, "mr", activity.MR.Number)
			if err != nil {
				return nil, nil, err
			}
			for _, note := range notes {
				for issueKey := range gitLabIssueReferenceKeysFromText(note.Body, projectPath) {
					linked[issueKey] = struct{}{}
				}
			}
		}

		if len(linked) > 0 {
			mrToIssueKeys[mrKey] = linked
		}
	}

	nestedActivities := nestGitLabIssues(activities, issueActivities, mrToIssueKeys)
	return nestedActivities, filterStandaloneGitLabIssues(nestedActivities, issueActivities), nil
}

func listGitLabIssuesClosedOnMergeRequest(ctx context.Context, client *gitlab.Client, projectID int64, mergeRequestIID int64) ([]*gitlab.Issue, error) {
	allIssues := make([]*gitlab.Issue, 0)
	opts := &gitlab.GetIssuesClosedOnMergeOptions{ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1}}

	for {
		issues, resp, err := client.MergeRequests.GetIssuesClosedOnMerge(projectID, mergeRequestIID, opts, gitlab.WithContext(ctx))
		if err != nil {
			return nil, err
		}
		allIssues = append(allIssues, issues...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}

	return allIssues, nil
}

func nestGitLabIssues(activities []PRActivity, issueActivities []IssueActivity, mrToIssueKeys map[string]map[string]struct{}) []PRActivity {
	issueByKey := make(map[string]IssueActivity, len(issueActivities))
	for _, issue := range issueActivities {
		projectPath := normalizeProjectPathWithNamespace(gitLabProjectPath(issue.Owner, issue.Repo))
		issueByKey[buildGitLabIssueKey(projectPath, issue.Issue.Number)] = issue
	}

	for i := range activities {
		activities[i].Issues = nil
		projectPath := normalizeProjectPathWithNamespace(gitLabProjectPath(activities[i].Owner, activities[i].Repo))
		mrKey := buildGitLabMergeRequestKey(projectPath, activities[i].MR.Number)
		linkedKeys := mrToIssueKeys[mrKey]
		if len(linkedKeys) == 0 {
			continue
		}
		for issueKey := range linkedKeys {
			issue, ok := issueByKey[issueKey]
			if !ok {
				continue
			}
			activities[i].Issues = append(activities[i].Issues, issue)
		}
		sort.Slice(activities[i].Issues, func(a, b int) bool {
			return activities[i].Issues[a].UpdatedAt.After(activities[i].Issues[b].UpdatedAt)
		})
	}

	return activities
}

func filterStandaloneGitLabIssues(activities []PRActivity, issueActivities []IssueActivity) []IssueActivity {
	linkedIssueKeys := make(map[string]struct{})
	for _, activity := range activities {
		for _, issue := range activity.Issues {
			projectPath := normalizeProjectPathWithNamespace(gitLabProjectPath(issue.Owner, issue.Repo))
			linkedIssueKeys[buildGitLabIssueKey(projectPath, issue.Issue.Number)] = struct{}{}
		}
	}

	standalone := make([]IssueActivity, 0, len(issueActivities))
	for _, issue := range issueActivities {
		projectPath := normalizeProjectPathWithNamespace(gitLabProjectPath(issue.Owner, issue.Repo))
		issueKey := buildGitLabIssueKey(projectPath, issue.Issue.Number)
		if _, linked := linkedIssueKeys[issueKey]; linked {
			continue
		}
		standalone = append(standalone, issue)
	}

	return standalone
}

func gitLabIssueReferenceKeysFromText(text, defaultProjectPath string) map[string]struct{} {
	results := make(map[string]struct{})
	if strings.TrimSpace(text) == "" {
		return results
	}

	for _, match := range gitLabIssueURLRefPattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 3 {
			continue
		}
		iid, ok := parsePositiveInt(match[2])
		if !ok {
			continue
		}
		results[buildGitLabIssueKey(match[1], iid)] = struct{}{}
	}

	for _, match := range gitLabIssueQualifiedRefPattern.FindAllStringSubmatch(text, -1) {
		if len(match) < 3 {
			continue
		}
		iid, ok := parsePositiveInt(match[2])
		if !ok {
			continue
		}
		results[buildGitLabIssueKey(match[1], iid)] = struct{}{}
	}

	defaultProjectPath = normalizeProjectPathWithNamespace(defaultProjectPath)
	if defaultProjectPath != "" {
		for _, match := range gitLabIssueRelativeURLRefPattern.FindAllStringSubmatch(text, -1) {
			if len(match) < 2 {
				continue
			}
			iid, ok := parsePositiveInt(match[1])
			if !ok {
				continue
			}
			results[buildGitLabIssueKey(defaultProjectPath, iid)] = struct{}{}
		}

		for _, match := range gitLabIssueSameProjectRefPattern.FindAllStringSubmatch(text, -1) {
			if len(match) < 2 {
				continue
			}
			iid, ok := parsePositiveInt(match[1])
			if !ok {
				continue
			}
			results[buildGitLabIssueKey(defaultProjectPath, iid)] = struct{}{}
		}
	}

	return results
}

func gitLabIssueKeyFromIssue(item *gitlab.Issue, defaultProjectPath string) (string, bool) {
	if item == nil || item.IID <= 0 {
		return "", false
	}

	if item.References != nil {
		if projectPath, iid, ok := parseGitLabQualifiedReference(item.References.Full); ok {
			return buildGitLabIssueKey(projectPath, iid), true
		}
	}

	defaultProjectPath = normalizeProjectPathWithNamespace(defaultProjectPath)
	if defaultProjectPath == "" {
		return "", false
	}
	return buildGitLabIssueKey(defaultProjectPath, int(item.IID)), true
}

func parseGitLabQualifiedReference(reference string) (string, int, bool) {
	for _, match := range gitLabIssueQualifiedRefPattern.FindAllStringSubmatch(reference, -1) {
		if len(match) < 3 {
			continue
		}
		iid, ok := parsePositiveInt(match[2])
		if !ok {
			continue
		}
		return normalizeProjectPathWithNamespace(match[1]), iid, true
	}
	return "", 0, false
}

func parsePositiveInt(raw string) (int, bool) {
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}

func parseGitLabMRProjectPath(key string) (string, bool) {
	idx := strings.LastIndex(key, "#!")
	if idx <= 0 {
		return "", false
	}
	return key[:idx], true
}

func parseGitLabIssueProjectPath(key string) (string, bool) {
	idx := strings.LastIndex(key, "##")
	if idx <= 0 {
		return "", false
	}
	return key[:idx], true
}

func isGitLabProjectAllowed(projectPath string) bool {
	if config.allowedRepos == nil || len(config.allowedRepos) == 0 {
		return true
	}

	normalized := normalizeProjectPathWithNamespace(projectPath)
	for repo := range config.allowedRepos {
		if strings.EqualFold(normalizeProjectPathWithNamespace(repo), normalized) {
			return true
		}
	}

	return false
}

func needsLowerPriorityPRChecks(currentLabel string) bool {
	return shouldUpdateLabel(currentLabel, "Commented", true) || shouldUpdateLabel(currentLabel, "Mentioned", true)
}

func mergeLabelWithPriority(currentLabel, candidateLabel string, isPR bool) string {
	if shouldUpdateLabel(currentLabel, candidateLabel, isPR) {
		return candidateLabel
	}
	return currentLabel
}

func listAllGitLabMergeRequestNotes(ctx context.Context, client *gitlab.Client, projectID int64, mrIID int64) ([]*gitlab.Note, error) {
	allNotes := make([]*gitlab.Note, 0)
	options := &gitlab.ListMergeRequestNotesOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
	}

	for {
		var (
			notes    []*gitlab.Note
			response *gitlab.Response
		)
		err := retryWithBackoff(func() error {
			var apiErr error
			notes, response, apiErr = client.Notes.ListMergeRequestNotes(projectID, mrIID, options, gitlab.WithContext(ctx))
			return apiErr
		}, fmt.Sprintf("GitLabListMergeRequestNotes %d!%d page %d", projectID, mrIID, options.Page))
		if err != nil {
			return nil, err
		}
		allNotes = append(allNotes, notes...)

		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}

	return allNotes, nil
}

func listAllGitLabIssueNotes(ctx context.Context, client *gitlab.Client, projectID int64, issueIID int64) ([]*gitlab.Note, error) {
	allNotes := make([]*gitlab.Note, 0)
	options := &gitlab.ListIssueNotesOptions{
		ListOptions: gitlab.ListOptions{PerPage: 100, Page: 1},
	}

	for {
		var (
			notes    []*gitlab.Note
			response *gitlab.Response
		)
		err := retryWithBackoff(func() error {
			var apiErr error
			notes, response, apiErr = client.Notes.ListIssueNotes(projectID, issueIID, options, gitlab.WithContext(ctx))
			return apiErr
		}, fmt.Sprintf("GitLabListIssueNotes %d#%d page %d", projectID, issueIID, options.Page))
		if err != nil {
			return nil, err
		}
		allNotes = append(allNotes, notes...)

		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}

	return allNotes, nil
}

func gitLabNotesInvolvement(notes []*gitlab.Note, description, currentUsername string, currentUserID int64) (bool, bool) {
	commented := false
	mentioned := containsGitLabUserMention(description, currentUsername)

	for _, note := range notes {
		if note == nil {
			continue
		}
		if matchesGitLabNoteAuthor(note.Author, currentUsername, currentUserID) {
			commented = true
		}
		if !mentioned && containsGitLabUserMention(note.Body, currentUsername) {
			mentioned = true
		}
		if commented && mentioned {
			break
		}
	}

	return commented, mentioned
}

func containsGitLabUserMention(text, username string) bool {
	if text == "" || username == "" {
		return false
	}
	needle := "@" + strings.ToLower(strings.TrimSpace(username))
	if needle == "@" {
		return false
	}
	return strings.Contains(strings.ToLower(text), needle)
}

func matchesGitLabNoteAuthor(author gitlab.NoteAuthor, username string, userID int64) bool {
	if userID > 0 && author.ID == userID {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(author.Username), strings.TrimSpace(username))
}

func matchesGitLabBasicUser(user *gitlab.BasicUser, username string, userID int64) bool {
	if user == nil {
		return false
	}
	if userID > 0 && user.ID == userID {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(user.Username), strings.TrimSpace(username))
}

func matchesGitLabIssueAuthor(author *gitlab.IssueAuthor, username string, userID int64) bool {
	if author == nil {
		return false
	}
	if userID > 0 && author.ID == userID {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(author.Username), strings.TrimSpace(username))
}

func matchesGitLabIssueAssignee(assignee *gitlab.IssueAssignee, username string, userID int64) bool {
	if assignee == nil {
		return false
	}
	if userID > 0 && assignee.ID == userID {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(assignee.Username), strings.TrimSpace(username))
}

func gitLabIssueAssigneeListContains(assignees []*gitlab.IssueAssignee, username string, userID int64) bool {
	for _, assignee := range assignees {
		if matchesGitLabIssueAssignee(assignee, username, userID) {
			return true
		}
	}
	return false
}

func gitLabBasicUserListContains(users []*gitlab.BasicUser, username string, userID int64) bool {
	for _, user := range users {
		if matchesGitLabBasicUser(user, username, userID) {
			return true
		}
	}
	return false
}

func gitLabApprovalStateReviewedByCurrentUser(state *gitlab.MergeRequestApprovalState, username string, userID int64) bool {
	if state == nil {
		return false
	}
	for _, rule := range state.Rules {
		if rule == nil {
			continue
		}
		if gitLabBasicUserListContains(rule.ApprovedBy, username, userID) {
			return true
		}
	}
	return false
}

func resolveAllowedGitLabProjects(ctx context.Context, client *gitlab.Client, allowedRepos map[string]bool) ([]gitLabProject, error) {
	if client == nil {
		return nil, fmt.Errorf("gitlab client is not configured")
	}

	if len(allowedRepos) == 0 {
		return []gitLabProject{}, nil
	}

	repoPaths := make([]string, 0, len(allowedRepos))
	for repo := range allowedRepos {
		normalized := normalizeProjectPathWithNamespace(repo)
		if normalized != "" {
			repoPaths = append(repoPaths, normalized)
		}
	}
	sort.Strings(repoPaths)

	projectIDCache := make(map[string]int64, len(repoPaths))
	projects := make([]gitLabProject, 0, len(repoPaths))
	for _, pathWithNamespace := range repoPaths {
		if id, ok := projectIDCache[pathWithNamespace]; ok {
			projects = append(projects, gitLabProject{PathWithNamespace: pathWithNamespace, ID: id})
			continue
		}

		var project *gitlab.Project
		err := retryWithBackoff(func() error {
			var apiErr error
			project, _, apiErr = client.Projects.GetProject(pathWithNamespace, nil, gitlab.WithContext(ctx))
			return apiErr
		}, fmt.Sprintf("GitLabGetProject %s", pathWithNamespace))
		if err != nil {
			return nil, fmt.Errorf("resolve project %s: %w", pathWithNamespace, err)
		}

		projectIDCache[pathWithNamespace] = project.ID
		projects = append(projects, gitLabProject{PathWithNamespace: pathWithNamespace, ID: project.ID})
	}

	return projects, nil
}

func listGitLabProjectMergeRequests(ctx context.Context, client *gitlab.Client, projectID int64, cutoff time.Time) ([]*gitlab.BasicMergeRequest, error) {
	allItems := make([]*gitlab.BasicMergeRequest, 0)
	options := &gitlab.ListProjectMergeRequestsOptions{
		ListOptions:  gitlab.ListOptions{PerPage: 100, Page: 1},
		State:        gitlab.Ptr("all"),
		UpdatedAfter: &cutoff,
	}

	for {
		var (
			items    []*gitlab.BasicMergeRequest
			response *gitlab.Response
		)
		err := retryWithBackoff(func() error {
			var apiErr error
			items, response, apiErr = client.MergeRequests.ListProjectMergeRequests(projectID, options, gitlab.WithContext(ctx))
			return apiErr
		}, fmt.Sprintf("GitLabListProjectMergeRequests %d page %d", projectID, options.Page))
		if err != nil {
			return nil, err
		}
		allItems = append(allItems, items...)

		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}

	return allItems, nil
}

func listGitLabProjectIssues(ctx context.Context, client *gitlab.Client, projectID int64, cutoff time.Time) ([]*gitlab.Issue, error) {
	allItems := make([]*gitlab.Issue, 0)
	options := &gitlab.ListProjectIssuesOptions{
		ListOptions:  gitlab.ListOptions{PerPage: 100, Page: 1},
		State:        gitlab.Ptr("all"),
		UpdatedAfter: &cutoff,
	}

	for {
		var (
			items    []*gitlab.Issue
			response *gitlab.Response
		)
		err := retryWithBackoff(func() error {
			var apiErr error
			items, response, apiErr = client.Issues.ListProjectIssues(projectID, options, gitlab.WithContext(ctx))
			return apiErr
		}, fmt.Sprintf("GitLabListProjectIssues %d page %d", projectID, options.Page))
		if err != nil {
			return nil, err
		}
		allItems = append(allItems, items...)

		if response == nil || response.NextPage == 0 {
			break
		}
		options.Page = response.NextPage
	}

	return allItems, nil
}

func normalizeProjectPathWithNamespace(repo string) string {
	trimmed := strings.TrimSpace(repo)
	return strings.Trim(trimmed, "/")
}

func splitGitLabPathWithNamespace(path string) (owner string, repo string, ok bool) {
	normalized := normalizeProjectPathWithNamespace(path)
	idx := strings.LastIndex(normalized, "/")
	if idx <= 0 || idx >= len(normalized)-1 {
		return "", "", false
	}
	return normalized[:idx], normalized[idx+1:], true
}

func gitLabProjectPath(owner, repo string) string {
	owner = normalizeProjectPathWithNamespace(owner)
	repo = strings.Trim(strings.TrimSpace(repo), "/")
	if repo != "" {
		if owner == "" {
			return repo
		}
		return owner + "/" + repo
	}
	return owner
}

func buildGitLabDedupKey(projectPath, itemType string, iid int64) string {
	return fmt.Sprintf("%s|%s|%d", strings.ToLower(projectPath), itemType, iid)
}

func toMergeRequestModelFromGitLab(item *gitlab.BasicMergeRequest) MergeRequestModel {
	if item == nil {
		return MergeRequestModel{}
	}

	state := strings.ToLower(item.State)
	merged := state == "merged" || item.MergedAt != nil
	normalizedState := "open"
	if merged || state == "closed" {
		normalizedState = "closed"
	}

	updatedAt := time.Time{}
	if item.UpdatedAt != nil {
		updatedAt = *item.UpdatedAt
	}

	userLogin := ""
	if item.Author != nil {
		userLogin = item.Author.Username
	}

	return MergeRequestModel{
		Number:    int(item.IID),
		Title:     item.Title,
		Body:      item.Description,
		State:     normalizedState,
		UpdatedAt: updatedAt,
		WebURL:    item.WebURL,
		UserLogin: userLogin,
		Merged:    merged,
	}
}

func toIssueModelFromGitLab(item *gitlab.Issue) IssueModel {
	if item == nil {
		return IssueModel{}
	}

	state := strings.ToLower(item.State)
	normalizedState := "open"
	if state == "closed" {
		normalizedState = "closed"
	}

	updatedAt := time.Time{}
	if item.UpdatedAt != nil {
		updatedAt = *item.UpdatedAt
	}

	userLogin := ""
	if item.Author != nil {
		userLogin = item.Author.Username
	}

	return IssueModel{
		Number:    int(item.IID),
		Title:     item.Title,
		Body:      item.Description,
		State:     normalizedState,
		UpdatedAt: updatedAt,
		WebURL:    item.WebURL,
		UserLogin: userLogin,
	}
}
