package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	gitlabMergeRequestsBkt = []byte("gitlab_merge_requests")
	gitlabIssuesBkt        = []byte("gitlab_issues")
	gitlabNotesBkt         = []byte("gitlab_notes")
	githubPullRequestsBkt  = []byte("pull_requests")
	githubIssuesBkt        = []byte("issues")
	githubCommentsBkt      = []byte("comments")
)

type Database struct {
	db *bolt.DB
}

func buildGitLabMergeRequestKey(pathWithNamespace string, iid int) string {
	return fmt.Sprintf("%s#!%d", normalizeProjectPathWithNamespace(pathWithNamespace), iid)
}

func buildGitLabIssueKey(pathWithNamespace string, iid int) string {
	return fmt.Sprintf("%s##%d", normalizeProjectPathWithNamespace(pathWithNamespace), iid)
}

func buildGitLabNoteKey(pathWithNamespace, itemType string, iid int, noteID int64) string {
	return fmt.Sprintf(
		"%s|%s|%d|%d",
		normalizeProjectPathWithNamespace(pathWithNamespace),
		strings.ToLower(strings.TrimSpace(itemType)),
		iid,
		noteID,
	)
}

func buildGitHubItemKey(owner, repo string, number int) string {
	return fmt.Sprintf("%s/%s#%d", strings.TrimSpace(owner), strings.TrimSpace(repo), number)
}

func buildGitHubPRReviewCommentKey(owner, repo string, prNumber int, commentID int64) string {
	return fmt.Sprintf("%s/%s#%d/pr_review_comment/%d", strings.TrimSpace(owner), strings.TrimSpace(repo), prNumber, commentID)
}

func (d *Database) save(bucket []byte, key string, data interface{}, debugMode bool, itemType string) error {
	jsonData, err := json.Marshal(data)
	if err != nil {
		if debugMode {
			fmt.Printf("  [DB] Error marshaling %s %s: %v\n", itemType, key, err)
		}
		return fmt.Errorf("failed to marshal %s: %w", itemType, err)
	}

	err = d.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucket)
		return b.Put([]byte(key), jsonData)
	})
	if err != nil {
		if debugMode {
			fmt.Printf("  [DB] Error saving %s %s: %v\n", itemType, key, err)
		}
		return err
	}

	if debugMode {
		fmt.Printf("  [DB] Saved %s %s\n", itemType, key)
	}
	return nil
}

func OpenDatabase(path string) (*Database, error) {
	db, err := bolt.Open(path, 0666, &bolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	if err := os.Chmod(path, 0666); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("failed to set database permissions: %w", err)
	}

	err = db.Update(func(tx *bolt.Tx) error {
		buckets := [][]byte{
			gitlabMergeRequestsBkt,
			gitlabIssuesBkt,
			gitlabNotesBkt,
			githubPullRequestsBkt,
			githubIssuesBkt,
			githubCommentsBkt,
		}
		for _, bucket := range buckets {
			_, err := tx.CreateBucketIfNotExists(bucket)
			if err != nil {
				return fmt.Errorf("failed to create bucket %s: %w", string(bucket), err)
			}
		}
		return nil
	})
	if err != nil {
		_ = db.Close()
		return nil, err
	}

	return &Database{db: db}, nil
}

func (d *Database) Close() error {
	return d.db.Close()
}

type GitLabMRWithLabel struct {
	MR    MergeRequestModel
	Label string
}

type GitLabIssueWithLabel struct {
	Issue IssueModel
	Label string
}

type GitLabNoteRecord struct {
	ProjectPath    string
	ItemType       string
	ItemIID        int
	NoteID         int64
	Body           string
	AuthorUsername string
	AuthorID       int64
}

type GitHubPRWithLabel struct {
	PR    MergeRequestModel
	Label string
}

type GitHubIssueWithLabel struct {
	Issue IssueModel
	Label string
}

type GitHubPRReviewCommentRecord struct {
	Owner          string
	Repo           string
	PRNumber       int
	CommentID      int64
	Body           string
	AuthorUsername string
	AuthorID       int64
}

func (d *Database) SaveGitLabMergeRequestWithLabel(pathWithNamespace string, mr MergeRequestModel, label string, debugMode bool) error {
	key := buildGitLabMergeRequestKey(pathWithNamespace, mr.Number)
	item := GitLabMRWithLabel{MR: mr, Label: label}
	return d.save(gitlabMergeRequestsBkt, key, item, debugMode, fmt.Sprintf("gitlab merge request with label %s", label))
}

