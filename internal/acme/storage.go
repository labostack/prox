package acme

import (
	"fmt"
	"path/filepath"

	"github.com/caddyserver/certmagic"

	"github.com/labostack/prox/internal/config"
)

// buildStorage creates the appropriate certmagic.Storage backend
// based on the ACME config's StorageType field.
func buildStorage(cfg *config.ACMEConfig, configDir string) (certmagic.Storage, error) {
	switch cfg.StorageType {
	case "", "file":
		path := resolveStoragePath(cfg.Storage, configDir)
		return &certmagic.FileStorage{Path: path}, nil
	case "s3":
		if cfg.S3 == nil {
			return nil, fmt.Errorf("acme.s3 config is required when storage_type is \"s3\"")
		}
		return NewS3Storage(cfg.S3)
	default:
		return nil, fmt.Errorf("unknown acme.storage_type %q", cfg.StorageType)
	}
}

// resolveStoragePath returns the storage directory for ACME data.
// If configured is empty, defaults to "acme/" relative to configDir.
// Relative paths are resolved relative to configDir.
func resolveStoragePath(configured, configDir string) string {
	if configured != "" {
		if filepath.IsAbs(configured) {
			return configured
		}
		return filepath.Join(configDir, configured)
	}
	return filepath.Join(configDir, "acme")
}
