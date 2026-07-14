// Package policy defines and evaluates retention policies: pure functions
// from (artifacts, policy, now) to a list of expiry decisions with
// human-quotable reasons. Nothing in this package touches the filesystem,
// which is exactly why every deletion stashd ever performs is unit-testable.
package policy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/JaydenCJ/stashd/internal/index"
	"github.com/JaydenCJ/stashd/internal/spec"
)

// Match narrows a rule to a subset of artifacts. Empty fields match
// everything, so an empty Match (or a nil one) is a catch-all.
type Match struct {
	// Tags must all be present with equal values on the artifact.
	Tags map[string]string `json:"tags,omitempty"`
	// Name is a glob over the artifact name (* and ?).
	Name string `json:"name,omitempty"`
	// Run is a glob over the artifact's run ID.
	Run string `json:"run,omitempty"`
}

// Rule is one retention constraint. First matching rule wins per artifact;
// later rules never see artifacts an earlier rule claimed.
type Rule struct {
	Name string `json:"name,omitempty"`
	// Match selects artifacts; omit for a catch-all default rule.
	Match *Match `json:"match,omitempty"`
	// MaxAge expires artifacts older than this duration ("72h", "7d").
	MaxAge string `json:"max_age,omitempty"`
	// KeepLast keeps only the newest N artifacts per group.
	KeepLast int `json:"keep_last,omitempty"`
	// GroupBy chooses the KeepLast grouping key: "name" (default) or "run".
	GroupBy string `json:"group_by,omitempty"`
}

// Policy is the full retention configuration, stored as policy.json in the
// store root. MaxTotalBytes is a store-wide physical budget applied after
// all rules: oldest unpinned artifacts are evicted until the store fits.
type Policy struct {
	Rules         []Rule `json:"rules,omitempty"`
	MaxTotalBytes string `json:"max_total_bytes,omitempty"`
}

// Decision records why one artifact is to be expired.
type Decision struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Rule   string `json:"rule"`
	Reason string `json:"reason"`
}

