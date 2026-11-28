package main

// v0.3.2 — `quotactl doctor --money`: the money-safety checklist (quota.MoneyChecklist; same checks,
// names and grades as the Python `python -m ab0t_quota doctor --money`).
//
// Unlike the posture doctor it does NOT run quota.Setup: it reads the config and the environment and
// contacts nothing — unless --live, which makes read-only requests: auth's subscription list (needs
// AB0T_AUTH_ADMIN_TOKEN, events.read) and billing's tier for --org. The price-list outcome and the
// handler ledger live inside the running service: there, call q.MoneyChecklist(ctx, extra).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ab0t-com/ab0t-quota-go/authevents"
	"github.com/ab0t-com/ab0t-quota-go/billing"
	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/internal/httpx"
	"github.com/ab0t-com/ab0t-quota-go/mesh"
	"github.com/ab0t-com/ab0t-quota-go/providers"
	"github.com/ab0t-com/ab0t-quota-go/quota"
)

type moneyFlags struct {
	on          bool
	live        bool
	org         string
	expectTier  string
	environment string
}

func addMoneyFlags(cmd *cobra.Command, m *moneyFlags) {
	cmd.Flags().BoolVar(&m.on, "money", false,
		"run the money-safety checklist (paid mode, price list, gift events, tier of a paying org, billing_model "+
			"for this runtime, dev escapes, negative balance, failed gifts) instead of the posture grading; no Setup")
	cmd.Flags().BoolVar(&m.live, "live", false,
		"(--money) read-only requests: auth's subscription for this service, billing's tier for --org")
	cmd.Flags().StringVar(&m.org, "org", "", "(--money --live) an org that pays: check the tier it resolves to")
	cmd.Flags().StringVar(&m.expectTier, "expect-tier", "", "(--money) the tier --org should resolve to")
	cmd.Flags().StringVar(&m.environment, "environment", "",
		"(--money) prod | staging | dev (default $AB0T_ENVIRONMENT, then $ENVIRONMENT)")
}

// gatherMoneyFacts collects what the CLI can see (see the file comment).
func gatherMoneyFacts(ctx context.Context, cfg *config.Config, m moneyFlags) quota.MoneyFacts {
	f := quota.MoneyFacts{Runtime: "go", Environment: m.environment}
	if f.Environment == "" {
		f.Environment = os.Getenv("AB0T_ENVIRONMENT")
	}
	if f.Environment == "" {
		f.Environment = os.Getenv("ENVIRONMENT")
	}
	mu := mesh.Resolve()
	switch {
	case cfg.Billing.EnablePaid:
		f.Paid, f.PaidDetail = "on", "billing.enable_paid=true in the config"
	case mu.Billing != "":
		f.Paid, f.PaidDetail = "on", mesh.EnvBillingServiceURL+" is set (wiring billing asserts paid mode)"
		if mu.BillingToken == "" {
			f.PaidDetail += "; but no billing key (" + mesh.EnvBillingServiceAPIKey + ") — billing will refuse every call"
		}
	default:
		f.Paid, f.PaidDetail = "off", mesh.EnvBillingServiceURL+" is not set (https://billing.service.ab0t.com) and "+
			"billing.enable_paid is not true"
	}
	set := os.Getenv("AB0T_AUTH_WEBHOOK_SECRET") != ""
	f.WebhookSecretSet = &set
	if !m.live {
		return f
	}
	if events, found, err := authevents.LookupSubscription(ctx, authevents.SubscribeInput{}); err == nil {
		if found {
			f.SubscribedEvents = events
		} else {
			f.SubscribedEvents = []string{}
		}
	}
	if m.org != "" {
		lk := &quota.TierLookupFact{Org: m.org, ExpectTier: m.expectTier}
		switch cfg.TierProvider.Type {
		case "mesh":
			bc, err := billing.New(mu)
			if err != nil {
				lk.Outcome, lk.Detail = "error", err.Error()
				break
			}
			tier, err := bc.GetOrgTier(ctx, m.org)
			var he *httpx.Error
			switch {
			case err == nil:
				lk.Outcome, lk.Tier = "ok", tier
			case httpx.IsStatus(err, 401) || httpx.IsStatus(err, 403):
				lk.Outcome = "forbidden"
			case errors.As(err, &he):
				lk.Outcome, lk.Detail = "error", fmt.Sprintf("HTTP %d", he.Status)
			default:
				lk.Outcome, lk.Detail = "unreachable", err.Error()
			}
		case "static":
			tier, err := providers.NewStaticProvider(cfg.TierProvider).GetTier(ctx, "", m.org)
			if err != nil {
				lk.Outcome, lk.Detail = "error", err.Error()
			} else {
				lk.Outcome, lk.Tier = "ok", tier
			}
		default:
			lk = nil // jwt: the tier comes from each token, not the org
		}
		f.TierLookup = lk
	}
	return f
}

// runDoctorMoney prints the checklist and returns the exit code: 0, or 1 with --fail-on-risk and a
// risk; 2 for a config error.
func runDoctorMoney(ctx context.Context, cmd *cobra.Command, configPath string, jsonOut, failOnRisk bool, m moneyFlags) int {
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "CONFIG ERROR — nothing was contacted:", err)
		return exitConfig
	}
	facts := gatherMoneyFacts(ctx, cfg, m)
	findings := quota.MoneyChecklist(quota.ConfigDocument(cfg), facts)
	code := exitOK
	if failOnRisk {
		for _, f := range findings {
			if f.Grade == "risk" {
				code = exitGate
				break
			}
		}
	}
	out := cmd.OutOrStdout()
	if jsonOut {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"schema": "ab0t-quota/doctor-money/v1", "runtime": "go",
			"facts": facts, "findings": findings, "exit_code": code})
		return code
	}
	fmt.Fprint(out, quota.RenderMoneyChecklist(findings))
	fmt.Fprintf(out, "\nDOCTOR --money VERDICT: exit %d\n", code)
	return code
}
