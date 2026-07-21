package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is intentionally small in Milestone 1. Package-backend and UI
// settings will extend it without changing the streaming-cache contract.
type Config struct {
	ListenAddress string              `yaml:"listen_address"`
	DataDirectory string              `yaml:"data_directory"`
	Cache         CacheSettings       `yaml:"cache"`
	Admin         AdminSettings       `yaml:"admin"`
	Upstreams     map[string]Upstream `yaml:"upstreams"`
}

type AdminSettings struct {
	Username     string `yaml:"username"`
	PasswordHash string `yaml:"password_hash"`
}

type CacheSettings struct {
	MaxSizeBytes     int64  `yaml:"max_size_bytes"`
	InactivityTTL    string `yaml:"inactivity_ttl"`
	CleanupInterval  string `yaml:"cleanup_interval"`
	WatchInterval    string `yaml:"watch_interval"`
	PrefetchVersions int    `yaml:"prefetch_versions"`
	PartialTTL       string `yaml:"partial_ttl"`
}

type LifecycleSettings struct {
	MaxSize          int64
	InactivityTTL    time.Duration
	CleanupInterval  time.Duration
	WatchInterval    time.Duration
	PrefetchVersions int
	PartialTTL       time.Duration
}

type Upstream struct {
	Kind        string   `yaml:"kind"`
	Primary     string   `yaml:"primary"`
	Backups     []string `yaml:"backups"`
	Security    bool     `yaml:"security"`
	MetadataTTL string   `yaml:"metadata_ttl"`
}

func Load(filename string) (Config, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", filename, err)
	}
	return Parse(data)
}

// Parse validates configuration bytes without writing them. It is used by the
// admin editor before an atomic configuration update.
func Parse(data []byte) (Config, error) {
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse YAML: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.ListenAddress == "" {
		return fmt.Errorf("listen_address is required")
	}
	if c.DataDirectory == "" {
		return fmt.Errorf("data_directory is required")
	}
	if len(c.Upstreams) == 0 {
		return fmt.Errorf("at least one upstream is required")
	}
	if _, err := c.Cache.Lifecycle(); err != nil {
		return fmt.Errorf("cache settings: %w", err)
	}
	for name, upstream := range c.Upstreams {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("upstream name cannot be empty")
		}
		if err := validateURL(upstream.Primary); err != nil {
			return fmt.Errorf("upstream %q primary: %w", name, err)
		}
		if upstream.Kind != "" && upstream.Kind != "apt" && upstream.Kind != "apk" && upstream.Kind != "pypi" {
			return fmt.Errorf("upstream %q kind %q is not supported", name, upstream.Kind)
		}
		if _, err := upstream.MetadataRefreshInterval(); err != nil {
			return fmt.Errorf("upstream %q metadata_ttl: %w", name, err)
		}
		for _, backup := range upstream.Backups {
			if err := validateURL(backup); err != nil {
				return fmt.Errorf("upstream %q backup: %w", name, err)
			}
		}
	}
	return nil
}

// Lifecycle resolves the cache policy. Zero values select Homir's conservative
// home-server defaults.
func (c CacheSettings) Lifecycle() (LifecycleSettings, error) {
	result := LifecycleSettings{
		MaxSize:          50 * 1000 * 1000 * 1000,
		InactivityTTL:    30 * 24 * time.Hour,
		CleanupInterval:  time.Hour,
		WatchInterval:    24 * time.Hour,
		PrefetchVersions: 5,
		PartialTTL:       30 * time.Minute,
	}
	if c.MaxSizeBytes != 0 {
		if c.MaxSizeBytes < 0 {
			return LifecycleSettings{}, fmt.Errorf("max_size_bytes must be positive")
		}
		result.MaxSize = c.MaxSizeBytes
	}
	var err error
	if c.InactivityTTL != "" {
		if result.InactivityTTL, err = time.ParseDuration(c.InactivityTTL); err != nil || result.InactivityTTL <= 0 {
			return LifecycleSettings{}, fmt.Errorf("inactivity_ttl must be a positive Go duration")
		}
	}
	if c.CleanupInterval != "" {
		if result.CleanupInterval, err = time.ParseDuration(c.CleanupInterval); err != nil || result.CleanupInterval <= 0 {
			return LifecycleSettings{}, fmt.Errorf("cleanup_interval must be a positive Go duration")
		}
	}
	if c.WatchInterval != "" {
		if result.WatchInterval, err = time.ParseDuration(c.WatchInterval); err != nil || result.WatchInterval <= 0 {
			return LifecycleSettings{}, fmt.Errorf("watch_interval must be a positive Go duration")
		}
	}
	if c.PrefetchVersions != 0 {
		if c.PrefetchVersions < 1 {
			return LifecycleSettings{}, fmt.Errorf("prefetch_versions must be positive")
		}
		result.PrefetchVersions = c.PrefetchVersions
	}
	if c.PartialTTL != "" {
		if result.PartialTTL, err = time.ParseDuration(c.PartialTTL); err != nil || result.PartialTTL <= 0 {
			return LifecycleSettings{}, fmt.Errorf("partial_ttl must be a positive Go duration")
		}
	}
	return result, nil
}

// MetadataRefreshInterval returns the policy applied to signed repository
// metadata. Security upstreams default to a shorter interval than general
// repositories, while preserving an administrator override.
func (u Upstream) MetadataRefreshInterval() (time.Duration, error) {
	if u.MetadataTTL != "" {
		ttl, err := time.ParseDuration(u.MetadataTTL)
		if err != nil || ttl <= 0 {
			return 0, fmt.Errorf("must be a positive Go duration such as 30m")
		}
		return ttl, nil
	}
	if u.Security {
		return 30 * time.Minute, nil
	}
	return 6 * time.Hour, nil
}

func validateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be an absolute HTTP(S) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme %q is not supported", u.Scheme)
	}
	return nil
}
