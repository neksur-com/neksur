// S3 ObjectCreated event consumer (via SNS+SQS fanout) — the third
// trigger source for L3 detection.
//
// Architecture (RESEARCH §Standard Stack lines 139-140):
//   - S3 bucket emits ObjectCreated:Put events on metadata.json writes.
//   - S3 → SNS topic (fanout to multiple subscribers).
//   - SNS → SQS queue (per-tenant; one queue per tenant in Phase 1).
//   - This consumer long-polls the SQS queue and pushes Hit{Source:
//     "s3-event"} for each verified event.
//
// Bypass-detection rationale: most Iceberg writes go through the L1
// gateway (Plan 01-06) and trigger Plan 01-04's ingest pipeline. But
// engines that write directly to S3 (e.g., Spark with no REST catalog
// configured, or a misconfigured user invoking `aws s3 cp` on a manifest
// file) bypass the gateway entirely. The SNS+SQS path catches those:
// the S3 ObjectCreated event fires regardless of how the write happened.
//
// The consumer is OPTIONAL — operators set NEKSUR_S3_EVENTS_QUEUE_URL
// to enable it. If unset, cmd/neksur-server doesn't start the consumer
// goroutine. The poller and webhook handler still cover the
// gateway-mediated path.

package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/sqs"
	sqstypes "github.com/aws/aws-sdk-go-v2/service/sqs/types"
)

// SQSReceiver abstracts the subset of *sqs.Client we need so tests can
// inject a fake without spinning up a LocalStack container per case.
type SQSReceiver interface {
	ReceiveMessage(ctx context.Context, params *sqs.ReceiveMessageInput, optFns ...func(*sqs.Options)) (*sqs.ReceiveMessageOutput, error)
	DeleteMessage(ctx context.Context, params *sqs.DeleteMessageInput, optFns ...func(*sqs.Options)) (*sqs.DeleteMessageOutput, error)
}

// snsEnvelope is the shape SQS receives when SNS fans out to it. The
// outer envelope wraps the actual S3 event as a JSON string in `Message`.
type snsEnvelope struct {
	Type      string `json:"Type"`
	MessageID string `json:"MessageId"`
	TopicArn  string `json:"TopicArn"`
	Message   string `json:"Message"`
	Timestamp string `json:"Timestamp"`
}

// s3Event is the inner S3 event shape (a list of records). Phase 1
// reads only `s3.bucket.name` + `s3.object.key` to reconstruct the
// metadata.json URI.
type s3Event struct {
	Records []s3Record `json:"Records"`
}

type s3Record struct {
	EventName string `json:"eventName"`
	S3        struct {
		Bucket struct {
			Name string `json:"name"`
		} `json:"bucket"`
		Object struct {
			Key string `json:"key"`
		} `json:"object"`
	} `json:"s3"`
}

// RunS3EventConsumer long-polls the given SQS queue and pushes Hit{Source:
// "s3-event"} for each ObjectCreated event whose key matches a metadata.json
// path. Tenant resolution is via the queue → tenant mapping established
// at queue creation time (one queue per tenant in Phase 1; the tenant ID
// can be encoded in the queue name or the SNS topic ARN).
//
// Parameters:
//   - ctx       — cancellation context.
//   - sqsClient — *sqs.Client (or a SQSReceiver-implementing fake).
//   - queueURL  — the SQS queue URL (operator-supplied via env).
//   - tenantID  — the tenant the queue is scoped to. Phase 1
//                 simplification (one queue per tenant); Phase 6 may
//                 add multi-tenant queue support if it scales better.
//   - in        — dispatch channel.
//
// On any per-message error we slog.Error + continue (one bad event
// must not poison the consumer).
func RunS3EventConsumer(
	ctx context.Context,
	sqsClient SQSReceiver,
	queueURL string,
	tenantID string,
	in chan<- Hit,
) {
	if queueURL == "" {
		slog.Info("dispatch/s3events: queue URL empty; consumer not started")
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Long-poll up to 20s for messages. AWS SDK supports max
		// MaxNumberOfMessages = 10.
		out, err := sqsClient.ReceiveMessage(ctx, &sqs.ReceiveMessageInput{
			QueueUrl:            &queueURL,
			MaxNumberOfMessages: 10,
			WaitTimeSeconds:     20,
		})
		if err != nil {
			slog.Error("dispatch/s3events: sqs.ReceiveMessage failed",
				"queue", queueURL, "err", err)
			// Don't tight-loop on persistent failure — yield to ctx.
			select {
			case <-ctx.Done():
				return
			default:
			}
			continue
		}

		for _, msg := range out.Messages {
			processSQSMessage(ctx, sqsClient, queueURL, tenantID, msg, in)
		}
	}
}

// processSQSMessage parses one SQS message body (which is an SNS
// envelope wrapping an S3 event list), extracts the metadata.json URI
// for each record, and pushes Hit + deletes the message.
func processSQSMessage(
	ctx context.Context,
	sqsClient SQSReceiver,
	queueURL string,
	tenantID string,
	msg sqstypes.Message,
	in chan<- Hit,
) {
	if msg.Body == nil {
		return
	}
	var env snsEnvelope
	if err := json.Unmarshal([]byte(*msg.Body), &env); err != nil {
		slog.Error("dispatch/s3events: parse SNS envelope failed",
			"err", err)
		return
	}

	// SNS-wrapped events have the actual S3 event as a JSON string in
	// `Message`; unwrapped events (direct S3 → SQS) have the S3 event
	// directly in the message body. Try both shapes.
	innerJSON := env.Message
	if innerJSON == "" {
		innerJSON = *msg.Body
	}
	var event s3Event
	if err := json.Unmarshal([]byte(innerJSON), &event); err != nil {
		slog.Error("dispatch/s3events: parse S3 event failed",
			"err", err)
		return
	}

	for _, r := range event.Records {
		if !strings.HasPrefix(r.EventName, "ObjectCreated:") {
			continue
		}
		// Reconstruct the metadata.json URI. The s3.object.key is URL-
		// encoded; AWS SDK provides a Decode helper but the SDK's wire
		// shape preserves the literal so for Phase 1 we keep the
		// straightforward concat.
		uri := fmt.Sprintf("s3://%s/%s", r.S3.Bucket.Name, r.S3.Object.Key)

		// Filter to metadata.json events only — Iceberg writes many
		// non-metadata files (data files, manifest lists) and we only
		// want the snapshot-bearing metadata.json.
		if !strings.HasSuffix(r.S3.Object.Key, "metadata.json") &&
			!strings.HasSuffix(r.S3.Object.Key, ".metadata.json") {
			continue
		}

		hit := Hit{
			TenantID:         tenantID,
			MetadataLocation: uri,
			Source:           "s3-event",
		}
		select {
		case <-ctx.Done():
			return
		case in <- hit:
		}
	}

	// Delete the message after pushing — AWS at-least-once delivery
	// would otherwise re-deliver until the visibility-timeout elapses.
	// In-process dedup catches accidental re-delivery; deleting promptly
	// is the cheap optimization.
	if msg.ReceiptHandle != nil {
		_, err := sqsClient.DeleteMessage(ctx, &sqs.DeleteMessageInput{
			QueueUrl:      &queueURL,
			ReceiptHandle: msg.ReceiptHandle,
		})
		if err != nil {
			slog.Error("dispatch/s3events: sqs.DeleteMessage failed",
				"queue", queueURL, "err", err)
		}
	}
}
