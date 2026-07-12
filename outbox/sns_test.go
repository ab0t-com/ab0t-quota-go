package outbox

// (c.6) — verify the concrete SNS publisher END-TO-END, at the SINK: publish
// through it → an SQS queue subscribed to the topic RECEIVES the message.
// Asserting the publish returned no error would only prove the emitter works
// (D-40); a real subscriber receiving is the guarantee. Runs against LocalStack
// (:4566); skips when unreachable. Creates + deletes its own topic/queue.

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awscfg "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sns"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

func localstack(t *testing.T) (*sns.Client, *sqs.Client) {
	t.Helper()
	ep := os.Getenv("AB0T_QUOTA_TEST_AWS_ENDPOINT")
	if ep == "" {
		ep = "http://localhost:4566"
	}
	ctx := context.Background()
	cfg, err := awscfg.LoadDefaultConfig(ctx,
		awscfg.WithRegion("us-east-1"),
		awscfg.WithCredentialsProvider(credentials.NewStaticCredentialsProvider("test", "test", "")),
	)
	if err != nil {
		t.Skipf("aws config: %v", err)
	}
	sc := sns.NewFromConfig(cfg, func(o *sns.Options) { o.BaseEndpoint = aws.String(ep) })
	qc := sqs.NewFromConfig(cfg, func(o *sqs.Options) { o.BaseEndpoint = aws.String(ep) })
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := sc.ListTopics(pctx, &sns.ListTopicsInput{}); err != nil {
		t.Skipf("LocalStack SNS not reachable at %s (%v)", ep, err)
	}
	return sc, qc
}

func TestSNSPublisher_DeliversToSubscriber_C6(t *testing.T) {
	sc, qc := localstack(t)
	ctx := context.Background()
	name := "ab0t-quota-test-" + strconv.FormatInt(time.Now().UnixNano(), 10)

	topic, err := sc.CreateTopic(ctx, &sns.CreateTopicInput{Name: &name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = sc.DeleteTopic(context.Background(), &sns.DeleteTopicInput{TopicArn: topic.TopicArn}) })

	queue, err := qc.CreateQueue(ctx, &sqs.CreateQueueInput{QueueName: &name})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = qc.DeleteQueue(context.Background(), &sqs.DeleteQueueInput{QueueUrl: queue.QueueUrl}) })
	attrs, err := qc.GetQueueAttributes(ctx, &sqs.GetQueueAttributesInput{QueueUrl: queue.QueueUrl, AttributeNames: []sqstypes.QueueAttributeName{sqstypes.QueueAttributeNameQueueArn}})
	if err != nil {
		t.Fatal(err)
	}
	queueARN := attrs.Attributes["QueueArn"]
	if _, err := sc.Subscribe(ctx, &sns.SubscribeInput{
		TopicArn: topic.TopicArn, Protocol: aws.String("sqs"), Endpoint: &queueARN,
		Attributes: map[string]string{"RawMessageDelivery": "true"},
	}); err != nil {
		t.Fatal(err)
	}

	pub := NewSNSPublisher(sc, *topic.TopicArn)
	if err := pub.Publish(ctx, Record{
		Key: "resv-1:stopped", Event: json.RawMessage(`{"resource_id":"r1","event":"stopped"}`),
		EventType: "stopped", ResourceType: "sandbox", ReservationID: "resv-1",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Assert AT THE SINK: the subscribed queue receives the message.
	got := ""
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out, err := qc.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{QueueUrl: queue.QueueUrl, MaxNumberOfMessages: 1, WaitTimeSeconds: 1})
		if err == nil && len(out.Messages) > 0 {
			got = *out.Messages[0].Body
			break
		}
	}
	if !strings.Contains(got, "resource_id") || !strings.Contains(got, "r1") {
		t.Fatalf("subscriber did not receive the published settlement event; body=%q", got)
	}

	// NEGATIVE CONTROL: a bad topic ARN → Publish errors (not silent success).
	bad := NewSNSPublisher(sc, "arn:aws:sns:us-east-1:000000000000:does-not-exist")
	if err := bad.Publish(ctx, Record{Event: json.RawMessage(`{}`), EventType: "x"}); err == nil {
		t.Error("publish to a non-existent topic must error, not silently succeed")
	}
}