func (d *Database) SaveGitLabIssueWithLabel(pathWithNamespace string, issue IssueModel, label string, debugMode bool) error {
	key := buildGitLabIssueKey(pathWithNamespace, issue.Number)
	item := GitLabIssueWithLabel{Issue: issue, Label: label}
	return d.save(gitlabIssuesBkt, key, item, debugMode, fmt.Sprintf("gitlab issue with label %s", label))
}

func (d *Database) SaveGitLabNote(note GitLabNoteRecord, debugMode bool) error {
	key := buildGitLabNoteKey(note.ProjectPath, note.ItemType, note.ItemIID, note.NoteID)
	return d.save(gitlabNotesBkt, key, note, debugMode, "gitlab note")
}

func (d *Database) SaveGitHubPullRequestWithLabel(owner, repo string, pr MergeRequestModel, label string, debugMode bool) error {
	key := buildGitHubItemKey(owner, repo, pr.Number)
	item := GitHubPRWithLabel{PR: pr, Label: label}
	return d.save(githubPullRequestsBkt, key, item, debugMode, fmt.Sprintf("github pull request with label %s", label))
}

func (d *Database) SaveGitHubIssueWithLabel(owner, repo string, issue IssueModel, label string, debugMode bool) error {
	key := buildGitHubItemKey(owner, repo, issue.Number)
	item := GitHubIssueWithLabel{Issue: issue, Label: label}
	return d.save(githubIssuesBkt, key, item, debugMode, fmt.Sprintf("github issue with label %s", label))
}

func (d *Database) SaveGitHubPRReviewComment(comment GitHubPRReviewCommentRecord, debugMode bool) error {
	key := buildGitHubPRReviewCommentKey(comment.Owner, comment.Repo, comment.PRNumber, comment.CommentID)
	return d.save(githubCommentsBkt, key, comment, debugMode, "github pr review comment")
}

func (d *Database) GetAllGitLabMergeRequestsWithLabels(debugMode bool) (map[string]MergeRequestModel, map[string]string, error) {
	items := make(map[string]MergeRequestModel)
	labels := make(map[string]string)

	if debugMode {
		fmt.Printf("  [DB] Reading all GitLab merge requests with labels from database...\n")
	}

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(gitlabMergeRequestsBkt)
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			var item GitLabMRWithLabel
			if err := json.Unmarshal(v, &item); err != nil {
				if debugMode {
					fmt.Printf("  [DB] Error unmarshaling gitlab merge request %s: %v\n", key, err)
				}
				return err
			}
			items[key] = item.MR
			labels[key] = item.Label
			return nil
		})
	})
	if err != nil {
		if debugMode {
			fmt.Printf("  [DB] Error reading GitLab merge requests: %v\n", err)
		}
		return nil, nil, err
	}

	if debugMode {
		fmt.Printf("  [DB] Loaded %d GitLab merge requests from database\n", len(items))
	}

	return items, labels, nil
}

func (d *Database) GetAllGitLabIssuesWithLabels(debugMode bool) (map[string]IssueModel, map[string]string, error) {
	items := make(map[string]IssueModel)
	labels := make(map[string]string)

	if debugMode {
		fmt.Printf("  [DB] Reading all GitLab issues with labels from database...\n")
	}

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(gitlabIssuesBkt)
		return b.ForEach(func(k, v []byte) error {
			key := string(k)
			var item GitLabIssueWithLabel
			if err := json.Unmarshal(v, &item); err != nil {
				if debugMode {
					fmt.Printf("  [DB] Error unmarshaling gitlab issue %s: %v\n", key, err)
				}
				return err
			}
			items[key] = item.Issue
			labels[key] = item.Label
			return nil
		})
	})
	if err != nil {
		if debugMode {
			fmt.Printf("  [DB] Error reading GitLab issues: %v\n", err)
		}
		return nil, nil, err
	}

	if debugMode {
		fmt.Printf("  [DB] Loaded %d GitLab issues from database\n", len(items))
	}

	return items, labels, nil
}

