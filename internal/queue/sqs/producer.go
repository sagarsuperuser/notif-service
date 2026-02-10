package sqsqueue

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type Producer struct {
	SQS      *sqs.Client
	QueueURL string

	// For FIFO: use bounded, bucketed MessageGroupIds to allow parallelism without exploding cardinality.
	// Example: tenantA:b1234, where bucket = hash(to) % GroupBuckets.
	// If GroupBuckets <= 0, defaults to 2000.
	GroupBuckets int
}

type SMSJob struct {
	TenantID       string            `json:"tenantId"`
	MessageID      string            `json:"messageId"`
	IdempotencyKey string            `json:"idempotencyKey"`
	To             string            `json:"to"`
	TemplateID     string            `json:"templateId"`
	Vars           map[string]string `json:"vars"`
	CampaignID     string            `json:"campaignId,omitempty"`
}

func (p *Producer) EnqueueSMS(ctx context.Context, tenantID, messageID, idempotencyKey, to, templateID string, vars map[string]string, campaignID string) error {
	job := SMSJob{
		TenantID: tenantID, MessageID: messageID, IdempotencyKey: idempotencyKey,
		To: to, TemplateID: templateID, Vars: vars, CampaignID: campaignID,
	}
	body, err := json.Marshal(job)
	if err != nil {
		return err
	}

	groupID := messageGroupIDBucketed(tenantID, to, p.GroupBuckets)
	_, err = p.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               &p.QueueURL,
		MessageBody:            str(string(body)),
		MessageGroupId:         str(groupID),
		MessageDeduplicationId: str(idempotencyKey),
	})
	return err
}

func str(s string) *string { return &s }

func messageGroupIDBucketed(tenantID, to string, buckets int) string {
	if buckets <= 0 {
		buckets = 2000
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(to))
	b := int(h.Sum32() % uint32(buckets))
	return fmt.Sprintf("%s:b%d", tenantID, b)
}
