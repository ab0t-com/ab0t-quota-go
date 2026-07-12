package outbox

// D-30/D-32 — the DDB outbox store is the DEFAULT durable store; verify it
// against DynamoDB Local (throwaway table, created + deleted; never touches
// existing data). Skips when DDB Local is unreachable.
//
// The load-bearing property + negative control: a DDB-backed emitter survives
// a "restart" (fresh store object) and resumes delivery from the durable table
// — and EnsureTable waits for the status GSI to be ACTIVE before ListPending.

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
)

func ddbOutboxClient(t *testing.T) *dynamodb.Client {
	t.Helper()
	endpoint := os.Getenv("AB0T_QUOTA_TEST_DDB_ENDPOINT")
	if endpoint == "" {
		endpoint = "http://localhost:8000"
	}
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-west-2"),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Skipf("ddb config: %v", err)
	}
	c := dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) { o.BaseEndpoint = aws.String(endpoint) })
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := c.ListTables(pctx, &dynamodb.ListTablesInput{}); err != nil {
		t.Skipf("DynamoDB Local not reachable at %s (%v)", endpoint, err)
	}
	return c
}

func TestDDBOutbox_EnsureTableAndSurviveRestart_D30(t *testing.T) {
	c := ddbOutboxClient(t)
	ctx := context.Background()
	table := "ab0t_quota_outbox_test_" + time.Now().Format("150405.000000")
	store := NewDDBStore(c, table)
	if err := store.EnsureTable(ctx, 30*time.Second); err != nil {
		t.Fatalf("EnsureTable: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table}) })

	if !store.Durable() {
		t.Fatal("DDB outbox must report Durable()=true")
	}
	// "Process 1": publish fails → intent PENDING in DDB.
	e1 := NewEmitter(store, &flakyPublisher{failFirst: true}, 900, "")
	if d, _ := e1.EmitViaOutbox(ctx, "resv-ddb:stopped", Record{
		Key: "resv-ddb:stopped", Event: json.RawMessage(`{"r":"1"}`), EventType: "stopped",
		ResourceType: "sandbox", ReservationID: "resv-ddb", FirstTS: nowEpoch(),
	}); d {
		t.Fatal("publish should have failed")
	}

	// "Process 2": brand-new store object against the SAME table resumes.
	store2 := NewDDBStore(c, table)
	e2 := NewEmitter(store2, &flakyPublisher{failFirst: false}, 900, "")
	if n, _ := e2.PendingCount(ctx); n != 1 {
		t.Fatalf("fresh DDB store should see 1 pending intent, got %d", n)
	}
	n, err := e2.Drain(ctx, 100)
	if err != nil || n != 1 {
		t.Fatalf("fresh process must resume delivery from DDB: delivered=%d err=%v", n, err)
	}
	if p, _ := e2.PendingCount(ctx); p != 0 {
		t.Errorf("nothing should remain pending, got %d", p)
	}
}
