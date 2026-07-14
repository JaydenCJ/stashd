// Tests for the value-language parsers. These feed retention policies —
// the code path that deletes data — so rejection cases matter as much as
// acceptance cases.
package spec

import (
	"testing"
	"time"
)

func TestParseDurationAcceptsGoAndCalendarUnits(t *testing.T) {
	cases := map[string]time.Duration{
		"90s":  90 * time.Second,
		"90m":  90 * time.Minute,
		"12h":  12 * time.Hour,
		"7d":   7 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"1.5d": 36 * time.Hour, // fractional days are common in policies
	}
	for in, want := range cases {
		d, err := ParseDuration(in)
		if err != nil || d != want {
			t.Errorf("ParseDuration(%q) = %v, %v; want %v", in, d, err, want)
		}
	}
}

func TestParseDurationRejectsNegative(t *testing.T) {
	// A negative retention window would expire everything instantly.
	for _, in := range []string{"-1h", "-2d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestParseDurationRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "soon", "7x", "d", "1.2.3d"} {
		if _, err := ParseDuration(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestFormatDurationPicksNaturalUnit(t *testing.T) {
	cases := map[time.Duration]string{
		30 * time.Second: "30s",
		90 * time.Minute: "1.5h",
		36 * time.Hour:   "1.5d",
		48 * time.Hour:   "2d",
	}
	for in, want := range cases {
		if got := FormatDuration(in); got != want {
			t.Errorf("FormatDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestParseSizeBareBytesAndBinaryUnits(t *testing.T) {
	cases := map[string]int64{
		"512": 512, "0": 0,
		"1KB": 1024, "1KiB": 1024, "1kb": 1024,
		"2MB": 2 << 20, "1.5GiB": 3 << 29, "1T": 1 << 40,
	}
	for in, want := range cases {
		n, err := ParseSize(in)
		if err != nil || n != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", in, n, err, want)
		}
	}
}

func TestParseSizeRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "big", "1XB", "-5MB", "MB"} {
		if _, err := ParseSize(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestFormatSizeRoundTripsReadably(t *testing.T) {
	cases := map[int64]string{
		914:             "914 B",
		14 * 1024:       "14.0 KiB",
		1536 * 1024:     "1.5 MiB",
		3 * (1 << 30):   "3.0 GiB",
		5 * (1 << 40):   "5.0 TiB",
		999 * (1 << 40): "999.0 TiB",
	}
	for in, want := range cases {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestParseTagSplitsOnFirstEquals(t *testing.T) {
	k, v, err := ParseTag("kind=screenshot=retina")
	if err != nil || k != "kind" || v != "screenshot=retina" {
		t.Fatalf("got %q=%q, %v", k, v, err)
	}
}

func TestParseTagAllowsEmptyValue(t *testing.T) {
	// Presence tags like "ephemeral=" are legitimate.
	k, v, err := ParseTag("ephemeral=")
	if err != nil || k != "ephemeral" || v != "" {
		t.Fatalf("got %q=%q, %v", k, v, err)
	}
}

func TestParseTagRejectsBadShapes(t *testing.T) {
	for _, in := range []string{"noequals", "=value", "bad key=v", "k\n=v", "a=b\nc"} {
		if _, _, err := ParseTag(in); err == nil {
			t.Fatalf("%q should be rejected", in)
		}
	}
}

func TestFormatTagsSortsKeys(t *testing.T) {
	got := FormatTags(map[string]string{"z": "1", "a": "2", "m": "3"})
	if got != "a=2,m=3,z=1" {
		t.Fatalf("got %q", got)
	}
	if FormatTags(nil) != "-" {
		t.Fatalf("empty tag map should render as -")
	}
}

func TestGlobMatchStarAndQuestion(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*", "anything", true},
		{"*.png", "shot-001.png", true},
		{"*.png", "shot-001.jpg", false},
		{"run-??", "run-42", true},
		{"run-??", "run-7", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"", "", true},
		{"", "x", false},
		// The naive greedy matcher fails these; backtracking must not.
		{"*aab", "aaab", true},
		{"*aab", "aaba", false},
	}
	for _, c := range cases {
		if got := GlobMatch(c.pattern, c.name); got != c.want {
			t.Errorf("GlobMatch(%q, %q) = %v, want %v", c.pattern, c.name, got, c.want)
		}
	}
}
