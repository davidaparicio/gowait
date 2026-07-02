// Package config loads gowait configuration from flags with environment
// variable fallbacks (flag > env > default).
package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"time"
)

// Config holds all runtime settings for gowait.
type Config struct {
	Listen        string
	BackendURL    *url.URL
	Capacity      int
	InactivityTTL time.Duration
	QueueTTL      time.Duration
	PollInterval  time.Duration
	CookieSecret  string
	AdminKey      string
	CookieSecure  bool
	PreserveHost  bool
}

// Load parses args (excluding the program name) into a Config.
func Load(args []string) (*Config, error) {
	fs := flag.NewFlagSet("gowait", flag.ContinueOnError)

	listen := fs.String("listen", envOr("GOWAIT_LISTEN", ":8080"), "address to listen on")
	backend := fs.String("backend", envOr("GOWAIT_BACKEND_URL", ""), "backend URL to proxy to (required)")
	capacity := fs.Int("capacity", envOrInt("GOWAIT_CAPACITY", 100), "max concurrent active users")
	inactivityTTL := fs.Duration("inactivity-ttl", envOrDuration("GOWAIT_INACTIVITY_TTL", 60*time.Second), "sliding inactivity window for admitted users")
	queueTTL := fs.Duration("queue-ttl", envOrDuration("GOWAIT_QUEUE_TTL", 30*time.Second), "eviction window for queued users that stop polling")
	pollInterval := fs.Duration("poll-interval", envOrDuration("GOWAIT_POLL_INTERVAL", 3*time.Second), "poll interval advertised to the waiting page")
	cookieSecret := fs.String("cookie-secret", envOr("GOWAIT_COOKIE_SECRET", ""), "HMAC secret for signing cookies (random if empty)")
	adminKey := fs.String("admin-key", envOr("GOWAIT_ADMIN_KEY", ""), "secret key for admin queue bypass (disabled if empty)")
	cookieSecure := fs.Bool("cookie-secure", envOrBool("GOWAIT_COOKIE_SECURE", false), "set the Secure attribute on cookies")
	preserveHost := fs.Bool("preserve-host", envOrBool("GOWAIT_PRESERVE_HOST", false), "forward the original Host header to the backend")

	if err := fs.Parse(args); err != nil {
		return nil, err
	}

	cfg := &Config{
		Listen:        *listen,
		Capacity:      *capacity,
		InactivityTTL: *inactivityTTL,
		QueueTTL:      *queueTTL,
		PollInterval:  *pollInterval,
		CookieSecret:  *cookieSecret,
		AdminKey:      *adminKey,
		CookieSecure:  *cookieSecure,
		PreserveHost:  *preserveHost,
	}

	if *backend == "" {
		return nil, fmt.Errorf("backend URL is required (-backend or GOWAIT_BACKEND_URL)")
	}
	u, err := url.Parse(*backend)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid backend URL %q: must be absolute, e.g. http://backend:8080", *backend)
	}
	cfg.BackendURL = u

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces cross-field constraints.
func (c *Config) Validate() error {
	if c.Capacity < 1 {
		return fmt.Errorf("capacity must be >= 1, got %d", c.Capacity)
	}
	if c.PollInterval <= 0 {
		return fmt.Errorf("poll-interval must be > 0, got %s", c.PollInterval)
	}
	if c.InactivityTTL <= c.PollInterval {
		return fmt.Errorf("inactivity-ttl (%s) must be greater than poll-interval (%s)", c.InactivityTTL, c.PollInterval)
	}
	if c.QueueTTL < 2*c.PollInterval {
		return fmt.Errorf("queue-ttl (%s) must be at least 2x poll-interval (%s)", c.QueueTTL, c.PollInterval)
	}
	return nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envOrInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func envOrDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func envOrBool(key string, def bool) bool {
	if v := os.Getenv(key); v != "" {
		return v == "true" || v == "1" || v == "yes"
	}
	return def
}
