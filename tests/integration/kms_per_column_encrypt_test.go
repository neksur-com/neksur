//go:build integration

// kms_per_column_encrypt_test.go — validates the Go-side KMS client and
// per-batch DEK cache (Pitfall 10 mitigation, D-2.07).
//
// This test uses LocalStack (from Phase1Fixture, with KMS service enabled)
// to exercise the AWS KMS API without real AWS credentials. It verifies:
//  1. BatchCache returns a cached DEK on second+ call for the same
//     (tenant, column, batchID) — exactly ONE GenerateDataKey API call.
//  2. AES-256-GCM encrypt + decrypt roundtrip with the DEK succeeds.
//
// Container infrastructure: LocalStack (already in Phase1Fixture, extended
// to include KMS service in testfixture/localstack.go).
package integration

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awskms "github.com/aws/aws-sdk-go-v2/service/kms"
	"github.com/aws/aws-sdk-go-v2/service/kms/types"
	"github.com/neksur-com/neksur/internal/crypto/kms"
)

// TestKMS_PerColumnEncrypt boots Phase1Fixture (which includes LocalStack with KMS),
// creates a CMK via LocalStack KMS, then exercises the Go-side kms.Client
// and kms.BatchCache to assert Pitfall 10 mitigation.
func TestKMS_PerColumnEncrypt(t *testing.T) {
	if testing.Short() {
		t.Skip("TestKMS_PerColumnEncrypt: requires LocalStack — skipping in short mode")
	}

	ctx := context.Background()
	fx := StartPhase1Fixture(t)
	t.Cleanup(fx.Terminate)

	kmsEndpoint := fx.LocalStack.KMSEndpoint()

	// Create a test CMK in LocalStack KMS.
	cmkArn := createTestCMK(t, ctx, kmsEndpoint)

	// Build the aws.Config pointing at LocalStack.
	awsCfg, err := localstackAWSConfig(ctx, kmsEndpoint)
	if err != nil {
		t.Fatalf("localstackAWSConfig: %v", err)
	}

	kmsClient := kms.NewClient(awsCfg)
	batchCache, err := kms.NewBatchCache(4096, 10*time.Minute)
	if err != nil {
		t.Fatalf("kms.NewBatchCache: %v", err)
	}

	const (
		tenantID   = "tenant-test-kms-01"
		columnName = "customer_email"
		batchID    = "batch-2026-05-16-001"
	)

	// Track how many real GenerateDataKey calls LocalStack receives.
	callCount := 0
	generateFn := func(tID, colName string) ([]byte, error) {
		callCount++
		plaintext, _, err := kmsClient.GenerateDataKey(ctx, cmkArn, tID, colName)
		return plaintext, err
	}

	// First call — cache miss, should invoke generateFn once.
	dek1, err := batchCache.GetOrGenerate(tenantID, columnName, batchID, generateFn)
	if err != nil {
		t.Fatalf("GetOrGenerate (first call): %v", err)
	}
	if len(dek1) != 32 {
		t.Errorf("expected 32-byte AES-256 DEK, got %d bytes", len(dek1))
	}
	if callCount != 1 {
		t.Errorf("expected exactly 1 GenerateDataKey call after first GetOrGenerate, got %d", callCount)
	}

	// Second call — same (tenant, column, batchID) — must be a cache HIT.
	dek2, err := batchCache.GetOrGenerate(tenantID, columnName, batchID, generateFn)
	if err != nil {
		t.Fatalf("GetOrGenerate (second call): %v", err)
	}
	if callCount != 1 {
		t.Errorf("Pitfall 10 violation: expected still 1 GenerateDataKey call after cache hit, got %d", callCount)
	}

	// DEKs must be byte-identical (same cached value).
	if string(dek1) != string(dek2) {
		t.Errorf("cache hit DEK differs from first DEK — cache not working")
	}

	// Third call — different batchID — must trigger a NEW GenerateDataKey.
	const batchID2 = "batch-2026-05-16-002"
	_, err = batchCache.GetOrGenerate(tenantID, columnName, batchID2, generateFn)
	if err != nil {
		t.Fatalf("GetOrGenerate (different batchID): %v", err)
	}
	if callCount != 2 {
		t.Errorf("expected 2nd GenerateDataKey call for new batchID, got total %d", callCount)
	}

	// AES-256-GCM encrypt + decrypt roundtrip with dek1.
	plaintext := []byte("customer@example.com")
	ciphertext, err := aesGCMEncrypt(dek1, plaintext)
	if err != nil {
		t.Fatalf("aesGCMEncrypt: %v", err)
	}

	decrypted, err := aesGCMDecrypt(dek1, ciphertext)
	if err != nil {
		t.Fatalf("aesGCMDecrypt: %v", err)
	}
	if string(decrypted) != string(plaintext) {
		t.Errorf("AES-256-GCM roundtrip: want %q, got %q", plaintext, decrypted)
	}

	// Verify wrong key fails decryption (sanity check).
	wrongKey := make([]byte, 32) // all zeros
	if _, err := aesGCMDecrypt(wrongKey, ciphertext); err == nil {
		t.Errorf("expected decryption with wrong key to fail, but it succeeded")
	}

	t.Logf("KMS per-column encrypt test: %d GenerateDataKey call(s), roundtrip PASS", callCount)
}

