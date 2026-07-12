package activations

// D-39 — the durable DDB activation store, verified against DynamoDB Local.
// Uses a UNIQUE throwaway table it creates + deletes (never touches existing
// data). Skips (does not fail) when DDB Local is unreachable.
//
// The load-bearing property (and its own negative control): MarkReleased is
// IDEMPOTENT — it returns the row the FIRST time and nil the SECOND. If the
// conditional-write transition were broken, the second call would return a row
// and the engine would double-decrement the gauge (QI-04). The assertion below
// is the teeth: a non-idempotent store fails it.

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

func ddbActTestClient(t *testing.T) *dynamodb.Client {
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

func makeActTable(t *testing.T, c *dynamodb.Client) string {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("ab0t_quota_activations_test_%d", time.Now().UnixNano())
	s := func(v string) *string { return &v }
	_, err := c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName:   &table,
		BillingMode: ddbtypes.BillingModePayPerRequest,
		AttributeDefinitions: []ddbtypes.AttributeDefinition{
			{AttributeName: s("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("GSI1PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			{AttributeName: s("GSI1SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
		},
		KeySchema: []ddbtypes.KeySchemaElement{
			{AttributeName: s("PK"), KeyType: ddbtypes.KeyTypeHash},
			{AttributeName: s("SK"), KeyType: ddbtypes.KeyTypeRange},
		},
		GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{{
			IndexName:  s("GSI1"),
			KeySchema:  []ddbtypes.KeySchemaElement{{AttributeName: s("GSI1PK"), KeyType: ddbtypes.KeyTypeHash}, {AttributeName: s("GSI1SK"), KeyType: ddbtypes.KeyTypeRange}},
			Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
		}},
	})
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	t.Cleanup(func() { _, _ = c.DeleteTable(context.Background(), &dynamodb.DeleteTableInput{TableName: &table}) })
	if err := dynamodb.NewTableExistsWaiter(c).Wait(ctx, &dynamodb.DescribeTableInput{TableName: &table}, 10*time.Second); err != nil {
		t.Fatalf("table not active: %v", err)
	}
	return table
}

func TestDDBActivationStore_Contract_D39(t *testing.T) {
	c := ddbActTestClient(t)
	table := makeActTable(t, c)
	store := NewDDBStore(c, table, 0)
	ctx := context.Background()
	uid := "u1"

	act := Activation{ActivationID: "act_ddb1", OrgID: "o1", UserID: &uid, ResourceKey: "sandboxes",
		State: StateOpen, Spend: map[string]float64{"sandboxes": 1}, OpenedAt: nowISO()}
	if err := store.PutOpen(ctx, act); err != nil {
		t.Fatal(err)
	}
	// Durable + queryable open set.
	if n, _ := store.CountOpen(ctx, "o1"); n != 1 {
		t.Fatalf("count_open = %d (want 1)", n)
	}
	opens, _ := store.ListOpen(ctx, "o1", 100)
	if len(opens) != 1 || opens[0].ActivationID != "act_ddb1" {
		t.Fatalf("list_open wrong: %+v", opens)
	}
	// PutOpen is idempotent (first writer wins).
	act2 := act
	act2.Spend = map[string]float64{"sandboxes": 999}
	_ = store.PutOpen(ctx, act2)
	got, _ := store.Get(ctx, "act_ddb1")
	if got.Spend["sandboxes"] != 1 {
		t.Errorf("PutOpen not first-writer-wins: spend=%v", got.Spend)
	}

	// THE property + negative control: MarkReleased idempotent.
	r1, _ := store.MarkReleased(ctx, "act_ddb1")
	r2, _ := store.MarkReleased(ctx, "act_ddb1")
	if r1 == nil {
		t.Error("first MarkReleased must return the row")
	}
	if r2 != nil {
		t.Error("D-39/QI-04: second MarkReleased returned a row — NOT idempotent; the engine would double-decrement")
	}
	// Released row drops out of the open set.
	if n, _ := store.CountOpen(ctx, "o1"); n != 0 {
		t.Errorf("released activation still in open set (count=%d)", n)
	}

	// Settle after release is idempotent too.
	s1, _ := store.MarkSettled(ctx, "act_ddb1", "0.30")
	s2, _ := store.MarkSettled(ctx, "act_ddb1", "0.30")
	if s1 == nil || s2 != nil {
		t.Errorf("MarkSettled not idempotent: s1=%v s2=%v", s1, s2)
	}
}

// Durable() self-report (used by the reconciler durability gate — D-39, wired
// in a later leg).
func TestDDBActivationStore_ReportsDurable_D39(t *testing.T) {
	if !NewDDBStore(nil, "", 0).Durable() {
		t.Error("DDB activation store must report Durable()=true")
	}
}
