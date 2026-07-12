package handlerledger

// TASK P5.1 remainder — real DynamoDB handler-ledger backend, verified
// against DynamoDB Local. Ticket 20260709_ab0t_quota_systemic_integrity_
// redesign (findings QG-01/QG-02).
//
// Schema (PRODUCT_SPEC §7):
//   PK: HANDLER#{handler}#{event_id}   SK: META         → LedgerRow
//   GSI1: GSI1PK=USER#{user_id}        GSI1SK=attempted_at(ISO)
//   GSI2: GSI2PK=STATUS#{status}       GSI2SK=attempted_at(ISO)
//   TTL attribute: ttl (epoch seconds, 90-day retention)
//   PK: BIZDEDUP#{sha256(key)}         SK: META         → dedup row (NO TTL)
//
// Concurrency: RecordAttempt uses an optimistic conditional PutItem (guarded
// on the observed Attempts count) so two replicas cannot both proceed on one
// (handler,event_id).

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	ddbtypes "github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

const (
	ddbRowTTL       = 90 * 24 * time.Hour
	ddbWriteTries   = 20
	ddbDefaultLease = 60
)

// ddbAPI is the subset of *dynamodb.Client this store needs.
type ddbAPI interface {
	GetItem(context.Context, *dynamodb.GetItemInput, ...func(*dynamodb.Options)) (*dynamodb.GetItemOutput, error)
	PutItem(context.Context, *dynamodb.PutItemInput, ...func(*dynamodb.Options)) (*dynamodb.PutItemOutput, error)
	Query(context.Context, *dynamodb.QueryInput, ...func(*dynamodb.Options)) (*dynamodb.QueryOutput, error)
	DeleteItem(context.Context, *dynamodb.DeleteItemInput, ...func(*dynamodb.Options)) (*dynamodb.DeleteItemOutput, error)
}

type ddbLedgerStore struct {
	c     ddbAPI
	table string
}

func ddbRowPK(handler, eventID string) string { return "HANDLER#" + handler + "#" + eventID }

func (s *ddbLedgerStore) key(pk, sk string) map[string]ddbtypes.AttributeValue {
	return map[string]ddbtypes.AttributeValue{
		"PK": &ddbtypes.AttributeValueMemberS{Value: pk},
		"SK": &ddbtypes.AttributeValueMemberS{Value: sk},
	}
}

func (s *ddbLedgerStore) marshalRow(row *LedgerRow) (map[string]ddbtypes.AttributeValue, error) {
	item, err := attributevalue.MarshalMap(row)
	if err != nil {
		return nil, err
	}
	pk := ddbRowPK(row.HandlerName, row.EventID)
	item["PK"] = &ddbtypes.AttributeValueMemberS{Value: pk}
	item["SK"] = &ddbtypes.AttributeValueMemberS{Value: "META"}
	iso := row.AttemptedAt.UTC().Format(time.RFC3339Nano)
	if row.UserID != "" {
		item["GSI1PK"] = &ddbtypes.AttributeValueMemberS{Value: "USER#" + row.UserID}
		item["GSI1SK"] = &ddbtypes.AttributeValueMemberS{Value: iso}
	}
	item["GSI2PK"] = &ddbtypes.AttributeValueMemberS{Value: "STATUS#" + string(row.Status)}
	item["GSI2SK"] = &ddbtypes.AttributeValueMemberS{Value: iso}
	item["ttl"] = &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", row.AttemptedAt.Add(ddbRowTTL).Unix())}
	return item, nil
}

func (s *ddbLedgerStore) getRow(ctx context.Context, handler, eventID string) (*LedgerRow, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.table,
		Key:            s.key(ddbRowPK(handler, eventID), "META"),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return nil, err
	}
	if out.Item == nil {
		return nil, nil
	}
	var row LedgerRow
	if err := attributevalue.UnmarshalMap(out.Item, &row); err != nil {
		return nil, err
	}
	return &row, nil
}

