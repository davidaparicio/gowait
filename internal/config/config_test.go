package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load([]string{"-backend", "http://localhost:9000"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q, want :8080", cfg.Listen)
	}
	if cfg.Capacity != 100 {
		t.Errorf("Capacity = %d, want 100", cfg.Capacity)
	}
	if cfg.InactivityTTL != 60*time.Second {
		t.Errorf("InactivityTTL = %s, want 60s", cfg.InactivityTTL)
	}
	if cfg.BackendURL.Host != "localhost:9000" {
		t.Errorf("BackendURL.Host = %q", cfg.BackendURL.Host)
	}
}

func TestLoadRequiresBackend(t *testing.T) {
	if _, err := Load(nil); err == nil {
		t.Fatal("expected error when backend is missing")
	}
}

func TestLoadRejectsInvalidBackend(t *testing.T) {
	for _, b := range []string{"not-a-url", "://nohost", "/relative/path"} {
		if _, err := Load([]string{"-backend", b}); err == nil {
			t.Errorf("backend %q accepted", b)
		}
	}
}

func TestLoadEnvFallback(t *testing.T) {
	t.Setenv("GOWAIT_CAPACITY", "7")
	t.Setenv("GOWAIT_INACTIVITY_TTL", "2m")
	cfg, err := Load([]string{"-backend", "http://b:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity != 7 {
		t.Errorf("Capacity = %d, want 7 (from env)", cfg.Capacity)
	}
	if cfg.InactivityTTL != 2*time.Minute {
		t.Errorf("InactivityTTL = %s, want 2m (from env)", cfg.InactivityTTL)
	}
}

func TestLoadFlagBeatsEnv(t *testing.T) {
	t.Setenv("GOWAIT_CAPACITY", "7")
	cfg, err := Load([]string{"-backend", "http://b:1", "-capacity", "3"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Capacity != 3 {
		t.Errorf("Capacity = %d, want 3 (flag beats env)", cfg.Capacity)
	}
}

func TestValidateConstraints(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"capacity < 1", []string{"-backend", "http://b:1", "-capacity", "0"}},
		{"inactivity <= poll", []string{"-backend", "http://b:1", "-inactivity-ttl", "3s", "-poll-interval", "3s"}},
		{"queue < 2x poll", []string{"-backend", "http://b:1", "-queue-ttl", "5s", "-poll-interval", "3s"}},
	}
	for _, tc := range cases {
		if _, err := Load(tc.args); err == nil {
			t.Errorf("%s: expected validation error", tc.name)
		}
	}
}
