package utils

import (
	"Medical-Web-Backend/internal/config"
	"strings"
)

// MinioPublicURL builds the public bucket URL without creating an SDK client.
func MinioPublicURL(cfg config.Config) string {
	scheme := "http"
	if cfg.MinIO.UseSSL {
		scheme = "https"
	}
	endpoint := strings.TrimRight(cfg.MinIO.Endpoint, "/")
	if !strings.HasPrefix(endpoint, "http://") && !strings.HasPrefix(endpoint, "https://") {
		endpoint = scheme + "://" + endpoint
	}
	return endpoint + "/" + strings.Trim(cfg.MinIO.Bucket, "/")
}