func (s *ddbLedgerStore) RecordAttempt(ctx context.Context, in AttemptInput) (*AttemptResult, error) {
	for i := 0; i < ddbWriteTries; i++ {
		existing, err := s.getRow(ctx, in.HandlerName, in.EventID)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if IsTerminal(existing.Status) {
				return &AttemptResult{Proceed: false, CachedRow: existing}, nil
			}
			if existing.Status == StatusInProgress && !existing.LeaseExpiresAt.IsZero() &&
				existing.LeaseExpiresAt.After(time.Now()) {
				return &AttemptResult{Proceed: false, CachedRow: existing}, nil
			}
		}
		attempts := 1
		if existing != nil {
			attempts = existing.Attempts + 1
		}
		lease := in.LeaseSeconds
		if lease == 0 {
			lease = ddbDefaultLease
		}
		now := time.Now().UTC()
		row := &LedgerRow{
			HandlerName:    in.HandlerName,
			EventID:        in.EventID,
			EventType:      in.EventType,
			Status:         StatusInProgress,
			UserID:         in.UserID,
			OrgID:          in.OrgID,
			Attempts:       attempts,
			AttemptedAt:    now,
			LeaseExpiresAt: now.Add(time.Duration(lease) * time.Second),
			EventPayload:   in.EventPayload,
		}
		item, err := s.marshalRow(row)
		if err != nil {
			return nil, err
		}
		put := &dynamodb.PutItemInput{TableName: &s.table, Item: item}
		if existing == nil {
			put.ConditionExpression = aws.String("attribute_not_exists(PK)")
		} else {
			put.ConditionExpression = aws.String("#a = :prev")
			put.ExpressionAttributeNames = map[string]string{"#a": "Attempts"}
			put.ExpressionAttributeValues = map[string]ddbtypes.AttributeValue{
				":prev": &ddbtypes.AttributeValueMemberN{Value: fmt.Sprintf("%d", existing.Attempts)},
			}
		}
		_, err = s.c.PutItem(ctx, put)
		if err == nil {
			return &AttemptResult{Proceed: true}, nil
		}
		var ccf *ddbtypes.ConditionalCheckFailedException
		if errors.As(err, &ccf) {
			continue // another replica raced us; re-read and retry
		}
		return nil, err
	}
	return nil, fmt.Errorf("handler ledger ddb: write contention exceeded %d retries", ddbWriteTries)
}

