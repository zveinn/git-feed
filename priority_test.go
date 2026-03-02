package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gitlab "gitlab.com/gitlab-org/api/client-go"
	bolt "go.etcd.io/bbolt"
)

func TestPRLabelPriority(t *testing.T) {
	tests := []struct {
		label    string
		priority int
	}{
		{"Authored", 1},
		{"Assigned", 2},
		{"Reviewed", 3},
		{"Review Requested", 4},
		{"Commented", 5},
		{"Mentioned", 6},
		{"Unknown", 999},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := getPRLabelPriority(tt.label)
			if got != tt.priority {
				t.Errorf("getPRLabelPriority(%q) = %d, want %d", tt.label, got, tt.priority)
			}
		})
	}
}

func TestIssueLabelPriority(t *testing.T) {
	tests := []struct {
		label    string
		priority int
	}{
		{"Authored", 1},
		{"Assigned", 2},
		{"Commented", 3},
		{"Mentioned", 4},
		{"Unknown", 999},
	}

	for _, tt := range tests {
		t.Run(tt.label, func(t *testing.T) {
			got := getIssueLabelPriority(tt.label)
			if got != tt.priority {
				t.Errorf("getIssueLabelPriority(%q) = %d, want %d", tt.label, got, tt.priority)
			}
		})
	}
}

func TestShouldUpdateLabel_PR(t *testing.T) {
	tests := []struct {
		name         string
		currentLabel string
		newLabel     string
		want         bool
	}{
		{"empty current should update", "", "Mentioned", true},
		{"higher priority should update", "Mentioned", "Authored", true},
		{"same priority should not update", "Authored", "Authored", false},
		{"lower priority should not update", "Authored", "Mentioned", false},
		{"from Mentioned to Reviewed", "Mentioned", "Reviewed", true},
		{"from Authored to Reviewed", "Authored", "Reviewed", false},
		{"from Commented to Assigned", "Commented", "Assigned", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUpdateLabel(tt.currentLabel, tt.newLabel, true)
			if got != tt.want {
				t.Errorf("shouldUpdateLabel(%q, %q, true) = %v, want %v",
					tt.currentLabel, tt.newLabel, got, tt.want)
			}
		})
	}
}

func TestShouldUpdateLabel_Issue(t *testing.T) {
	tests := []struct {
		name         string
		currentLabel string
		newLabel     string
		want         bool
	}{
		{"empty current should update", "", "Mentioned", true},
		{"higher priority should update", "Mentioned", "Authored", true},
		{"same priority should not update", "Authored", "Authored", false},
		{"lower priority should not update", "Authored", "Mentioned", false},
		{"from Mentioned to Commented", "Mentioned", "Commented", true},
		{"from Authored to Commented", "Authored", "Commented", false},
		{"from Commented to Assigned", "Commented", "Assigned", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shouldUpdateLabel(tt.currentLabel, tt.newLabel, false)
			if got != tt.want {
				t.Errorf("shouldUpdateLabel(%q, %q, false) = %v, want %v",
					tt.currentLabel, tt.newLabel, got, tt.want)
			}
		})
	}
}

func TestNormalizeGitLabBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "defaults to gitlab.com when empty",
			raw:  "",
			want: "https://gitlab.com/api/v4",
		},
		{
			name: "normalizes host with trailing slash",
			raw:  "http://10.10.1.207/",
			want: "http://10.10.1.207/api/v4",
		},
		{
			name: "normalizes host without trailing slash",
			raw:  "http://10.10.1.207",
			want: "http://10.10.1.207/api/v4",
		},
		{
			name: "normalizes gitlab.com",
			raw:  "https://gitlab.com",
			want: "https://gitlab.com/api/v4",
		},
		{
			name: "normalizes subpath base",
			raw:  "https://gitlab.example.com/gitlab",
			want: "https://gitlab.example.com/gitlab/api/v4",
		},
		{
			name: "does not double append api v4",
			raw:  "https://host/api/v4",
			want: "https://host/api/v4",
		},
		{
			name: "trims trailing slash on existing api v4 path",
			raw:  "https://host/api/v4/",
			want: "https://host/api/v4",
		},
		{
			name:    "rejects missing scheme",
			raw:     "gitlab.example.com",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizeGitLabBaseURL(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("normalizeGitLabBaseURL(%q) expected error, got nil", tt.raw)
				}
				return
			}

			if err != nil {
				t.Fatalf("normalizeGitLabBaseURL(%q) unexpected error: %v", tt.raw, err)
			}

			if got != tt.want {
				t.Errorf("normalizeGitLabBaseURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

func TestRetryWithBackoff_GitLab429UsesRetryAfterHeader(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retry" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"rate limited"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":42,"username":"tester"}`)
	}))
	defer server.Close()

	oldDebugMode := config.debugMode
	oldCtx := config.ctx
	oldProgress := config.progress
	oldRetryAfter := retryAfter
	t.Cleanup(func() {
		config.debugMode = oldDebugMode
		config.ctx = oldCtx
		config.progress = oldProgress
		retryAfter = oldRetryAfter
	})

	config.debugMode = true
	config.ctx = context.Background()
	config.progress = nil

	waits := make([]time.Duration, 0, 2)
	retryAfter = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	err := retryWithBackoff(func() error {
		request, reqErr := http.NewRequestWithContext(config.ctx, http.MethodGet, server.URL+"/retry", nil)
		if reqErr != nil {
			return reqErr
		}

		response, reqErr := http.DefaultClient.Do(request)
		if reqErr != nil {
			return reqErr
		}
		defer response.Body.Close()

		if response.StatusCode >= http.StatusBadRequest {
			return gitlab.CheckResponse(response)
		}

		return nil
	}, "GitLabCurrentUser")
	if err != nil {
		t.Fatalf("retryWithBackoff failed: %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("expected 2 API calls, got %d", calls.Load())
	}
	if len(waits) != 1 {
		t.Fatalf("expected one retry wait, got %d", len(waits))
	}
	if waits[0] != 7*time.Second {
		t.Fatalf("expected Retry-After wait 7s, got %v", waits[0])
	}
}

func TestRetryWithBackoff_GitLab429FallsBackWhenRetryAfterMissing(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retry" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"rate limited"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":42,"username":"tester"}`)
	}))
	defer server.Close()

	oldDebugMode := config.debugMode
	oldCtx := config.ctx
	oldProgress := config.progress
	oldRetryAfter := retryAfter
	t.Cleanup(func() {
		config.debugMode = oldDebugMode
		config.ctx = oldCtx
		config.progress = oldProgress
		retryAfter = oldRetryAfter
	})

	config.debugMode = true
	config.ctx = context.Background()
	config.progress = nil

	waits := make([]time.Duration, 0, 2)
	retryAfter = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	err := retryWithBackoff(func() error {
		request, reqErr := http.NewRequestWithContext(config.ctx, http.MethodGet, server.URL+"/retry", nil)
		if reqErr != nil {
			return reqErr
		}

		response, reqErr := http.DefaultClient.Do(request)
		if reqErr != nil {
			return reqErr
		}
		defer response.Body.Close()

		if response.StatusCode >= http.StatusBadRequest {
			return gitlab.CheckResponse(response)
		}

		return nil
	}, "GitLabCurrentUser")
	if err != nil {
		t.Fatalf("retryWithBackoff failed: %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("expected 2 API calls, got %d", calls.Load())
	}
	if len(waits) != 1 {
		t.Fatalf("expected one retry wait, got %d", len(waits))
	}
	if waits[0] != 1*time.Second {
		t.Fatalf("expected fallback wait 1s, got %v", waits[0])
	}
}

