// Package ddbguard is the DynamoDB preflight (D-76). Redis is checked five ways; DDB was
// checked ZERO — and the DDB holds the activation ledger (authoritative for identity + cost,
// D-33) and the billing outbox (money events nothing can reconstruct). It is MORE
// load-bearing than Redis, and until now the library asserted nothing about it at all.
//
// The severities are NOT the same (a guard that cries wolf gets routed around — D-49's
// false-503 lesson):
//
//	table missing / not ACTIVE        → FATAL
//	a GSI not ACTIVE                  → FATAL (real DynamoDB backfills a GSI ASYNCHRONOUSLY;
//	                                    a query against a CREATING index silently MISSES rows —
//	                                    money events that exist and are never drained. DDB Local
//	                                    makes it immediate, so no DDB-Local test catches it.)
//	TTL enabled on an attribute we do NOT write → FATAL (DynamoDB may DELETE rows the library
//	                                    never marked — including OPEN activations)
//	TTL disabled                      → WARN (rows never reap: growth and cost; nothing is lost)
//	PITR disabled                     → FATAL (a money store with no point-in-time recovery)
//	PITR unanswerable                 → FATAL unless the operator asserts ddb_pitr_confirmed.
//	                                    DynamoDB Local CANNOT answer DescribeContinuousBackups
//	                                    (UnknownOperationException) — PITR is the one thing ONLY
//	                                    real AWS can confirm, so it lands on D-32's shape: an
//	                                    absent signal needs an assertion ON THE RECORD.
//
// D-75 applies here too: these are re-verified on the reconciler's interval, and a
// safe→unsafe transition at runtime is LOUD, NOT FATAL (degrade + alert), never a crash.
package ddbguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// PITRConfirmEnv mirrors storage.ddb_pitr_confirmed (config wins).
const PITRConfirmEnv = "AB0T_QUOTA_DDB_PITR_CONFIRMED"

// ErrDDBPreflight is the typed startup refusal (D-76).
var ErrDDBPreflight = errors.New("quota: unsafe DynamoDB table")

// Describer is the narrow control-plane surface the preflight needs — satisfied by
// *dynamodb.Client. Narrow so tests drive the REAL checks with canned answers.
type Describer interface {
	DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	DescribeTimeToLive(ctx context.Context, in *dynamodb.DescribeTimeToLiveInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
	DescribeContinuousBackups(ctx context.Context, in *dynamodb.DescribeContinuousBackupsInput, optFns ...func(*dynamodb.Options)) (*dynamodb.DescribeContinuousBackupsOutput, error)
}

// EnvPITRConfirmed reports the env form of the operator's on-the-record assertion.
func EnvPITRConfirmed() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(PITRConfirmEnv))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// PreflightError builds the loud, typed refusal — cause and remedy.
func PreflightError(store, detail string) error {
	return fmt.Errorf("%w: ab0t-quota cannot use its %s DynamoDB table: %s. This table holds state "+
		"the library cannot reconstruct (the activation ledger is authoritative for identity and cost; "+
		"the outbox holds money events). Remedy: fix the table configuration named above, or — for "+
		"point-in-time recovery on a control plane that cannot report it (DynamoDB Local, some "+
		"emulators) — put the assertion on the record with storage.ddb_pitr_confirmed: true (env: %s=true)",
		ErrDDBPreflight, store, detail, PITRConfirmEnv)
}

