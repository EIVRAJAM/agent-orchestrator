package cli

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
)

type trackerWritebackDTO struct {
	ID          string     `json:"id"`
	ProjectID   string     `json:"projectId,omitempty"`
	SessionID   string     `json:"sessionId"`
	Status      string     `json:"status"`
	Attempts    int        `json:"attempts"`
	FailureCode string     `json:"failureCode,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	UpdatedAt   time.Time  `json:"updatedAt"`
	ClaimedAt   *time.Time `json:"claimedAt,omitempty"`
}

type trackerWritebackListDTO struct {
	Items   []trackerWritebackDTO `json:"items"`
	Limit   int                   `json:"limit"`
	Offset  int                   `json:"offset"`
	HasMore bool                  `json:"hasMore"`
}

func newTrackerCommand(ctx *commandContext) *cobra.Command {
	cmd := &cobra.Command{Use: "tracker", Short: "Inspect tracker intake and write-back state"}
	cmd.AddCommand(newTrackerWritebacksCommand(ctx))
	return cmd
}

func newTrackerWritebacksCommand(ctx *commandContext) *cobra.Command {
	var status string
	var jsonOutput bool
	var limit, offset int
	cmd := &cobra.Command{
		Use:   "writebacks",
		Short: "List durable tracker write-back states",
		Args:  usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			q := url.Values{}
			if strings.TrimSpace(status) != "" {
				q.Set("status", strings.TrimSpace(status))
			}
			if limit != 0 {
				q.Set("limit", strconv.Itoa(limit))
			}
			if offset != 0 {
				q.Set("offset", strconv.Itoa(offset))
			}
			path := "tracker/writebacks"
			if encoded := q.Encode(); encoded != "" {
				path += "?" + encoded
			}
			var response trackerWritebackListDTO
			if err := ctx.getJSON(cmd.Context(), path, &response); err != nil {
				return err
			}
			if jsonOutput {
				return writeJSON(cmd.OutOrStdout(), response)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tPROJECT\tSESSION\tSTATUS\tATTEMPTS\tCODE\tUPDATED")
			for _, item := range response.Items {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
					item.ID, item.ProjectID, item.SessionID, item.Status, item.Attempts,
					item.FailureCode, item.UpdatedAt.UTC().Format(time.RFC3339))
			}
			_ = tw.Flush()
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "limit=%d offset=%d hasMore=%t\n", response.Limit, response.Offset, response.HasMore)
			return err
		},
	}
	cmd.Flags().StringVar(&status, "status", "", "Filter by durable status: pending, claimed, sent, uncertain, failed")
	cmd.Flags().IntVar(&limit, "limit", 50, "Maximum rows (1-100)")
	cmd.Flags().IntVar(&offset, "offset", 0, "Rows to skip")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Print JSON")
	return cmd
}
