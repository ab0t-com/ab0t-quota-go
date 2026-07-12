package outbox

// D-29/D-30/D-32 — DynamoDB is the DEFAULT durable outbox store (a real
// durable store, not an evictable cache). Self-provisioned from config;
// EnsureTable creates the table + status GSI and WAITS for ACTIVE (real
// DynamoDB backfills a GSI async, so draining against a still-backfilling
// index silently misses pending money — DDB-Local makes it immediate so no
// DDB-Local test catches it; the wait is the production guard).
//
// Schema: PK=OUTBOX#{key}, SK=META; GSI "gsi_status"
// (gsi_status_pk=OUTBOXSTATUS#{status}, gsi_status_sk=first_ts) for ListPending.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

type ddbAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
}

// DDBStore is the durable DynamoDB outbox.
type DDBStore struct {
	c     ddbAPI
	table string
}

// NewDDBStore wraps a *dynamodb.Client (or any ddbAPI). table defaults to
// "ab0t_quota_outbox".
func NewDDBStore(client ddbAPI, table string) *DDBStore {
	if table == "" {
		table = "ab0t_quota_outbox"
	}
	return &DDBStore{c: client, table: table}
}

func (s *DDBStore) pk(key string) string { return "OUTBOX#" + key }

func (s *DDBStore) item(r Record) map[string]ddbtypes.AttributeValue {
	m := map[string]ddbtypes.AttributeValue{
		"PK":            &ddbtypes.AttributeValueMemberS{Value: s.pk(r.Key)},
		"SK":            &ddbtypes.AttributeValueMemberS{Value: "META"},
		"okey":          &ddbtypes.AttributeValueMemberS{Value: r.Key},
		"event":         &ddbtypes.AttributeValueMemberS{Value: string(r.Event)},
		"event_type":    &ddbtypes.AttributeValueMemberS{Value: r.EventType},
		"resource_type": &ddbtypes.AttributeValueMemberS{Value: r.ResourceType},
		"reservationid": &ddbtypes.AttributeValueMemberS{Value: r.ReservationID},
		"status":        &ddbtypes.AttributeValueMemberS{Value: r.Status},
		"first_ts":      &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(r.FirstTS, 'f', -1, 64)},
		"attempts":      &ddbtypes.AttributeValueMemberN{Value: strconv.Itoa(r.Attempts)},
		"gsi_status_pk": &ddbtypes.AttributeValueMemberS{Value: "OUTBOXSTATUS#" + r.Status},
		"gsi_status_sk": &ddbtypes.AttributeValueMemberN{Value: strconv.FormatFloat(r.FirstTS, 'f', -1, 64)},
	}
	if r.Reason != "" {
		m["reason"] = &ddbtypes.AttributeValueMemberS{Value: r.Reason}
	}
	return m
}

func rec(item map[string]ddbtypes.AttributeValue) Record {
	s := func(k string) string {
		if v, ok := item[k].(*ddbtypes.AttributeValueMemberS); ok {
			return v.Value
		}
		return ""
	}
	n := func(k string) float64 {
		if v, ok := item[k].(*ddbtypes.AttributeValueMemberN); ok {
			f, _ := strconv.ParseFloat(v.Value, 64)
			return f
		}
		return 0
	}
	return Record{
		Key: s("okey"), Event: []byte(s("event")), EventType: s("event_type"),
		ResourceType: s("resource_type"), ReservationID: s("reservationid"),
		Status: s("status"), FirstTS: n("first_ts"), Attempts: int(n("attempts")), Reason: s("reason"),
	}
}

func (s *DDBStore) get(ctx context.Context, key string) (*Record, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: &s.table, ConsistentRead: aws.Bool(true),
		Key: map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: s.pk(key)}, "SK": &ddbtypes.AttributeValueMemberS{Value: "META"}},
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}
	r := rec(out.Item)
	return &r, nil
}