// VerifyTable verifies one table. Never returns an error for a FINDING — it returns
// (capabilityValue, fatal, warn) and lets the caller choose the consequence: REFUSE at boot,
// DEGRADE + alert at runtime (D-75).
func VerifyTable(ctx context.Context, c Describer, table, ttlAttribute string, pitrConfirmed bool) (string, string, string) {
	// --- table + GSIs ---
	out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &table})
	if err != nil || out == nil || out.Table == nil {
		detail := fmt.Sprintf("table %q not found or not describable", table)
		if err != nil {
			detail += " (" + err.Error() + ")"
		}
		return "UNSAFE (" + detail + ")", detail, ""
	}
	if out.Table.TableStatus != types.TableStatusActive {
		detail := fmt.Sprintf("table %q is %s, not ACTIVE", table, out.Table.TableStatus)
		return "UNSAFE (" + detail + ")", detail, ""
	}
	for _, gsi := range out.Table.GlobalSecondaryIndexes {
		if gsi.IndexStatus != types.IndexStatusActive {
			name := ""
			if gsi.IndexName != nil {
				name = *gsi.IndexName
			}
			detail := fmt.Sprintf("GSI %q is %s, not ACTIVE — queries against a backfilling index "+
				"silently MISS rows (D-32)", name, gsi.IndexStatus)
			return "UNSAFE (" + detail + ")", detail, ""
		}
	}

	// --- TTL ---
	warn := ""
	ttlNote := "ttl=?"
	ttlOut, terr := c.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: &table})
	if terr != nil || ttlOut == nil || ttlOut.TimeToLiveDescription == nil {
		ttlNote = "ttl=unverified"
		warn = fmt.Sprintf("TTL could not be verified on %q", table)
	} else {
		d := ttlOut.TimeToLiveDescription
		switch d.TimeToLiveStatus {
		case types.TimeToLiveStatusEnabled, types.TimeToLiveStatusEnabling:
			attr := ""
			if d.AttributeName != nil {
				attr = *d.AttributeName
			}
			if attr != ttlAttribute {
				detail := fmt.Sprintf("TTL is enabled on attribute %q, but this library writes its "+
					"expiry to %q — DynamoDB may DELETE rows the library never marked for expiry "+
					"(including OPEN activations)", attr, ttlAttribute)
				return "UNSAFE (" + detail + ")", detail, ""
			}
			ttlNote = "ttl=" + attr
		default:
			ttlNote = "ttl=DISABLED"
			warn = fmt.Sprintf("TTL is DISABLED on %q: rows the library marks with %q will never reap "+
				"(unbounded growth and cost — nothing is lost). Enable TTL on %q.",
				table, ttlAttribute, ttlAttribute)
		}
	}

	// --- PITR (a money store) ---
	cb, cerr := c.DescribeContinuousBackups(ctx, &dynamodb.DescribeContinuousBackupsInput{TableName: &table})
	if cerr != nil || cb == nil || cb.ContinuousBackupsDescription == nil ||
		cb.ContinuousBackupsDescription.PointInTimeRecoveryDescription == nil {
		// DynamoDB Local answers UnknownOperationException: PITR is the ONE thing only real AWS
		// can confirm. Absent signal ⇒ operator assertion, on the record (D-32's shape).
		if pitrConfirmed {
			return fmt.Sprintf("ACTIVE (%s, pitr=asserted by operator — control plane cannot report it)",
				ttlNote), "", warn
		}
		detail := fmt.Sprintf("point-in-time recovery could not be verified on %q and "+
			"storage.ddb_pitr_confirmed is not set — an unverified backup posture on a money store is "+
			"not a safe one", table)
		return "UNSAFE (" + detail + ")", detail, warn
	}
	pitr := cb.ContinuousBackupsDescription.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus
	if pitr != types.PointInTimeRecoveryStatusEnabled {
		if pitrConfirmed {
			return fmt.Sprintf("ACTIVE (%s, pitr=%s — WAIVED by operator)", ttlNote, pitr), "", warn
		}
		detail := fmt.Sprintf("PITR (point-in-time recovery) is %s on %q — this table holds "+
			"money/ledger state that cannot be reconstructed", pitr, table)
		return "UNSAFE (" + detail + ")", detail, warn
	}
	return fmt.Sprintf("ACTIVE (%s, pitr=ENABLED)", ttlNote), "", warn
}

// TableOK is the health predicate: only an affirmative ACTIVE is healthy. Missing, empty, or
// UNSAFE is not (D-49/D-51 — absence is not a value).
func TableOK(v string) bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(v)), "ACTIVE")
}
