// Package spec parses the small user-facing value languages of stashd:
// retention durations ("7d"), byte sizes ("2GiB"), tag pairs ("k=v"),
// and name globs ("*.png"). All parsers are pure and reject ambiguity
// loudly, because retention policies delete data.
package spec

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ParseDuration accepts everything time.ParseDuration accepts, plus the
// retention-friendly units "d" (24h days) and "w" (7-day weeks), e.g.
// "90s", "12h", "7d", "2w", "1.5d". Negative durations are rejected —
// a negative retention window is always a mistake.
func ParseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return 0, fmt.Errorf("negative duration %q", s)
		}
		return d, nil
	}
	var mult time.Duration
	switch s[len(s)-1] {
	case 'd':
		mult = 24 * time.Hour
	case 'w':
		mult = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid duration %q (use s, m, h, d, or w)", s)
	}
	v, err := strconv.ParseFloat(s[:len(s)-1], 64)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("negative duration %q", s)
	}
	return time.Duration(v * float64(mult)), nil
}

// FormatDuration renders a duration in the largest natural unit with one
// decimal, matching the units ParseDuration accepts.
func FormatDuration(d time.Duration) string {
	switch {
	case d >= 24*time.Hour:
		return trimUnit(float64(d)/float64(24*time.Hour), "d")
	case d >= time.Hour:
		return trimUnit(float64(d)/float64(time.Hour), "h")
	case d >= time.Minute:
		return trimUnit(float64(d)/float64(time.Minute), "m")
	default:
		return trimUnit(float64(d)/float64(time.Second), "s")
	}
}

func trimUnit(v float64, unit string) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + unit
}

// sizeUnits maps accepted size suffixes to their byte multiplier. All
// multipliers are binary (powers of 1024); "MB" and "MiB" are synonyms,
// documented as such so nobody is surprised by a 4.8% budget difference.
var sizeUnits = map[string]int64{
	"":    1,
	"b":   1,
	"k":   1 << 10,
	"kb":  1 << 10,
	"kib": 1 << 10,
	"m":   1 << 20,
	"mb":  1 << 20,
	"mib": 1 << 20,
	"g":   1 << 30,
	"gb":  1 << 30,
	"gib": 1 << 30,
	"t":   1 << 40,
	"tb":  1 << 40,
	"tib": 1 << 40,
}

// ParseSize parses a human byte size such as "512", "64KB", "1.5GiB".
// Suffixes are case-insensitive and binary-based.
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	i := len(s)
	for i > 0 {
		c := s[i-1]
		if c >= '0' && c <= '9' || c == '.' {
			break
		}
		i--
	}
	num := strings.TrimSpace(s[:i])
	unit := strings.ToLower(strings.TrimSpace(s[i:]))
	mult, ok := sizeUnits[unit]
	if !ok {
		return 0, fmt.Errorf("invalid size unit %q in %q", s[i:], s)
	}
	v, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	if v < 0 {
		return 0, fmt.Errorf("negative size %q", s)
	}
	f := v * float64(mult)
	if f > math.MaxInt64 {
		return 0, fmt.Errorf("size %q overflows", s)
	}
	return int64(f), nil
}

// FormatSize renders bytes in the largest binary unit with one decimal:
// "914 B", "13.7 KiB", "1.2 GiB".
func FormatSize(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for i, u := range units {
		v /= 1024
		if v < 1024 || i == len(units)-1 {
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return "" // unreachable
}

// ParseTag splits a "key=value" pair. Keys are restricted to a safe,
// query-friendly charset; values may be empty ("presence tags") but may
// not contain newlines, which would corrupt line-oriented output.
func ParseTag(s string) (key, value string, err error) {
	eq := strings.IndexByte(s, '=')
	if eq < 0 {
		return "", "", fmt.Errorf("invalid tag %q (expected key=value)", s)
	}
	key, value = s[:eq], s[eq+1:]
	if err := ValidateTagKey(key); err != nil {
		return "", "", err
	}
	if strings.ContainsAny(value, "\n\r") {
		return "", "", fmt.Errorf("tag value for %q contains a newline", key)
	}
	return key, value, nil
}

// ValidateTagKey enforces the tag-key charset: [A-Za-z0-9._-], non-empty.
func ValidateTagKey(key string) error {
	if key == "" {
		return fmt.Errorf("empty tag key")
	}
	for _, r := range key {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("invalid tag key %q (allowed: letters, digits, . _ -)", key)
		}
	}
	return nil
}

// FormatTags renders a tag map as "k=v,k2=v2" with keys sorted, so output
// is byte-stable across runs.
func FormatTags(tags map[string]string) string {
	if len(tags) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ",")
}

// GlobMatch reports whether name matches pattern, where '*' matches any
// run of characters (including none) and '?' matches exactly one. There
// are no path semantics: artifact names are labels, not paths.
func GlobMatch(pattern, name string) bool {
	// Iterative backtracking matcher: O(len(pattern)*len(name)) worst case,
	// no recursion, no allocations.
	p, n := 0, 0
	starP, starN := -1, 0
	for n < len(name) {
		switch {
		case p < len(pattern) && (pattern[p] == '?' || pattern[p] == name[n]):
			p++
			n++
		case p < len(pattern) && pattern[p] == '*':
			starP, starN = p, n
			p++
		case starP >= 0:
			starN++
			p, n = starP+1, starN
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
