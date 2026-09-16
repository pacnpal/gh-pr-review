package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/agynio/gh-pr-review/internal/ghcli"
	"github.com/agynio/gh-pr-review/internal/resolver"
)

const (
	defaultFirstReviews  = 100
	defaultFirstThreads  = 100
	defaultFirstComments = 100
)

// Service fetches and shapes pull request review reports.
type Service struct {
	API ghcli.API
}

// Options controls data retrieval and shaping for the report.
type Options struct {
	Reviewer             string
	States               []State
	StatesProvided       bool
	RequireUnresolved    bool
	RequireNotOutdated   bool
	TailReplies          int
	IncludeCommentNodeID bool
}

// NewService constructs a report service using the provided GraphQL API client.
func NewService(api ghcli.API) *Service {
	return &Service{API: api}
}

// Fetch generates a review report for the given pull request.
func (s *Service) Fetch(pr resolver.Identity, opts Options) (Report, error) {
	return s.FetchContext(context.Background(), pr, opts)
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type reviewNode struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	Body        *string `json:"body"`
	SubmittedAt *string `json:"submittedAt"`
	DatabaseID  *int    `json:"databaseId"`
	Author      *struct {
		Login string `json:"login"`
	} `json:"author"`
}

type commentNode struct {
	ID         string `json:"id"`
	DatabaseID int    `json:"databaseId"`
	Body       string `json:"body"`
	CreatedAt  string `json:"createdAt"`
	Author     *struct {
		Login string `json:"login"`
	} `json:"author"`
	PullRequestReview *struct {
		DatabaseID *int   `json:"databaseId"`
		State      string `json:"state"`
		ID         string `json:"id"`
	} `json:"pullRequestReview"`
	ReplyTo *struct {
		ID         string `json:"id"`
		DatabaseID int    `json:"databaseId"`
	} `json:"replyTo"`
}

type commentConnection struct {
	Nodes    []commentNode `json:"nodes"`
	PageInfo pageInfo      `json:"pageInfo"`
}

type threadNode struct {
	ID         string            `json:"id"`
	Path       string            `json:"path"`
	Line       *int              `json:"line"`
	IsResolved bool              `json:"isResolved"`
	IsOutdated bool              `json:"isOutdated"`
	Comments   commentConnection `json:"comments"`
}

type reportResponse struct {
	Repository *struct {
		PullRequest *struct {
			Reviews struct {
				Nodes    []reviewNode `json:"nodes"`
				PageInfo pageInfo     `json:"pageInfo"`
			} `json:"reviews"`
			ReviewThreads struct {
				Nodes    []threadNode `json:"nodes"`
				PageInfo pageInfo     `json:"pageInfo"`
			} `json:"reviewThreads"`
		} `json:"pullRequest"`
	} `json:"repository"`
}

type contextGraphQLAPI interface {
	GraphQLContext(context.Context, string, map[string]interface{}, interface{}) error
}

func (s *Service) graphQL(ctx context.Context, query string, variables map[string]interface{}, result interface{}) error {
	if api, ok := s.API.(contextGraphQLAPI); ok {
		return api.GraphQLContext(ctx, query, variables, result)
	}
	return s.API.GraphQL(query, variables, result)
}

