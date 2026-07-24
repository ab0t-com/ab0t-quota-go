// doctor — T-1/T-4 (program board tooling lane; ticket
// 20260721_setup_and_doctor_verbs; ST-CLI-1). `capabilities` answers "will
// this boot"; `doctor` grades POSTURE — the class the boot gates deliberately
// let through (persistence behind an assertion, PITR asserted, already-evicted
// keys) — and is HONEST about what it cannot see: a dimension it could not
// observe is `not_checked` WITH the reason, never inferred.
//
// THE HONEST ASYMMETRY, stated rather than papered over (D-8): this runtime's
// doctor runs full quota.Setup (like `capabilities`), which connects to the
// declared stores, MAY CREATE the library's declared tables if absent, and
// loads the counter script. It therefore never claims read-only. The Python
// doctor's only server-visible write is a SCRIPT LOAD; each runtime states
// its own side effects (ST-CLI-1).
//
// Exit taxonomy (shared with the Python CLI, pinned by ST-CLI-1):
//   0 ok · 1 gate refusal · 2 config error · 3 unreachable/credentials ·
//   4 internal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ab0t-com/ab0t-quota-go/config"
	"github.com/ab0t-com/ab0t-quota-go/quota"
	"github.com/ab0t-com/ab0t-quota-go/redisguard"
)

const (
	exitOK       = 0
	exitGate     = 1
	exitConfig   = 2
	exitReach    = 3
	exitInternal = 4

	reportSchema  = "ab0t-quota/preflight-report/v1"
	postureSchema = "ab0t-quota/doctor-posture/v1"
)

type postureFinding struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Grade   string `json:"grade"` // good | attention | risk | not_checked | info
	Detail  string `json:"detail"`
	Remedy  string `json:"remedy,omitempty"`
	Checked bool   `json:"checked"`
}

type gateLine struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type doctorJSON struct {
	Schema  string `json:"schema"`
	Library struct {
		Runtime string `json:"runtime"`
		Version string `json:"version"`
	} `json:"library"`
	Config struct {
		Path       string `json:"path"`
		EngineMode string `json:"engine_mode"`
	} `json:"config"`
	ResolvedPlan []map[string]any   `json:"resolved_plan"`
	Gates        []gateLine         `json:"gates"`
	Capabilities quota.Capabilities `json:"capabilities"`
	Posture      struct {
		Schema      string           `json:"schema"`
		SideEffects []string         `json:"side_effects"`
		Findings    []postureFinding `json:"findings"`
		NotChecked  []map[string]any `json:"not_checked"`
		Verdict     struct {
			ExitCode int `json:"exit_code"`
		} `json:"verdict"`
	} `json:"posture"`
	Verdict struct {
		Boot     string `json:"boot"`
		ExitCode int    `json:"exit_code"`
	} `json:"verdict"`
}

var doctorSideEffects = []string{
	"doctor ran full quota.Setup (NOT read-only): it connected to the declared " +
		"stores, may have CREATED the library's declared tables if absent, and " +
		"loaded the counter script into Redis's script cache (D-8 — this runtime " +
		"never claims read-only; the Python doctor's only write is the SCRIPT LOAD)",
	"no background loops were started: no billing heartbeats, no outbox drain " +
		"(nothing publishes or settles money)",
	"doctor never contacts the mesh beyond what Setup itself performs",
}

func nc(id, name, reason string) postureFinding {
	return postureFinding{ID: id, Name: name, Grade: "not_checked",
		Detail: reason, Checked: false}
}

// gatesFromCapabilities derives the gate ledger from the capability verdicts
// Setup published — the values ARE the cross-runtime capability contract
// (ST-PREFLIGHT-1); Setup succeeding means no refuse-severity gate failed.
func gatesFromCapabilities(c quota.Capabilities) []gateLine {
	status := func(v string, badPrefixes ...string) string {
		lv := strings.ToLower(v)
		for _, p := range badPrefixes {
			if strings.HasPrefix(lv, strings.ToLower(p)) {
				return "warn"
			}
		}
		if strings.HasPrefix(lv, "unknown") {
			return "warn"
		}
		return "pass"
	}
	return []gateLine{
		{"D-71", "redis_topology", status(c.RedisTopology, "cluster"), c.RedisTopology},
		{"D-72", "counter_eviction_policy", status(c.CounterEvictionPolicy, "EVICTING"), c.CounterEvictionPolicy},
		{"D-73", "redis_scripting", status(c.RedisScripting, "OFF"), c.RedisScripting},
		{"D-74", "redis_version", status(c.RedisVersion, "below_floor"), c.RedisVersion},
		{"D-80", "counter_evictions_observed", status(c.CounterEvictionsObserved, "evictions_observed"), c.CounterEvictionsObserved},
		{"D-81", "redis_persist_status", status(c.RedisPersistStatus, "FAILING"), c.RedisPersistStatus},
		{"D-77", "memory_headroom", status(c.MemoryHeadroom, "low_headroom", "unbounded"), c.MemoryHeadroom},
	}
}