func TestRetryWithBackoff_GitLab429UsesRateLimitResetWhenRetryAfterMissing(t *testing.T) {
	var calls atomic.Int32
	resetUnix := time.Now().Add(10 * time.Second).Unix()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retry" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		if calls.Add(1) == 1 {
			w.Header().Set("Ratelimit-Reset", strconv.FormatInt(resetUnix, 10))
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"message":"rate limited"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":42,"username":"tester"}`)
	}))
	defer server.Close()

	oldDebugMode := config.debugMode
	oldCtx := config.ctx
	oldProgress := config.progress
	oldRetryAfter := retryAfter
	t.Cleanup(func() {
		config.debugMode = oldDebugMode
		config.ctx = oldCtx
		config.progress = oldProgress
		retryAfter = oldRetryAfter
	})

	config.debugMode = true
	config.ctx = context.Background()
	config.progress = nil

	waits := make([]time.Duration, 0, 2)
	retryAfter = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	err := retryWithBackoff(func() error {
		request, reqErr := http.NewRequestWithContext(config.ctx, http.MethodGet, server.URL+"/retry", nil)
		if reqErr != nil {
			return reqErr
		}

		response, reqErr := http.DefaultClient.Do(request)
		if reqErr != nil {
			return reqErr
		}
		defer response.Body.Close()

		if response.StatusCode >= http.StatusBadRequest {
			return gitlab.CheckResponse(response)
		}

		return nil
	}, "GitLabCurrentUser")
	if err != nil {
		t.Fatalf("retryWithBackoff failed: %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("expected 2 API calls, got %d", calls.Load())
	}
	if len(waits) != 1 {
		t.Fatalf("expected one retry wait, got %d", len(waits))
	}
	if waits[0] < 8*time.Second || waits[0] > 10*time.Second {
		t.Fatalf("expected Ratelimit-Reset wait between 8s and 10s, got %v", waits[0])
	}
}

func TestRetryWithBackoff_GitLab5xxRetriesWithExponentialBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/retry" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}

		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprint(w, `{"message":"temporary outage"}`)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":42,"username":"tester"}`)
	}))
	defer server.Close()

	oldDebugMode := config.debugMode
	oldCtx := config.ctx
	oldProgress := config.progress
	oldRetryAfter := retryAfter
	t.Cleanup(func() {
		config.debugMode = oldDebugMode
		config.ctx = oldCtx
		config.progress = oldProgress
		retryAfter = oldRetryAfter
	})

	config.debugMode = true
	config.ctx = context.Background()
	config.progress = nil

	waits := make([]time.Duration, 0, 2)
	retryAfter = func(d time.Duration) <-chan time.Time {
		waits = append(waits, d)
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}

	err := retryWithBackoff(func() error {
		request, reqErr := http.NewRequestWithContext(config.ctx, http.MethodGet, server.URL+"/retry", nil)
		if reqErr != nil {
			return reqErr
		}

		response, reqErr := http.DefaultClient.Do(request)
		if reqErr != nil {
			return reqErr
		}
		defer response.Body.Close()

		if response.StatusCode >= http.StatusBadRequest {
			return gitlab.CheckResponse(response)
		}

		return nil
	}, "GitLabCurrentUser")
	if err != nil {
		t.Fatalf("retryWithBackoff failed: %v", err)
	}

	if calls.Load() != 2 {
		t.Fatalf("expected 2 API calls, got %d", calls.Load())
	}
	if len(waits) != 1 {
		t.Fatalf("expected one retry wait, got %d", len(waits))
	}
	if waits[0] != 1*time.Second {
		t.Fatalf("expected 5xx wait 1s, got %v", waits[0])
	}
}

