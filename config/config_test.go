package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// Load tolerates a missing file and LoadRequired does not. The difference is the whole point: a bot
// whose config decides what gets sent must not run on defaults because a migration dropped the file.
func TestLoadRequiredRejectsAMissingFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := NaggerConfig{QuotaEnabled: true}

	if err := Load("nagger", &cfg); err != nil {
		t.Fatalf("Load on a missing file returned %v; want nil (optional tuning file)", err)
	}
	if err := LoadRequired("nagger", &cfg); err == nil {
		t.Fatal("LoadRequired accepted a missing file; want a named error")
	}
}

func TestLoadRequiredAcceptsAPresentFile(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := NaggerConfig{QuotaEnabled: true}
	if err := Save("nagger", NaggerConfig{QuotaEnabled: false}); err != nil {
		t.Fatal(err)
	}
	if err := LoadRequired("nagger", &cfg); err != nil {
		t.Fatalf("LoadRequired on a present file: %v", err)
	}
	if cfg.QuotaEnabled {
		t.Error("quota_enabled did not load as false")
	}
}

func TestLoadRequiredNamesThePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	err := LoadRequired("nagger", &NaggerConfig{})
	if err == nil {
		t.Fatal("want an error")
	}
	want := filepath.Join(dir, "guild", "nagger.json")
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("error %q does not name %q", got, want)
	}
}
