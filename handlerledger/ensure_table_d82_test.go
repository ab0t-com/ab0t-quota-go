package handlerledger

// D-82 — the handler ledger must PROVISION its table, like the outbox and the activation
// store already do. It used to ASSUME the table existed: a client who wired it hit a
// ResourceNotFoundException at their FIRST auth webhook, in production.
//
// It stayed invisible for the most instructive reason (D-78): the only thing that had ever
// exercised this store was a fake — and *a fake never notices, because a fake creates nothing.*
// So this test runs against a REAL DynamoDB control plane.

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

func TestEnsureTable_CreatesAndProvisions_D82(t *testing.T) {
	endpoint := os.Getenv("AB0T_QUOTA_TEST_DDB_ENDPOINT")
	if endpoint == "" {
		t.Skip("AB0T_QUOTA_TEST_DDB_ENDPOINT not set — the real DynamoDB leg is operator-gated")
	}
	ctx := context.Background()
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("x", "x", "")))
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	c := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })

	table := "d82_go_handler_ledger"
	_, _ = c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})
	defer c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})

	store, err := newDDBLedgerStore(c, table)
	if err != nil {
		t.Fatalf("newDDBLedgerStore: %v", err)
	}
	prov, ok := store.(interface {
		EnsureTable(context.Context, time.Duration) error
		Table() string
	})
	if !ok {
		t.Fatal("the DDB ledger store must expose EnsureTable (D-82) — it used to ASSUME its table existed")
	}
	if err := prov.EnsureTable(ctx, 30*time.Second); err != nil {
		t.Fatalf("EnsureTable must CREATE the table: %v", err)
	}

	out, err := c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(table)})
	if err != nil || out.Table.TableStatus != "ACTIVE" {
		t.Fatalf("the table must exist and be ACTIVE: %v", err)
	}

	// It must WORK against the table it just made — the conditional write the whole idempotency
	// guarantee rests on, asserted against REAL DynamoDB rather than a fake that accepts anything.
	in := AttemptInput{HandlerName: "h", EventID: "e-d82", EventType: "x",
		EventPayload: json.RawMessage(`{}`), UserID: "u1", OrgID: "o1"}
	first, err := store.RecordAttempt(ctx, in)
	if err != nil {
		t.Fatalf("first attempt: %v", err)
	}
	if !first.Proceed {
		t.Fatal("the first attempt must proceed")
	}
	second, err := store.RecordAttempt(ctx, in)
	if err != nil {
		t.Fatalf("second attempt: %v", err)
	}
	if second.Proceed {
		t.Fatal("REAL DynamoDB must SHORT-CIRCUIT the duplicate — the conditional write is the guarantee")
	}

	// Idempotent: a second EnsureTable on an existing table is a no-op, not a crash.
	if err := prov.EnsureTable(ctx, 30*time.Second); err != nil {
		t.Fatalf("EnsureTable must be idempotent: %v", err)
	}
}