func TestDatabaseGitLabRoundTripWithLabels(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "gitlab.db")
	db, err := OpenDatabase(dbPath)
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close()

	updated := time.Date(2026, 1, 15, 12, 0, 0, 0, time.UTC)
	mr := MergeRequestModel{
		Number:    7,
		Title:     "cached mr",
		Body:      "mr body",
		State:     "open",
		UpdatedAt: updated,
		WebURL:    "https://gitlab.example/group/repo/-/merge_requests/7",
		UserLogin: "alice",
	}
	issue := IssueModel{
		Number:    11,
		Title:     "cached issue",
		Body:      "issue body",
		State:     "closed",
		UpdatedAt: updated.Add(-time.Hour),
		WebURL:    "https://gitlab.example/group/repo/-/issues/11",
		UserLogin: "bob",
	}

	if err := db.SaveGitLabMergeRequestWithLabel("group/repo", mr, "Reviewed", false); err != nil {
		t.Fatalf("SaveGitLabMergeRequestWithLabel failed: %v", err)
	}
	if err := db.SaveGitLabIssueWithLabel("group/repo", issue, "Commented", false); err != nil {
		t.Fatalf("SaveGitLabIssueWithLabel failed: %v", err)
	}
	if err := db.SaveGitLabNote(GitLabNoteRecord{
		ProjectPath:    "group/repo",
		ItemType:       "mr",
		ItemIID:        7,
		NoteID:         301,
		Body:           "note body",
		AuthorUsername: "alice",
		AuthorID:       42,
	}, false); err != nil {
		t.Fatalf("SaveGitLabNote failed: %v", err)
	}

	allMRs, mrLabels, err := db.GetAllGitLabMergeRequestsWithLabels(false)
	if err != nil {
		t.Fatalf("GetAllGitLabMergeRequestsWithLabels failed: %v", err)
	}
	mrKey := "group/repo#!7"
	if len(allMRs) != 1 {
		t.Fatalf("MR count = %d, want 1", len(allMRs))
	}
	if allMRs[mrKey].Title != "cached mr" {
		t.Fatalf("MR title = %q, want cached mr", allMRs[mrKey].Title)
	}
	if mrLabels[mrKey] != "Reviewed" {
		t.Fatalf("MR label = %q, want Reviewed", mrLabels[mrKey])
	}

	allIssues, issueLabels, err := db.GetAllGitLabIssuesWithLabels(false)
	if err != nil {
		t.Fatalf("GetAllGitLabIssuesWithLabels failed: %v", err)
	}
	issueKey := "group/repo##11"
	if len(allIssues) != 1 {
		t.Fatalf("Issue count = %d, want 1", len(allIssues))
	}
	if allIssues[issueKey].Title != "cached issue" {
		t.Fatalf("Issue title = %q, want cached issue", allIssues[issueKey].Title)
	}
	if issueLabels[issueKey] != "Commented" {
		t.Fatalf("Issue label = %q, want Commented", issueLabels[issueKey])
	}

	noteCount := 0
	err = db.db.View(func(tx *bolt.Tx) error {
		return tx.Bucket(gitlabNotesBkt).ForEach(func(_, _ []byte) error {
			noteCount++
			return nil
		})
	})
	if err != nil {
		t.Fatalf("reading gitlab notes bucket failed: %v", err)
	}
	if noteCount != 1 {
		t.Fatalf("GitLab note count = %d, want 1", noteCount)
	}

	hasData, err := db.HasGitLabData()
	if err != nil {
		t.Fatalf("HasGitLabData failed: %v", err)
	}
	if !hasData {
		t.Fatalf("HasGitLabData = false, want true")
	}
}

func TestLoadGitLabCachedActivities_OfflineParityFiltersAndOrder(t *testing.T) {
	originalConfig := config
	defer func() { config = originalConfig }()

	dbPath := filepath.Join(t.TempDir(), "gitlab.db")
	db, err := OpenDatabase(dbPath)
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	newMR := MergeRequestModel{Number: 2, Title: "new mr", State: "open", UpdatedAt: now.Add(-2 * time.Hour), UserLogin: "alice"}
	oldMR := MergeRequestModel{Number: 1, Title: "old mr", State: "closed", UpdatedAt: now.Add(-48 * time.Hour), UserLogin: "alice"}
	newIssue := IssueModel{Number: 4, Title: "new issue", State: "open", UpdatedAt: now.Add(-90 * time.Minute), UserLogin: "bob"}

	if err := db.SaveGitLabMergeRequestWithLabel("group/repo", newMR, "Authored", false); err != nil {
		t.Fatalf("save new MR failed: %v", err)
	}
	if err := db.SaveGitLabMergeRequestWithLabel("group/repo", oldMR, "Reviewed", false); err != nil {
		t.Fatalf("save old MR failed: %v", err)
	}
	if err := db.SaveGitLabIssueWithLabel("group/repo", newIssue, "Commented", false); err != nil {
		t.Fatalf("save issue failed: %v", err)
	}
	if err := db.SaveGitLabMergeRequestWithLabel("other/repo", MergeRequestModel{Number: 8, Title: "other", UpdatedAt: now.Add(-time.Hour)}, "Authored", false); err != nil {
		t.Fatalf("save other repo MR failed: %v", err)
	}

	config = Config{
		db:           db,
		allowedRepos: map[string]bool{"group/repo": true},
		debugMode:    false,
	}

	activities, issueActivities, err := loadGitLabCachedActivities(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("loadGitLabCachedActivities failed: %v", err)
	}

	if len(activities) != 1 {
		t.Fatalf("MR activities count = %d, want 1", len(activities))
	}
	if activities[0].MR.Title != "new mr" || activities[0].Label != "Authored" || activities[0].Owner != "group" || activities[0].Repo != "repo" {
		t.Fatalf("unexpected MR activity %+v", activities[0])
	}

	if len(issueActivities) != 1 {
		t.Fatalf("Issue activities count = %d, want 1", len(issueActivities))
	}
	if issueActivities[0].Issue.Title != "new issue" || issueActivities[0].Label != "Commented" || issueActivities[0].Owner != "group" || issueActivities[0].Repo != "repo" {
		t.Fatalf("unexpected issue activity %+v", issueActivities[0])
	}

}

