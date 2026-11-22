package config

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	DefaultMaxUploadBytes = 4 * 1024 * 1024 * 1024 // 4 GiB
	DefaultOutboxTTL      = 24 * time.Hour
	DefaultPruneInterval  = 15 * time.Minute
	DefaultHEICFormat     = "png"
	DefaultJPEGQuality    = 90
	DefaultConvertWorkers = 1
)

// Duration wraps time.Duration for JSON encoding using time.ParseDuration syntax.
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = parsed
	return nil
}

// Config holds the mailcat configuration (SPEC.md section 3).
type Config struct {
	Listen         []string `json:"listen"`
	Token          string   `json:"token"`
	InboxDir       string   `json:"inbox_dir"`
	OutboxDir      string   `json:"outbox_dir"`
	MaxUploadBytes int64    `json:"max_upload_bytes"`
	OutboxTTL      Duration `json:"outbox_ttl"`
	PruneInterval  Duration `json:"prune_interval"`
	DeleteOnPull   bool     `json:"delete_on_pull"`
	HEICFormat     string   `json:"heic_format"`
	JPEGQuality    int      `json:"jpeg_quality"`
	KeepHEIC       bool     `json:"keep_heic"`
	ConvertWorkers int      `json:"convert_workers"`
}

// Path returns the default config file path: $XDG_CONFIG_HOME/mailcat/config.json
// or ~/.config/mailcat/config.json.
func Path() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "mailcat", "config.json"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".config", "mailcat", "config.json"), nil
}

// Default returns a Config populated with spec defaults and XDG-resolved directories.
// Token is left empty; call GenerateToken when creating a new config.
func Default() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}

	downloadBase := os.Getenv("XDG_DOWNLOAD_DIR")
	if downloadBase == "" {
		downloadBase = filepath.Join(home, "Downloads")
	}

	stateBase := os.Getenv("XDG_STATE_HOME")
	if stateBase == "" {
		stateBase = filepath.Join(home, ".local", "state")
	}

	return &Config{
		Listen:         []string{"127.0.0.1:8766"},
		InboxDir:       filepath.Join(downloadBase, "mailcat"),
		OutboxDir:      filepath.Join(stateBase, "mailcat", "outbox"),
		MaxUploadBytes: DefaultMaxUploadBytes,
		OutboxTTL:      Duration{DefaultOutboxTTL},
		PruneInterval:  Duration{DefaultPruneInterval},
		DeleteOnPull:   false,
		HEICFormat:     DefaultHEICFormat,
		JPEGQuality:    DefaultJPEGQuality,
		KeepHEIC:       false,
		ConvertWorkers: DefaultConvertWorkers,
	}, nil
}

// GenerateToken returns 32 cryptographically random bytes encoded as base64url (unpadded).
func GenerateToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Load reads, permission-checks, parses, and validates the config file at path.
func Load(path string) (*Config, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("config path %s is not a regular file", path)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		return nil, fmt.Errorf("config file %s has permissions %04o; must be 0600", path, perm)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	cfg, err := Default()
	if err != nil {
		return nil, err
	}

	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks all config constraints from SPEC.md section 3.
func (c *Config) Validate() error {
	if len(c.Listen) == 0 {
		return errors.New("listen must contain at least one address")
	}
	for _, addr := range c.Listen {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return fmt.Errorf("invalid listen address %q: %w", addr, err)
		}
		if host == "" {
			return fmt.Errorf("listen address %q must bind a specific host, not all interfaces", addr)
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
			return fmt.Errorf("listen address %q must not bind unspecified address", addr)
		}
		if port == "" {
			return fmt.Errorf("listen address %q must include a port", addr)
		}
	}
	if c.Token == "" {
		return errors.New("token must not be empty")
	}
	if c.InboxDir == "" {
		return errors.New("inbox_dir must not be empty")
	}
	if c.OutboxDir == "" {
		return errors.New("outbox_dir must not be empty")
	}
	if c.MaxUploadBytes <= 0 {
		return errors.New("max_upload_bytes must be positive")
	}
	if c.OutboxTTL.Duration <= 0 {
		return errors.New("outbox_ttl must be positive")
	}
	if c.PruneInterval.Duration <= 0 {
		return errors.New("prune_interval must be positive")
	}
	switch c.HEICFormat {
	case "keep", "png", "jpeg", "avif":
	default:
		return fmt.Errorf("heic_format %q must be one of keep, png, jpeg, avif", c.HEICFormat)
	}
	if c.JPEGQuality < 1 || c.JPEGQuality > 100 {
		return errors.New("jpeg_quality must be between 1 and 100")
	}
	if c.ConvertWorkers < 1 {
		return errors.New("convert_workers must be at least 1")
	}
	return nil
}

// Write serialises cfg to path with mode 0600. If force is false and path exists,
// Write returns an error wrapping os.ErrExist.
func Write(path string, cfg *Config, force bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	data = append(data, '\n')

	if !force {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			os.Remove(path)
			return err
		}
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			os.Remove(path)
			return err
		}
		return f.Close()
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return err
	}
	return nil
}
