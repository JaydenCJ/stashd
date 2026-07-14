// Tests for retention-policy parsing and evaluation. Evaluation is a pure
// function of (artifacts, policy, now), so every deletion stashd can ever
// make is pinned down here with a fixed clock.
package policy

import (
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/stashd/internal/index"
)

// now is the fixed evaluation clock for all tests.
var now = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// mk builds an artifact created `age` before now.
func mk(seq int, name, run string, age time.Duration, tags map[string]string) *index.Artifact {
	return &index.Artifact{
		ID:      index.NewID("sha256:"+name, seq),
		Seq:     seq,
		Digest:  "sha256:blob-" + name,
		Name:    name,
		Run:     run,
		Size:    1000,
		Tags:    tags,
		Created: now.Add(-age),
	}
}

func ids(ds []Decision) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = d.ID
	}
	return out
}

func TestParseValidPolicy(t *testing.T) {
	p, err := Parse([]byte(`{
	  "rules": [
	    {"name": "shots", "match": {"tags": {"kind": "screenshot"}}, "max_age": "72h", "keep_last": 10},
	    {"max_age": "30d"}
	  ],
	  "max_total_bytes": "2GiB"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 2 || p.MaxTotalBytes != "2GiB" {
		t.Fatalf("parsed wrong: %+v", p)
	}
}

func TestParseRejectsInvalidValues(t *testing.T) {
	bad := []string{
		// A typo like "max_agee" must not silently disable retention.
		`{"rules": [{"max_agee": "7d"}]}`,
		`{"rules": [{"max_age": "soon"}]}`,
		`{"rules": [{"keep_last": -1, "max_age": "1d"}]}`,
		`{"rules": [{}]}`, // a rule that constrains nothing is a mistake
		`{"rules": [{"max_age": "1d", "group_by": "size"}]}`,
		`{"rules": [{"max_age": "1d", "match": {"tags": {"bad key": "v"}}}]}`,
		`{"max_total_bytes": "lots"}`,
	}
	for _, doc := range bad {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("should reject: %s", doc)
		}
	}
}

func TestMatchTagsAreConjunctive(t *testing.T) {
	m := &Match{Tags: map[string]string{"kind": "diff", "step": "3"}}
	yes := mk(1, "d", "", 0, map[string]string{"kind": "diff", "step": "3", "extra": "x"})
	no := mk(2, "d", "", 0, map[string]string{"kind": "diff"})
	if !m.Matches(yes) || m.Matches(no) {
		t.Fatal("tag matching must require every pair")
	}
	// A nil match is the documented catch-all.
	var catchAll *Match
	if !catchAll.Matches(mk(3, "anything", "", 0, nil)) {
		t.Fatal("nil match must match everything")
	}
}

func TestMatchNameAndRunGlobs(t *testing.T) {
	m := &Match{Name: "*.png", Run: "run-*"}
	if !m.Matches(mk(1, "shot.png", "run-9", 0, nil)) {
		t.Fatal("should match")
	}
	if m.Matches(mk(2, "shot.png", "job-9", 0, nil)) {
		t.Fatal("run glob should exclude")
	}
	if m.Matches(mk(3, "shot.jpg", "run-9", 0, nil)) {
		t.Fatal("name glob should exclude")
	}
}

func TestEvaluateMaxAgeExpiresOldOnly(t *testing.T) {
	p := &Policy{Rules: []Rule{{MaxAge: "72h"}}}
	old := mk(1, "old.log", "", 100*time.Hour, nil)
	fresh := mk(2, "fresh.log", "", 10*time.Hour, nil)
	ds := Evaluate([]*index.Artifact{old, fresh}, p, now)
	if len(ds) != 1 || ds[0].ID != old.ID {
		t.Fatalf("got %v", ids(ds))
	}
	if !strings.Contains(ds[0].Reason, "max_age 72h") {
		t.Fatalf("reason should quote the rule: %q", ds[0].Reason)
	}
}

func TestEvaluateExactlyMaxAgeSurvives(t *testing.T) {
	// Boundary: age == max_age is kept; only strictly older expires.
	p := &Policy{Rules: []Rule{{MaxAge: "72h"}}}
	edge := mk(1, "edge.log", "", 72*time.Hour, nil)
	if ds := Evaluate([]*index.Artifact{edge}, p, now); len(ds) != 0 {
		t.Fatalf("boundary artifact expired: %v", ids(ds))
	}
}

func TestEvaluateKeepLastGroupsByName(t *testing.T) {
	p := &Policy{Rules: []Rule{{KeepLast: 2}}}
	arts := []*index.Artifact{
		mk(1, "report.md", "", 4*time.Hour, nil),
		mk(2, "report.md", "", 3*time.Hour, nil),
		mk(3, "report.md", "", 2*time.Hour, nil),
		mk(4, "other.md", "", 5*time.Hour, nil), // different group, untouched
	}
	ds := Evaluate(arts, p, now)
	if len(ds) != 1 || ds[0].ID != arts[0].ID {
		t.Fatalf("oldest report.md should expire, got %v", ids(ds))
	}
	if !strings.Contains(ds[0].Reason, `group "report.md"`) {
		t.Fatalf("reason should name the group: %q", ds[0].Reason)
	}
}

func TestEvaluateKeepLastGroupsByRun(t *testing.T) {
	p := &Policy{Rules: []Rule{{KeepLast: 1, GroupBy: "run"}}}
	arts := []*index.Artifact{
		mk(1, "a.log", "run-1", 3*time.Hour, nil),
		mk(2, "b.log", "run-1", 2*time.Hour, nil),
		mk(3, "c.log", "run-2", 1*time.Hour, nil),
	}
	ds := Evaluate(arts, p, now)
	if len(ds) != 1 || ds[0].ID != arts[0].ID {
		t.Fatalf("got %v", ids(ds))
	}
}

func TestEvaluateKeepLastCountsAgeSurvivorsOnly(t *testing.T) {
	// keep_last ranks only artifacts that survived max_age, so the two
	// constraints compose instead of double-counting.
	p := &Policy{Rules: []Rule{{MaxAge: "72h", KeepLast: 1}}}
	arts := []*index.Artifact{
		mk(1, "x.log", "", 100*time.Hour, nil), // expired by age
		mk(2, "x.log", "", 10*time.Hour, nil),  // rank 2 among survivors → keep_last
		mk(3, "x.log", "", 5*time.Hour, nil),   // rank 1 among survivors → kept
	}
	ds := Evaluate(arts, p, now)
	if len(ds) != 2 {
		t.Fatalf("want 2 decisions, got %v", ids(ds))
	}
	reasons := ds[0].Reason + " | " + ds[1].Reason
	if !strings.Contains(reasons, "max_age") || !strings.Contains(reasons, "keep_last") {
		t.Fatalf("want one age and one rank decision: %q", reasons)
	}
}

func TestEvaluateFirstMatchingRuleWins(t *testing.T) {
	// The screenshot rule claims the artifact, so the strict catch-all
	// below it must not also apply.
	p := &Policy{Rules: []Rule{
		{Name: "shots", Match: &Match{Tags: map[string]string{"kind": "screenshot"}}, MaxAge: "168h"},
		{Name: "everything", MaxAge: "1h"},
	}}
	shot := mk(1, "s.png", "", 24*time.Hour, map[string]string{"kind": "screenshot"})
	ds := Evaluate([]*index.Artifact{shot}, p, now)
	if len(ds) != 0 {
		t.Fatalf("first rule (168h) should have claimed it: %v", ids(ds))
	}
}

func TestEvaluatePinnedIsUntouchable(t *testing.T) {
	p := &Policy{Rules: []Rule{{MaxAge: "1h", KeepLast: 1}}, MaxTotalBytes: "1"}
	a := mk(1, "keep.md", "", 1000*time.Hour, nil)
	a.Pinned = true
	if ds := Evaluate([]*index.Artifact{a}, p, now); len(ds) != 0 {
		t.Fatalf("pinned artifact expired: %v", ids(ds))
	}
}

func TestEvaluateByteBudgetEvictsOldestFirst(t *testing.T) {
	p := &Policy{MaxTotalBytes: "2500"} // each artifact is 1000 bytes
	arts := []*index.Artifact{
		mk(1, "a", "", 4*time.Hour, nil),
		mk(2, "b", "", 3*time.Hour, nil),
		mk(3, "c", "", 2*time.Hour, nil),
	}
	ds := Evaluate(arts, p, now)
	if len(ds) != 1 || ds[0].ID != arts[0].ID {
		t.Fatalf("oldest should be evicted, got %v", ids(ds))
	}
	if ds[0].Rule != "max_total_bytes" {
		t.Fatalf("rule label = %q", ds[0].Rule)
	}
}

func TestEvaluateByteBudgetCountsDedupedBlobsOnce(t *testing.T) {
	// Two artifacts share one blob: physical usage is 1000, not 2000, so a
	// 1500-byte budget is already satisfied and nothing may be evicted.
	p := &Policy{MaxTotalBytes: "1500"}
	a := mk(1, "same", "", 2*time.Hour, nil)
	b := mk(2, "same-copy", "", 1*time.Hour, nil)
	b.Digest = a.Digest
	if ds := Evaluate([]*index.Artifact{a, b}, p, now); len(ds) != 0 {
		t.Fatalf("dedup-aware budget violated: %v", ids(ds))
	}
}

func TestEvaluateByteBudgetEvictionFreesOnlyLastReference(t *testing.T) {
	// a and b share a blob; evicting b alone frees nothing, so the budget
	// pass must keep going until a real byte win lands.
	p := &Policy{MaxTotalBytes: "1500"}
	a := mk(1, "shared", "", 4*time.Hour, nil)
	b := mk(2, "shared-copy", "", 3*time.Hour, nil)
	b.Digest = a.Digest
	c := mk(3, "unique", "", 2*time.Hour, nil)
	ds := Evaluate([]*index.Artifact{a, b, c}, p, now)
	// physical = 2000; must evict both shared refs (freeing 1000) to reach 1000.
	if len(ds) != 2 {
		t.Fatalf("want both shared refs evicted, got %v", ids(ds))
	}
}

func TestEvaluateEmptyPolicyExpiresNothing(t *testing.T) {
	arts := []*index.Artifact{mk(1, "a", "", 9999*time.Hour, nil)}
	if ds := Evaluate(arts, &Policy{}, now); len(ds) != 0 {
		t.Fatalf("empty policy expired artifacts: %v", ids(ds))
	}
}

func TestOverrideBuildsLabelledCatchAll(t *testing.T) {
	p, err := Override("7d", 3, "1GiB")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Rules) != 1 || p.Rules[0].Name != "cli-override" || p.MaxTotalBytes != "1GiB" {
		t.Fatalf("override shape wrong: %+v", p)
	}
	if _, err := Override("banana", 0, ""); err == nil {
		t.Fatal("invalid override duration should be rejected")
	}
}

func TestDecisionsComeBackInCreationOrder(t *testing.T) {
	p := &Policy{Rules: []Rule{{MaxAge: "1h"}}}
	arts := []*index.Artifact{
		mk(1, "first", "", 10*time.Hour, nil),
		mk(2, "second", "", 9*time.Hour, nil),
		mk(3, "third", "", 8*time.Hour, nil),
	}
	ds := Evaluate(arts, p, now)
	if len(ds) != 3 || ds[0].Name != "first" || ds[2].Name != "third" {
		t.Fatalf("decisions out of order: %v", ids(ds))
	}
}
