package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/agynio/gh-pr-review/internal/report"
	"github.com/agynio/gh-pr-review/internal/resolver"
)

const reviewViewFetchTimeout = 30 * time.Second

func newReviewViewCommand() *cobra.Command {
	opts := &reviewViewOptions{}

	cmd := &cobra.Command{
		Use:   "view [<number> | <url>]",
		Short: "View a structured review summary (GraphQL)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.Selector = args[0]
			}
			return runReviewView(cmd, opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Repo, "repo", "R", "", "Repository in 'owner/repo' format")
	cmd.Flags().IntVar(&opts.Pull, "pr", 0, "Pull request number")
	cmd.Flags().StringVar(&opts.Reviewer, "reviewer", "", "Filter to a specific reviewer (login)")
	cmd.Flags().StringSliceVar(&opts.States, "states", nil, "Comma-separated review states (APPROVED, CHANGES_REQUESTED, COMMENTED, DISMISSED)")
	cmd.Flags().BoolVar(&opts.Unresolved, "unresolved", false, "Only include unresolved threads")
	cmd.Flags().BoolVar(&opts.NotOutdated, "not_outdated", false, "Exclude outdated threads")
	cmd.Flags().IntVar(&opts.TailReplies, "tail", 0, "Limit to the last N replies per thread (0 = all)")
	cmd.Flags().BoolVar(&opts.IncludeCommentNodeID, "include-comment-node-id", false, "Include comment_node_id fields for parent comments and replies")
	cmd.Flags().BoolVar(&opts.Watch, "watch", false, "Watch for new reviews and comments")
	cmd.Flags().DurationVar(&opts.Interval, "interval", 30*time.Second, "Refresh interval when using --watch")

	return cmd
}

type reviewViewOptions struct {
	Repo                 string
	Pull                 int
	Selector             string
	Reviewer             string
	States               []string
	Unresolved           bool
	NotOutdated          bool
	TailReplies          int
	IncludeCommentNodeID bool
	Watch                bool
	Interval             time.Duration
}

func runReviewView(cmd *cobra.Command, opts *reviewViewOptions) error {
	if opts.TailReplies < 0 {
		return fmt.Errorf("invalid --tail value %d: must be non-negative", opts.TailReplies)
	}
	if opts.Watch && opts.Interval <= 0 {
		return fmt.Errorf("--interval must be positive")
	}

	selector, err := resolver.NormalizeSelector(opts.Selector, opts.Pull)
	if err != nil {
		return err
	}

	states, statesProvided, err := parseStateFilters(opts.States)
	if err != nil {
		return err
	}

	identity, err := resolver.Resolve(selector, opts.Repo, os.Getenv("GH_HOST"))
	if err != nil {
		return err
	}

	service := report.NewService(apiClientFactory(identity.Host))
	fetchOptions := report.Options{
		Reviewer:             strings.TrimSpace(opts.Reviewer),
		States:               states,
		StatesProvided:       statesProvided,
		RequireUnresolved:    opts.Unresolved,
		RequireNotOutdated:   opts.NotOutdated,
		TailReplies:          opts.TailReplies,
		IncludeCommentNodeID: opts.IncludeCommentNodeID,
	}

	if !opts.Watch {
		output, err := service.FetchContext(cmd.Context(), identity, fetchOptions)
		if err != nil {
			return err
		}
		return encodeJSON(cmd, output)
	}
	fetchOptions.TailReplies = 0

	seen := make(map[string]struct{})
	initial := true
	for {
		refreshCtx, cancel := context.WithTimeout(cmd.Context(), reviewViewFetchTimeout)
		output, err := service.FetchContext(refreshCtx, identity, fetchOptions)
		cancel()
		if err != nil {
			if cmd.Context().Err() != nil {
				return nil
			}
			if initial {
				return err
			}
			if _, writeErr := fmt.Fprintf(cmd.ErrOrStderr(), "watch refresh failed: %v\n", err); writeErr != nil {
				return writeErr
			}
		} else {
			updates := newReviewViewEvents(output, seen)
			if initial && opts.TailReplies > 0 {
				for i := range updates.Reviews {
					for j := range updates.Reviews[i].Comments {
						replies := updates.Reviews[i].Comments[j].ThreadComments
						if len(replies) > opts.TailReplies {
							updates.Reviews[i].Comments[j].ThreadComments = replies[len(replies)-opts.TailReplies:]
						}
					}
				}
			}
			if initial || len(updates.Reviews) > 0 {
				if err := encodeJSON(cmd, updates); err != nil {
					return err
				}
			}
			initial = false
		}

		timer := time.NewTimer(opts.Interval)
		select {
		case <-cmd.Context().Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

func newReviewViewEvents(current report.Report, seen map[string]struct{}) report.Report {
	updates := report.Report{Reviews: []report.ReportReview{}}
	for _, review := range current.Reviews {
		reviewUpdate := review
		reviewUpdate.Comments = nil
		reviewIsNew := markReviewViewEvent(seen, "review:"+review.ID)

		for _, comment := range review.Comments {
			commentUpdate := comment
			commentUpdate.ThreadComments = []report.ThreadReply{}
			commentIsNew := markReviewViewEvent(seen, "comment:"+comment.NodeID)

			for _, reply := range comment.ThreadComments {
				replyIsNew := markReviewViewEvent(seen, "comment:"+reply.NodeID)
				if commentIsNew || replyIsNew {
					commentUpdate.ThreadComments = append(commentUpdate.ThreadComments, reply)
				}
			}

			if commentIsNew || len(commentUpdate.ThreadComments) > 0 {
				reviewUpdate.Comments = append(reviewUpdate.Comments, commentUpdate)
			}
		}

		if reviewIsNew || len(reviewUpdate.Comments) > 0 {
			updates.Reviews = append(updates.Reviews, reviewUpdate)
		}
	}
	return updates
}

func markReviewViewEvent(seen map[string]struct{}, key string) bool {
	if _, ok := seen[key]; ok {
		return false
	}
	seen[key] = struct{}{}
	return true
}

func parseStateFilters(raw []string) ([]report.State, bool, error) {
	if len(raw) == 0 {
		return nil, false, nil
	}

	valid := map[string]report.State{
		"APPROVED":          report.StateApproved,
		"CHANGES_REQUESTED": report.StateChangesRequested,
		"COMMENTED":         report.StateCommented,
		"DISMISSED":         report.StateDismissed,
	}
	allowed := make([]string, 0, len(valid))
	for key := range valid {
		allowed = append(allowed, key)
	}
	sort.Strings(allowed)

	temp := make(map[report.State]struct{})
	states := make([]report.State, 0, len(raw))
	for _, entry := range raw {
		parts := strings.Split(entry, ",")
		for _, part := range parts {
			candidate := strings.ToUpper(strings.TrimSpace(part))
			if candidate == "" {
				continue
			}
			state, ok := valid[candidate]
			if !ok {
				return nil, false, fmt.Errorf("invalid review state %q (allowed: %s)", part, strings.Join(allowed, ", "))
			}
			if _, seen := temp[state]; seen {
				continue
			}
			temp[state] = struct{}{}
			states = append(states, state)
		}
	}

	if len(states) == 0 {
		return nil, false, fmt.Errorf("no valid states provided")
	}

	return states, true, nil
}