// createTestCMK creates an AES-256 symmetric CMK in LocalStack KMS and
// returns its key ARN. This is the key the integration test uses for
// GenerateDataKey calls.
func createTestCMK(t *testing.T, ctx context.Context, kmsEndpoint string) string {
	t.Helper()

	awsCfg, err := localstackAWSConfig(ctx, kmsEndpoint)
	if err != nil {
		t.Fatalf("createTestCMK: localstackAWSConfig: %v", err)
	}

	client := awskms.NewFromConfig(awsCfg)
	resp, err := client.CreateKey(ctx, &awskms.CreateKeyInput{
		Description: aws.String("neksur-test-kms-per-column-encrypt"),
		KeySpec:     types.KeySpecSymmetricDefault,
		KeyUsage:    types.KeyUsageTypeEncryptDecrypt,
	})
	if err != nil {
		t.Fatalf("createTestCMK: CreateKey: %v", err)
	}
	if resp.KeyMetadata == nil || resp.KeyMetadata.KeyId == nil {
		t.Fatalf("createTestCMK: nil KeyMetadata or KeyId in response")
	}

	arn := aws.ToString(resp.KeyMetadata.Arn)
	if arn == "" {
		arn = aws.ToString(resp.KeyMetadata.KeyId)
	}
	t.Logf("createTestCMK: created CMK %s", arn)
	return arn
}

// localstackAWSConfig builds an aws.Config pointing at LocalStack.
// Uses static credentials (LocalStack accepts any credentials).
func localstackAWSConfig(ctx context.Context, endpoint string) (aws.Config, error) {
	return config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(aws.CredentialsProviderFunc(func(_ context.Context) (aws.Credentials, error) {
			return aws.Credentials{
				AccessKeyID:     "test",
				SecretAccessKey: "test",
			}, nil
		})),
		config.WithEndpointResolverWithOptions(aws.EndpointResolverWithOptionsFunc(
			func(service, region string, opts ...interface{}) (aws.Endpoint, error) {
				return aws.Endpoint{
					URL:               endpoint,
					HostnameImmutable: true,
				}, nil
			},
		)),
	)
}

// aesGCMEncrypt encrypts plaintext with AES-256-GCM using the given key.
// Returns (nonce || ciphertext || tag) concatenated for storage.
func aesGCMEncrypt(key, plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aesGCMEncrypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesGCMEncrypt: new GCM: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("aesGCMEncrypt: read nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// aesGCMDecrypt decrypts ciphertext (nonce || ciphertext || tag) with
// AES-256-GCM using the given key.
func aesGCMDecrypt(key, ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aesGCMDecrypt: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("aesGCMDecrypt: new GCM: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return nil, fmt.Errorf("aesGCMDecrypt: ciphertext too short (%d bytes)", len(ciphertext))
	}
	return gcm.Open(nil, ciphertext[:nonceSize], ciphertext[nonceSize:], nil)
}
