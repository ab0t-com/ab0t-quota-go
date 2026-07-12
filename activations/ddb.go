package activations

// D-39 — the activation ledger is authoritative for IDENTITY and COST (D-33),
// so it may NOT live in an evictable cache. This is the durable DynamoDB
// activation store; it becomes the default when DDB is reachable. Redis stays
// permitted only under the durability machine-check (outbox.CheckRedisDurability).
//
// Schema: PK=ACT#{id}, SK=META. GSI1 (GSI1PK) = "ORGOPEN#{org}" for OPEN rows
// (the open-set index); flipped to "CLOSED#{org}" on release/settle so it
// drops out of the ListOpen query. TTL attribute `ttl` on released/settled.
//
// ⚠️ Verified against DynamoDB Local only — real AWS (IAM, GSI throughput/TTL
// reaping) UNVERIFIED. Pre-deploy gate.

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type ddbAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	UpdateItem(context.Context, *dynamodb.UpdateItemInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

// DDBStore is the durable DynamoDB activation store.
type DDBStore struct {
	c     ddbAPI
	table string
	ttl   time.Duration
}

// NewDDBStore builds a durable activation store. table defaults to
// "ab0t_quota_activations"; ttl<=0 → DefaultReleasedTTL.
func NewDDBStore(client ddbAPI, table string, ttl time.Duration) *DDBStore {
	if table == "" {
		table = "ab0t_quota_activations"
	}
	if ttl <= 0 {
		ttl = DefaultReleasedTTL
	}
	return &DDBStore{c: client, table: table, ttl: ttl}
}

// Durable reports the store is crash + eviction durable (D-39).
func (s *DDBStore) Durable() bool { return true }

// EnsureTable creates the table + GSI1 (idempotent) and waits for ACTIVE.
// Self-provision so the durable store is the zero-config default (D-39/D-32).
func (s *DDBStore) EnsureTable(ctx context.Context, timeout time.Duration) error {
	if _, err := s.c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table}); err != nil {
		var nf *ddbtypes.ResourceNotFoundException
		if !errors.As(err, &nf) {
			return err
		}
		str := func(v string) *string { return &v }
		if _, err := s.c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:   &s.table,
			BillingMode: ddbtypes.BillingModePayPerRequest,
			AttributeDefinitions: []ddbtypes.AttributeDefinition{
				{AttributeName: str("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: str("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: str("GSI1PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: str("GSI1SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			},
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: str("PK"), KeyType: ddbtypes.KeyTypeHash},
				{AttributeName: str("SK"), KeyType: ddbtypes.KeyTypeRange},
			},
			GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{{
				IndexName:  str("GSI1"),
				KeySchema:  []ddbtypes.KeySchemaElement{{AttributeName: str("GSI1PK"), KeyType: ddbtypes.KeyTypeHash}, {AttributeName: str("GSI1SK"), KeyType: ddbtypes.KeyTypeRange}},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			}},
		}); err != nil {
			return err
		}
	}
	deadline := time.Now().Add(timeout)
	for {
		desc, err := s.c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table})
		if err == nil && desc.Table != nil && desc.Table.TableStatus == ddbtypes.TableStatusActive {
			for _, g := range desc.Table.GlobalSecondaryIndexes {
				if g.IndexName != nil && *g.IndexName == "GSI1" && g.IndexStatus == ddbtypes.IndexStatusActive {
					return nil
				}
			}
		}
		if time.Now().After(deadline) {
			return errors.New("activation table not ready before timeout (GSI1 must be ACTIVE before ListOpen)")
		}
		time.Sleep(300 * time.Millisecond)
	}
}

func (s *DDBStore) keyAV(id string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"PK": &ddbtypes.AttributeValueMemberS{Value: "ACT#" + id},
		"SK": &ddbtypes.AttributeValueMemberS{Value: "META"},
	}
}

