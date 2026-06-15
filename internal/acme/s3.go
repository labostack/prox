package acme

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	mathrand "math/rand"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/caddyserver/certmagic"

	"github.com/labostack/prox/internal/config"
)

// lockTTL is how long an S3-based advisory lock is valid before
// it can be considered stale and overridden by another caller.
const lockTTL = 5 * time.Minute

// lockPollInterval is the base interval between lock acquisition attempts.
const lockPollInterval = 1 * time.Second

// lockPrefix is appended to the storage prefix for lock objects.
const lockPrefix = ".locks/"

// S3Storage implements certmagic.Storage and certmagic.Locker
// using an S3-compatible object storage backend.
type S3Storage struct {
	client   *s3.Client
	bucket   string
	prefix   string
	holderID string

	// mu protects heldLocks.
	mu        sync.Mutex
	heldLocks map[string]bool

	// keyMu provides per-key serialization of lock acquisition attempts
	// within the same process. Each key gets its own mutex so that one
	// domain's slow lock acquisition doesn't block other domains.
	keyMu keyMutexMap
}

// keyMutexMap provides per-key mutexes for serializing lock operations.
type keyMutexMap struct {
	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock acquires the mutex for the given key, creating it if needed.
func (m *keyMutexMap) lock(key string) {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*sync.Mutex)
	}
	km, ok := m.locks[key]
	if !ok {
		km = &sync.Mutex{}
		m.locks[key] = km
	}
	m.mu.Unlock()
	km.Lock()
}

// unlock releases the mutex for the given key.
func (m *keyMutexMap) unlock(key string) {
	m.mu.Lock()
	km, ok := m.locks[key]
	m.mu.Unlock()
	if ok {
		km.Unlock()
	}
}

// Interface guards.
var (
	_ certmagic.Storage = (*S3Storage)(nil)
	_ certmagic.Locker  = (*S3Storage)(nil)
)

// NewS3Storage creates an S3Storage from the ACME S3 config.
func NewS3Storage(cfg *config.ACMES3Config) (*S3Storage, error) {
	region := cfg.Region
	if region == "" {
		region = "us-east-1"
	}

	opts := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(region),
	}

	// Non-AWS providers may not implement automatic checksums.
	if cfg.Endpoint != "" {
		opts = append(opts, awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))
	}

	// Static credentials take precedence over the default chain.
	if cfg.AccessKey != "" && cfg.SecretKey != "" {
		opts = append(opts, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.AccessKey, cfg.SecretKey, ""),
		))
	}

	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), opts...)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
		})
	}
	if cfg.UsePathStyle {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.UsePathStyle = true
		})
	}

	client := s3.NewFromConfig(awsCfg, s3Opts...)

	prefix := cfg.Prefix
	// Allow empty prefix (bucket root). Only normalize non-empty prefixes.
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}

	// Generate a unique holder ID for lock ownership.
	holderID, err := generateHolderID()
	if err != nil {
		return nil, fmt.Errorf("generating lock holder ID: %w", err)
	}

	slog.Debug("s3 storage initialized",
		"bucket", cfg.Bucket,
		"prefix", prefix,
		"region", region,
		"endpoint", cfg.Endpoint,
		"path_style", cfg.UsePathStyle,
	)

	return &S3Storage{
		client:    client,
		bucket:    cfg.Bucket,
		prefix:    prefix,
		holderID:  holderID,
		heldLocks: make(map[string]bool),
	}, nil
}

// key prepends the storage prefix to a certmagic key.
func (s *S3Storage) key(k string) string {
	return s.prefix + k
}

// lockKey returns the S3 key for an advisory lock.
func (s *S3Storage) lockKey(k string) string {
	return s.prefix + lockPrefix + k
}

// Store puts a value into S3.
func (s *S3Storage) Store(ctx context.Context, key string, value []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
		Body:   bytes.NewReader(value),
	})
	if err != nil {
		return fmt.Errorf("s3 store %q: %w", key, err)
	}
	return nil
}

// Load retrieves a value from S3.
func (s *S3Storage) Load(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return nil, fs.ErrNotExist
		}
		return nil, fmt.Errorf("s3 load %q: %w", key, err)
	}
	defer out.Body.Close()

	data, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("s3 load %q: reading body: %w", key, err)
	}
	return data, nil
}

// Delete removes a value from S3.
func (s *S3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		return fmt.Errorf("s3 delete %q: %w", key, err)
	}
	return nil
}

// Exists checks whether a key exists in S3.
func (s *S3Storage) Exists(ctx context.Context, key string) bool {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	return err == nil
}

// List returns all keys matching a prefix.
func (s *S3Storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	fullPrefix := s.key(prefix)

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(fullPrefix),
	}

	// When non-recursive, use "/" as delimiter to list only immediate children.
	if !recursive {
		input.Delimiter = aws.String("/")
	}

	var keys []string
	paginator := s3.NewListObjectsV2Paginator(s.client, input)
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3 list %q: %w", prefix, err)
		}

		for _, obj := range page.Contents {
			// Strip the storage prefix to return certmagic-relative keys.
			k := strings.TrimPrefix(aws.ToString(obj.Key), s.prefix)
			if k != "" {
				keys = append(keys, k)
			}
		}

		// Include "directory" prefixes for non-recursive listing.
		if !recursive {
			for _, cp := range page.CommonPrefixes {
				k := strings.TrimPrefix(aws.ToString(cp.Prefix), s.prefix)
				k = strings.TrimSuffix(k, "/")
				if k != "" {
					keys = append(keys, k)
				}
			}
		}
	}

	return keys, nil
}

