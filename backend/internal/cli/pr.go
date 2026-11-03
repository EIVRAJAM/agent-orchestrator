package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

type mergePRRequest struct {
	PRURL           string `json:"prUrl"`
	ExpectedHeadSHA string `json:"expectedHeadSha"`
}

type mergePRResponse struct {
	OK       bool   `json:"ok"`
	PRNumber int    `json:"prNumber"`
	Method   string `json:"method"`
}

type resolveCommentsRequest struct {
	CommentIDs []string `json:"commentIds,omitempty"`
}

type resolveCommentsResponse struct {
	OK       bool `json:"ok"`
	Resolved int  `json:"resolved"`
}

func newPRCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pr",
		Short: "Manage pull requests",
	}
	cmd.AddCommand(newPRMergeCommand(ctx))
	cmd.AddCommand(newPRResolveCommentsCommand(ctx))
	return cmd
}

func newPRMergeCommand(ctx *commandContext) *cobra.Command {
	var prURL string
	var expectedHeadSHA string
	cmd := &cobra.Command{
		Use:   "merge <pr-number>",
		Short: "Merge a pull request",
		Args:  usageArgs(cobra.ExactArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, err := normalizePRNumber(args[0])
			if err != nil {
				return err
			}
			prURL = strings.TrimSpace(prURL)
			expectedHeadSHA = strings.TrimSpace(expectedHeadSHA)
			if prURL == "" || expectedHeadSHA == "" {
				return usageError{errors.New("--pr-url and --head-sha are required")}
			}
			_, _, _, urlNumber, err := cliParsePRURL(prURL)
			if err != nil || strconv.Itoa(urlNumber) != prNumber {
				return usageError{errors.New("--pr-url must be a valid PR/MR URL matching the PR number")}
			}
			var res mergePRResponse
			if err := ctx.postJSON(
				cmd.Context(),
				"prs/"+url.PathEscape(prNumber)+"/merge",
				mergePRRequest{PRURL: prURL, ExpectedHeadSHA: expectedHeadSHA},
				&res,
			); err != nil {
				return err
			}
			if method := strings.TrimSpace(res.Method); method != "" {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged PR #%d using %s\n", res.PRNumber, method)
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "merged PR #%d\n", res.PRNumber)
			return err
		},
	}
	cmd.Flags().StringVar(&prURL, "pr-url", "", "Exact PR/MR URL to merge")
	cmd.Flags().StringVar(&expectedHeadSHA, "head-sha", "", "Exact provider head SHA the user reviewed")
	return cmd
}

func newPRResolveCommentsCommand(ctx *commandContext) *cobra.Command {
	return &cobra.Command{
		Use:   "resolve-comments <pr-number> [comment-id...]",
		Short: "Resolve review threads on a pull request (by comment or thread id)",
		Args:  usageArgs(cobra.MinimumNArgs(1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			prNumber, err := normalizePRNumber(args[0])
			if err != nil {
				return err
			}
			commentIDs := make([]string, 0, len(args)-1)
			for _, id := range args[1:] {
				id = strings.TrimSpace(id)
				if id == "" {
					return usageError{errors.New("comment id must not be blank")}
				}
				commentIDs = append(commentIDs, id)
			}
			var res resolveCommentsResponse
			if err := ctx.postJSON(
				cmd.Context(),
				"prs/"+url.PathEscape(prNumber)+"/resolve-comments",
				resolveCommentsRequest{CommentIDs: commentIDs},
				&res,
			); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "resolved %d review thread(s) on PR #%s\n", res.Resolved, prNumber)
			return err
		},
	}
}

func normalizePRNumber(raw string) (string, error) {
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "#")
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return "", usageError{errors.New("PR number must be a positive integer")}
	}
	return strconv.Itoa(n), nil
}
