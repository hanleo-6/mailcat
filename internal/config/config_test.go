package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDefaultAndXDGPaths(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DOWNLOAD_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")

	cfgPath, err := Path()
	if err != nil {
		t.Fatalf("Path() error: %v", err)
	}
	wantCfgPath := filepath.Join(tmp, ".config", "mailcat", "config.json")
	if cfgPath != wantCfgPath {
		t.Fatalf("Path() = %q, want %q", cfgPath, wantCfgPath)
	}

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default() error: %v", err)
	}
	if len(cfg.Listen) != 1 || cfg.Listen[0] != "127.0.0.1:8766" {
		t.Errorf("Listen = %v, want [127.0.0.1:8766]", cfg.Listen)
	}
	if want := filepath.Join(tmp, "Downloads", "mailcat"); cfg.InboxDir != want {
		t.Errorf("InboxDir = %q, want %q", cfg.InboxDir, want)
	}
	if want := filepath.Join(tmp, ".local", "state", "mailcat", "outbox"); cfg.OutboxDir != want {
		t.Errorf("OutboxDir = %q, want %q", cfg.OutboxDir, want)
	}
	if cfg.MaxUploadBytes != 4*1024*1024*1024 {
		t.Errorf("MaxUploadBytes = %d, want 4 GiB", cfg.MaxUploadBytes)
	}
	if cfg.OutboxTTL.Duration != 24*time.Hour {
		t.Errorf("OutboxTTL = %v, want 24h", cfg.OutboxTTL.Duration)
	}
	if cfg.PruneInterval.Duration != 15*time.Minute {
		t.Errorf("PruneInterval = %v, want 15m", cfg.PruneInterval.Duration)
	}
	if cfg.DeleteOnPull {
		t.Errorf("DeleteOnPull = true, want false")
	}
	if cfg.HEICFormat != "png" {
		t.Errorf("HEICFormat = %q, want png", cfg.HEICFormat)
	}
	if cfg.JPEGQuality != 90 {
		t.Errorf("JPEGQuality = %d, want 90", cfg.JPEGQuality)
	}
	if cfg.KeepHEIC {
		t.Errorf("KeepHEIC = true, want false")
	}
	if cfg.ConvertWorkers != 1 {
		t.Errorf("ConvertWorkers = %d, want 1", cfg.ConvertWorkers)
	}

	// Custom XDG env vars override defaults.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmp, "cfg"))
	t.Setenv("XDG_DOWNLOAD_DIR", filepath.Join(tmp, "dl"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "state"))

	cfgPath, err = Path()
	if err != nil {
		t.Fatalf("Path() with XDG error: %v", err)
	}
	if want := filepath.Join(tmp, "cfg", "mailcat", "config.json"); cfgPath != want {
		t.Errorf("Path() = %q, want %q", cfgPath, want)
	}
	cfg, err = Default()
	if err != nil {
		t.Fatalf("Default() with XDG error: %v", err)
	}
	if want := filepath.Join(tmp, "dl", "mailcat"); cfg.InboxDir != want {
		t.Errorf("InboxDir = %q, want %q", cfg.InboxDir, want)
	}
	if want := filepath.Join(tmp, "state", "mailcat", "outbox"); cfg.OutboxDir != want {
		t.Errorf("OutboxDir = %q, want %q", cfg.OutboxDir, want)
	}
}

func TestLoadPermissionCheck(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "config.json")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.Token = "test-secret-token"

	if err := Write(path, cfg, false); err != nil {
		t.Fatalf("Write(): %v", err)
	}

	// 0600 should load cleanly.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load(0600) failed: %v", err)
	}
	if loaded.Token != cfg.Token {
		t.Errorf("loaded token = %q, want %q", loaded.Token, cfg.Token)
	}

	// Non-0600 modes must be rejected.
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o660, 0o700} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatalf("Chmod(%04o): %v", mode, err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("Load(%04o) succeeded, want permission error", mode)
		}
	}
}

func TestWriteForceBehavior(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "mailcat", "config.json")

	cfg, err := Default()
	if err != nil {
		t.Fatalf("Default(): %v", err)
	}
	cfg.Token = "first-token"

	if err := Write(path, cfg, false); err != nil {
		t.Fatalf("initial Write(): %v", err)
	}

	// Without --force, Write must fail with ErrExist.
	cfg.Token = "second-token"
	if err := Write(path, cfg, false); !errors.Is(err, os.ErrExist) {
		t.Fatalf("second Write(force=false) err = %v, want os.ErrExist", err)
	}

	// With --force, Write overwrites and preserves 0600.
	if err := Write(path, cfg, true); err != nil {
		t.Fatalf("Write(force=true) error: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after force overwrite: %v", err)
	}
	if loaded.Token != "second-token" {
		t.Errorf("loaded token = %q, want second-token", loaded.Token)
	}
}

func TestValidateListenAddresses(t *testing.T) {
	cases := []struct {
		name    string
		listen  []string
		wantErr bool
	}{
		{"loopback", []string{"127.0.0.1:8765"}, false},
		{"multiple specific", []string{"127.0.0.1:8765", "192.168.1.20:8765"}, false},
		{"ipv6 loopback", []string{"[::1]:8765"}, false},
		{"empty list", []string{}, true},
		{"all interfaces shorthand", []string{":8765"}, true},
		{"ipv4 wildcard", []string{"0.0.0.0:8765"}, true},
		{"ipv6 wildcard", []string{"[::]:8765"}, true},
		{"missing port", []string{"127.0.0.1"}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Default()
			if err != nil {
				t.Fatalf("Default(): %v", err)
			}
			cfg.Token = "tok"
			cfg.Listen = tc.listen

			err = cfg.Validate()
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%v) = nil, want error", tc.listen)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%v) = %v, want nil", tc.listen, err)
			}
		})
	}
}

func TestGenerateToken(t *testing.T) {
	tok, err := GenerateToken()
	if err != nil {
		t.Fatalf("GenerateToken(): %v", err)
	}
	// 32 bytes base64url without padding is 43 characters.
	if len(tok) != 43 {
		t.Errorf("len(token) = %d, want 43", len(tok))
	}
	if strings.ContainsAny(tok, "+/=") {
		t.Errorf("token %q is not unpadded base64url", tok)
	}
}
