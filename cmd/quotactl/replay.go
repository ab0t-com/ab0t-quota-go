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
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/spf13/cobra"
)

func newReplayCmd() *cobra.Command {
	var (
		file      string
		secret    string
		targetURL string
		bareSig   bool
	)
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Replay one or more saved events through a receiver endpoint",
		Long: `Reads JSON event payloads from --file (one event per line or a JSON
array) and POSTs them with a valid HMAC signature to --target.

The receiver uses delivery dedup so replay is safe — already-processed
events are short-circuited.

Bare-hex signing (--bare): the receiver accepts both "sha256=<hex>" and
"<hex>". Use --bare if your target rejects the prefixed form.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if secret == "" {
				secret = os.Getenv("AB0T_AUTH_WEBHOOK_SECRET")
			}
			if file == "" || targetURL == "" || secret == "" {
				return errors.New("--file and --target are required, and --secret (or AB0T_AUTH_WEBHOOK_SECRET)")
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			events, err := splitEvents(raw)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			client := &http.Client{Timeout: 15 * time.Second}
			failed := 0
			for i, ev := range events {
				sig := authevents.SignBody(ev, secret)
				if !bareSig {
					sig = "sha256=" + sig
				}
				req, _ := http.NewRequestWithContext(ctx, "POST", targetURL, bytes.NewReader(ev))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Event-Signature", sig)
				resp, err := client.Do(req)
				if err != nil {
					return fmt.Errorf("event %d: %w", i, err)
				}
				body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				resp.Body.Close()
				fmt.Fprintf(cmd.OutOrStdout(), "[%d] HTTP %d %s\n", i, resp.StatusCode, truncate(string(body), 120))
				if resp.StatusCode < 200 || resp.StatusCode > 299 {
					failed++
				}
			}
			// v0.1.10: any refused event is a failure (exit non-zero), not a log line.
			if failed > 0 {
				return fmt.Errorf("%d of %d events were not accepted by %s (see the HTTP lines above)",
					failed, len(events), targetURL)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to event JSON (one per line or JSON array)")
	// v0.1.10: NO env default here — pflag prints a non-empty default in --help,
	// which printed the live secret. The env is read at run time instead.
	cmd.Flags().StringVar(&secret, "secret", "", "HMAC secret (default: $AB0T_AUTH_WEBHOOK_SECRET; prefer the env — argv is visible in ps)")
	cmd.Flags().StringVar(&targetURL, "target", "", "receiver URL (https://svc/api/quotas/_webhooks/auth)")
	cmd.Flags().BoolVar(&bareSig, "bare", false, "send bare-hex signature (no sha256= prefix)")
	return cmd
}

func splitEvents(raw []byte) ([][]byte, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, errors.New("empty input")
	}
	if raw[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return nil, fmt.Errorf("decode array: %w", err)
		}
		out := make([][]byte, len(arr))
		for i, r := range arr {
			out[i] = []byte(r)
		}
		return out, nil
	}
	var out [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