// gradePosture grades what THIS runtime can see from the Setup snapshot, and
// says plainly what it cannot.
func gradePosture(cfg *config.Config, c quota.Capabilities) []postureFinding {
	var out []postureFinding

	// Persistence — the ticket's headline: a Redis with no persistence boots
	// fine and loses the outbox on restart.
	persist := c.RedisPersistStatus
	outboxOnRedis := strings.Contains(c.Outbox, "Redis")
	switch {
	case strings.HasPrefix(persist, "FAILING"):
		out = append(out, postureFinding{"P-PERSIST", "redis_persistence", "risk",
			"persistence configured but FAILING: " + persist,
			"free the disk / fix the volume; verify aof_last_write_status=ok", true})
	case strings.Contains(c.Outbox, "NON-DURABLE"):
		out = append(out, postureFinding{"P-PERSIST", "redis_persistence", "risk",
			"the outbox resolves to a NON-durable store: it boots and loses money " +
				"events on restart (" + c.Outbox + ")",
			"enable Redis persistence or move the outbox to DynamoDB", true})
	case strings.HasPrefix(persist, "unknown") || persist == "":
		out = append(out, nc("P-PERSIST", "redis_persistence",
			"persistence facts unavailable (INFO persistence unreadable or no "+
				"Redis counter store) — posture unknown"))
	case outboxOnRedis:
		out = append(out, postureFinding{"P-PERSIST", "redis_persistence", "attention",
			"outbox rides this Redis (" + c.Outbox + "); persistence facts: " + persist +
				". If durability rests on an operator assertion, it is a promise, not an observation.",
			"prefer the DynamoDB outbox for money events", true})
	default:
		out = append(out, postureFinding{"P-PERSIST", "redis_persistence", "good",
			"persist facts: " + persist + "; outbox: " + c.Outbox, "", true})
	}

	// D-80 facts — already-evicted keys = a counter that is wrong NOW.
	ev := c.CounterEvictionsObserved
	switch {
	case strings.HasPrefix(ev, "evictions_observed"):
		out = append(out, postureFinding{"P-EVICT", "eviction_facts", "risk",
			"this Redis has ALREADY evicted keys — the counter may be WRONG RIGHT " +
				"NOW (an evicted gauge reads low: phantom headroom, over-admission). " + ev,
			"reconcile the counter and set maxmemory-policy noeviction", true})
	case strings.HasPrefix(ev, "unknown") || ev == "":
		out = append(out, nc("P-EVICT", "eviction_facts",
			"INFO stats unavailable — cannot tell whether this server has already "+
				"evicted (a visibility answer, not a verdict)"))
	default:
		out = append(out, postureFinding{"P-EVICT", "eviction_facts", "good", ev, "", true})
	}

	// PITR / TTL — graded from what Setup verified; stated where it did not.
	hl := c.DDBHandlerLedger
	switch {
	case strings.Contains(hl, "pitr=asserted"):
		out = append(out, postureFinding{"P-PITR", "ddb_pitr", "attention",
			"handler-ledger PITR rests on the operator assertion — an assertion is " +
				"a promise, not a backup (" + hl + ")",
			"on real AWS, verify: aws dynamodb describe-continuous-backups", true})
	case strings.Contains(hl, "pitr=ENABLED"):
		out = append(out, postureFinding{"P-PITR", "ddb_pitr", "good", hl, "", true})
	default:
		out = append(out, nc("P-PITR", "ddb_pitr",
			"table backup posture beyond the handler ledger is not separately "+
				"verified by this runtime's doctor — stated, not implied. Verify "+
				"PITR per table out-of-band (provision --emit terraform enables it)"))
	}

	// ACL / IAM breadth — NOT probed in this runtime version. Say so.
	out = append(out, nc("P-ACL", "redis_acl_breadth",
		"NOT probed in the Go runtime version — compare the server ACL "+
			"out-of-band with 'quotactl provision --emit acl'. A NOPERM on any "+
			"probe above is a permission answer, never a breadth verdict (D-8)"))
	out = append(out, nc("P-IAM", "iam_breadth",
		"NOT verified: IAM policy breadth requires iam:Get*/SimulatePrincipalPolicy, "+
			"which the runtime credential need not hold. Compare out-of-band with "+
			"'quotactl provision --emit iam'. AccessDenied is a permission answer, "+
			"never missing infrastructure (D-8)"))

	// Encryption in transit — from the redacted resolved plan (offline fact).
	if row, ok := c.Resolved["storage.redis_url"]; ok && row.Value != "" {
		switch {
		case strings.HasPrefix(row.Value, "rediss://"):
			out = append(out, postureFinding{"P-TLS", "encryption_in_transit", "good",
				"Redis over TLS (rediss://)", "", true})
		case strings.HasPrefix(row.Value, "redis://"):
			out = append(out, postureFinding{"P-TLS", "encryption_in_transit", "attention",
				"Redis over PLAINTEXT (redis://). Whether that is acceptable depends " +
					"on the network boundary, which doctor cannot see; most compliance " +
					"regimes require TLS in transit.",
				"use rediss:// (supported unchanged)", true})
		default:
			out = append(out, nc("P-TLS", "encryption_in_transit",
				"no Redis URL in the resolved plan ("+row.Value+")"))
		}
	} else {
		out = append(out, nc("P-TLS", "encryption_in_transit",
			"no resolved redis_url row — nothing to grade"))
	}
	out = append(out, nc("P-REST", "encryption_at_rest",
		"NOT verified: Redis at-rest encryption is not observable over the "+
			"protocol; DynamoDB SSE was not probed in this version — check "+
			"SSEDescription via DescribeTable for the audit record"))

	// Memory headroom (D-77) — the cliff before 3am.
	if strings.HasPrefix(c.MemoryHeadroom, "low_headroom") {
		out = append(out, postureFinding{"P-MEM", "memory_headroom", "attention",
			c.MemoryHeadroom, "raise maxmemory or reduce load", true})
	}
	return out
}

