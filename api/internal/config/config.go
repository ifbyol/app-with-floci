// Package config reads the API's runtime configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
)

type Config struct {
	Addr string

	// FlociEndpoint is the AWS API endpoint - Floci's port 4566. Every
	// control-plane call (Describe*) goes here.
	FlociEndpoint string
	Region        string

	// Resource identifiers, matching what floci/init/ready.d/* creates.
	DBInstanceID string
	CacheGroupID string
	SearchDomain string

	DBUser     string
	DBPassword string
	DBName     string

	// SearchOverride replaces the endpoint DescribeDomain advertises.
	//
	// Floci hardcodes the OpenSearch endpoint to the Docker container name
	// (http://floci-opensearch-<domain>:9200), which only resolves inside the
	// Docker network. That is correct when the API itself is a container on
	// that network - the local compose stack - so the override is left unset
	// there. In Kubernetes the API is a pod and cannot resolve Docker names,
	// so it is set to http://floci:9400, the port the OpenSearch container
	// publishes on the pod IP.
	SearchOverride string

	// SearchPublishedPort is the host port Floci tells Docker to bind the
	// OpenSearch container's 9200 on. It is the first port of
	// FLOCI_SERVICES_OPENSEARCH_PROXY_BASE_PORT and deterministic for a single
	// domain; DescribeDomain never reports it, so it cannot be discovered.
	SearchPublishedPort int

	ReadyTimeoutSeconds int
	CacheTTLSeconds     int
}

func Load() (*Config, error) {
	c := &Config{
		Addr:                env("API_ADDR", ":8080"),
		FlociEndpoint:       env("AWS_ENDPOINT_URL", "http://floci:4566"),
		Region:              env("AWS_REGION", "us-east-1"),
		DBInstanceID:        env("DB_INSTANCE_ID", "flociflix-db"),
		CacheGroupID:        env("CACHE_GROUP_ID", "flociflix-cache"),
		SearchDomain:        env("SEARCH_DOMAIN", "flociflix-search"),
		DBUser:              os.Getenv("APP_DB_USER"),
		DBPassword:          os.Getenv("APP_DB_PASSWORD"),
		DBName:              os.Getenv("APP_DB_NAME"),
		SearchOverride:      os.Getenv("OPENSEARCH_ENDPOINT_OVERRIDE"),
		ReadyTimeoutSeconds: envInt("READY_TIMEOUT_SECONDS", 600),
		CacheTTLSeconds:     envInt("CACHE_TTL_SECONDS", 60),
		SearchPublishedPort: envInt("SEARCH_PUBLISHED_PORT", 9400),
	}
	if c.DBUser == "" || c.DBPassword == "" || c.DBName == "" {
		return nil, fmt.Errorf("APP_DB_USER, APP_DB_PASSWORD and APP_DB_NAME are required")
	}
	return c, nil
}

// DSN builds a libpq URL for the discovered RDS endpoint. The password is
// escaped rather than interpolated so characters like @ and / cannot corrupt
// the URL.
func (c *Config) DSN(host string, port int32) string {
	u := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.DBUser, c.DBPassword),
		Host:   fmt.Sprintf("%s:%d", host, port),
		Path:   "/" + c.DBName,
	}
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	return u.String()
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