func TestLoadGitLabCachedActivities_NestsLinkedIssuesAndExcludesStandalone(t *testing.T) {
	originalConfig := config
	defer func() { config = originalConfig }()

	dbPath := filepath.Join(t.TempDir(), "gitlab.db")
	db, err := OpenDatabase(dbPath)
	if err != nil {
		t.Fatalf("OpenDatabase failed: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	mr := MergeRequestModel{Number: 9, Title: "mr", Body: "no direct refs", State: "open", UpdatedAt: now.Add(-2 * time.Hour), UserLogin: "alice"}
	linkedIssue := IssueModel{Number: 41, Title: "linked", State: "open", UpdatedAt: now.Add(-time.Hour), UserLogin: "bob"}
	standaloneIssue := IssueModel{Number: 42, Title: "standalone", State: "open", UpdatedAt: now.Add(-30 * time.Minute), UserLogin: "carol"}

	if err := db.SaveGitLabMergeRequestWithLabel("group/repo", mr, "Authored", false); err != nil {
		t.Fatalf("save MR failed: %v", err)
	}
	if err := db.SaveGitLabIssueWithLabel("group/repo", linkedIssue, "Commented", false); err != nil {
		t.Fatalf("save linked issue failed: %v", err)
	}
	if err := db.SaveGitLabIssueWithLabel("group/repo", standaloneIssue, "Mentioned", false); err != nil {
		t.Fatalf("save standalone issue failed: %v", err)
	}
	if err := db.SaveGitLabNote(GitLabNoteRecord{
		ProjectPath:    "group/repo",
		ItemType:       "mr",
		ItemIID:        9,
		NoteID:         9001,
		Body:           "Tracking issue group/repo#41",
		AuthorUsername: "alice",
		AuthorID:       1,
	}, false); err != nil {
		t.Fatalf("save MR note failed: %v", err)
	}

	config = Config{
		db:           db,
		allowedRepos: map[string]bool{"group/repo": true},
		debugMode:    false,
	}

	activities, issueActivities, err := loadGitLabCachedActivities(now.Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("loadGitLabCachedActivities failed: %v", err)
	}

	if len(activities) != 1 {
		t.Fatalf("MR activities count = %d, want 1", len(activities))
	}
	if len(activities[0].Issues) != 1 || activities[0].Issues[0].Issue.Number != 41 {
		t.Fatalf("nested issues = %+v, want only issue 41", activities[0].Issues)
	}

	if len(issueActivities) != 1 || issueActivities[0].Issue.Number != 42 {
		t.Fatalf("standalone issues = %+v, want only issue 42", issueActivities)
	}
}

func TestFetchGitLabProjectActivities_PaginatesAndFiltersByCutoff(t *testing.T) {
	cutoff := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

	var projectRequestPath string
	mrPageCalls := map[int]int{}
	issuePageCalls := map[int]int{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/closes_issues"):
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/approval_state"):
			_, _ = w.Write([]byte(`{"approval_rules_overwritten": false, "rules": []}`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/notes"):
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests"):
			if r.URL.Query().Get("state") != "all" {
				t.Fatalf("merge request state query = %q, want all", r.URL.Query().Get("state"))
			}
			if r.URL.Query().Get("updated_after") == "" {
				t.Fatalf("merge request updated_after query must be set")
			}

			page := parsePageQuery(r)
			mrPageCalls[page]++
			writePageHeaders(w, page)

			if page == 1 {
				_, _ = w.Write([]byte(`[
					{"iid": 7, "title": "MR page 1", "description": "desc", "state": "opened", "updated_at": "2026-01-11T12:00:00Z", "web_url": "https://gitlab.example/mr/7", "author": {"username": "alice"}}
				]`))
				return
			}

			_, _ = w.Write([]byte(`[
				{"iid": 8, "title": "MR page 2", "description": "desc", "state": "merged", "updated_at": "2026-01-12T12:00:00Z", "web_url": "https://gitlab.example/mr/8", "author": {"username": "bob"}}
			]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/issues"):
			if r.URL.Query().Get("state") != "all" {
				t.Fatalf("issue state query = %q, want all", r.URL.Query().Get("state"))
			}
			if r.URL.Query().Get("updated_after") == "" {
				t.Fatalf("issue updated_after query must be set")
			}

			page := parsePageQuery(r)
			issuePageCalls[page]++
			writePageHeaders(w, page)

			if page == 1 {
				_, _ = w.Write([]byte(`[
					{"id": 201, "iid": 11, "title": "Issue page 1", "description": "desc", "state": "opened", "updated_at": "2026-01-11T08:00:00Z", "web_url": "https://gitlab.example/issues/11", "author": {"username": "carol"}}
				]`))
				return
			}

			_, _ = w.Write([]byte(`[
				{"id": 202, "iid": 12, "title": "Issue old", "description": "desc", "state": "closed", "updated_at": "2026-01-09T08:00:00Z", "web_url": "https://gitlab.example/issues/12", "author": {"username": "dave"}}
			]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
			projectRequestPath = r.URL.Path
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                  101,
				"path_with_namespace": "group/subgroup/repo",
			})

		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, _, err := newGitLabClient("token", server.URL)
	if err != nil {
		t.Fatalf("newGitLabClient failed: %v", err)
	}

	activities, issues, err := fetchGitLabProjectActivities(
		context.Background(),
		client,
		map[string]bool{"group/subgroup/repo": true},
		cutoff,
		"alice",
		0,
		nil,
	)
	if err != nil {
		t.Fatalf("fetchGitLabProjectActivities failed: %v", err)
	}

	if projectRequestPath != "/api/v4/projects/group%2Fsubgroup%2Frepo" && projectRequestPath != "/api/v4/projects/group/subgroup/repo" {
		t.Fatalf("project path = %q, want full path-with-namespace to be preserved", projectRequestPath)
	}

	if mrPageCalls[1] != 1 || mrPageCalls[2] != 1 {
		t.Fatalf("merge request pagination calls = %+v, want page 1 and 2 exactly once", mrPageCalls)
	}
	if issuePageCalls[1] != 1 || issuePageCalls[2] != 1 {
		t.Fatalf("issue pagination calls = %+v, want page 1 and 2 exactly once", issuePageCalls)
	}

	if len(activities) != 2 {
		t.Fatalf("got %d merge request activities, want 2", len(activities))
	}
	if activities[0].Owner != "group/subgroup" || activities[0].Repo != "repo" {
		t.Fatalf("unexpected project mapping for merge request activity: owner=%q repo=%q", activities[0].Owner, activities[0].Repo)
	}

	mergedFound := false
	for _, activity := range activities {
		if activity.MR.Number == 8 {
			if !activity.MR.Merged || activity.MR.State != "closed" {
				t.Fatalf("merged MR mapping invalid: merged=%v state=%q", activity.MR.Merged, activity.MR.State)
			}
			mergedFound = true
		}
	}
	if !mergedFound {
		t.Fatalf("expected merged MR iid 8 in results")
	}

	if len(issues) != 1 {
		t.Fatalf("got %d issue activities, want 1 after cutoff filtering", len(issues))
	}
	if issues[0].Issue.Number != 11 {
		t.Fatalf("issue number = %d, want 11", issues[0].Issue.Number)
	}
}

func parsePageQuery(r *http.Request) int {
	pageParam := r.URL.Query().Get("page")
	if pageParam == "" {
		return 1
	}
	page, err := strconv.Atoi(pageParam)
	if err != nil {
		return 1
	}
	return page
}

func writePageHeaders(w http.ResponseWriter, page int) {
	w.Header().Set("X-Page", strconv.Itoa(page))
	w.Header().Set("X-Per-Page", "100")
	w.Header().Set("X-Total", "2")
	w.Header().Set("X-Total-Pages", "2")
	if page == 1 {
		w.Header().Set("X-Next-Page", "2")
	} else {
		w.Header().Set("X-Next-Page", "")
	}
}

func TestFetchGitLabProjectActivities_DerivesLabelsFromSources(t *testing.T) {
	cutoff := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	approvalCalls := map[int64]int{}
	mrNoteCalls := map[int64]int{}
	issueNoteCalls := map[int64]int{}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/closes_issues"):
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests/") && strings.HasSuffix(r.URL.Path, "/approval_state"):
			iid := parseResourceIID(t, r.URL.Path, "merge_requests", "approval_state")
			approvalCalls[iid]++
			if iid == 2 {
				_, _ = w.Write([]byte(`{"approval_rules_overwritten": false, "rules": [{"id": 1, "approved_by": [{"id": 42, "username": "me"}]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"approval_rules_overwritten": false, "rules": []}`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests/") && strings.HasSuffix(r.URL.Path, "/notes"):
			iid := parseResourceIID(t, r.URL.Path, "merge_requests", "notes")
			mrNoteCalls[iid]++
			if iid == 3 {
				_, _ = w.Write([]byte(`[
					{"id": 301, "body": "left a review comment", "author": {"id": 42, "username": "me"}}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/issues/") && strings.HasSuffix(r.URL.Path, "/notes"):
			iid := parseResourceIID(t, r.URL.Path, "issues", "notes")
			issueNoteCalls[iid]++
			if iid == 21 {
				_, _ = w.Write([]byte(`[
					{"id": 401, "body": "I commented", "author": {"id": 42, "username": "me"}}
				]`))
				return
			}
			if iid == 22 {
				_, _ = w.Write([]byte(`[
					{"id": 402, "body": "pinging @Me for visibility", "author": {"id": 7, "username": "alice"}}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests"):
			_, _ = w.Write([]byte(`[
				{"iid": 1, "title": "Authored and assigned", "description": "desc", "state": "opened", "updated_at": "2026-01-11T12:00:00Z", "web_url": "https://gitlab.example/mr/1", "author": {"id": 42, "username": "me"}, "assignees": [{"id": 42, "username": "me"}]},
				{"iid": 2, "title": "Reviewed via approvals", "description": "desc", "state": "opened", "updated_at": "2026-01-11T13:00:00Z", "web_url": "https://gitlab.example/mr/2", "author": {"id": 7, "username": "alice"}},
				{"iid": 3, "title": "Commented via notes", "description": "desc", "state": "opened", "updated_at": "2026-01-11T14:00:00Z", "web_url": "https://gitlab.example/mr/3", "author": {"id": 8, "username": "bob"}}
			]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[
				{"id": 521, "iid": 21, "title": "Commented issue", "description": "desc", "state": "opened", "updated_at": "2026-01-11T08:00:00Z", "web_url": "https://gitlab.example/issues/21", "author": {"id": 7, "username": "alice"}},
				{"id": 522, "iid": 22, "title": "Mentioned issue", "description": "desc", "state": "opened", "updated_at": "2026-01-11T09:00:00Z", "web_url": "https://gitlab.example/issues/22", "author": {"id": 9, "username": "carol"}}
			]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                  101,
				"path_with_namespace": "group/subgroup/repo",
			})

		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, _, err := newGitLabClient("token", server.URL)
	if err != nil {
		t.Fatalf("newGitLabClient failed: %v", err)
	}

	activities, issues, err := fetchGitLabProjectActivities(
		context.Background(),
		client,
		map[string]bool{"group/subgroup/repo": true},
		cutoff,
		"me",
		42,
		nil,
	)
	if err != nil {
		t.Fatalf("fetchGitLabProjectActivities failed: %v", err)
	}

	mrLabels := map[int]string{}
	for _, activity := range activities {
		mrLabels[activity.MR.Number] = activity.Label
	}

	if mrLabels[1] != "Authored" {
		t.Fatalf("MR 1 label = %q, want Authored", mrLabels[1])
	}
	if mrLabels[2] != "Reviewed" {
		t.Fatalf("MR 2 label = %q, want Reviewed", mrLabels[2])
	}
	if mrLabels[3] != "Commented" {
		t.Fatalf("MR 3 label = %q, want Commented", mrLabels[3])
	}

	if approvalCalls[1] != 0 {
		t.Fatalf("MR 1 approval calls = %d, want 0 due to authored/assigned short-circuit", approvalCalls[1])
	}
	if approvalCalls[2] != 1 || approvalCalls[3] != 1 {
		t.Fatalf("approval calls = %+v, want MR 2 and 3 exactly once", approvalCalls)
	}
	if mrNoteCalls[2] != 0 {
		t.Fatalf("MR 2 notes calls = %d, want 0 because Reviewed outranks note-based labels", mrNoteCalls[2])
	}
	if mrNoteCalls[3] != 1 {
		t.Fatalf("MR 3 notes calls = %d, want 1", mrNoteCalls[3])
	}

	issueLabels := map[int]string{}
	for _, issue := range issues {
		issueLabels[issue.Issue.Number] = issue.Label
	}
	if issueLabels[21] != "Commented" {
		t.Fatalf("Issue 21 label = %q, want Commented", issueLabels[21])
	}
	if issueLabels[22] != "Mentioned" {
		t.Fatalf("Issue 22 label = %q, want Mentioned", issueLabels[22])
	}
	if issueNoteCalls[21] != 1 || issueNoteCalls[22] != 1 {
		t.Fatalf("issue note calls = %+v, want issue 21 and 22 exactly once", issueNoteCalls)
	}
}

func TestLoadEnvFile_DoesNotOverrideExistingEnv(t *testing.T) {
	envPath := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(envPath, []byte("FOO=fromfile\n"), 0o644); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	t.Setenv("FOO", "fromenv")

	if err := loadEnvFile(envPath); err != nil {
		t.Fatalf("loadEnvFile failed: %v", err)
	}

	if got := os.Getenv("FOO"); got != "fromenv" {
		t.Fatalf("FOO = %q, want fromenv", got)
	}
}

func TestResolveAllowedRepos_PerPlatformAndFallback(t *testing.T) {
	tests := []struct {
		name          string
		platform      string
		flagValue     string
		githubAllowed string
		gitlabAllowed string
		legacyAllowed string
		want          string
	}{
		{
			name:          "flag overrides all env vars",
			platform:      "gitlab",
			flagValue:     "flag/repo",
			githubAllowed: "gh/repo",
			gitlabAllowed: "gl/repo",
			legacyAllowed: "legacy/repo",
			want:          "flag/repo",
		},
		{
			name:          "github uses platform-specific var",
			platform:      "github",
			githubAllowed: "owner/repo1,owner/repo2",
			legacyAllowed: "legacy/repo",
			want:          "owner/repo1,owner/repo2",
		},
		{
			name:          "gitlab uses platform-specific var",
			platform:      "gitlab",
			gitlabAllowed: "group/repo,group/subgroup/repo",
			legacyAllowed: "legacy/repo",
			want:          "group/repo,group/subgroup/repo",
		},
		{
			name:          "fallback to legacy var when platform var missing",
			platform:      "gitlab",
			legacyAllowed: "legacy/team/repo",
			want:          "legacy/team/repo",
		},
		{
			name:     "empty when nothing provided",
			platform: "github",
			want:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GITHUB_ALLOWED_REPOS", tt.githubAllowed)
			t.Setenv("GITLAB_ALLOWED_REPOS", tt.gitlabAllowed)
			t.Setenv("ALLOWED_REPOS", tt.legacyAllowed)

			got := resolveAllowedRepos(tt.platform, tt.flagValue)
			if got != tt.want {
				t.Fatalf("resolveAllowedRepos(%q, %q) = %q, want %q", tt.platform, tt.flagValue, got, tt.want)
			}
		})
	}
}

func TestValidateConfig_PlatformBranching(t *testing.T) {
	if err := validateConfig("gitlab", "", "", false, "/tmp/.env", nil); err == nil {
		t.Fatalf("validateConfig(gitlab, empty token) error = nil, want non-nil")
	}
	if err := validateConfig("gitlab", "token", "", false, "/tmp/.env", map[string]bool{}); err == nil {
		t.Fatalf("validateConfig(gitlab, empty allowed repos) error = nil, want non-nil")
	}
	if err := validateConfig("gitlab", "token", "", false, "/tmp/.env", map[string]bool{"group/subgroup/repo": true}); err != nil {
		t.Fatalf("validateConfig(gitlab, valid inputs) error = %v, want nil", err)
	}

	if err := validateConfig("github", "", "user", false, "/tmp/.env", nil); err == nil {
		t.Fatalf("validateConfig(github, empty token) error = nil, want non-nil")
	}
	if err := validateConfig("github", "token", "", false, "/tmp/.env", nil); err == nil {
		t.Fatalf("validateConfig(github, empty username) error = nil, want non-nil")
	}
	if err := validateConfig("github", "token", "user", false, "/tmp/.env", nil); err != nil {
		t.Fatalf("validateConfig(github, valid inputs) error = %v, want nil", err)
	}

	if err := validateConfig("gitlab", "", "", true, "/tmp/.env", nil); err != nil {
		t.Fatalf("validateConfig(gitlab, local mode) error = %v, want nil", err)
	}
	if err := validateConfig("github", "", "", true, "/tmp/.env", nil); err != nil {
		t.Fatalf("validateConfig(github, local mode) error = %v, want nil", err)
	}
}

func TestMergeLabelWithPriority_TableDriven(t *testing.T) {
	tests := []struct {
		name     string
		labels   []string
		isPR     bool
		expected string
	}{
		{
			name:     "PR fold keeps highest-priority label despite later lower-priority candidates",
			labels:   []string{"Mentioned", "Authored", "Review Requested", "Commented", "Assigned"},
			isPR:     true,
			expected: "Authored",
		},
		{
			name:     "Issue fold ignores unknown labels and preserves best known label",
			labels:   []string{"Mentioned", "Commented", "Unknown", "Mentioned"},
			isPR:     false,
			expected: "Commented",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := ""
			for _, label := range tt.labels {
				current = mergeLabelWithPriority(current, label, tt.isPR)
			}
			if current != tt.expected {
				t.Fatalf("final label = %q, want %q", current, tt.expected)
			}
		})
	}
}

func TestFetchGitLabProjectActivities_LinksIssuesUsingEndpointAndFallback(t *testing.T) {
	cutoff := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests/") && strings.HasSuffix(r.URL.Path, "/closes_issues"):
			iid := parseResourceIID(t, r.URL.Path, "merge_requests", "closes_issues")
			if iid == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"message":"endpoint unavailable"}`))
				return
			}
			if iid == 2 {
				_, _ = w.Write([]byte(`[
					{"id": 602, "iid": 22, "title": "Issue via endpoint", "state": "opened", "updated_at": "2026-01-11T10:00:00Z", "references": {"full": "group/subgroup/repo#22"}}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests/") && strings.HasSuffix(r.URL.Path, "/notes"):
			iid := parseResourceIID(t, r.URL.Path, "merge_requests", "notes")
			if iid == 1 {
				_, _ = w.Write([]byte(`[
					{"id": 701, "body": "Follow-up in #21", "author": {"id": 7, "username": "alice"}}
				]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/merge_requests"):
			_, _ = w.Write([]byte(`[
				{"iid": 1, "title": "MR fallback", "description": "no issue refs", "state": "opened", "updated_at": "2026-01-11T12:00:00Z", "web_url": "https://gitlab.example/mr/1", "author": {"id": 42, "username": "me"}},
				{"iid": 2, "title": "MR endpoint", "description": "no refs", "state": "opened", "updated_at": "2026-01-11T13:00:00Z", "web_url": "https://gitlab.example/mr/2", "author": {"id": 42, "username": "me"}}
			]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && strings.Contains(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[
				{"id": 521, "iid": 21, "title": "Issue from fallback", "description": "desc", "state": "opened", "updated_at": "2026-01-11T08:00:00Z", "web_url": "https://gitlab.example/issues/21", "author": {"id": 7, "username": "alice"}},
				{"id": 522, "iid": 22, "title": "Issue from endpoint", "description": "desc", "state": "opened", "updated_at": "2026-01-11T09:00:00Z", "web_url": "https://gitlab.example/issues/22", "author": {"id": 8, "username": "bob"}},
				{"id": 523, "iid": 23, "title": "Standalone issue", "description": "desc", "state": "opened", "updated_at": "2026-01-11T07:00:00Z", "web_url": "https://gitlab.example/issues/23", "author": {"id": 9, "username": "carol"}}
			]`))

		case strings.HasPrefix(r.URL.Path, "/api/v4/projects/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":                  101,
				"path_with_namespace": "group/subgroup/repo",
			})

		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client, _, err := newGitLabClient("token", server.URL)
	if err != nil {
		t.Fatalf("newGitLabClient failed: %v", err)
	}

	activities, issues, err := fetchGitLabProjectActivities(
		context.Background(),
		client,
		map[string]bool{"group/subgroup/repo": true},
		cutoff,
		"me",
		42,
		nil,
	)
	if err != nil {
		t.Fatalf("fetchGitLabProjectActivities failed: %v", err)
	}

	if len(activities) != 2 {
		t.Fatalf("got %d merge request activities, want 2", len(activities))
	}

	mrIssues := map[int]map[int]bool{}
	for _, activity := range activities {
		linked := map[int]bool{}
		for _, issue := range activity.Issues {
			linked[issue.Issue.Number] = true
		}
		mrIssues[activity.MR.Number] = linked
	}

	if !mrIssues[1][21] {
		t.Fatalf("MR 1 should link fallback issue 21")
	}
	if !mrIssues[2][22] {
		t.Fatalf("MR 2 should link endpoint issue 22")
	}

	if len(issues) != 1 || issues[0].Issue.Number != 23 {
		t.Fatalf("standalone issues = %+v, want only issue 23", issues)
	}
}

func TestGitLabIssueReferenceKeysFromText_ParsesLocalQualifiedAndURLRefs(t *testing.T) {
	refs := gitLabIssueReferenceKeysFromText(
		"Fixes #12 and group/subgroup/repo#34 and https://gitlab.example/group/other/-/issues/56 and /-/issues/78",
		"group/subgroup/repo",
	)

	expected := []string{
		buildGitLabIssueKey("group/subgroup/repo", 12),
		buildGitLabIssueKey("group/subgroup/repo", 34),
		buildGitLabIssueKey("group/other", 56),
		buildGitLabIssueKey("group/subgroup/repo", 78),
	}

	for _, key := range expected {
		if _, ok := refs[key]; !ok {
			t.Fatalf("missing parsed reference key %q in %+v", key, refs)
		}
	}

	noiseRefs := gitLabIssueReferenceKeysFromText(
		"ignore #0 #x project/repo#-5 /-/issues/0 https://gitlab.example/group/repo/-/issues/not-a-number and text#42",
		"group/subgroup/repo",
	)
	if len(noiseRefs) != 0 {
		t.Fatalf("unexpected refs parsed from noise: %+v", noiseRefs)
	}
}

func TestGitLabCLIWithMockServer_ShowsMergeRequestsAndIssues(t *testing.T) {
	const (
		mrTitle    = "MR E2E Unique Title"
		issueTitle = "Issue E2E Unique Title"
	)
	updatedAt := time.Now().UTC().Format(time.RFC3339)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/user":
			_, _ = w.Write([]byte(`{"id":42,"username":"me"}`))

		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/101/merge_requests/1/closes_issues":
			_, _ = w.Write([]byte(`[]`))

		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/101/merge_requests":
			_, _ = w.Write([]byte(`[
				{"iid":1,"title":"` + mrTitle + `","description":"desc","state":"opened","updated_at":"` + updatedAt + `","web_url":"https://gitlab.example/mr/1","author":{"id":42,"username":"me"}}
			]`))

		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/101/issues":
			_, _ = w.Write([]byte(`[
				{"id":301,"iid":2,"title":"` + issueTitle + `","description":"desc","state":"opened","updated_at":"` + updatedAt + `","web_url":"https://gitlab.example/issues/2","author":{"id":42,"username":"me"}}
			]`))

		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v4/projects/") && !strings.Contains(r.URL.Path, "/merge_requests") && !strings.Contains(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`{"id":101,"path_with_namespace":"group/subgroup/repo"}`))

		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	homeDir := t.TempDir()
	configDir := filepath.Join(homeDir, ".git-feed")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("failed to create config directory: %v", err)
	}
	envFile := filepath.Join(configDir, ".env")
	envContent := strings.Join([]string{
		"GITLAB_BASE_URL=" + server.URL,
		"GITLAB_TOKEN=token",
		"ALLOWED_REPOS=group/subgroup/repo",
		"",
	}, "\n")
	if err := os.WriteFile(envFile, []byte(envContent), 0o600); err != nil {
		t.Fatalf("failed to write test env file: %v", err)
	}

	modCache := filepath.Join(homeDir, "gomodcache")
	goCache := filepath.Join(homeDir, "gocache")
	if err := os.MkdirAll(modCache, 0o755); err != nil {
		t.Fatalf("failed to create GOMODCACHE: %v", err)
	}
	if err := os.MkdirAll(goCache, 0o755); err != nil {
		t.Fatalf("failed to create GOCACHE: %v", err)
	}

	cmd := exec.CommandContext(ctx, "go", "run", ".", "--platform", "gitlab", "--debug", "--time", "1d")
	var stdoutBuf bytes.Buffer
	var stderrBuf bytes.Buffer
	cmd.Stdout = &stdoutBuf
	cmd.Stderr = &stderrBuf
	cmd.Env = append(os.Environ(),
		"HOME="+homeDir,
		"GITLAB_BASE_URL="+server.URL,
		"GITLAB_TOKEN=token",
		"ALLOWED_REPOS=group/subgroup/repo",
		"GOMODCACHE="+modCache,
		"GOCACHE="+goCache,
		"GOFLAGS=-modcacherw",
	)

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("go run timed out")
	}
	if err != nil {
		t.Fatalf("go run failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdoutBuf.String(), stderrBuf.String())
	}

	output := stdoutBuf.String()
	if !strings.Contains(output, mrTitle) {
		t.Fatalf("stdout missing MR title %q\nstdout:\n%s", mrTitle, output)
	}
	if !strings.Contains(output, issueTitle) {
		t.Fatalf("stdout missing issue title %q\nstdout:\n%s", issueTitle, output)
	}
	if !strings.Contains(output, "OPEN PULL REQUESTS:") {
		t.Fatalf("stdout missing section header OPEN PULL REQUESTS:\nstdout:\n%s", output)
	}
}

func parseResourceIID(t *testing.T, path string, resource string, suffix string) int64 {
	t.Helper()
	parts := strings.Split(path, "/")
	resourceIndex := -1
	for i := range parts {
		if parts[i] == resource {
			resourceIndex = i
			break
		}
	}
	if resourceIndex == -1 || resourceIndex+1 >= len(parts) {
		t.Fatalf("could not parse resource iid from path %q", path)
	}
	if !strings.HasSuffix(path, "/"+suffix) {
		t.Fatalf("path %q missing expected suffix %q", path, suffix)
	}
	iid, err := strconv.ParseInt(parts[resourceIndex+1], 10, 64)
	if err != nil {
		t.Fatalf("could not parse iid from path %q: %v", path, err)
	}
	return iid
}
