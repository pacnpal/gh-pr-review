package report

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	_ "embed"

	"github.com/agynio/gh-pr-review/internal/resolver"
)

//go:embed testdata/report_response.json
var reportResponseFixture []byte

func TestServiceFetchShapesReport(t *testing.T) {
	fake := &stubAPI{t: t, payload: reportResponseFixture}
	svc := NewService(fake)

	identity := resolver.Identity{Owner: "agyn", Repo: "sandbox", Number: 51}
	result, err := svc.Fetch(identity, Options{
		Reviewer:           "alice",
		States:             []State{StateApproved, StateCommented},
		StatesProvided:     true,
		RequireNotOutdated: true,
		TailReplies:        1,
	})
	if err != nil {
		t.Fatalf("fetch report: %v", err)
	}

	if len(result.Reviews) != 1 {
		t.Fatalf("expected 1 review after filtering, got %d", len(result.Reviews))
	}
	review := result.Reviews[0]
	if review.ID != "R1" {
		t.Fatalf("expected review R1, got %s", review.ID)
	}
	if len(review.Comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(review.Comments))
	}
	comment := review.Comments[0]
	if comment.ThreadID != "T1" {
		t.Fatalf("expected parent thread T1, got %s", comment.ThreadID)
	}
	if comment.CommentNodeID != nil {
		t.Fatalf("expected comment_node_id omitted by default, got %v", comment.CommentNodeID)
	}
	if len(comment.ThreadComments) != 1 {
		t.Fatalf("expected 1 reply after tail filter, got %d", len(comment.ThreadComments))
	}
	if comment.ThreadComments[0].Body != "Reply beta" {
		t.Fatalf("expected reply body 'Reply beta', got %s", comment.ThreadComments[0].Body)
	}
	if comment.ThreadComments[0].CommentNodeID != nil {
		t.Fatalf("expected reply comment_node_id omitted by default, got %v", comment.ThreadComments[0].CommentNodeID)
	}

	rawStates, ok := fake.lastVariables["states"]
	if !ok {
		t.Fatalf("expected states variable propagated, variables: %#v", fake.lastVariables)
	}
	statesVar, ok := rawStates.([]string)
	if !ok || len(statesVar) != 2 {
		t.Fatalf("expected states variable propagated as []string, got %#v", rawStates)
	}
}

func TestServiceFetchIncludesCommentNodeID(t *testing.T) {
	fake := &stubAPI{t: t, payload: reportResponseFixture}
	svc := NewService(fake)

	identity := resolver.Identity{Owner: "agyn", Repo: "sandbox", Number: 51}
	result, err := svc.Fetch(identity, Options{IncludeCommentNodeID: true})
	if err != nil {
		t.Fatalf("fetch report with comment node ids: %v", err)
	}

	if len(result.Reviews) == 0 {
		t.Fatal("expected reviews in result")
	}
	review := result.Reviews[0]
	if len(review.Comments) == 0 {
		t.Fatal("expected comments for review")
	}
	comment := review.Comments[0]
	if comment.CommentNodeID == nil || *comment.CommentNodeID != "C301" {
		t.Fatalf("expected comment_node_id C301, got %v", comment.CommentNodeID)
	}
	if len(comment.ThreadComments) == 0 {
		t.Fatal("expected replies to be present")
	}
	if comment.ThreadComments[0].CommentNodeID == nil || *comment.ThreadComments[0].CommentNodeID == "" {
		t.Fatalf("expected reply comment node id, got %v", comment.ThreadComments[0].CommentNodeID)
	}
}

func TestServiceFetchErrorsOnMissingReviewDBID(t *testing.T) {
	broken := map[string]any{}
	if err := json.Unmarshal(reportResponseFixture, &broken); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	repo := broken["repository"].(map[string]any)
	pr := repo["pullRequest"].(map[string]any)
	reviews := pr["reviews"].(map[string]any)
	nodes := reviews["nodes"].([]any)
	first := nodes[0].(map[string]any)
	delete(first, "databaseId")

	modified, err := json.Marshal(broken)
	if err != nil {
		t.Fatalf("marshal modified: %v", err)
	}

	fake := &stubAPI{t: t, payload: modified}
	svc := NewService(fake)

	_, err = svc.Fetch(resolver.Identity{Owner: "agyn", Repo: "sandbox", Number: 51}, Options{})
	if err == nil {
		t.Fatal("expected error for missing databaseId")
	}
	if !strings.Contains(err.Error(), "review missing databaseId") {
		t.Fatalf("expected missing databaseId error, got %v", err)
	}
}

