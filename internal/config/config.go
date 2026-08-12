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
	"strings"
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

	// QueuePageLimit caps how many rows the queue page renders at once. Zero,
	// the default, renders everything due.
	//
	// A display cap and nothing else. It never gated a reading session — "Read
	// next" and every grade redirect fetch the single most important due
	// element, ignoring this entirely — so a smaller number never meant less
	// reading, only a shorter list. Under its old name (daily_limit) it read
	// as a workload cap, which is what made a page listing 60 rows under a
	// heading saying "137 due today" so confusing. Whatever it is set to, the
	// page now says when it has truncated.
	QueuePageLimit int `yaml:"queue_page_limit"`

	// DailyLimit is the former name of QueuePageLimit, kept only so that a
	// config still carrying it fails loudly rather than having the setting
	// silently ignored. A pointer so an absent key is distinguishable from a
	// zero one.
	DailyLimit *int `yaml:"daily_limit"`

	// ExtractDelayDays is how long before a passage pulled out while reading
	// first becomes due. Zero means today. Lightly fuzzed a few days either
	// way (see ir.FuzzedFirstDueDays) rather than landing on the exact same
	// date every time — pulling several passages from one article in a
	// sitting used to put them all back in front of the reader together.
	//
	// This governs only extracts made by hand. A batch of annotations arriving
	// at once — a wallabag sync, a book import — is a different situation with
	// its own settings: see AnnotationDelayDays.
	ExtractDelayDays int `yaml:"extract_delay_days"`

	// AnnotationDelayDays is the fewest days ahead a freshly imported
	// annotation can first become due; AnnotationDelaySpreadDays is how much
	// further out on top of that it might land, spread across the window
	// per annotation rather than uniformly — see ir.FuzzedAnnotationDelay.
	//
	// Both apply to a wallabag sync's highlights and to an uploaded book or
	// PDF's annotations alike (when queued outright rather than sent through
	// triage), because either can arrive hundreds at a time. The floor keeps
	// a large import from surfacing before its context has had a chance to
	// fade; the spread is what actually scatters the batch across the
	// following weeks instead of the "1..delay" range still leaving some of
	// them due tomorrow, and instead of a poorly seeded spread landing every
	// annotation in one document on the exact same day regardless of the
	// window's width.
	AnnotationDelayDays       int `yaml:"annotation_delay_days"`
	AnnotationDelaySpreadDays int `yaml:"annotation_delay_spread_days"`

	Sources Sources `yaml:"sources"`

	// Ingest holds one-shot, operator-run importers — deliberately not
	// nested under Sources. A Sources entry is *polled* on SyncInterval and
	// carries a sync watermark in the database; Substack is neither. It has
	// no "changed since" listing to poll, only a fixed archive an operator
	// walks by hand when their subscription happens to be active, so
	// putting it under sources: would imply a background sync that does
	// not exist and never will for this provider.
	Ingest Ingest `yaml:"ingest"`

	// Location is the resolved Timezone, filled in by Load.
	Location *time.Location `yaml:"-"`
}

// Sources holds per-provider configuration. Adding a provider adds a field.
type Sources struct {
	Wallabag Wallabag `yaml:"wallabag"`
}

// Ingest holds one-shot importers — see Config.Ingest for why these are
// kept separate from Sources.
type Ingest struct {
	Substack Substack `yaml:"substack"`
}

// Substack configures the substack importer (internal/substack, driven by
// the "import-substack" subcommand). See internal/substack's own package
// doc for what it does with these; this type only carries what YAML can
// spell.
type Substack struct {
	// Host is the publication's own domain, e.g. "example.substack.com", or
	// a custom domain the publication has mapped onto Substack.
	Host string `yaml:"host"`

	// SessionCookie is the substack.sid cookie value from a browser signed
	// into an account with an active paid subscription to Host. A secret —
	// see internal/substack.Config.SessionID for why it must never be
	// logged or reported.
	SessionCookie string `yaml:"session_cookie"`

	// CacheDir is where fetched posts are cached between runs. Defaults to
	// ./substack-cache when empty — see Load.
	CacheDir string `yaml:"cache_dir"`

	// Tag is applied to every document this importer hands back, so a
	// backfilled Substack post is easy to find in wallabag afterward.
	// Defaults to the publication's own subdomain (the first label of
	// Host) when empty — see Load.
	Tag string `yaml:"tag"`
}

// Enabled reports whether enough is configured to run the substack importer.
func (s Substack) Enabled() bool {
	return s.Host != "" && s.SessionCookie != ""
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
		Bind:                      "0.0.0.0:8080",
		Database:                  "./increader.db",
		Timezone:                  "Local",
		SyncInterval:              Duration{30 * time.Minute},
		ExtractDelayDays:          10,
		AnnotationDelayDays:       30,
		AnnotationDelaySpreadDays: 60,
	}
	if err := yaml.Unmarshal([]byte(expanded), &config); err != nil {
		return Config{}, fmt.Errorf("config: parse %s: %w", path, err)
	}

	location, err := time.LoadLocation(config.Timezone)
	if err != nil {
		return Config{}, fmt.Errorf("config: unknown timezone %q: %w", config.Timezone, err)
	}
	config.Location = location

	if config.DailyLimit != nil {
		return Config{}, fmt.Errorf("config: daily_limit has been renamed to queue_page_limit, " +
			"and 0 (the new default) now means no limit — it only ever capped how many rows the " +
			"queue page rendered, never how much you could read")
	}
	if config.QueuePageLimit < 0 {
		return Config{}, fmt.Errorf("config: queue_page_limit cannot be negative, got %d (0 means no limit)",
			config.QueuePageLimit)
	}
	if config.ExtractDelayDays < 0 {
		return Config{}, fmt.Errorf("config: extract_delay_days cannot be negative, got %d",
			config.ExtractDelayDays)
	}
	if config.AnnotationDelayDays < 0 {
		return Config{}, fmt.Errorf("config: annotation_delay_days cannot be negative, got %d",
			config.AnnotationDelayDays)
	}
	if config.AnnotationDelaySpreadDays < 0 {
		return Config{}, fmt.Errorf("config: annotation_delay_spread_days cannot be negative, got %d",
			config.AnnotationDelaySpreadDays)
	}
	if config.Database == "" {
		return Config{}, fmt.Errorf("config: database path is required")
	}

	// A Host with no SessionCookie is not "not configured" — it is
	// half-configured, and the importer would fail only when actually run,
	// on a VPS over ssh, minutes into an archive walk. Rejecting it here,
	// the same way the daily_limit rename is rejected above, means the
	// operator finds out at config load instead.
	if config.Ingest.Substack.Host != "" && config.Ingest.Substack.SessionCookie == "" {
		return Config{}, fmt.Errorf(
			"config: ingest.substack.host is set but session_cookie is empty; " +
				"either set both or neither",
		)
	}
	if config.Ingest.Substack.Host != "" {
		if config.Ingest.Substack.CacheDir == "" {
			config.Ingest.Substack.CacheDir = "./substack-cache"
		}
		if config.Ingest.Substack.Tag == "" {
			// The publication's subdomain, i.e. the first label of Host:
			// "example.substack.com" and a custom-mapped "example.com"
			// both yield "example", which is a reasonable tag either way.
			config.Ingest.Substack.Tag = strings.SplitN(config.Ingest.Substack.Host, ".", 2)[0]
		}
	}

	return config, nil
}