// Stat returns information about a key in S3.
func (s *S3Storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(s.key(key)),
	})
	if err != nil {
		if isNotFound(err) {
			return certmagic.KeyInfo{}, fs.ErrNotExist
		}
		return certmagic.KeyInfo{}, fmt.Errorf("s3 stat %q: %w", key, err)
	}

	modified := time.Time{}
	if out.LastModified != nil {
		modified = *out.LastModified
	}

	size := int64(0)
	if out.ContentLength != nil {
		size = *out.ContentLength
	}

	return certmagic.KeyInfo{
		Key:        key,
		Modified:   modified,
		Size:       size,
		IsTerminal: !strings.HasSuffix(key, "/"),
	}, nil
}

// Lock acquires an advisory lock on a key using S3 objects.
// It polls until the lock is acquired or the context is cancelled.
// Lock acquisition is serialized per-key within the same process to avoid
// concurrent PutObject calls to the same key (prevents 429 rate limits)
// while allowing independent domains to lock in parallel.
func (s *S3Storage) Lock(ctx context.Context, key string) error {
	// Serialize lock attempts per key to prevent thundering herd on S3 —
	// multiple goroutines hitting PutObject for the same lock key
	// simultaneously causes rate limiting on some providers.
	s.keyMu.lock(key)
	defer s.keyMu.unlock(key)

	lk := s.lockKey(key)
	start := time.Now()

	for {
		// Check if there's an existing lock.
		acquired, err := s.tryLock(ctx, lk)
		if err != nil {
			return fmt.Errorf("s3 lock %q: %w", key, err)
		}
		if acquired {
			s.mu.Lock()
			s.heldLocks[key] = true
			s.mu.Unlock()

			slog.Debug("s3 lock acquired",
				"key", key,
				"holder", s.holderID,
				"waited", time.Since(start).Round(time.Millisecond),
			)
			return nil
		}

		// Wait before retrying with jitter to avoid synchronized retries.
		jitter := time.Duration(mathrand.Int63n(int64(lockPollInterval / 2)))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(lockPollInterval + jitter):
		}
	}
}

// Unlock releases an advisory lock on a key.
func (s *S3Storage) Unlock(ctx context.Context, key string) error {
	s.mu.Lock()
	held := s.heldLocks[key]
	delete(s.heldLocks, key)
	s.mu.Unlock()

	if !held {
		return nil
	}

	lk := s.lockKey(key)
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(lk),
	})
	if err != nil {
		return fmt.Errorf("s3 unlock %q: %w", key, err)
	}

	slog.Debug("s3 lock released", "key", key)
	return nil
}

// tryLock attempts a single lock acquisition. Returns true if the lock
// was acquired, false if it's held by another process.
func (s *S3Storage) tryLock(ctx context.Context, lockKey string) (bool, error) {
	// Check existing lock.
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(lockKey),
	})
	if err != nil {
		if isNotFound(err) {
			// No lock exists — try to create one.
			return s.createLock(ctx, lockKey)
		}
		return false, err
	}

	// Lock exists — check if it's expired.
	expiresStr := ""
	if out.Metadata != nil {
		expiresStr = out.Metadata["lock-expires"]
	}
	if expiresStr != "" {
		expires, err := time.Parse(time.RFC3339, expiresStr)
		if err == nil && time.Now().After(expires) {
			// Stale lock — override it.
			slog.Debug("s3 lock expired, overriding",
				"key", lockKey,
				"expired_at", expiresStr,
			)
			return s.createLock(ctx, lockKey)
		}
	}

	// Check if we already hold this lock (re-entrant).
	if out.Metadata != nil && out.Metadata["lock-holder"] == s.holderID {
		return true, nil
	}

	return false, nil
}

// createLock writes a lock object with holder and expiry metadata.
func (s *S3Storage) createLock(ctx context.Context, lockKey string) (bool, error) {
	expires := time.Now().Add(lockTTL).UTC().Format(time.RFC3339)

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(lockKey),
		Body:   bytes.NewReader([]byte(s.holderID)),
		Metadata: map[string]string{
			"lock-holder":  s.holderID,
			"lock-expires": expires,
		},
	})
	if err != nil {
		return false, fmt.Errorf("creating lock: %w", err)
	}
	return true, nil
}

// isNotFound returns true if the error indicates an S3 object was not found.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}

	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return true
	}

	// HeadObject returns a generic error with "NotFound" status —
	// match on the HTTP status code string.
	if strings.Contains(err.Error(), "NotFound") ||
		strings.Contains(err.Error(), "404") {
		return true
	}

	return false
}

// generateHolderID creates a random hex string for lock ownership.
func generateHolderID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
