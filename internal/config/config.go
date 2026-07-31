// Package config loads increader's YAML configuration.
//
// It is a leaf package: it declares plain data and imports nothing from the
// rest of the application. In particular it does not import internal/wallabag,
// so provider credentials are described here as ordinary strings and mapped
// onto the provider's own config type in main.
package config

import (
	"fmt"
	"os"
	"regexp"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole application configuration.
type Config struct {
	// Bind is the listen address. It defaults to 0.0.0.0 because inside a
	// container 127.0.0.1 is the container's own loopback and would be
	// unreachable; restricting exposure is the container runtime's job, via
	// a published port bound to the host's loopback.
	Bind string `yaml:"bind"`

	// Database is the path to the SQLite file.
	Database string `yaml:"database"`

	// Timezone names the location used to decide what "today" is. Scheduling
	// works in whole days, so this determines when material becomes due.
	Timezone string `yaml:"timezone"`

	// SyncInterval is how often sources are polled in the background.
	SyncInterval Duration `yaml:"sync_interval"`

	// DailyLimit caps how many elements the queue offers per day.
	DailyLimit int `yaml:"daily_limit"`

	// ExtractDelayDays is how long before a newly made or newly imported
	// extract first becomes due. Zero means today.
	ExtractDelayDays int `yaml:"extract_delay_days"`

	Sources Sources `yaml:"sources"`

	// Location is the resolved Timezone, filled in by Load.
	Location *time.Location `yaml:"-"`
}

// Sources holds per-provider configuration. Adding a provider adds a field.
type Sources struct {
	Wallabag Wallabag `yaml:"wallabag"`
}

// Wallabag is one wallabag account's connection details.
type Wallabag struct {
	URL          string `yaml:"url"`
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`
	Username     string `yaml:"username"`
	Password     string `yaml:"password"`
}

// Enabled reports whether enough is configured to talk to wallabag.
func (w Wallabag) Enabled() bool {
	return w.URL != "" && w.ClientID != "" && w.ClientSecret != "" &&
		w.Username != "" && w.Password != ""
}

// Duration is a time.Duration that can be written as "30m" in YAML.
//
// gopkg.in/yaml.v3 has no built-in decoding for time.Duration — it would try
// to read an integer count of nanoseconds — so the human-friendly spelling
// needs this wrapper.
type Duration struct {
	time.Duration
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var text string
	if err := node.Decode(&text); err != nil {
		return fmt.Errorf("config: duration must be a string like \"30m\": %w", err)
	}
	parsed, err := time.ParseDuration(text)
	if err != nil {
		return fmt.Errorf("config: invalid duration %q: %w", text, err)
	}
	d.Duration = parsed
	return nil
}

// placeholder matches ${NAME} references in the config file.
//
// Only the braced form is substituted. A bare $NAME is left alone, because
// passwords legitimately contain dollar signs and silently mangling one
// produces an authentication failure with no obvious cause.
var placeholder = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Load reads and validates the configuration at path.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("config: read %s: %w", path, err)
	}

	expanded := placeholder.ReplaceAllStringFunc(string(raw), func(match string) string {
		name := placeholder.FindStringSubmatch(match)[1]
		return os.Getenv(name)
	})

	config := Config{
		Bind:             "0.0.0.0:8080",
		Database:         "./increader.db",
		Timezone:         "Local",
		SyncInterval:     Duration{30 * time.Minute},
		DailyLimit:       60,
		ExtractDelayDays: 10,
	}
	if err := yaml.Unmarshal([]byte(expanded), &config); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}

	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return Config{}, fmt.Errorf("config: unknown timezone %q: %w", config.Timezone, err)
	}
	config.Location = location

	if config.DailyLimit <= 0 {
		return Config{}, fmt.Errorf("config: daily_limit must be positive, got %d", config.DailyLimit)
	}
	if config.ExtractDelayDays < 0 {
		return Config{}, fmt.Errorf("config: extract_delay_days cannot be negative, got %d",
			config.ExtractDelayDays)
	}
	if config.Database == "" {
		return Config{}, fmt.Errorf("config: database path is required")
	}

	return config, nil
}
