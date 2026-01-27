package awsutil

import (
	"context"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	configv2 "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
)

func NewSQSClient(ctx context.Context, region string) (*sqs.Client, error) {
	endpoint := os.Getenv("LOCALSTACK_ENDPOINT") // e.g. http://localhost:4566 for LocalStack

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

	// For LocalStack: set service client BaseEndpoint (v2-style, not deprecated)
	if endpoint != "" {
		return sqs.NewFromConfig(cfg, func(o *sqs.Options) {
			o.BaseEndpoint = aws.String(endpoint)
		}), nil
	}

	// Real AWS
	return sqs.NewFromConfig(cfg), nil
}
