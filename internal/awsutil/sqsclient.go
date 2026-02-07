package awsutil

import (
	"context"

	"github.com/aws/aws-sdk-go-v2/aws"
	configv2 "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func NewSQSClient(ctx context.Context, region, endpoint string) (*sqs.Client, error) {
	// Always load config normally
	opts := []func(*configv2.LoadOptions) error{
		configv2.WithRegion(region),
	}

	// If LocalStack endpoint is set, use static dummy creds (LocalStack accepts these)
	if endpoint != "" {
		opts = append(opts, configv2.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("test", "test", ""),
		))
	}

	cfg, err := configv2.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}

	// For LocalStack: set service client BaseEndpoint (v2-style)
	if endpoint != "" {
		return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		}), nil
	}

	// Real AWS
	return sqs.NewFromConfig(cfg), nil
}
