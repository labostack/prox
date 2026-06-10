package acme

import (
	"context"
	"testing"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/labostack/prox/internal/config"
)

// --- NewS3Storage ---

func TestNewS3Storage_Defaults(t *testing.T) {
	cfg := &config.ACMES3Config{
		Bucket:    "test-bucket",
		AccessKey: "AKID",
		SecretKey: "secret",
	}

	s, err := NewS3Storage(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.bucket != "test-bucket" {
		t.Errorf("bucket = %q, want %q", s.bucket, "test-bucket")
	}
	if s.prefix != "acme/" {
		t.Errorf("prefix = %q, want %q", s.prefix, "acme/")
	}
	if s.holderID == "" {
		t.Error("holderID should not be empty")
	}
}

func TestNewS3Storage_CustomPrefix(t *testing.T) {
	cfg := &config.ACMES3Config{
		Bucket:    "test-bucket",
		Prefix:    "custom/prefix",
		AccessKey: "AKID",
		SecretKey: "secret",
	}

	s, err := NewS3Storage(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Prefix should have trailing slash.
	if s.prefix != "custom/prefix/" {
		t.Errorf("prefix = %q, want %q", s.prefix, "custom/prefix/")
	}
}

func TestNewS3Storage_PrefixTrailingSlash(t *testing.T) {
	cfg := &config.ACMES3Config{
		Bucket:    "test-bucket",
		Prefix:    "my-prefix/",
		AccessKey: "AKID",
		SecretKey: "secret",
	}

	s, err := NewS3Storage(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if s.prefix != "my-prefix/" {
		t.Errorf("prefix = %q, want %q", s.prefix, "my-prefix/")
	}
}

// --- Key construction ---

func TestS3Storage_Key(t *testing.T) {
	s := &S3Storage{prefix: "acme/"}

	tests := []struct {
		input string
		want  string
	}{
		{"certificates/example.com/cert.pem", "acme/certificates/example.com/cert.pem"},
		{"acme-v02/account.json", "acme/acme-v02/account.json"},
	}

	for _, tt := range tests {
		got := s.key(tt.input)
		if got != tt.want {
			t.Errorf("key(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestS3Storage_LockKey(t *testing.T) {
	s := &S3Storage{prefix: "acme/"}

	got := s.lockKey("certificates/example.com")
	want := "acme/.locks/certificates/example.com"
	if got != want {
		t.Errorf("lockKey = %q, want %q", got, want)
	}
}

// --- Interface compliance ---

func TestS3Storage_ImplementsStorage(t *testing.T) {
	var _ certmagic.Storage = (*S3Storage)(nil)
}

func TestS3Storage_ImplementsLocker(t *testing.T) {
	var _ certmagic.Locker = (*S3Storage)(nil)
}

// --- isNotFound ---

func TestIsNotFound_NoSuchKey(t *testing.T) {
	// Simulate an error message containing "NotFound".
	err := &mockError{msg: "operation error S3: HeadObject, https response error StatusCode: 404, NotFound"}
	if !isNotFound(err) {
		t.Error("expected isNotFound to return true for NotFound error")
	}
}

func TestIsNotFound_OtherError(t *testing.T) {
	err := &mockError{msg: "access denied"}
	if isNotFound(err) {
		t.Error("expected isNotFound to return false for non-404 error")
	}
}

type mockError struct{ msg string }

func (e *mockError) Error() string { return e.msg }

// --- generateHolderID ---

func TestGenerateHolderID_Uniqueness(t *testing.T) {
	id1, err := generateHolderID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	id2, err := generateHolderID()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if id1 == id2 {
		t.Error("two generated IDs should not be equal")
	}

	if len(id1) != 16 { // 8 bytes = 16 hex chars
		t.Errorf("holder ID length = %d, want 16", len(id1))
	}
}

// --- buildStorage factory ---

func TestBuildStorage_DefaultFile(t *testing.T) {
	cfg := &config.ACMEConfig{
		Email: "test@example.com",
	}

	storage, err := buildStorage(cfg, "/etc/prox")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs, ok := storage.(*certmagic.FileStorage)
	if !ok {
		t.Fatalf("expected *certmagic.FileStorage, got %T", storage)
	}
	if fs.Path != "/etc/prox/acme" {
		t.Errorf("path = %q, want %q", fs.Path, "/etc/prox/acme")
	}
}

func TestBuildStorage_ExplicitFile(t *testing.T) {
	cfg := &config.ACMEConfig{
		Email:       "test@example.com",
		StorageType: "file",
		Storage:     "/custom/certs",
	}

	storage, err := buildStorage(cfg, "/etc/prox")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	fs, ok := storage.(*certmagic.FileStorage)
	if !ok {
		t.Fatalf("expected *certmagic.FileStorage, got %T", storage)
	}
	if fs.Path != "/custom/certs" {
		t.Errorf("path = %q, want %q", fs.Path, "/custom/certs")
	}
}

func TestBuildStorage_S3(t *testing.T) {
	cfg := &config.ACMEConfig{
		Email:       "test@example.com",
		StorageType: "s3",
		S3: &config.ACMES3Config{
			Bucket:    "my-certs",
			AccessKey: "AKID",
			SecretKey: "secret",
		},
	}

	storage, err := buildStorage(cfg, "/etc/prox")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	s3s, ok := storage.(*S3Storage)
	if !ok {
		t.Fatalf("expected *S3Storage, got %T", storage)
	}
	if s3s.bucket != "my-certs" {
		t.Errorf("bucket = %q, want %q", s3s.bucket, "my-certs")
	}
}

func TestBuildStorage_S3MissingConfig(t *testing.T) {
	cfg := &config.ACMEConfig{
		Email:       "test@example.com",
		StorageType: "s3",
	}

	_, err := buildStorage(cfg, "/etc/prox")
	if err == nil {
		t.Fatal("expected error for S3 without config")
	}
}

func TestBuildStorage_UnknownType(t *testing.T) {
	cfg := &config.ACMEConfig{
		Email:       "test@example.com",
		StorageType: "redis",
	}

	_, err := buildStorage(cfg, "/etc/prox")
	if err == nil {
		t.Fatal("expected error for unknown storage type")
	}
}

// --- Lock held tracking ---

func TestS3Storage_UnlockNotHeld(t *testing.T) {
	s := &S3Storage{
		heldLocks: make(map[string]bool),
	}

	// Unlocking a key that was never locked should be a no-op.
	err := s.Unlock(context.Background(), "some-key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- lockTTL constant ---

func TestLockTTL_Value(t *testing.T) {
	if lockTTL != 5*time.Minute {
		t.Errorf("lockTTL = %v, want 5m", lockTTL)
	}
}
