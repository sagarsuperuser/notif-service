package sqsqueue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

type Producer struct {
	SQS      *sqs.Client
	QueueURL string
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

	groupID := fmt.Sprintf("%s:%s", tenantID, to) // FIFO ordering per phone
	_, err = p.SQS.SendMessage(ctx, &sqs.SendMessageInput{
		QueueUrl:               &p.QueueURL,
		MessageBody:            str(string(body)),
		MessageGroupId:         str(groupID),
		MessageDeduplicationId: str(idempotencyKey),
	})
	return err
}

func str(s string) *string { return &s }
