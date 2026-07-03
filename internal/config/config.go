// Package config loads gowait configuration from flags with environment
// variable fallbacks (flag > env > default).
package config

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/davidaparicio/gowait/internal/waitpage"
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
	Store         string // "memory" or "valkey"
	ValkeyURL     string
	ValkeyPrefix  string
	Metrics       bool
	WaitTitle     string
	WaitBrand     string
	WaitMessage   string
	WaitLang      string // "en" or "fr"
	WaitTemplate  string // path to a custom waiting page template
	ProbeURL      string // backend health URL; empty disables the prober
	ProbeInterval time.Duration
	ProbeMin      int // capacity floor for the prober
	ProbeMax      int // capacity ceiling; 0 = the -capacity value
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
	storeKind := fs.String("store", envOr("GOWAIT_STORE", "memory"), "state store: memory or valkey")
	valkeyURL := fs.String("valkey-url", envOr("GOWAIT_VALKEY_URL", ""), "Valkey/Redis URL (valkey://host:port) when -store=valkey")
	valkeyPrefix := fs.String("valkey-prefix", envOr("GOWAIT_VALKEY_PREFIX", "gowait:"), "key prefix in Valkey (use a {hash-tag}: prefix on Valkey Cluster)")
	metricsOn := fs.Bool("metrics", envOrBool("GOWAIT_METRICS", true), "expose Prometheus metrics at /gowait/metrics")
	waitTitle := fs.String("wait-title", envOr("GOWAIT_WAIT_TITLE", ""), "waiting page title (localized default if empty)")
	waitBrand := fs.String("wait-brand", envOr("GOWAIT_WAIT_BRAND", ""), "brand name shown on the waiting page (hidden if empty)")
	waitMessage := fs.String("wait-message", envOr("GOWAIT_WAIT_MESSAGE", ""), "waiting page message (localized default if empty)")
	waitLang := fs.String("wait-lang", envOr("GOWAIT_WAIT_LANG", "en"), "waiting page language: "+strings.Join(waitpage.Langs(), " or "))
	waitTemplate := fs.String("wait-template", envOr("GOWAIT_WAIT_TEMPLATE", ""), "path to a custom waiting page html/template (embedded page if empty)")
	probeURL := fs.String("probe-url", envOr("GOWAIT_PROBE_URL", ""), "backend health URL enabling the adaptive-capacity prober (off if empty)")
	probeInterval := fs.Duration("probe-interval", envOrDuration("GOWAIT_PROBE_INTERVAL", 10*time.Second), "health probe cadence, also the probe timeout")
	probeMin := fs.Int("probe-min", envOrInt("GOWAIT_PROBE_MIN", 1), "capacity floor for the prober")
	probeMax := fs.Int("probe-max", envOrInt("GOWAIT_PROBE_MAX", 0), "capacity ceiling for the prober (0 = the -capacity value)")

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
		Store:         *storeKind,
		ValkeyURL:     *valkeyURL,
		ValkeyPrefix:  *valkeyPrefix,
		Metrics:       *metricsOn,
		WaitTitle:     *waitTitle,
		WaitBrand:     *waitBrand,
		WaitMessage:   *waitMessage,
		WaitLang:      *waitLang,
		WaitTemplate:  *waitTemplate,
		ProbeURL:      *probeURL,
		ProbeInterval: *probeInterval,
		ProbeMin:      *probeMin,
		ProbeMax:      *probeMax,
	}
	if cfg.ProbeMax == 0 {
		cfg.ProbeMax = cfg.Capacity
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
	switch c.Store {
	case "memory":
	case "valkey":
		if c.ValkeyURL == "" {
			return fmt.Errorf("valkey store requires -valkey-url or GOWAIT_VALKEY_URL")
		}
	default:
		return fmt.Errorf("unknown store %q: must be memory or valkey", c.Store)
	}
	if !waitpage.SupportedLang(c.WaitLang) {
		return fmt.Errorf("unknown wait-lang %q: must be %s", c.WaitLang, strings.Join(waitpage.Langs(), " or "))
	}
	if c.ProbeURL != "" {
		u, err := url.Parse(c.ProbeURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("invalid probe-url %q: must be absolute, e.g. http://backend:8080/healthz", c.ProbeURL)
		}
		if c.ProbeInterval <= 0 {
			return fmt.Errorf("probe-interval must be > 0, got %s", c.ProbeInterval)
		}
		if c.ProbeMin < 1 {
			return fmt.Errorf("probe-min must be >= 1, got %d", c.ProbeMin)
		}
		if c.ProbeMax < c.ProbeMin {
			return fmt.Errorf("probe-max (%d) must be >= probe-min (%d)", c.ProbeMax, c.ProbeMin)
		}
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
