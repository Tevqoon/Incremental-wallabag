package wallabag

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// timeLayout is wallabag's timestamp format. Note the missing colon in the zone
// offset: this is RFC3339 *almost*, which means time.RFC3339 will not parse it
// and reaching for the standard layout here silently fails on every record.
const timeLayout = "2006-01-02T15:04:05-0700"

// Time is a time.Time that understands wallabag's timestamp format.
//
// Go note: embedding time.Time (rather than declaring a named field) promotes
// all of its methods, so a Time value can be used like a time.Time directly —
// t.IsZero(), t.Before(x) and so on all work.
type Time struct {
	time.Time
}

// UnmarshalJSON implements json.Unmarshaler.
//
// Go note: the receiver must be a pointer. Unmarshalling has to mutate the
// value, and a value receiver would silently write to a copy — one of the few
// places where the pointer/value choice changes behaviour rather than just
// performance.
func (t *Time) UnmarshalJSON(data []byte) error {
	// A missing or explicitly null timestamp is normal (published_at often is),
	// and leaves the zero Time, which callers test with IsZero.
	if bytes.Equal(data, []byte("null")) {
		return nil
	}

	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return fmt.Errorf("wallabag: timestamp is not a string: %w", err)
	}
	if s == "" {
		return nil
	}

	parsed, err := time.Parse(timeLayout, s)
	if err != nil {
		return fmt.Errorf("wallabag: parse timestamp %q: %w", s, err)
	}
	t.Time = parsed
	return nil
}

// MarshalJSON implements json.Marshaler so round-tripping stays symmetric.
func (t Time) MarshalJSON() ([]byte, error) {
	if t.IsZero() {
		return []byte("null"), nil
	}
	return json.Marshal(t.Format(timeLayout))
}

// Entry is a single saved article.
//
// Only the fields increader actually uses are declared. encoding/json ignores
// unknown fields by default, so wallabag can add more without breaking this.
type Entry struct {
	ID       int    `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	GivenURL string `json:"given_url"`

	// Content is the extracted article body. It is empty when the entry came
	// from a listing requested with detail=metadata.
	Content string `json:"content"`

	Language    string `json:"language"`
	DomainName  string `json:"domain_name"`
	ReadingTime int    `json:"reading_time"`

	// IsArchived and IsStarred are ints in the API, not bools — 0 or 1.
	// IsPublic, inconsistently, really is a bool.
	IsArchived int  `json:"is_archived"`
	IsStarred  int  `json:"is_starred"`
	IsPublic   bool `json:"is_public"`

	// PublishedBy is a list of author names, often null.
	PublishedBy []string `json:"published_by"`

	PublishedAt Time `json:"published_at"`
	CreatedAt   Time `json:"created_at"`
	UpdatedAt   Time `json:"updated_at"`

	Tags        []Tag        `json:"tags"`
	Annotations []Annotation `json:"annotations"`
}

// Author returns the first author name, or an empty string when unknown.
func (e Entry) Author() string {
	if len(e.PublishedBy) == 0 {
		return ""
	}
	return e.PublishedBy[0]
}

// Tag is a label attached to an entry.
type Tag struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	Slug  string `json:"slug"`
}

// Annotation is a highlight the reader made in wallabag's own interface.
//
// The API also returns annotator.js "ranges" (XPath plus character offsets)
// locating the highlight in the original DOM. increader deliberately ignores
// them: they are brittle across the HTML sanitisation this app performs, and
// Quote carries the text itself, which is all that is needed to re-locate a
// passage.
type Annotation struct {
	ID        int    `json:"id"`
	Text      string `json:"text"`
	Quote     string `json:"quote"`
	CreatedAt Time   `json:"created_at"`
	UpdatedAt Time   `json:"updated_at"`
}

// entryPage is the HAL-shaped envelope wrapping a page of entries.
type entryPage struct {
	Page     int `json:"page"`
	Limit    int `json:"limit"`
	Pages    int `json:"pages"`
	Total    int `json:"total"`
	Embedded struct {
		Items []Entry `json:"items"`
	} `json:"_embedded"`
}
