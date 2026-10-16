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
			if secret == "" {
				secret = os.Getenv("AB0T_AUTH_WEBHOOK_SECRET")
			}
			if input == "" {
				return errors.New("--input required")
			}
			if !dryRun && (targetURL == "" || secret == "") {
				return errors.New("--target and --secret (or AB0T_AUTH_WEBHOOK_SECRET) are required unless --dry-run")
			}
			raw, err := os.ReadFile(input)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			client := &http.Client{Timeout: 15 * time.Second}
			sent, failed := 0, 0
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
					fmt.Fprintln(cmd.OutOrStdout(), "DRY:", string(body))
					continue
				}
				sent++
				sig := authevents.SignBody(body, secret)
				if !bareSig {
					sig = "sha256=" + sig
				}
				req, _ := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Event-Signature", sig)
				resp, err := client.Do(req)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "[%d] %s: %v\n", i, userID, err)
					failed++
					continue
				}
				respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				fmt.Fprintf(cmd.OutOrStdout(), "[%d] %s → HTTP %d %s\n", i, userID, resp.StatusCode,
					truncate(string(respBody), 80))
				if resp.StatusCode < 200 || resp.StatusCode > 299 {
					failed++
				}
			}
			// v0.1.10: refused rows are a failure (exit non-zero); re-run is safe —
			// business dedup makes already-granted users no-ops.
			if failed > 0 {
				return fmt.Errorf("%d of %d backfill events were not accepted by %s — fix the cause and re-run "+
					"(already-granted users are no-ops)", failed, sent, targetURL)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&input, "input", "", "path to CSV (user_id[,org_id])")
	// v0.1.10: no env default (pflag would print the live secret in --help).
	cmd.Flags().StringVar(&secret, "secret", "", "HMAC secret (default: $AB0T_AUTH_WEBHOOK_SECRET; prefer the env — argv is visible in ps)")
	cmd.Flags().StringVar(&targetURL, "target", "", "receiver URL")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print events without sending")
	cmd.Flags().BoolVar(&bareSig, "bare", false, "send bare-hex signature")
	return cmd
}