func TestServiceFetchPaginatesReviewsThreadsAndComments(t *testing.T) {
	fake := &pagingAPI{t: t, topPages: [][]byte{[]byte(`{
		"repository":{"pullRequest":{
			"reviews":{"nodes":[{"id":"R1","state":"APPROVED","body":"first","submittedAt":"2025-12-03T10:00:00Z","databaseId":101,"author":{"login":"alice"}}],"pageInfo":{"hasNextPage":true,"endCursor":"REV1"}},
			"reviewThreads":{"nodes":[{"id":"T1","path":"main.go","line":1,"isResolved":false,"isOutdated":false,"comments":{"nodes":[{"id":"C1","databaseId":1,"body":"parent","createdAt":"2025-12-03T10:01:00Z","author":{"login":"alice"},"pullRequestReview":{"id":"R1","state":"APPROVED","databaseId":101},"replyTo":null}],"pageInfo":{"hasNextPage":true,"endCursor":"COMMENT1"}}}],"pageInfo":{"hasNextPage":true,"endCursor":"THREAD1"}}
		}}}`), []byte(`{
		"repository":{"pullRequest":{
			"reviews":{"nodes":[{"id":"R2","state":"COMMENTED","body":"second","submittedAt":"2025-12-03T10:02:00Z","databaseId":202,"author":{"login":"bob"}}],"pageInfo":{"hasNextPage":false,"endCursor":null}},
			"reviewThreads":{"nodes":[{"id":"T2","path":"other.go","line":2,"isResolved":false,"isOutdated":false,"comments":{"nodes":[],"pageInfo":{"hasNextPage":false,"endCursor":null}}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}
		}}}`)}, commentPage: []byte(`{
		"node":{"comments":{"nodes":[{"id":"C2","databaseId":2,"body":"reply","createdAt":"2025-12-03T10:03:00Z","author":{"login":"bob"},"pullRequestReview":{"id":"R1","state":"APPROVED","databaseId":101},"replyTo":{"id":"C1","databaseId":1}}],"pageInfo":{"hasNextPage":false,"endCursor":null}}}
	}`)}

	result, err := NewService(fake).FetchContext(context.Background(), resolver.Identity{Owner: "agyn", Repo: "sandbox", Number: 51}, Options{})
	if err != nil {
		t.Fatalf("fetch paginated report: %v", err)
	}
	if len(result.Reviews) != 2 {
		t.Fatalf("expected both review pages, got %#v", result.Reviews)
	}
	if got := result.Reviews[0].Comments[0].ThreadComments; len(got) != 1 || got[0].Body != "reply" {
		t.Fatalf("expected comment page reply, got %#v", got)
	}
	if len(fake.variables) != 3 {
		t.Fatalf("expected two top-level pages and one comment page, got %d calls", len(fake.variables))
	}
	if fake.variables[1]["reviewsAfter"] != "REV1" || fake.variables[1]["threadsAfter"] != "THREAD1" {
		t.Fatalf("expected top-level cursors on second page, got %#v", fake.variables[1])
	}
	if fake.variables[2]["threadID"] != "T1" || fake.variables[2]["commentsAfter"] != "COMMENT1" {
		t.Fatalf("expected thread comment cursor, got %#v", fake.variables[2])
	}
}

type stubAPI struct {
	t             *testing.T
	payload       []byte
	lastQuery     string
	lastVariables map[string]interface{}
}

type pagingAPI struct {
	t           *testing.T
	topPages    [][]byte
	commentPage []byte
	variables   []map[string]interface{}
	topCall     int
}

func (p *pagingAPI) REST(string, string, map[string]string, interface{}, interface{}) error {
	p.t.Fatal("unexpected REST call in report service test")
	return nil
}

func (p *pagingAPI) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	p.variables = append(p.variables, variables)
	payload := p.commentPage
	if !strings.Contains(query, "node(id: $threadID)") {
		payload = p.topPages[p.topCall]
		p.topCall++
	}
	return json.Unmarshal(payload, result)
}

func (s *stubAPI) REST(string, string, map[string]string, interface{}, interface{}) error {
	s.t.Fatalf("unexpected REST call in report service test")
	return nil
}

func (s *stubAPI) GraphQL(query string, variables map[string]interface{}, result interface{}) error {
	s.lastQuery = query
	s.lastVariables = variables
	if query != reportQuery {
		s.t.Fatalf("unexpected query: %s", query)
	}
	return json.Unmarshal(s.payload, result)
}
