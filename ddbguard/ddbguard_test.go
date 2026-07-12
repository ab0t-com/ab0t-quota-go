package ddbguard

// D-76 — the DynamoDB preflight. Twin of Python's TestDDBPreflight. Redis is checked five
// ways; DDB was checked ZERO — and it holds the activation ledger AND the outbox.
//
// The fakes drive the REAL VerifyTable (a test of a copy of the logic asserts nothing about
// the logic). The DDB-Local leg lives in the Python lane's artifact: DynamoDB Local answers
// DescribeTable/DescribeTimeToLive with REAL semantics and CANNOT answer
// DescribeContinuousBackups — so PITR is precisely the thing only real AWS can confirm.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type fakeDDB struct {
	status          types.TableStatus
	gsi             []types.IndexStatus
	ttlStatus       types.TimeToLiveStatus
	ttlAttr         string
	pitr            types.PointInTimeRecoveryStatus
	pitrUnsupported bool
	missing         bool
}

func (f fakeDDB) DescribeTable(ctx context.Context, in *dynamodb.DescribeTableInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error) {
	if f.missing {
		return nil, errors.New("ResourceNotFoundException: Requested resource not found")
	}
	var gsis []types.GlobalSecondaryIndexDescription
	for i, s := range f.gsi {
		gsis = append(gsis, types.GlobalSecondaryIndexDescription{
			IndexName: aws.String(string(rune('a'+i)) + "-index"), IndexStatus: s})
	}
	return &dynamodb.DescribeTableOutput{Table: &types.TableDescription{
		TableStatus: f.status, GlobalSecondaryIndexes: gsis}}, nil
}

func (f fakeDDB) DescribeTimeToLive(ctx context.Context, in *dynamodb.DescribeTimeToLiveInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error) {
	d := &types.TimeToLiveDescription{TimeToLiveStatus: f.ttlStatus}
	if f.ttlAttr != "" {
		d.AttributeName = aws.String(f.ttlAttr)
	}
	return &dynamodb.DescribeTimeToLiveOutput{TimeToLiveDescription: d}, nil
}

func (f fakeDDB) DescribeContinuousBackups(ctx context.Context, in *dynamodb.DescribeContinuousBackupsInput, _ ...func(*dynamodb.Options)) (*dynamodb.DescribeContinuousBackupsOutput, error) {
	if f.pitrUnsupported {
		return nil, errors.New("UnknownOperationException: An unknown operation was requested")
	}
	return &dynamodb.DescribeContinuousBackupsOutput{
		ContinuousBackupsDescription: &types.ContinuousBackupsDescription{
			PointInTimeRecoveryDescription: &types.PointInTimeRecoveryDescription{
				PointInTimeRecoveryStatus: f.pitr}}}, nil
}

func good() fakeDDB {
	return fakeDDB{status: types.TableStatusActive, gsi: []types.IndexStatus{types.IndexStatusActive},
		ttlStatus: types.TimeToLiveStatusEnabled, ttlAttr: "ttl",
		pitr: types.PointInTimeRecoveryStatusEnabled}
}

func TestVerifyTable_D76(t *testing.T) {
	ctx := context.Background()

	// [control] a correct table passes — a guard that refuses everything says nothing.
	if v, fatal, warn := VerifyTable(ctx, good(), "tbl", "ttl", false); fatal != "" || warn != "" {
		t.Fatalf("a correct table must pass: v=%q fatal=%q warn=%q", v, fatal, warn)
	} else if !TableOK(v) {
		t.Fatalf("capability must read ACTIVE, got %q", v)
	}

	f := good()
	f.missing = true
	if _, fatal, _ := VerifyTable(ctx, f, "tbl", "ttl", false); !strings.Contains(fatal, "not found") {
		t.Errorf("a missing table must be FATAL, got %q", fatal)
	}

	// Real DynamoDB backfills a GSI asynchronously — a query against a CREATING index
	// silently MISSES rows (money events that exist and are never drained, D-32).
	f = good()
	f.gsi = []types.IndexStatus{types.IndexStatusCreating}
	if _, fatal, _ := VerifyTable(ctx, f, "tbl", "ttl", false); !strings.Contains(fatal, "GSI") {
		t.Errorf("a backfilling GSI must be FATAL, got %q", fatal)
	}

	// The dangerous one: TTL on an attribute we do not write ⇒ DynamoDB may DELETE rows the
	// library never marked — including OPEN activations.
	f = good()
	f.ttlAttr = "expires"
	if _, fatal, _ := VerifyTable(ctx, f, "tbl", "ttl", false); !strings.Contains(fatal, "expires") {
		t.Errorf("TTL on the WRONG attribute must be FATAL, got %q", fatal)
	}

	// A disabled TTL never deletes anything — rows simply pile up. WARN, never refuse
	// (refusing would be the D-49 false-503 mistake).
	f = good()
	f.ttlStatus = types.TimeToLiveStatusDisabled
	f.ttlAttr = ""
	if _, fatal, warn := VerifyTable(ctx, f, "tbl", "ttl", false); fatal != "" || warn == "" {
		t.Errorf("a DISABLED TTL must WARN, not refuse: fatal=%q warn=%q", fatal, warn)
	}

	// A money store with no point-in-time recovery.
	f = good()
	f.pitr = types.PointInTimeRecoveryStatusDisabled
	if _, fatal, _ := VerifyTable(ctx, f, "tbl", "ttl", false); !strings.Contains(fatal, "PITR") {
		t.Errorf("PITR disabled must be FATAL, got %q", fatal)
	}

	// DynamoDB Local cannot answer DescribeContinuousBackups at all ⇒ unverified ⇒ refuse,
	// unless the operator puts the assertion ON THE RECORD (D-32's shape).
	f = good()
	f.pitrUnsupported = true
	if _, fatal, _ := VerifyTable(ctx, f, "tbl", "ttl", false); !strings.Contains(fatal, "ddb_pitr_confirmed") {
		t.Errorf("an unverifiable PITR must refuse and name the assertion, got %q", fatal)
	}
	v, fatal, _ := VerifyTable(ctx, f, "tbl", "ttl", true)
	if fatal != "" || !strings.Contains(strings.ToLower(v), "assert") {
		t.Errorf("the operator assertion must allow start AND be visible: v=%q fatal=%q", v, fatal)
	}
}