func (s *DDBStore) item(a Activation) map[string]ddbtypes.AttributeValue {
	blob, _ := json.Marshal(a)
	gsi := "ORGOPEN#" + a.OrgID
	if a.State != StateOpen {
		gsi = "CLOSED#" + a.OrgID
	}
	m := map[string]ddbtypes.AttributeValue{
		"PK":     &ddbtypes.AttributeValueMemberS{Value: "ACT#" + a.ActivationID},
		"SK":     &ddbtypes.AttributeValueMemberS{Value: "META"},
		"GSI1PK": &ddbtypes.AttributeValueMemberS{Value: gsi},
		"GSI1SK": &ddbtypes.AttributeValueMemberS{Value: a.OpenedAt},
		"state":  &ddbtypes.AttributeValueMemberS{Value: a.State},
		"row":    &ddbtypes.AttributeValueMemberS{Value: string(blob)},
	}
	if a.State != StateOpen && s.ttl > 0 {
		m["ttl"] = &ddbtypes.AttributeValueMemberN{Value: strconv.FormatInt(time.Now().Add(s.ttl).Unix(), 10)}
	}
	return m
}

func (s *DDBStore) rowFromItem(item map[string]ddbtypes.AttributeValue) (*Activation, error) {
	v, ok := item["row"].(*ddbtypes.AttributeValueMemberS)
	if !ok {
		return nil, nil
	}
	var a Activation
	if err := json.Unmarshal([]byte(v.Value), &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *DDBStore) PutOpen(ctx context.Context, a Activation) error {
	if a.State == "" {
		a.State = StateOpen
	}
	if a.Spend == nil {
		a.Spend = map[string]float64{}
	}
	_, err := s.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &s.table, Item: s.item(a),
		ConditionExpression: aws.String("attribute_not_exists(PK)"), // first writer wins
	})
	if err != nil {
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return nil // already present (minted id is unique) — no-op
		}
		return err
	}
	return nil
}

func (s *DDBStore) Get(ctx context.Context, id string) (*Activation, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, Key: s.keyAV(id), ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}
	return s.rowFromItem(out.Item)
}

// transition idempotently moves a row from `from` to `to`, rewriting the JSON
// row + GSI + ttl. Returns the row ONLY if THIS call performed the transition.
func (s *DDBStore) transition(ctx context.Context, id, from, to string, mutate func(*Activation)) (*Activation, error) {
	cur, err := s.Get(ctx, id)
	if err != nil || cur == nil || cur.State != from {
		return nil, err
	}
	next := *cur
	next.State = to
	mutate(&next)
	// Conditional put guarded on the observed state — loser (already moved) fails.
	item := s.item(next)
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName:                 &s.table,
		Item:                      item,
		ConditionExpression:       aws.String("#s = :from"),
		ExpressionAttributeNames:  map[string]string{"#s": "state"},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":from": &ddbtypes.AttributeValueMemberS{Value: from}},
	})
	if err != nil {
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			return nil, nil // raced / already transitioned
		}
		return nil, err
	}
	return &next, nil
}

func (s *DDBStore) MarkReleased(ctx context.Context, id string) (*Activation, error) {
	return s.transition(ctx, id, StateOpen, StateReleased, func(a *Activation) {
		t := nowISO()
		a.ReleasedAt = &t
	})
}

func (s *DDBStore) MarkSettled(ctx context.Context, id, cost string) (*Activation, error) {
	for _, from := range []string{StateReleased, StateOpen} {
		row, err := s.transition(ctx, id, from, StateSettled, func(a *Activation) {
			t := nowISO()
			a.SettledAt = &t
			a.Cost = &cost
		})
		if err != nil {
			return nil, err
		}
		if row != nil {
			return row, nil
		}
	}
	return nil, nil
}

func (s *DDBStore) ListOpen(ctx context.Context, orgID string, limit int) ([]Activation, error) {
	if limit <= 0 {
		limit = 100
	}
	out, err := s.c.Query(ctx, &dynamodb.QueryInput{
		TableName: &s.table, IndexName: aws.String("GSI1"),
		KeyConditionExpression:    aws.String("GSI1PK = :pk"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":pk": &ddbtypes.AttributeValueMemberS{Value: "ORGOPEN#" + orgID}},
		Limit:                     aws.Int32(int32(limit)), ScanIndexForward: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	var res []Activation
	for _, it := range out.Items {
		a, err := s.rowFromItem(it)
		if err != nil {
			return nil, err
		}
		if a != nil && a.State == StateOpen {
			res = append(res, *a)
		}
	}
	return res, nil
}

func (s *DDBStore) CountOpen(ctx context.Context, orgID string) (int, error) {
	opens, err := s.ListOpen(ctx, orgID, 10000)
	return len(opens), err
}
