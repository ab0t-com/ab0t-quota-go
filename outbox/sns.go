package outbox

// (c.6) — the concrete SNS settlement publisher, the load-bearing link of the
// money-emit path (D-56/D-63). Until this existed, the whole-chain gate
// correctly refused enable_paid in every real deployment (Go's billing was
// safely OFF, not working). The emitter publishes settlement events through
// this; billing subscribes to the topic. Mirrors Python's SNS emit.
//
// ⚠️ Verified against LocalStack SNS/SQS only — real AWS SNS (IAM, delivery
// retries, DLQ) UNVERIFIED. Pre-deploy gate.

import (
	"context"
	"encoding/json"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	snstypes "github.com/aws/aws-sdk-go-v2/service/sns/types"
)

// snsAPI is the subset of the SNS client this publisher needs.
type snsAPI interface {
	Publish(context.Context, *sns.PublishInput, ...func(*sns.Options)) (*sns.PublishOutput, error)
}

// SNSPublisher delivers outbox Records to an SNS topic. Implements Publisher.
type SNSPublisher struct {
	client   snsAPI
	topicARN string
}

// NewSNSPublisher wraps an *sns.Client (or any snsAPI) for a topic.
func NewSNSPublisher(client snsAPI, topicARN string) *SNSPublisher {
	return &SNSPublisher{client: client, topicARN: topicARN}
}

// Publish delivers one record. The message body is the record's Event payload
// (or the whole record if Event is empty); event_type/resource_type ride as
// message attributes for subscriber filtering.
func (p *SNSPublisher) Publish(ctx context.Context, r Record) error {
	msg := string(r.Event)
	if msg == "" {
		b, err := json.Marshal(r)
		if err != nil {
			return err
		}
		msg = string(b)
	}
	_, err := p.client.Publish(ctx, &sns.PublishInput{
		TopicArn: &p.topicARN,
		Message:  &msg,
		MessageAttributes: map[string]snstypes.MessageAttributeValue{
			"event_type":    {DataType: aws.String("String"), StringValue: aws.String(r.EventType)},
			"resource_type": {DataType: aws.String("String"), StringValue: aws.String(r.ResourceType)},
		},
	})
	return err
}