func newDoctorCmd() *cobra.Command {
	var configPath string
	var jsonOut, failOnRisk bool
	var timeoutSec float64
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Grade production POSTURE over the boot evaluators; honest about what it cannot check",
		Long: `Grades persistence, eviction FACTS, PITR-by-assertion, encryption in
transit, and names what it could NOT check (ACL/IAM breadth in this runtime).
Exit mirrors the boot verdict: 0 ok / 1 gate refusal / 2 config /
3 unreachable / 4 internal; --fail-on-risk turns RISK findings into exit 1.

Honest side-effect statement (D-8 — this run is NOT read-only): doctor runs
full quota.Setup, which connects to the declared stores, MAY CREATE the
library's declared tables if absent, and loads the counter script. The Python
doctor's only write is a SCRIPT LOAD; each runtime states its own.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(context.Background(),
				time.Duration(timeoutSec*float64(time.Second)))
			defer cancel()
			cfg, err := config.LoadConfig(configPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, "CONFIG ERROR — nothing was contacted:", err)
				os.Exit(exitConfig)
			}
			q, err := quota.Setup(ctx, quota.Options{ConfigOverride: cfg, SkipBackgroundLoops: true})
			if err != nil {
				if errors.Is(err, redisguard.ErrRedisUnreachable) {
					fmt.Fprintln(os.Stderr, "REACHABILITY (exit 3, never a gate verdict):", err)
					os.Exit(exitReach)
				}
				fmt.Fprintln(os.Stderr, "BOOT GATE REFUSAL (exit 1):", err)
				os.Exit(exitGate)
			}
			defer q.Close(context.Background())
			caps := q.Capabilities()
			findings := gradePosture(cfg, caps)
			code := exitOK
			if failOnRisk {
				for _, f := range findings {
					if f.Grade == "risk" {
						code = exitGate
						break
					}
				}
			}
			if jsonOut {
				doc := doctorJSON{Schema: reportSchema}
				doc.Library.Runtime = "go"
				doc.Library.Version = version
				doc.Config.Path = configPath
				doc.Config.EngineMode = cfg.EngineMode
				for name, row := range caps.Resolved {
					doc.ResolvedPlan = append(doc.ResolvedPlan, map[string]any{
						"name": name, "value": row.Value, "source": row.Source})
				}
				doc.Gates = gatesFromCapabilities(caps)
				doc.Capabilities = caps
				doc.Posture.Schema = postureSchema
				doc.Posture.SideEffects = doctorSideEffects
				doc.Posture.Findings = findings
				for _, f := range findings {
					if f.Grade == "not_checked" {
						doc.Posture.NotChecked = append(doc.Posture.NotChecked,
							map[string]any{"name": f.Name, "reason": f.Detail})
					}
				}
				doc.Posture.Verdict.ExitCode = code
				doc.Verdict.Boot = "would_start"
				doc.Verdict.ExitCode = code
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				if err := enc.Encode(doc); err != nil {
					return err
				}
			} else {
				fmt.Println("DOCTOR POSTURE — grades are remediation advice; the exit code is the boot verdict")
				for _, f := range findings {
					tag := strings.ToUpper(f.Grade)
					fmt.Printf("  %-12s %-24s %s\n", tag, f.Name, f.Detail)
					if f.Remedy != "" {
						fmt.Printf("               remedy: %s\n", f.Remedy)
					}
				}
				fmt.Println("SIDE EFFECTS, stated:")
				for _, s := range doctorSideEffects {
					fmt.Println("  *", s)
				}
				fmt.Printf("\nDOCTOR VERDICT: exit %d\n", code)
			}
			if code != exitOK {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "config file path (defaults to env/search)")
	cmd.Flags().BoolVar(&jsonOut, "json", false,
		"emit preflight-report/v1 EXTENDED with a doctor-posture/v1 section")
	cmd.Flags().BoolVar(&failOnRisk, "fail-on-risk", false,
		"RISK posture findings fail the exit code (advice by default)")
	cmd.Flags().Float64Var(&timeoutSec, "timeout", 15, "overall Setup timeout seconds")
	return cmd
}
