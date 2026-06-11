package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/spf13/cobra"
)

// Backfill synthesizes signup events for users that already exist in auth
// but missed the credit-grant pass. Reads a CSV of user_id,org_id,tier_id
// from --input.
func newBackfillCmd() *cobra.Command {
	var (
		input     string
		secret    string
		targetURL string
		dryRun    bool
		bareSig   bool
	)
	cmd := &cobra.Command{
		Use:   "backfill",
		Short: "Synthesize signup events for legacy users",
		Long: `Reads --input (one user_id per line, or 'user_id,org_id'
per line) and posts a synthetic org.created event for each. The receiver's
business-dedup ensures already-granted users are no-ops.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if input == "" {
				return errors.New("--input required")
			}
			raw, err := os.ReadFile(input)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			client := &http.Client{Timeout: 15 * time.Second}
			for i, row := range bytes.Split(raw, []byte("\n")) {
				row = bytes.TrimSpace(row)
				if len(row) == 0 || bytes.HasPrefix(row, []byte("#")) {
					continue
				}
				parts := bytes.SplitN(row, []byte(","), 2)
				userID := string(bytes.TrimSpace(parts[0]))
				var orgID string
				if len(parts) > 1 {
					orgID = string(bytes.TrimSpace(parts[1]))
				}
				ev := map[string]any{
					"event_type":  "org.created",
					"event_id":    fmt.Sprintf("backfill-%s-%d", userID, time.Now().UnixNano()),
					"occurred_at": time.Now().UTC().Format(time.RFC3339),
					"data":        map[string]string{"user_id": userID, "org_id": orgID},
				}
				body, _ := json.Marshal(ev)
				if dryRun {
					fmt.Println("DRY:", string(body))
					continue
				}
				sig := authevents.SignBody(body, secret)
				if !bareSig {
					sig = "sha256=" + sig
				}
				req, _ := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Event-Signature", sig)
				resp, err := client.Do(req)
				if err != nil {
					fmt.Fprintf(os.Stderr, "[%d] %s: %v\n", i, userID, err)
					continue
				}
				respBody, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				fmt.Printf("[%d] %s → HTTP %d %s\n", i, userID, resp.StatusCode,
					truncate(string(respBody), 80))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "path to CSV (user_id[,org_id])")
	cmd.Flags().StringVar(&secret, "secret", os.Getenv("AB0T_AUTH_WEBHOOK_SECRET"), "HMAC secret")
	cmd.Flags().StringVar(&targetURL, "target", "", "receiver URL")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print events without sending")
	cmd.Flags().BoolVar(&bareSig, "bare", false, "send bare-hex signature")
	return cmd
}
