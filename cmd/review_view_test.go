package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	_ "embed"

	"github.com/agynio/gh-pr-review/internal/ghcli"
	"github.com/agynio/gh-pr-review/internal/report"
)

//go:embed testdata/report_response.json
var viewResponse []byte

func TestReviewViewCommandFiltersOutput(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &fakeViewAPI{payload: viewResponse, t: t}
	apiClientFactory = func(host string) ghcli.API {
		if host == "" {
			t.Fatalf("expected host to be resolved, got empty")
		}
		return fake
	}

	root := newRootCommand()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"review", "view", "--repo", "agyn/repo", "--reviewer", "alice", "--states", "APPROVED,COMMENTED", "--not_outdated", "--tail", "1", "51"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}

	var payload struct {
		Reviews []struct {
			ID       string  `json:"id"`
			Body     *string `json:"body"`
			Comments []struct {
				ThreadID       string  `json:"thread_id"`
				CommentNodeID  *string `json:"comment_node_id"`
				ThreadComments []struct {
					Body          string  `json:"body"`
					CommentNodeID *string `json:"comment_node_id"`
				} `json:"thread_comments"`
			} `json:"comments"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if len(payload.Reviews) != 1 {
		t.Fatalf("expected 1 review in filtered output, got %d", len(payload.Reviews))
	}
	review := payload.Reviews[0]
	if review.ID != "R1" {
		t.Fatalf("expected review R1, got %s", review.ID)
	}
	if review.Body == nil {
		t.Fatalf("expected review body to be present for R1")
	}
	if len(review.Comments) != 1 {
		t.Fatalf("expected 1 comment for R1, got %d", len(review.Comments))
	}
	comment := review.Comments[0]
	if comment.ThreadID == "" {
		t.Fatal("expected thread_id to be present")
	}
	if comment.CommentNodeID != nil {
		t.Fatalf("expected comment_node_id omitted by default, got %v", *comment.CommentNodeID)
	}
	if len(comment.ThreadComments) != 1 {
		t.Fatalf("expected 1 reply after tail filter, got %d", len(comment.ThreadComments))
	}
	if comment.ThreadComments[0].Body != "Reply beta" {
		t.Fatalf("expected last reply body 'Reply beta', got %s", comment.ThreadComments[0].Body)
	}
	if comment.ThreadComments[0].CommentNodeID != nil {
		t.Fatalf("expected reply comment_node_id omitted by default, got %v", *comment.ThreadComments[0].CommentNodeID)
	}

	rawStates, ok := fake.variables["states"].([]string)
	if !ok || len(rawStates) != 2 {
		t.Fatalf("expected states variable propagated, got %#v", fake.variables["states"])
	}
}

func TestReviewViewCommandInvalidState(t *testing.T) {
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"review", "view", "--repo", "agyn/repo", "--states", "unknown", "51"})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected error for invalid state")
	}
	if !strings.Contains(err.Error(), "invalid review state") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestReviewViewCommandIncludesCommentNodeID(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	fake := &fakeViewAPI{payload: viewResponse, t: t}
	apiClientFactory = func(host string) ghcli.API { return fake }

	root := newRootCommand()
	buf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"review", "view", "--repo", "agyn/repo", "--include-comment-node-id", "51"})

	if err := root.Execute(); err != nil {
		t.Fatalf("execute command: %v", err)
	}

	var payload struct {
		Reviews []struct {
			Comments []struct {
				CommentNodeID  *string `json:"comment_node_id"`
				ThreadComments []struct {
					CommentNodeID *string `json:"comment_node_id"`
				} `json:"thread_comments"`
			} `json:"comments"`
		} `json:"reviews"`
	}
	if err := json.Unmarshal(buf.Bytes(), &payload); err != nil {
		t.Fatalf("parse json: %v", err)
	}
	if len(payload.Reviews) == 0 || len(payload.Reviews[0].Comments) == 0 {
		t.Fatal("expected comments in report output")
	}
	comment := payload.Reviews[0].Comments[0]
	if comment.CommentNodeID == nil || *comment.CommentNodeID == "" {
		t.Fatalf("expected comment_node_id to be populated, got %v", comment.CommentNodeID)
	}
	if len(comment.ThreadComments) > 0 {
		if comment.ThreadComments[0].CommentNodeID == nil || *comment.ThreadComments[0].CommentNodeID == "" {
			t.Fatalf("expected reply comment_node_id populated, got %v", comment.ThreadComments[0].CommentNodeID)
		}
	}
}

func TestReviewViewCommandWatchesForNewReviewsAndComments(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	secondPayload := appendReviewAndReply(t, viewResponse)
	ctx, cancel := context.WithCancel(context.Background())
	fake := &fakeViewAPI{payloads: [][]byte{viewResponse, viewResponse, []byte("{"), secondPayload}, t: t, onCall: func(call int) {
		if call == 4 {
			cancel()
		}
	}}
	apiClientFactory = func(host string) ghcli.API { return fake }

	root := newRootCommand()
	buf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	root.SetOut(buf)
	root.SetErr(errBuf)
	root.SetArgs([]string{"review", "view", "--repo", "agyn/repo", "--watch", "--interval", "1ms", "--tail", "1", "51"})

	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("execute command: %v", err)
	}

	decoder := json.NewDecoder(buf)
	var snapshots []report.Report
	for decoder.More() {
		var snapshot report.Report
		if err := decoder.Decode(&snapshot); err != nil {
			t.Fatalf("decode watched output: %v", err)
		}
		snapshots = append(snapshots, snapshot)
	}

	if len(snapshots) != 2 {
		t.Fatalf("expected initial snapshot and one update, got %d", len(snapshots))
	}
	if !strings.Contains(errBuf.String(), "watch refresh failed") {
		t.Fatalf("expected transient refresh error on stderr, got %q", errBuf.String())
	}
	if len(snapshots[0].Reviews) != 2 {
		t.Fatalf("expected full initial snapshot, got %#v", snapshots[0].Reviews)
	}
	if got := len(snapshots[0].Reviews[0].Comments[0].ThreadComments); got != 1 {
		t.Fatalf("expected --tail to limit the initial snapshot to 1 reply, got %d", got)
	}
	update := snapshots[1]
	if len(update.Reviews) != 2 {
		t.Fatalf("expected changed review plus new review, got %#v", update.Reviews)
	}
	if update.Reviews[0].ID != "R1" || len(update.Reviews[0].Comments) != 2 {
		t.Fatalf("expected reply burst and new parent comment for R1, got %#v", update.Reviews[0])
	}
	comment := update.Reviews[0].Comments[0]
	if comment.ThreadID != "T1" || len(comment.ThreadComments) != 2 || comment.ThreadComments[0].Body != "Reply delta" || comment.ThreadComments[1].Body != "Reply epsilon" {
		t.Fatalf("expected all new replies despite --tail 1, got %#v", comment)
	}
	if comment.CommentNodeID != nil || comment.ThreadComments[0].CommentNodeID != nil {
		t.Fatalf("expected comment node IDs to remain opt-in, got %#v", comment)
	}
	newParent := update.Reviews[0].Comments[1]
	if newParent.ThreadID != "T3" || newParent.Body != "New parent comment" || len(newParent.ThreadComments) != 0 {
		t.Fatalf("expected new parent comment, got %#v", newParent)
	}
	if update.Reviews[1].ID != "R3" {
		t.Fatalf("expected new review R3, got %#v", update.Reviews[1])
	}
}

func TestReviewViewCommandCancelsInFlightRefresh(t *testing.T) {
	originalFactory := apiClientFactory
	defer func() { apiClientFactory = originalFactory }()

	started := make(chan struct{})
	fake := &fakeViewAPI{t: t, blockUntilCanceled: true, started: started}
	apiClientFactory = func(host string) ghcli.API { return fake }

	ctx, cancel := context.WithCancel(context.Background())
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"review", "view", "--repo", "agyn/repo", "--watch", "--interval", "1ms", "51"})

	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for refresh to start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected cancellation to stop watch cleanly, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("watch did not cancel the in-flight refresh")
	}
}

func TestReviewViewCommandRejectsNonPositiveWatchInterval(t *testing.T) {
	root := newRootCommand()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"review", "view", "--repo", "agyn/repo", "--watch", "--interval", "0s", "51"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "--interval must be positive") {
		t.Fatalf("expected positive interval error, got %v", err)
	}
}

func appendReviewAndReply(t *testing.T, payload []byte) []byte {
	t.Helper()
	var response map[string]interface{}
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	pullRequest := response["repository"].(map[string]interface{})["pullRequest"].(map[string]interface{})
	reviews := pullRequest["reviews"].(map[string]interface{})["nodes"].([]interface{})
	pullRequest["reviews"].(map[string]interface{})["nodes"] = append(reviews, map[string]interface{}{
		"id": "R3", "state": "COMMENTED", "body": "New review", "submittedAt": "2025-12-03T10:06:00Z",
		"databaseId": 303, "author": map[string]interface{}{"login": "carol"},
	})
	threads := pullRequest["reviewThreads"].(map[string]interface{})["nodes"].([]interface{})
	comments := threads[0].(map[string]interface{})["comments"].(map[string]interface{})["nodes"].([]interface{})
	threads[0].(map[string]interface{})["comments"].(map[string]interface{})["nodes"] = append(comments, map[string]interface{}{
		"id": "C304", "databaseId": 304, "body": "Reply delta", "createdAt": "2025-12-03T10:06:00Z",
		"author":            map[string]interface{}{"login": "carol"},
		"pullRequestReview": map[string]interface{}{"id": "R1", "state": "APPROVED", "databaseId": 101},
		"replyTo":           map[string]interface{}{"id": "C303", "databaseId": 303},
	}, map[string]interface{}{
		"id": "C305", "databaseId": 305, "body": "Reply epsilon", "createdAt": "2025-12-03T10:07:00Z",
		"author":            map[string]interface{}{"login": "dave"},
		"pullRequestReview": map[string]interface{}{"id": "R1", "state": "APPROVED", "databaseId": 101},
		"replyTo":           map[string]interface{}{"id": "C304", "databaseId": 304},
	})
	pullRequest["reviewThreads"].(map[string]interface{})["nodes"] = append(threads, map[string]interface{}{
		"id": "T3", "path": "other.go", "line": 12, "isResolved": false, "isOutdated": false,
		"comments": map[string]interface{}{"nodes": []interface{}{map[string]interface{}{
			"id": "C501", "databaseId": 501, "body": "New parent comment", "createdAt": "2025-12-03T10:08:00Z",
			"author":            map[string]interface{}{"login": "carol"},
			"pullRequestReview": map[string]interface{}{"id": "R1", "state": "APPROVED", "databaseId": 101},
			"replyTo":           nil,
		}}},
	})

	updated, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return updated
}

type fakeViewAPI struct {
	t                  *testing.T
	payload            []byte
	payloads           [][]byte
	calls              int
	onCall             func(int)
	variables          map[string]interface{}
	blockUntilCanceled bool
	started            chan struct{}
}

func (f *fakeViewAPI) REST(string, string, map[string]string, interface{}, interface{}) error {
	f.t.Fatalf("unexpected REST call in view command")
	return nil
}

func (f *fakeViewAPI) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	f.variables = variables
	f.calls++
	payload := f.payload
	if len(f.payloads) > 0 {
		payload = f.payloads[min(f.calls-1, len(f.payloads)-1)]
	}
	if err := json.Unmarshal(payload, result); err != nil {
		return err
	}
	if f.onCall != nil {
		f.onCall(f.calls)
	}
	return nil
}

func (f *fakeViewAPI) GraphQLContext(ctx context.Context, query string, variables map[string]interface{}, result interface{}) error {
	if f.blockUntilCanceled {
		close(f.started)
		<-ctx.Done()
		return ctx.Err()
	}
	return f.GraphQL(query, variables, result)
}
