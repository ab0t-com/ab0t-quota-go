package handlerledger

// Ticket 20261016 review B-18 (lane B, re-verified at dynamodb_store.go:294-305):
// EnsureTable created the ledger table with PK/SK only, while QueryByUser,
// QueryByStatus and DeleteUser query GSI1/GSI2 — so on a table the library made
// itself every per-user query and every GDPR DeleteUser failed with
// ValidationException ("The table does not have the specified index"). The
// Python runtime fixed the same class in its own ledger. RED on v0.1.9 against a
// real DynamoDB control plane (operator-gated, like D-82).

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func TestEnsureTable_ProvisionsTheIndexesItsQueriesUse_20261016(t *testing.T) {
	endpoint := os.Getenv("AB0T_QUOTA_TEST_DDB_ENDPOINT")
	if endpoint == "" {
		t.Skip("AB0T_QUOTA_TEST_DDB_ENDPOINT not set — the real DynamoDB leg is operator-gated")
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("x", "x", "")))
	if err != nil {
		t.Fatal(err)
	}
	c := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
	table := "gsi_20261016_go_handler_ledger"
	_, _ = c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	defer c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})

	store, err := newDDBLedgerStore(c, table)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.(interface {
		EnsureTable(context.Context, time.Duration) error
	}).EnsureTable(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	in := AttemptInput{HandlerName: "h", EventID: "e1", EventType: "org.created",
		EventPayload: json.RawMessage(`{}`), UserID: "u-gdpr", OrgID: "o1"}
	if _, err := store.RecordAttempt(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordOutcome(ctx, OutcomeInput{HandlerName: "h", EventID: "e1", Status: StatusFailedPermanent}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.QueryByUser(ctx, "u-gdpr", QueryOptions{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("QueryByUser on a self-provisioned table: rows=%d err=%v", len(rows), err)
	}
	if rows, err := store.QueryByStatus(ctx, StatusFailedPermanent, QueryOptions{}); err != nil || len(rows) != 1 {
		t.Fatalf("QueryByStatus: rows=%d err=%v", len(rows), err)
	}
	n, err := store.DeleteUser(ctx, "u-gdpr")
	if err != nil || n != 1 {
		t.Fatalf("DeleteUser (GDPR) on a self-provisioned table: n=%d err=%v", n, err)
	}
}
