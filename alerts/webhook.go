package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ab0t-com/ab0t-quota-go/engine"
)

// WebhookDispatcher POSTs alerts to an external URL.
//
// SSRF guard: rejects file://, refuses localhost / 127.0.0.1 / link-local
// / private RFC1918 ranges UNLESS AllowPrivateNetworks is true. Default
// is off because Alerts often live outside the cluster's trust boundary.
type WebhookDispatcher struct {
	URL                  string
	HTTPClient           *http.Client
	AllowPrivateNetworks bool
}

// NewWebhookDispatcher returns a WebhookDispatcher with a 10s HTTP timeout
// and SSRF guard off by default.
func NewWebhookDispatcher(rawURL string) (*WebhookDispatcher, error) {
	if rawURL == "" {
		return nil, errors.New("webhook: URL required")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("webhook: bad URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("webhook: scheme %q rejected", u.Scheme)
	}
	return &WebhookDispatcher{
		URL:        rawURL,
		HTTPClient: &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Send POSTs a JSON alert.
func (w *WebhookDispatcher) Send(ctx context.Context, level Level, r engine.Result) error {
	if err := w.checkSSRF(); err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"level":     level,
		"resource":  r.Resource,
		"tier":      r.TierID,
		"used":      r.Used,
		"limit":     r.Limit,
		"threshold": r.Threshold,
		"timestamp": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return fmt.Errorf("webhook: encode: %w", err)
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", w.URL, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("webhook: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("webhook: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (w *WebhookDispatcher) checkSSRF() error {
	if w.AllowPrivateNetworks {
		return nil
	}
	u, err := url.Parse(w.URL)
	if err != nil {
		return fmt.Errorf("webhook: bad URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return errors.New("webhook: empty host")
	}
	if strings.EqualFold(host, "localhost") {
		return errors.New("webhook: SSRF guard rejected localhost")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		// can't resolve → let HTTP request handle it
		return nil
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
			isPrivate(ip) {
			return fmt.Errorf("webhook: SSRF guard rejected %s", ip)
		}
	}
	return nil
}

func isPrivate(ip net.IP) bool {
	private := []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"}
	for _, cidr := range private {
		_, n, _ := net.ParseCIDR(cidr)
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
