package ingest

import "testing"

// TestSlugOf covers every URL shape and decoration the design doc records
// Substack actually producing, plus the negative case of a URL that is not
// a post permalink at all.
func TestSlugOf(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "publication subdomain",
			url:  "https://example.substack.com/p/my-great-post",
			want: "my-great-post",
		},
		{
			name: "open.substack.com cross-post form",
			url:  "https://open.substack.com/pub/example/p/my-great-post",
			want: "my-great-post",
		},
		{
			name: "custom domain",
			url:  "https://newsletter.example.com/p/my-great-post",
			want: "my-great-post",
		},
		{
			name: "utm decoration",
			url:  "https://example.substack.com/p/my-great-post?utm_source=substack&utm_medium=email",
			want: "my-great-post",
		},
		{
			name: "referral and welcome params",
			url:  "https://example.substack.com/p/my-great-post?r=abc123&showWelcome=true",
			want: "my-great-post",
		},
		{
			name: "triedRedirect param",
			url:  "https://open.substack.com/pub/example/p/my-great-post?triedRedirect=true",
			want: "my-great-post",
		},
		{
			name: "trailing slash",
			url:  "https://example.substack.com/p/my-great-post/",
			want: "my-great-post",
		},
		{
			name: "no /p/ segment at all",
			url:  "https://example.substack.com/about",
			want: "",
		},
		{
			name: "publication homepage",
			url:  "https://example.substack.com/",
			want: "",
		},
		{
			name: "unparseable URL",
			url:  "://not a url",
			want: "",
		},
		{
			name: "empty string",
			url:  "",
			want: "",
		},
		{
			name: "trailing /p/ with nothing after it",
			url:  "https://example.substack.com/p/",
			want: "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := slugOf(test.url); got != test.want {
				t.Errorf("slugOf(%q) = %q, want %q", test.url, got, test.want)
			}
		})
	}
}
