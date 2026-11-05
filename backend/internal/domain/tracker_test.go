package domain

import "testing"

func TestParseCanonicalIssueIDRoundTripsProviders(t *testing.T) {
	cases := []struct {
		raw      string
		provider TrackerProvider
		native   string
		host     string
	}{
		{"github:acme/demo#12", TrackerProviderGitHub, "acme/demo#12", ""},
		{"gitlab:group/sub/repo#7", TrackerProviderGitLab, "group/sub/repo#7", ""},
		{"gitlab:group/repo#7@gitlab.internal:8443", TrackerProviderGitLab, "group/repo#7", "gitlab.internal:8443"},
	}
	for _, tc := range cases {
		got, ok := ParseCanonicalIssueID(tc.raw)
		if !ok || got.Provider != tc.provider || got.Native != tc.native || got.Host != tc.host {
			t.Fatalf("ParseCanonicalIssueID(%q) = %+v ok=%v", tc.raw, got, ok)
		}
	}
	for _, raw := range []string{"", "github:", "github:acme/demo", "github:acme/demo#x", "unknown:acme/demo#1"} {
		if _, ok := ParseCanonicalIssueID(raw); ok {
			t.Fatalf("ParseCanonicalIssueID(%q) accepted malformed id", raw)
		}
	}
}