func (d *Database) GetAllGitHubPullRequestsWithLabels(debugMode bool) (map[string]MergeRequestModel, map[string]string, error) {
	items := make(map[string]MergeRequestModel)
	labels := make(map[string]string)

	if debugMode {
		fmt.Printf("  [DB] Reading all GitHub pull requests with labels from database...\n")
	}

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(githubPullRequestsBkt)
		if b == nil {
			return nil
		}

		return b.ForEach(func(k, v []byte) error {
			key := string(k)

			var item GitHubPRWithLabel
			if err := json.Unmarshal(v, &item); err == nil {
				if item.PR.Number != 0 || item.Label != "" {
					items[key] = item.PR
					labels[key] = item.Label
					return nil
				}
			}

			var pr MergeRequestModel
			if err := json.Unmarshal(v, &pr); err != nil {
				if debugMode {
					fmt.Printf("  [DB] Error unmarshaling github pull request %s: %v\n", key, err)
				}
				return err
			}

			items[key] = pr
			labels[key] = ""
			return nil
		})
	})
	if err != nil {
		if debugMode {
			fmt.Printf("  [DB] Error reading GitHub pull requests: %v\n", err)
		}
		return nil, nil, err
	}

	if debugMode {
		fmt.Printf("  [DB] Loaded %d GitHub pull requests from database\n", len(items))
	}

	return items, labels, nil
}

func (d *Database) GetAllGitHubIssuesWithLabels(debugMode bool) (map[string]IssueModel, map[string]string, error) {
	items := make(map[string]IssueModel)
	labels := make(map[string]string)

	if debugMode {
		fmt.Printf("  [DB] Reading all GitHub issues with labels from database...\n")
	}

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(githubIssuesBkt)
		if b == nil {
			return nil
		}

		return b.ForEach(func(k, v []byte) error {
			key := string(k)

			var item GitHubIssueWithLabel
			if err := json.Unmarshal(v, &item); err == nil {
				if item.Issue.Number != 0 || item.Label != "" {
					items[key] = item.Issue
					labels[key] = item.Label
					return nil
				}
			}

			var issue IssueModel
			if err := json.Unmarshal(v, &issue); err != nil {
				if debugMode {
					fmt.Printf("  [DB] Error unmarshaling github issue %s: %v\n", key, err)
				}
				return err
			}

			items[key] = issue
			labels[key] = ""
			return nil
		})
	})
	if err != nil {
		if debugMode {
			fmt.Printf("  [DB] Error reading GitHub issues: %v\n", err)
		}
		return nil, nil, err
	}

	if debugMode {
		fmt.Printf("  [DB] Loaded %d GitHub issues from database\n", len(items))
	}

	return items, labels, nil
}

func (d *Database) HasGitLabData() (bool, error) {
	hasData := false
	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(gitlabMergeRequestsBkt)
		if b != nil && b.Stats().KeyN > 0 {
			hasData = true
			return nil
		}

		b = tx.Bucket(gitlabIssuesBkt)
		if b != nil && b.Stats().KeyN > 0 {
			hasData = true
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return hasData, nil
}

func (d *Database) GetGitLabNotes(pathWithNamespace, itemType string, iid int) ([]GitLabNoteRecord, error) {
	notes := make([]GitLabNoteRecord, 0)
	prefix := fmt.Sprintf(
		"%s|%s|%d|",
		normalizeProjectPathWithNamespace(pathWithNamespace),
		strings.ToLower(strings.TrimSpace(itemType)),
		iid,
	)

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(gitlabNotesBkt)
		if b == nil {
			return nil
		}

		c := b.Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			var record GitLabNoteRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			notes = append(notes, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return notes, nil
}

func (d *Database) GetGitHubPRReviewComments(owner, repo string, prNumber int) ([]GitHubPRReviewCommentRecord, error) {
	comments := make([]GitHubPRReviewCommentRecord, 0)
	prefix := fmt.Sprintf("%s/%s#%d/pr_review_comment/", strings.TrimSpace(owner), strings.TrimSpace(repo), prNumber)

	err := d.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket(githubCommentsBkt)
		if b == nil {
			return nil
		}

		c := b.Cursor()
		for k, v := c.Seek([]byte(prefix)); k != nil && strings.HasPrefix(string(k), prefix); k, v = c.Next() {
			var record GitHubPRReviewCommentRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return err
			}
			comments = append(comments, record)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return comments, nil
}