func (s *DDBStore) PutIntent(ctx context.Context, r Record) (Record, error) {
	if r.Status == "" {
		r.Status = StatusPending
	}
	_, err := s.c.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: &s.table, Item: s.item(r),
		ConditionExpression: aws.String("attribute_not_exists(PK)"),
	})
	if err == nil {
		return r, nil
	}
	var ccf *ddbtypes.ConditionalCheckFailedException
	if errors.As(err, &ccf) {
		ex, gerr := s.get(ctx, r.Key)
		if gerr != nil {
			return r, gerr
		}
		if ex != nil {
			return *ex, nil
		}
		return r, nil
	}
	return r, err
}

func (s *DDBStore) MarkDelivered(ctx context.Context, key string) error {
	_, err := s.c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: &s.table,
		Key:       map[string]ddbtypes.AttributeValue{"PK": &ddbtypes.AttributeValueMemberS{Value: s.pk(key)}, "SK": &ddbtypes.AttributeValueMemberS{Value: "META"}},
	})
	return err
}

func (s *DDBStore) MarkVoided(ctx context.Context, key, reason string) error {
	r, err := s.get(ctx, key)
	if err != nil || r == nil {
		return err
	}
	r.Status = StatusVoided
	r.Reason = reason
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: s.item(*r)})
	return err
}

func (s *DDBStore) BumpAttempt(ctx context.Context, key string) error {
	r, err := s.get(ctx, key)
	if err != nil || r == nil {
		return err
	}
	r.Attempts++
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: s.item(*r)})
	return err
}

func (s *DDBStore) ListPending(ctx context.Context, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = 100
	}
	out, err := s.c.Query(ctx, &dynamodb.QueryInput{
		TableName: &s.table, IndexName: aws.String("gsi_status"),
		KeyConditionExpression:    aws.String("gsi_status_pk = :pk"),
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{":pk": &ddbtypes.AttributeValueMemberS{Value: "OUTBOXSTATUS#" + StatusPending}},
		Limit:                     aws.Int32(int32(limit)), ScanIndexForward: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	var res []Record
	for _, it := range out.Items {
		res = append(res, rec(it))
	}
	return res, nil
}

func (s *DDBStore) Durable() bool { return true }

// EnsureTable creates the table + gsi_status GSI (idempotent) and WAITS for
// both to be ACTIVE. Bounded; a timeout is a loud error (never drain against a
// backfilling index).
func (s *DDBStore) EnsureTable(ctx context.Context, gsiActiveTimeout time.Duration) error {
	_, derr := s.c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table})
	if derr != nil {
		var nf *ddbtypes.ResourceNotFoundException
		if !errors.As(derr, &nf) {
			return derr // real error (perms, endpoint) — don't mask it
		}
		str := func(v string) *string { return &v }
		_, err := s.c.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName:   &s.table,
			BillingMode: ddbtypes.BillingModePayPerRequest,
			AttributeDefinitions: []ddbtypes.AttributeDefinition{
				{AttributeName: str("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: str("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: str("gsi_status_pk"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: str("gsi_status_sk"), AttributeType: ddbtypes.ScalarAttributeTypeN},
			},
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: str("PK"), KeyType: ddbtypes.KeyTypeHash},
				{AttributeName: str("SK"), KeyType: ddbtypes.KeyTypeRange},
			},
			GlobalSecondaryIndexes: []ddbtypes.GlobalSecondaryIndex{{
				IndexName:  str("gsi_status"),
				KeySchema:  []ddbtypes.KeySchemaElement{{AttributeName: str("gsi_status_pk"), KeyType: ddbtypes.KeyTypeHash}, {AttributeName: str("gsi_status_sk"), KeyType: ddbtypes.KeyTypeRange}},
				Projection: &ddbtypes.Projection{ProjectionType: ddbtypes.ProjectionTypeAll},
			}},
		})
		if err != nil {
			return err
		}
	}
	deadline := time.Now().Add(gsiActiveTimeout)
	for {
		desc, err := s.c.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table})
		if err == nil && desc.Table != nil && desc.Table.TableStatus == ddbtypes.TableStatusActive {
			gsiActive := false
			for _, g := range desc.Table.GlobalSecondaryIndexes {
				if g.IndexName != nil && *g.IndexName == "gsi_status" && g.IndexStatus == ddbtypes.IndexStatusActive {
					gsiActive = true
				}
			}
			if gsiActive {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("outbox table %s not ready within %s (money events must not drain against a backfilling index)", s.table, gsiActiveTimeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}