// FetchContext generates a review report and cancels in-flight GraphQL calls with ctx.
func (s *Service) FetchContext(ctx context.Context, pr resolver.Identity, opts Options) (Report, error) {
	variables := map[string]interface{}{
		"owner":         pr.Owner,
		"name":          pr.Repo,
		"number":        pr.Number,
		"firstReviews":  defaultFirstReviews,
		"firstThreads":  defaultFirstThreads,
		"firstComments": defaultFirstComments,
	}
	if opts.StatesProvided {
		states := make([]string, len(opts.States))
		for i, st := range opts.States {
			states[i] = string(st)
		}
		variables["states"] = states
	}

	reviewNodes := []reviewNode{}
	threadNodes := []threadNode{}
	reviewsMore, threadsMore := true, true
	for reviewsMore || threadsMore {
		var response reportResponse
		if err := s.graphQL(ctx, reportQuery, variables, &response); err != nil {
			return Report{}, err
		}
		if response.Repository == nil || response.Repository.PullRequest == nil {
			return Report{}, errors.New("pull request not found or inaccessible")
		}
		prData := response.Repository.PullRequest
		if reviewsMore {
			reviewNodes = append(reviewNodes, prData.Reviews.Nodes...)
			reviewsMore = prData.Reviews.PageInfo.HasNextPage
			if reviewsMore {
				if prData.Reviews.PageInfo.EndCursor == "" {
					return Report{}, errors.New("reviews pagination missing end cursor")
				}
				variables["reviewsAfter"] = prData.Reviews.PageInfo.EndCursor
			}
		}
		if threadsMore {
			threadNodes = append(threadNodes, prData.ReviewThreads.Nodes...)
			threadsMore = prData.ReviewThreads.PageInfo.HasNextPage
			if threadsMore {
				if prData.ReviewThreads.PageInfo.EndCursor == "" {
					return Report{}, errors.New("threads pagination missing end cursor")
				}
				variables["threadsAfter"] = prData.ReviewThreads.PageInfo.EndCursor
			}
		}
	}

	for i := range threadNodes {
		for threadNodes[i].Comments.PageInfo.HasNextPage {
			cursor := threadNodes[i].Comments.PageInfo.EndCursor
			if cursor == "" {
				return Report{}, errors.New("comments pagination missing end cursor")
			}
			var response struct {
				Node *struct {
					Comments commentConnection `json:"comments"`
				} `json:"node"`
			}
			if err := s.graphQL(ctx, threadCommentsQuery, map[string]interface{}{
				"threadID":      threadNodes[i].ID,
				"firstComments": defaultFirstComments,
				"commentsAfter": cursor,
			}, &response); err != nil {
				return Report{}, err
			}
			if response.Node == nil {
				return Report{}, errors.New("review thread not found or inaccessible")
			}
			threadNodes[i].Comments.Nodes = append(threadNodes[i].Comments.Nodes, response.Node.Comments.Nodes...)
			threadNodes[i].Comments.PageInfo = response.Node.Comments.PageInfo
		}
	}

	reviews := make([]Review, 0, len(reviewNodes))

	for _, node := range reviewNodes {
		if node.DatabaseID == nil {
			return Report{}, errors.New("review missing databaseId")
		}
		if node.Author == nil || node.Author.Login == "" {
			return Report{}, errors.New("review missing author login")
		}
		state, ok := parseState(node.State)
		if !ok {
			return Report{}, fmt.Errorf("unknown review state %q", node.State)
		}
		review := Review{
			ID:          node.ID,
			State:       state,
			Body:        node.Body,
			AuthorLogin: node.Author.Login,
			DatabaseID:  *node.DatabaseID,
		}
		if node.SubmittedAt != nil && strings.TrimSpace(*node.SubmittedAt) != "" {
			parsed, err := time.Parse(time.RFC3339, *node.SubmittedAt)
			if err != nil {
				return Report{}, fmt.Errorf("parse review submittedAt: %w", err)
			}
			review.SubmittedAt = &parsed
		}
		reviews = append(reviews, review)
	}

	threads := make([]Thread, 0, len(threadNodes))
	for _, node := range threadNodes {
		thread := Thread{
			ID:         node.ID,
			Path:       node.Path,
			Line:       node.Line,
			IsResolved: node.IsResolved,
			IsOutdated: node.IsOutdated,
			Comments:   make([]ThreadComment, 0, len(node.Comments.Nodes)),
		}

		for _, comment := range node.Comments.Nodes {
			if comment.ID == "" {
				return Report{}, errors.New("comment missing id")
			}
			if comment.Author == nil || comment.Author.Login == "" {
				return Report{}, errors.New("comment missing author login")
			}
			createdAt, err := time.Parse(time.RFC3339, comment.CreatedAt)
			if err != nil {
				return Report{}, fmt.Errorf("parse comment createdAt: %w", err)
			}
			var reviewDatabaseID *int
			if comment.PullRequestReview != nil {
				reviewDatabaseID = comment.PullRequestReview.DatabaseID
			}
			var replyTo *int
			var replyToNode *string
			if comment.ReplyTo != nil {
				replyID := comment.ReplyTo.DatabaseID
				replyTo = &replyID
				if comment.ReplyTo.ID != "" {
					replyNode := comment.ReplyTo.ID
					replyToNode = &replyNode
				}
			}

			thread.Comments = append(thread.Comments, ThreadComment{
				NodeID:             comment.ID,
				DatabaseID:         comment.DatabaseID,
				Body:               comment.Body,
				CreatedAt:          createdAt,
				AuthorLogin:        comment.Author.Login,
				ReviewDatabaseID:   reviewDatabaseID,
				ReplyToDatabaseID:  replyTo,
				ReplyToCommentNode: replyToNode,
			})
		}

		threads = append(threads, thread)
	}

	filters := FilterOptions{
		Reviewer:             opts.Reviewer,
		States:               opts.States,
		RequireUnresolved:    opts.RequireUnresolved,
		RequireNotOutdated:   opts.RequireNotOutdated,
		TailReplies:          opts.TailReplies,
		IncludeCommentNodeID: opts.IncludeCommentNodeID,
	}

	return BuildReport(reviews, threads, filters), nil
}

func parseState(raw string) (State, bool) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case string(StateApproved):
		return StateApproved, true
	case string(StateChangesRequested):
		return StateChangesRequested, true
	case string(StateCommented):
		return StateCommented, true
	case string(StateDismissed):
		return StateDismissed, true
	default:
		return "", false
	}
}
