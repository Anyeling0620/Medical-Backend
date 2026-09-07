package utils

import (
	"Medical-Web-Backend/internal/config"
	"strings"
)

func MinioPublicURL(cfg config.Config) string {
	scheme := "http"

	if cfg.MinIO.UseSSL {
		scheme = "https"
	}

	endpoint := strings.TrimRight(cfg.MinIO.Endpoint, "/")

	// Allow MINIO_ENDPOINT to be configured either as host:port or as a full URL.
	if !strings.HasPrefix(endpoint, "http://") &&
		!strings.HasPrefix(endpoint, "https://") {
		endpoint = scheme + "://" + endpoint
	}

	return endpoint + "/" + strings.Trim(cfg.MinIO.Bucket, "/")
}