func TestTableOK_AbsenceIsNotHealth_D76(t *testing.T) {
	for _, v := range []string{"", "UNSAFE (PITR is DISABLED)", "unknown"} {
		if TableOK(v) {
			t.Errorf("%q must NOT read healthy (D-49/D-51)", v)
		}
	}
	if !TableOK("ACTIVE (ttl=ttl, pitr=ENABLED)") {
		t.Error("a verified table must read healthy")
	}
}

func TestPreflightError_NamesCauseAndRemedy_D76(t *testing.T) {
	err := PreflightError("outbox", "PITR is DISABLED")
	if !errors.Is(err, ErrDDBPreflight) {
		t.Error("the refusal must be errors.Is-able")
	}
	for _, tok := range []string{"outbox", "PITR", "ddb_pitr_confirmed"} {
		if !strings.Contains(err.Error(), tok) {
			t.Errorf("the refusal must name %q", tok)
		}
	}
}

// ---------------------------------------------------------------------------
// D-78 — assert at the REAL sink. A double can prove what YOUR code does; it can never
// prove the OTHER side behaves as you modelled it.
// ---------------------------------------------------------------------------
//
// Everything above runs against `fakeDDB` — MY model of DynamoDB's control plane. That model
// is exactly the kind of thing that silently drifts from the system it imitates (my D-75
// alerts "worked" against a stub and reached nobody in production). So this leg runs the SAME
// checks against a REAL DynamoDB control plane (DynamoDB Local), and asserts the model agrees
// with it.
//
// HONEST BOUNDARY (do not mistake this for certification): DynamoDB Local answers
// DescribeTable and DescribeTimeToLive with real semantics, but CANNOT answer
// DescribeContinuousBackups (UnknownOperationException). So this leg certifies the
// table/GSI/TTL judgements against a real control plane, and certifies that PITR falls to the
// operator-assertion path. It does NOT certify PITR behaviour on real AWS, nor that real
// DynamoDB backfills a GSI asynchronously (DDB Local makes it immediate — which is precisely
// why D-32's GSI-ACTIVE wait exists and why that row remains AWS-unverified).

func TestRealDDBLocal_TheModelAgreesWithTheRealControlPlane_D78(t *testing.T) {
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

	table := "d78_ddbguard_probe"
	_, _ = c.CreateTable(ctx, &dynamodb.CreateTableInput{
		TableName: aws.String(table),
		KeySchema: []types.KeySchemaElement{{AttributeName: aws.String("pk"), KeyType: types.KeyTypeHash}},
		AttributeDefinitions: []types.AttributeDefinition{
			{AttributeName: aws.String("pk"), AttributeType: types.ScalarAttributeTypeS}},
		BillingMode: types.BillingModePayPerRequest,
	})
	defer c.DeleteTable(ctx, &dynamodb.DeleteTableInput{TableName: aws.String(table)})

	// PITR is unanswerable here ⇒ refuse without the assertion, exactly as the fake predicted.
	if _, fatal, _ := VerifyTable(ctx, c, table, "ttl", false); !strings.Contains(fatal, "ddb_pitr_confirmed") {
		t.Fatalf("REAL control plane: an unverifiable PITR must refuse and name the assertion, got %q", fatal)
	}
	// With the assertion: a fresh table has TTL DISABLED ⇒ WARN, never a refusal.
	_, fatal, warn := VerifyTable(ctx, c, table, "ttl", true)
	if fatal != "" || warn == "" {
		t.Fatalf("REAL control plane: a fresh table must WARN (TTL disabled), not refuse: fatal=%q warn=%q", fatal, warn)
	}

	// Enable TTL on the attribute the library actually writes ⇒ clean.
	if _, err := c.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
		TableName: aws.String(table),
		TimeToLiveSpecification: &types.TimeToLiveSpecification{
			Enabled: aws.Bool(true), AttributeName: aws.String("ttl")},
	}); err != nil {
		t.Fatalf("UpdateTimeToLive: %v", err)
	}
	v, fatal, warn := VerifyTable(ctx, c, table, "ttl", true)
	if fatal != "" || warn != "" || !strings.Contains(v, "ttl=ttl") {
		t.Fatalf("REAL control plane: a correct table must pass cleanly: v=%q fatal=%q warn=%q", v, fatal, warn)
	}

	// And the dangerous one, against the REAL control plane: a TTL pointed at an attribute we
	// do NOT write means DynamoDB may delete rows we never marked ⇒ FATAL.
	if _, fatal, _ := VerifyTable(ctx, c, table, "expires_at", true); !strings.Contains(strings.ToLower(fatal), "ttl") {
		t.Fatalf("REAL control plane: TTL on the WRONG attribute must be FATAL, got %q", fatal)
	}
}
