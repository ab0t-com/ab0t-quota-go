package handlerledger

// TASK P5.1 remainder — DynamoDB ledger conformance against DynamoDB Local.
// Ticket 20260709_ab0t_quota_systemic_integrity_redesign (QG-01/QG-02).
//
// Runs the SAME runConformance suite InMemory + Redis pass, plus
// AutoSelect-returns-real-store and restart-idempotency. Uses a UNIQUE
// throwaway table it creates and deletes — it never touches existing data.
// Skips (does not fail) when no DynamoDB Local is reachable, so the suite
// stays green on boxes without it; run here it exercises the real backend.
//
// Endpoint: AB0T_QUOTA_TEST_DDB_ENDPOINT (default http://localhost:8000).

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

func ddbTestClient(t *testing.T) *dynamodb.Client {
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
		t.Skipf("DynamoDB Local not reachable at %s (%v) — skipping DDB conformance", endpoint, err)
	}
	return c
}

func makeDDBTable(t *testing.T, c *dynamodb.Client) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("ab0t_quota_handler_ledger_test_%d", time.Now().UnixNano())
	s := func(v string) *string { return &v }
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &table,
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: s("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("GSI1PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("GSI1SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("GSI2PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("GSI2SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: s("PK"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: s("SK"), KeyType: ddbtypes.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{
			{
				IndexName:  s("GSI1"),
				KeySchema:  []ddbtypes.KeySchemaElement{{AttributeName: s("GSI1PK"), KeyType: ddbtypes.KeyTypeHash}, {AttributeName: s("GSI1SK"), KeyType: ddbtypes.KeyTypeRange}},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
			{
				IndexName:  s("GSI2"),
				KeySchema:  []ddbtypes.KeySchemaElement{{AttributeName: s("GSI2PK"), KeyType: ddbtypes.KeyTypeHash}, {AttributeName: s("GSI2SK"), KeyType: ddbtypes.KeyTypeRange}},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			},
		},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() {
		_, _ = c.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table})
	})
	// DynamoDB Local activates synchronously, but wait defensively.
	waiter := dynamodb.NewTableExistsWaiter(c)
	if err := waiter.Wait(ctx, &dynamodb.DescribeTableInput{TableName: &table}, 10*time.Second); err != nil {
		t.Fatalf("table not active: %v", err)
	}
	return table
}

func TestConformance_DDB(t *testing.T) {
	c := ddbTestClient(t)
	table := makeDDBTable(t, c)
	runConformance(t, "DDB", func() LedgerStore { return &ddbLedgerStore{c: c, table: table} })
}

func TestDDBLedger_AutoSelectReturnsRealStore(t *testing.T) {
	c := ddbTestClient(t)
	store := AutoSelectStore(AutoSelectOptions{DDBClient: c, DDBTable: "ab0t_quota_handler_ledger"})
	if _, isMemory := store.(*InMemoryLedgerStore); isMemory {
		t.Fatal("QG-02: a real DDB client must yield the durable ddb ledger, not *InMemoryLedgerStore")
	}
	if _, isDDB := store.(*ddbLedgerStore); !isDDB {
		t.Fatalf("expected *ddbLedgerStore, got %T", store)
	}
}

func TestDDBLedger_TerminalSurvivesReconnect_QG01(t *testing.T) {
	c := ddbTestClient(t)
	table := makeDDBTable(t, c)
	ctx := context.Background()
	s1 := &ddbLedgerStore{c: c, table: table}
	if res, err := s1.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "e1", UserID: "u1"}); err != nil || !res.Proceed {
		t.Fatalf("first attempt: res=%+v err=%v", res, err)
	}
	if err := s1.RecordOutcome(ctx, OutcomeInput{HandlerName: "h", EventID: "e1", Status: StatusSuccess}); err != nil {
		t.Fatal(err)
	}
	// Fresh store handle (new "process") against the same durable table.
	s2 := &ddbLedgerStore{c: c, table: table}
	res, err := s2.RecordAttempt(ctx, AttemptInput{HandlerName: "h", EventID: "e1", UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Proceed {
		t.Error("QG-01: a completed event re-attempted must NOT proceed — idempotency not durable")
	}
	if res.CachedRow == nil || res.CachedRow.Status != StatusSuccess {
		t.Errorf("expected cached terminal row, got %+v", res.CachedRow)
	}
}
