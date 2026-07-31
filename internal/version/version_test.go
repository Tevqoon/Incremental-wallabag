package version

import (
	"strings"
	"testing"
	"time"
)

func TestShort(t *testing.T) {
	moment := time.Date(2026, 7, 31, 16, 35, 0, 0, time.UTC)

	tests := []struct {
		name string
		info Info
		want []string
		not  []string
	}{
		{
			name: "a clean build shows an abbreviated commit and its date",
			info: Info{Revision: "a589e8cb3464d0492cdb5e2cba97c1578cefcefe", Time: moment},
			want: []string{"a589e8c", "2026-07-31"},
			not:  []string{"dirty", "a589e8cb3464"},
		},
		{
			name: "a dirty build says so, because the commit alone does not identify it",
			info: Info{Revision: "a589e8cb3464d0492cdb5e2cba97c1578cefcefe", Time: moment, Dirty: true},
			want: []string{"a589e8c", "dirty"},
		},
		{
			name: "an unstamped build is labelled rather than blank",
			info: Info{},
			want: []string{"development build"},
		},
		{
			name: "a revision with no timestamp still renders",
			info: Info{Revision: "abc1234"},
			want: []string{"abc1234"},
			not:  []string{"("},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := test.info.Short()
			for _, want := range test.want {
				if !strings.Contains(got, want) {
					t.Errorf("Short() = %q, want it to contain %q", got, want)
				}
			}
			for _, unwanted := range test.not {
				if strings.Contains(got, unwanted) {
					t.Errorf("Short() = %q, want it not to contain %q", got, unwanted)
				}
			}
		})
	}
}

// TestCurrentDoesNotPanic covers the path taken under `go test`, where the
// toolchain stamps nothing.
func TestCurrentDoesNotPanic(t *testing.T) {
	if Current().Short() == "" {
		t.Error("Short() returned an empty string; the footer would be blank")
	}
}
