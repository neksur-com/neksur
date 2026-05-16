// Package kms provides the Go-side AWS KMS wrapper for Neksur's
// per-column encryption path (D-2.07 + RESEARCH PATTERNS lines 812-826).
//
// Architecture context:
//
//	The live Spark write-path derives data encryption keys (DEKs) on the
//	JVM side via the neksur-spark-policy KmsKeyProvider (Plan 02-06). This
//	Go-side client exists to:
//	  (a) Support any future Go-driven encryption path (e.g., Phase 4 REST
//	      API write surface that writes directly without Spark).
//	  (b) Serve as the reference implementation pattern for the per-tenant
//	      CMK + per-column DEK discipline documented in D-2.07.
//
// Per-tenant CMK + per-column DEK discipline (D-2.07):
//
//	Each tenant's Customer-Managed Key (CMK) lives in the tenant's AWS
//	account. Neksur uses a cross-account KMS grant (tenant grants Neksur's
//	IAM role GenerateDataKey + Decrypt privileges on their CMK). The
//	EncryptionContext map ({"neksur:tenant": tenantID, "neksur:column":
//	columnName}) binds each DEK to its (tenant, column) pair — AWS KMS
//	enforces the context on decryption, preventing cross-tenant DEK reuse
//	even if two tenants use the same column name.
//
// Pitfall 10 mitigation (per-batch DEK caching):
//
//	Calling GenerateDataKey once per row is expensive and risks KMS rate
//	limits (400 requests/sec per region per account). The BatchCache in
//	batch_cache.go solves this: one GenerateDataKey call per (tenant,
//	column, batchID) tuple; subsequent rows in the same batch reuse the
//	cached plaintext DEK.
package kms

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
)

// Client wraps the AWS SDK Go v2 KMS client and exposes the narrow
// surface Neksur requires: GenerateDataKey for envelope encryption.
//
// Thread-safety: the embedded *awskms.Client is safe for concurrent
// use; the Go-side Client inherits that safety.
type Client struct {
	kms *awskms.Client
}

// NewClient constructs a KMS Client from an already-loaded aws.Config.
// Per Phase 0.5 CC3 (no second AWS session), callers MUST pass the same
// aws.Config constructed at startup (e.g., via config.LoadDefaultConfig
// in main.go) — do NOT call config.LoadDefaultConfig inside this
// function or create a separate AWS session.
func NewClient(awsConfig aws.Config) *Client {
	return &Client{
		kms: awskms.NewFromConfig(awsConfig),
	}
}

// GenerateDataKey calls AWS KMS GenerateDataKey to derive a new 256-bit
// AES data encryption key under the specified Customer-Managed Key (CMK).
//
// Per RESEARCH PATTERNS lines 812-826 verbatim shape:
//
//	Returns (plaintext DEK, encrypted DEK ciphertext, error).
//	The plaintext DEK is used in-process for the duration of the write batch
//	then zeroed. The encrypted DEK ciphertext is stored alongside the
//	encrypted column data so the column can be decrypted later (standard
//	envelope encryption).
//
// EncryptionContext (D-2.07 cross-account CMK isolation):
//
//	The context map {"neksur:tenant": tenantID, "neksur:column": columnName}
//	is passed to every GenerateDataKey call. AWS KMS requires the SAME
//	context map on decryption — any context mismatch causes decryption to
//	fail. This enforces (tenant, column) binding at the KMS API level,
//	not just application-level convention.
//
// Error wrapping: all errors are wrapped with fmt.Errorf("kms: %s: %w",
// op, err) per CLAUDE.md "errors wrapped, NO panics".
func (c *Client) GenerateDataKey(ctx context.Context, cmkArn, tenantID, columnName string) (plaintext []byte, ciphertext []byte, err error) {
	const op = "GenerateDataKey"

	input := &awskms.GenerateDataKeyInput{
		KeyId:   aws.String(cmkArn),
		KeySpec: types.DataKeySpecAes256,
		EncryptionContext: map[string]string{
			"neksur:tenant": tenantID,
			"neksur:column": columnName,
		},
	}

	resp, err := c.kms.GenerateDataKey(ctx, input)
	if err != nil {
		return nil, nil, fmt.Errorf("kms: %s: %w", op, err)
	}
	if len(resp.Plaintext) == 0 {
		return nil, nil, fmt.Errorf("kms: %s: empty plaintext DEK returned", op)
	}
	if len(resp.CiphertextBlob) == 0 {
		return nil, nil, fmt.Errorf("kms: %s: empty ciphertext DEK returned", op)
	}

	// Return copies to prevent the caller from accidentally sharing
	// the underlying slice with the SDK's response buffer.
	plainOut := make([]byte, len(resp.Plaintext))
	copy(plainOut, resp.Plaintext)
	cipherOut := make([]byte, len(resp.CiphertextBlob))
	copy(cipherOut, resp.CiphertextBlob)

	return plainOut, cipherOut, nil
}
