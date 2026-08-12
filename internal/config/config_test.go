package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write puts a config file in a temp dir and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

const minimal = `
database: ./test.db
timezone: UTC
`

// TestQueuePageLimitDefaultsToUnlimited: the queue page lists everything due
// unless someone asks otherwise. Nothing in increader caps a day's reading, and
// the default should not look like it does.
func TestQueuePageLimitDefaultsToUnlimited(t *testing.T) {
	config, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.QueuePageLimit != 0 {
		t.Errorf("queue_page_limit = %d, want 0 (no limit)", config.QueuePageLimit)
	}
}

// TestDailyLimitIsRejected guards the rename. daily_limit sounded like a cap on
// how much you could read in a day and was nothing of the sort — it only
// trimmed the queue page's list. A config still carrying it must fail loudly:
// silently ignoring the key would leave a reader believing a limit was in force
// that no longer exists under that name.
func TestDailyLimitIsRejected(t *testing.T) {
	_, err := Load(write(t, minimal+"daily_limit: 60\n"))
	if err == nil {
		t.Fatal("Load accepted the old daily_limit key")
	}
	if !strings.Contains(err.Error(), "queue_page_limit") {
		t.Errorf("error does not name the new setting: %v", err)
	}
}

// TestQueuePageLimitRejectsNegative: zero already means unlimited, so a
// negative is a mistake rather than a spelling of it.
func TestQueuePageLimitRejectsNegative(t *testing.T) {
	if _, err := Load(write(t, minimal+"queue_page_limit: -5\n")); err == nil {
		t.Error("Load accepted a negative queue_page_limit")
	}
}

// TestQueuePageLimitIsRead: a reader who does want a shorter page gets one.
func TestQueuePageLimitIsRead(t *testing.T) {
	config, err := Load(write(t, minimal+"queue_page_limit: 25\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.QueuePageLimit != 25 {
		t.Errorf("queue_page_limit = %d, want 25", config.QueuePageLimit)
	}
}

// TestAnnotationDelayDefaults: a bulk import should not surface for a month,
// and should be spread wide once it does — see ir.FuzzedAnnotationDelay.
func TestAnnotationDelayDefaults(t *testing.T) {
	config, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.AnnotationDelayDays != 30 {
		t.Errorf("annotation_delay_days = %d, want 30", config.AnnotationDelayDays)
	}
	if config.AnnotationDelaySpreadDays != 60 {
		t.Errorf("annotation_delay_spread_days = %d, want 60", config.AnnotationDelaySpreadDays)
	}
}

// TestAnnotationDelayIsRead: both settings are independently overridable.
func TestAnnotationDelayIsRead(t *testing.T) {
	config, err := Load(write(t, minimal+"annotation_delay_days: 14\nannotation_delay_spread_days: 7\n"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.AnnotationDelayDays != 14 {
		t.Errorf("annotation_delay_days = %d, want 14", config.AnnotationDelayDays)
	}
	if config.AnnotationDelaySpreadDays != 7 {
		t.Errorf("annotation_delay_spread_days = %d, want 7", config.AnnotationDelaySpreadDays)
	}
}

// TestAnnotationDelayRejectsNegative applies to both settings independently.
func TestAnnotationDelayRejectsNegative(t *testing.T) {
	if _, err := Load(write(t, minimal+"annotation_delay_days: -1\n")); err == nil {
		t.Error("Load accepted a negative annotation_delay_days")
	}
	if _, err := Load(write(t, minimal+"annotation_delay_spread_days: -1\n")); err == nil {
		t.Error("Load accepted a negative annotation_delay_spread_days")
	}
}

// TestIngestSubstackIsRead exercises a full ingest.substack block, including
// ${VAR} substitution — the same mechanism sources.wallabag relies on for
// its own secrets, so Substack's session cookie must go through it too
// rather than ever sitting in the committed config.yaml directly.
func TestIngestSubstackIsRead(t *testing.T) {
	t.Setenv("SUBSTACK_SID", "s%3Atest-cookie-value")

	body := minimal + `
ingest:
  substack:
    host: example.substack.com
    session_cookie: ${SUBSTACK_SID}
    cache_dir: /data/substack-cache
    tag: newsletter
`
	config, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sub := config.Ingest.Substack
	if sub.Host != "example.substack.com" {
		t.Errorf("Host = %q, want %q", sub.Host, "example.substack.com")
	}
	if sub.SessionCookie != "s%3Atest-cookie-value" {
		t.Errorf("SessionCookie = %q, want the substituted env value", sub.SessionCookie)
	}
	if sub.CacheDir != "/data/substack-cache" {
		t.Errorf("CacheDir = %q, want %q", sub.CacheDir, "/data/substack-cache")
	}
	if sub.Tag != "newsletter" {
		t.Errorf("Tag = %q, want %q", sub.Tag, "newsletter")
	}
	if !sub.Enabled() {
		t.Error("Enabled() = false, want true with host and cookie both set")
	}
}

// TestIngestSubstackHostWithoutCookieIsRejected: a half-configured importer
// that fails only when actually run — minutes into an archive walk, on a
// VPS over ssh — is worse than one that refuses to load at all.
func TestIngestSubstackHostWithoutCookieIsRejected(t *testing.T) {
	body := minimal + "ingest:\n  substack:\n    host: example.substack.com\n"
	_, err := Load(write(t, body))
	if err == nil {
		t.Fatal("Load accepted ingest.substack.host with no session_cookie")
	}
	if !strings.Contains(err.Error(), "session_cookie") {
		t.Errorf("error does not name the missing setting: %v", err)
	}
}

// TestIngestSubstackDefaults: an operator who sets only host and
// session_cookie still gets a usable cache directory and a sensible tag,
// derived from the publication's own subdomain.
func TestIngestSubstackDefaults(t *testing.T) {
	body := minimal + "ingest:\n  substack:\n    host: example.substack.com\n    session_cookie: s%3Aabc\n"
	config, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	sub := config.Ingest.Substack
	if sub.CacheDir != "./substack-cache" {
		t.Errorf("CacheDir = %q, want default %q", sub.CacheDir, "./substack-cache")
	}
	if sub.Tag != "example" {
		t.Errorf("Tag = %q, want default %q (the subdomain)", sub.Tag, "example")
	}
}

// TestIngestSubstackNotConfiguredIsDisabled: the zero value must not be
// mistaken for a real, if minimal, configuration.
func TestIngestSubstackNotConfiguredIsDisabled(t *testing.T) {
	config, err := Load(write(t, minimal))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if config.Ingest.Substack.Enabled() {
		t.Error("Enabled() = true with no ingest.substack block at all")
	}
}
