package services

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// AwsSqsOptions configure an awssqs service. When Key and Secret are empty the AWS default
// credential chain (environment, web identity / IRSA, instance role) is used.
type AwsSqsOptions struct {
	Queue       string `json:"queue"`
	Region      string `json:"region"`
	Key         string `json:"key"`
	Secret      string `json:"secret"`
	Account     string `json:"account"`
	EndpointURL string `json:"endpointUrl"`
	TransportOptions
}

type awsSqsService struct {
	opts AwsSqsOptions
	deps Deps
	http *httpClient

	mu     sync.Mutex
	client *sqs.Client
}

func newAwsSqsService(raw []byte, deps Deps, r *Redactor) (Service, error) {
	var opts AwsSqsOptions
	if err := decodeOptions(TypeAwsSqs, raw, &opts); err != nil {
		return nil, err
	}
	r.Add(opts.Secret, opts.Key)
	if (opts.Key == "") != (opts.Secret == "") {
		return nil, fmt.Errorf("%w: awssqs: key and secret must be set together", ErrInvalidOptions)
	}
	if opts.EndpointURL != "" {
		u, err := parseEndpoint(opts.EndpointURL)
		if err != nil {
			return nil, fmt.Errorf("%w: awssqs: endpointUrl: %v", ErrInvalidOptions, err)
		}
		if err := validateEndpoint(u, []string{"http", "https"}, deps.Network.ValidateURL); err != nil {
			return nil, fmt.Errorf("awssqs: endpointUrl: %w", err)
		}
	}
	hc, err := newHTTPClient(TypeAwsSqs, deps, opts.TransportOptions, r, nil)
	if err != nil {
		return nil, err
	}
	return &awsSqsService{opts: opts, deps: deps, http: hc}, nil
}

// sqsClient lazily builds the SQS client. Failures are not cached so a transient
// credential-chain error is retried on the next delivery.
func (s *awsSqsService) sqsClient(ctx context.Context) (*sqs.Client, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		return s.client, nil
	}
	{
		var cfg aws.Config
		if s.opts.Key != "" && s.opts.Region != "" {
			// Fully specified: do not consult the environment or shared config files.
			cfg = aws.Config{
				Region: s.opts.Region,
				Credentials: aws.NewCredentialsCache(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: s.opts.Key, SecretAccessKey: s.opts.Secret, Source: "patrol-notifications"}, nil
				})),
			}
		} else {
			var loadOpts []func(*awsconfig.LoadOptions) error
			if s.opts.Region != "" {
				loadOpts = append(loadOpts, awsconfig.WithRegion(s.opts.Region))
			}
			if s.opts.Key != "" {
				key, secret := s.opts.Key, s.opts.Secret
				loadOpts = append(loadOpts, awsconfig.WithCredentialsProvider(aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
					return aws.Credentials{AccessKeyID: key, SecretAccessKey: secret, Source: "patrol-notifications"}, nil
				})))
			}
			loaded, err := awsconfig.LoadDefaultConfig(ctx, loadOpts...)
			if err != nil {
				return nil, fmt.Errorf("awssqs: loading AWS configuration: %w", err)
			}
			cfg = loaded
		}
		cfg.HTTPClient = s.http.client
		if cfg.Region == "" {
			return nil, fmt.Errorf("%w: awssqs: region is not configured", ErrInvalidOptions)
		}
		s.client = sqs.NewFromConfig(cfg, func(o *sqs.Options) {
			if s.opts.EndpointURL != "" {
				o.BaseEndpoint = aws.String(s.opts.EndpointURL)
			}
			o.RetryMaxAttempts = s.http.retry.MaxRetries + 1
		})
	}
	return s.client, nil
}

func (s *awsSqsService) Send(ctx context.Context, n Notification, dest Destination) error {
	client, err := s.sqsClient(ctx)
	if err != nil {
		return err
	}
	queue := strings.TrimSpace(firstNonEmpty(dest.Recipient, s.opts.Queue))
	if queue == "" {
		return fmt.Errorf("%w: awssqs: queue is not configured", ErrInvalidNotification)
	}
	queueURL := queue
	if !strings.HasPrefix(queue, "https://") && !strings.HasPrefix(queue, "http://") {
		in := &sqs.GetQueueUrlInput{QueueName: aws.String(queue)}
		if s.opts.Account != "" {
			in.QueueOwnerAWSAccountId = aws.String(s.opts.Account)
		}
		out, err := client.GetQueueUrl(ctx, in)
		if err != nil {
			return fmt.Errorf("%w: awssqs: resolving queue %q: %v", ErrDeliveryFailed, queue, err)
		}
		queueURL = aws.ToString(out.QueueUrl)
	} else if u, err := url.Parse(queue); err != nil || validateEndpoint(u, []string{"http", "https"}, s.deps.Network.ValidateURL) != nil {
		return fmt.Errorf("%w: awssqs: queue URL is not allowed", ErrURLNotAllowed)
	}
	in := &sqs.SendMessageInput{QueueUrl: aws.String(queueURL), MessageBody: aws.String(n.Message)}
	if q := n.AwsSqs; q != nil {
		if len(q.MessageAttributes) > 0 {
			in.MessageAttributes = make(map[string]sqstypes.MessageAttributeValue, len(q.MessageAttributes))
			for k, v := range q.MessageAttributes {
				in.MessageAttributes[k] = sqstypes.MessageAttributeValue{DataType: aws.String("String"), StringValue: aws.String(v)}
			}
		}
		if q.MessageGroupID != "" {
			in.MessageGroupId = aws.String(q.MessageGroupID)
		}
		if q.MessageDeduplicationID != "" {
			in.MessageDeduplicationId = aws.String(q.MessageDeduplicationID)
		}
	}
	if _, err := client.SendMessage(ctx, in); err != nil {
		return fmt.Errorf("%w: awssqs: sending message: %v", ErrDeliveryFailed, err)
	}
	return nil
}