func (s *ddbLedgerStore) RecordOutcome(ctx context.Context, in OutcomeInput) error {
	row, err := s.getRow(ctx, in.HandlerName, in.EventID)
	if err != nil {
		return err
	}
	if row == nil {
		return nil
	}
	row.Status = in.Status
	row.Reason = in.Reason
	row.SideEffectID = in.SideEffectID
	row.Error = in.Error
	if in.Attempts > 0 {
		row.Attempts = in.Attempts
	}
	row.CompletedAt = time.Now().UTC()
	row.LeaseExpiresAt = time.Time{}
	item, err := s.marshalRow(row)
	if err != nil {
		return err
	}
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

func (s *ddbLedgerStore) GetRow(ctx context.Context, handler, eventID string) (*LedgerRow, error) {
	return s.getRow(ctx, handler, eventID)
}

func (s *ddbLedgerStore) AlreadyDone(ctx context.Context, dedupKey string) (bool, error) {
	out, err := s.c.GetItem(ctx, &dynamodb.GetItemInput{
		TableName:      &s.table,
		Key:            s.key("BIZDEDUP#"+HashKey(dedupKey), "META"),
		ConsistentRead: aws.Bool(true),
	})
	if err != nil {
		return false, err
	}
	return out.Item != nil, nil
}

func (s *ddbLedgerStore) MarkDone(ctx context.Context, in MarkDoneInput) error {
	item, err := attributevalue.MarshalMap(in)
	if err != nil {
		return err
	}
	item["PK"] = &ddbtypes.AttributeValueMemberS{Value: "BIZDEDUP#" + HashKey(in.DedupKey)}
	item["SK"] = &ddbtypes.AttributeValueMemberS{Value: "META"}
	// No ttl attribute — promotional/credit dedup rows never expire.
	_, err = s.c.PutItem(ctx, &dynamodb.PutItemInput{TableName: &s.table, Item: item})
	return err
}

func (s *ddbLedgerStore) queryGSI(ctx context.Context, index, pkAttr, pkVal string, opt QueryOptions) ([]*LedgerRow, error) {
	out, err := s.c.Query(ctx, &dynamodb.QueryInput{
		TableName:                &s.table,
		IndexName:                &index,
		KeyConditionExpression:   aws.String("#pk = :pk"),
		ExpressionAttributeNames: map[string]string{"#pk": pkAttr},
		ExpressionAttributeValues: map[string]ddbtypes.AttributeValue{
			":pk": &ddbtypes.AttributeValueMemberS{Value: pkVal},
		},
		ScanIndexForward: aws.Bool(false), // newest first
	})
	if err != nil {
		return nil, err
	}
	var rows []*LedgerRow
	for _, it := range out.Items {
		var row LedgerRow
		if err := attributevalue.UnmarshalMap(it, &row); err != nil {
			return nil, err
		}
		if !opt.Since.IsZero() && row.AttemptedAt.Before(opt.Since) {
			continue
		}
		cp := row
		rows = append(rows, &cp)
		if opt.Limit > 0 && len(rows) >= opt.Limit {
			break
		}
	}
	return rows, nil
}

func (s *ddbLedgerStore) QueryByUser(ctx context.Context, userID string, opt QueryOptions) ([]*LedgerRow, error) {
	return s.queryGSI(ctx, "GSI1", "GSI1PK", "USER#"+userID, opt)
}

func (s *ddbLedgerStore) QueryByStatus(ctx context.Context, status LedgerStatus, opt QueryOptions) ([]*LedgerRow, error) {
	return s.queryGSI(ctx, "GSI2", "GSI2PK", "STATUS#"+string(status), opt)
}

func (s *ddbLedgerStore) DeleteUser(ctx context.Context, userID string) (int, error) {
	rows, err := s.queryGSI(ctx, "GSI1", "GSI1PK", "USER#"+userID, QueryOptions{})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		_, err := s.c.DeleteItem(ctx, &dynamodb.DeleteItemInput{
			TableName: &s.table,
			Key:       s.key(ddbRowPK(r.HandlerName, r.EventID), "META"),
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// EnsureTable (D-82) CREATES the ledger table if absent (idempotent), WAITS for it to be
// ACTIVE, and enables TTL on `ttl` — the attribute this store actually writes.
//
// This store previously ASSUMED its table existed. The outbox and the activation store both
// provision theirs; the handler ledger did neither — so a client who wired it hit a
// ResourceNotFoundException at their FIRST auth webhook, in production. It stayed invisible
// for the most instructive reason (D-78): the only thing that had ever exercised it was a
// fake, and *a fake never notices, because a fake creates nothing*.
//
// Enabling TTL on `ttl` also means the D-76 preflight — which FAILS a table whose TTL points
// at an attribute we do not write (DynamoDB would delete rows we never marked) — passes on a
// table we provisioned ourselves.
func (s *ddbLedgerStore) EnsureTable(ctx context.Context, activeTimeout time.Duration) error {
	full, ok := s.c.(ddbAdminAPI)
	if !ok {
		return nil // a test double / restricted client: nothing to provision
	}
	if _, err := full.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table}); err != nil {
		var nf *ddbtypes.ResourceNotFoundException
		if !errors.As(err, &nf) {
			return fmt.Errorf("handler-ledger describe_table: %w", err) // perms/endpoint — don't mask
		}
		_, cerr := full.CreateTable(ctx, &dynamodb.CreateTableInput{
			TableName: &s.table,
			KeySchema: []ddbtypes.KeySchemaElement{
				{AttributeName: strPtr("PK"), KeyType: ddbtypes.KeyTypeHash},
				{AttributeName: strPtr("SK"), KeyType: ddbtypes.KeyTypeRange},
			},
			AttributeDefinitions: []ddbtypes.AttributeDefinition{
				{AttributeName: strPtr("PK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
				{AttributeName: strPtr("SK"), AttributeType: ddbtypes.ScalarAttributeTypeS},
			},
			BillingMode: ddbtypes.BillingModePayPerRequest,
		})
		if cerr != nil {
			return fmt.Errorf("handler-ledger create_table: %w", cerr)
		}
		slog.Info("created handler-ledger table (D-82)", "table", s.table)
	}

	if activeTimeout <= 0 {
		activeTimeout = 60 * time.Second
	}
	deadline := time.Now().Add(activeTimeout)
	for {
		out, err := full.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: &s.table})
		if err == nil && out.Table != nil && out.Table.TableStatus == ddbtypes.TableStatusActive {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("handler-ledger table %s did not become ACTIVE within %s — refusing "+
				"to run the idempotency ledger against a table that is not ready (D-82)", s.table, activeTimeout)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Best-effort: a client whose IAM forbids UpdateTimeToLive still gets a working ledger —
	// the D-76 preflight will WARN that rows never reap (growth/cost, not a correctness leak).
	cur, err := full.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: &s.table})
	if err == nil && cur.TimeToLiveDescription != nil &&
		cur.TimeToLiveDescription.TimeToLiveStatus != ddbtypes.TimeToLiveStatusEnabled &&
		cur.TimeToLiveDescription.TimeToLiveStatus != ddbtypes.TimeToLiveStatusEnabling {
		if _, terr := full.UpdateTimeToLive(ctx, &dynamodb.UpdateTimeToLiveInput{
			TableName: &s.table,
			TimeToLiveSpecification: &ddbtypes.TimeToLiveSpecification{
				Enabled: boolPtr(true), AttributeName: strPtr("ttl")},
		}); terr != nil {
			slog.Warn("handler-ledger TTL not enabled — rows will not reap; nothing is lost (D-76 warns)",
				"table", s.table, "err", terr)
		}
	}
	return nil
}

// ddbAdminAPI is the control-plane surface EnsureTable needs (satisfied by *dynamodb.Client).
type ddbAdminAPI interface {
	DescribeTable(context.Context, *dynamodb.DescribeTableInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTableOutput, error)
	CreateTable(context.Context, *dynamodb.CreateTableInput, ...func(*dynamodb.Options)) (*dynamodb.CreateTableOutput, error)
	DescribeTimeToLive(context.Context, *dynamodb.DescribeTimeToLiveInput, ...func(*dynamodb.Options)) (*dynamodb.DescribeTimeToLiveOutput, error)
	UpdateTimeToLive(context.Context, *dynamodb.UpdateTimeToLiveInput, ...func(*dynamodb.Options)) (*dynamodb.UpdateTimeToLiveOutput, error)
}

// Table reports the table this store writes to (for the D-76 preflight).
func (s *ddbLedgerStore) Table() string { return s.table }

// Client exposes the DDB client for the D-76 preflight (nil for a test double).
func (s *ddbLedgerStore) Client() any { return s.c }

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
