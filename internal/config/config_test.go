package config

import (
	"os"
	"testing"
	"time"
)

func TestDefaultUpdateRepositoryMatchesReleasePublisher(t *testing.T) {
	t.Setenv("WINDLASS_UPDATE_REPO", "")
	t.Setenv("WINDLASS_LOG_LEVEL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateRepo != "Sulaiman-Dauda/windlass" {
		t.Fatalf("UpdateRepo = %q", cfg.UpdateRepo)
	}
}

// Compose, Git and plugins inherit the process environment, so the update
// token must be gone from it once read.
func TestLoadKeepsUpdateTokenOutOfChildEnvironments(t *testing.T) {
	t.Setenv("WINDLASS_UPDATE_TOKEN", "test-not-a-token")
	t.Setenv("WINDLASS_LOG_LEVEL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpdateToken != "test-not-a-token" {
		t.Errorf("UpdateToken = %q", cfg.UpdateToken)
	}
	if _, ok := os.LookupEnv("WINDLASS_UPDATE_TOKEN"); ok {
		t.Error("WINDLASS_UPDATE_TOKEN is still in the environment")
	}
}

func TestImageUpdateInterval(t *testing.T) {
	for _, tc := range []struct {
		value   string
		want    time.Duration
		invalid bool
	}{
		{"", 24 * time.Hour, false},
		{"6h", 6 * time.Hour, false},
		{"1h", time.Hour, false},
		{"59m", 0, true},
		{"-1h", 0, true},
		{"daily", 0, true},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("WINDLASS_IMAGE_UPDATE_INTERVAL", tc.value)
			t.Setenv("WINDLASS_LOG_LEVEL", "")
			cfg, err := Load()
			if (err != nil) != tc.invalid {
				t.Fatalf("Load error = %v", err)
			}
			if !tc.invalid && cfg.ImageUpdateInterval != tc.want {
				t.Fatalf("interval = %v, want %v", cfg.ImageUpdateInterval, tc.want)
			}
		})
	}
}