// Parse decodes and validates a policy document. Unknown fields are
// rejected: a typo like "max_agee" must not silently disable retention.
func Parse(data []byte) (*Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("policy: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks every duration, size, and enum in the policy.
func (p *Policy) Validate() error {
	for i, r := range p.Rules {
		label := r.label(i)
		if r.MaxAge == "" && r.KeepLast == 0 {
			return fmt.Errorf("policy: %s has neither max_age nor keep_last", label)
		}
		if r.MaxAge != "" {
			if _, err := spec.ParseDuration(r.MaxAge); err != nil {
				return fmt.Errorf("policy: %s: %w", label, err)
			}
		}
		if r.KeepLast < 0 {
			return fmt.Errorf("policy: %s has negative keep_last", label)
		}
		switch r.GroupBy {
		case "", "name", "run":
		default:
			return fmt.Errorf("policy: %s has invalid group_by %q (use name or run)", label, r.GroupBy)
		}
		if r.Match != nil {
			for k := range r.Match.Tags {
				if err := spec.ValidateTagKey(k); err != nil {
					return fmt.Errorf("policy: %s: %w", label, err)
				}
			}
		}
	}
	if p.MaxTotalBytes != "" {
		if _, err := spec.ParseSize(p.MaxTotalBytes); err != nil {
			return fmt.Errorf("policy: %w", err)
		}
	}
	return nil
}

// Empty reports whether the policy constrains nothing.
func (p *Policy) Empty() bool {
	return p == nil || (len(p.Rules) == 0 && p.MaxTotalBytes == "")
}

func (r *Rule) label(i int) string {
	if r.Name != "" {
		return fmt.Sprintf("rule %q", r.Name)
	}
	return fmt.Sprintf("rule[%d]", i)
}

// Matches reports whether the rule claims the artifact.
func (m *Match) Matches(a *index.Artifact) bool {
	if m == nil {
		return true
	}
	for k, v := range m.Tags {
		if a.Tags[k] != v {
			return false
		}
	}
	if m.Name != "" && !spec.GlobMatch(m.Name, a.Name) {
		return false
	}
	if m.Run != "" && !spec.GlobMatch(m.Run, a.Run) {
		return false
	}
	return true
}

// Evaluate returns the expiry decisions for arts under p at time now.
// Pinned artifacts are never expired, by any rule or by the byte budget.
// The policy must have passed Validate; malformed values are a bug here.
func Evaluate(arts []*index.Artifact, p *Policy, now time.Time) []Decision {
	if p.Empty() {
		return nil
	}
	expired := map[string]Decision{}

	// Pass 1: assign each unpinned artifact to its first matching rule.
	claimed := make(map[int][]*index.Artifact, len(p.Rules))
	for _, a := range arts {
		if a.Pinned {
			continue
		}
		for i := range p.Rules {
			if p.Rules[i].Match.Matches(a) {
				claimed[i] = append(claimed[i], a)
				break
			}
		}
	}

	// Pass 2: per rule, apply max_age then keep_last among age-survivors.
	for i := range p.Rules {
		r := &p.Rules[i]
		label := r.label(i)
		var survivors []*index.Artifact
		if r.MaxAge != "" {
			maxAge, _ := spec.ParseDuration(r.MaxAge)
			for _, a := range claimed[i] {
				age := now.Sub(a.Created)
				if age > maxAge {
					expired[a.ID] = Decision{
						ID: a.ID, Name: a.Name, Rule: label,
						Reason: fmt.Sprintf("max_age %s exceeded (age %s)", r.MaxAge, spec.FormatDuration(age)),
					}
				} else {
					survivors = append(survivors, a)
				}
			}
		} else {
			survivors = claimed[i]
		}
		if r.KeepLast > 0 {
			groups := map[string][]*index.Artifact{}
			for _, a := range survivors {
				key := a.Name
				if r.GroupBy == "run" {
					key = a.Run
				}
				groups[key] = append(groups[key], a)
			}
			for key, g := range groups {
				sortNewestFirst(g)
				for rank, a := range g {
					if rank >= r.KeepLast {
						expired[a.ID] = Decision{
							ID: a.ID, Name: a.Name, Rule: label,
							Reason: fmt.Sprintf("keep_last %d exceeded (rank %d in group %q)", r.KeepLast, rank+1, key),
						}
					}
				}
			}
		}
	}

	// Pass 3: store-wide physical byte budget. Bytes are counted per unique
	// digest (deduped blobs cost their size once); eviction frees bytes only
	// when the last surviving reference to a blob goes away.
	if p.MaxTotalBytes != "" {
		budget, _ := spec.ParseSize(p.MaxTotalBytes)
		var survivors []*index.Artifact
		refs := map[string]int{}
		sizes := map[string]int64{}
		for _, a := range arts {
			if _, gone := expired[a.ID]; gone {
				continue
			}
			survivors = append(survivors, a)
			refs[a.Digest]++
			sizes[a.Digest] = a.Size
		}
		var physical int64
		for _, sz := range sizes {
			physical += sz
		}
		// Oldest first; pinned artifacts hold their bytes but are untouchable.
		sortNewestFirst(survivors)
		for i := len(survivors) - 1; i >= 0 && physical > budget; i-- {
			a := survivors[i]
			if a.Pinned {
				continue
			}
			expired[a.ID] = Decision{
				ID: a.ID, Name: a.Name, Rule: "max_total_bytes",
				Reason: fmt.Sprintf("store over budget %s (%s)", p.MaxTotalBytes, spec.FormatSize(physical)),
			}
			refs[a.Digest]--
			if refs[a.Digest] == 0 {
				physical -= a.Size
			}
		}
	}

	// Stable output: decisions in artifact creation order.
	out := make([]Decision, 0, len(expired))
	for _, a := range arts {
		if d, ok := expired[a.ID]; ok {
			out = append(out, d)
		}
	}
	return out
}

// Override builds the ad-hoc policy behind `stashd gc --max-age/--keep-last/
// --max-bytes`: one catch-all rule, labelled so gc output says where the
// decision came from.
func Override(maxAge string, keepLast int, maxBytes string) (*Policy, error) {
	p := &Policy{MaxTotalBytes: maxBytes}
	if maxAge != "" || keepLast > 0 {
		p.Rules = []Rule{{Name: "cli-override", MaxAge: maxAge, KeepLast: keepLast}}
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}

// sortNewestFirst orders by creation time descending, sequence number
// breaking ties so ordering is total even with equal timestamps.
func sortNewestFirst(arts []*index.Artifact) {
	sort.Slice(arts, func(i, j int) bool {
		if !arts[i].Created.Equal(arts[j].Created) {
			return arts[i].Created.After(arts[j].Created)
		}
		return arts[i].Seq > arts[j].Seq
	})
}
