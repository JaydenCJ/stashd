// Tests for the metadata index: atomic records, sequence numbering, and
// the reference-resolution rules that feed rm and gc.
package index

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newIndex(t *testing.T) *Index {
	t.Helper()
	ix, err := New(filepath.Join(t.TempDir(), "meta"))
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

func art(seq int, digest, name string) *Artifact {
	full := digest + strings.Repeat("0", 64-len(digest))
	return &Artifact{
		ID:      NewID("sha256:"+full, seq),
		Seq:     seq,
		Digest:  "sha256:" + full,
		Name:    name,
		Size:    int64(100 * seq),
		Media:   "text/plain",
		Created: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC).Add(time.Duration(seq) * time.Hour),
	}
}

func TestNewIDIsStableAndDistinctPerSeq(t *testing.T) {
	a := NewID("sha256:aaaa", 1)
	b := NewID("sha256:aaaa", 1)
	c := NewID("sha256:aaaa", 2)
	if a != b {
		t.Fatal("same inputs must give the same id")
	}
	if a == c {
		t.Fatal("different seq must give a different id (dedup keeps both records)")
	}
	if len(a) != 12 {
		t.Fatalf("id length = %d, want 12", len(a))
	}
}

func TestPutGetRoundTripsAllFields(t *testing.T) {
	ix := newIndex(t)
	a := art(1, "ab12", "report.md")
	a.Run = "run-42"
	a.Tags = map[string]string{"kind": "report"}
	a.Pinned = true
	if err := ix.Put(a); err != nil {
		t.Fatal(err)
	}
	got, err := ix.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "report.md" || got.Run != "run-42" || !got.Pinned ||
		got.Tags["kind"] != "report" || !got.Created.Equal(a.Created) {
		t.Fatalf("round trip lost fields: %+v", got)
	}
}

func TestListSortsBySequence(t *testing.T) {
	ix := newIndex(t)
	for _, seq := range []int{3, 1, 2} {
		if err := ix.Put(art(seq, "cd34", "a.txt")); err != nil {
			t.Fatal(err)
		}
	}
	arts, err := ix.List()
	if err != nil || len(arts) != 3 {
		t.Fatalf("got %d, %v", len(arts), err)
	}
	for i, a := range arts {
		if a.Seq != i+1 {
			t.Fatalf("position %d has seq %d", i, a.Seq)
		}
	}
}

func TestNextSeqStartsAtOneAndAdvances(t *testing.T) {
	ix := newIndex(t)
	if n, _ := ix.NextSeq(); n != 1 {
		t.Fatalf("fresh index NextSeq = %d, want 1", n)
	}
	ix.Put(art(1, "ab", "x"))
	ix.Put(art(7, "cd", "y")) // gaps happen after rm; next must clear the max
	if n, _ := ix.NextSeq(); n != 8 {
		t.Fatalf("NextSeq = %d, want 8", n)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	ix := newIndex(t)
	a := art(1, "ab", "x")
	ix.Put(a)
	if err := ix.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Get(a.ID); err == nil {
		t.Fatal("record should be gone")
	}
	if err := ix.Delete(a.ID); err != nil {
		t.Fatalf("second delete errored: %v", err)
	}
}

func TestResolveExactIDAndUniquePrefix(t *testing.T) {
	ix := newIndex(t)
	a := art(1, "ab12", "x")
	ix.Put(a)
	for _, ref := range []string{a.ID, a.ID[:4]} {
		got, err := ix.Resolve(ref)
		if err != nil || got.ID != a.ID {
			t.Fatalf("Resolve(%q) = %v, %v", ref, got, err)
		}
	}
}

func TestResolveDigestPrefixBothForms(t *testing.T) {
	ix := newIndex(t)
	a := art(1, "deadbeef", "x")
	ix.Put(a)
	for _, ref := range []string{"deadbeef", "sha256:deadbeef"} {
		got, err := ix.Resolve(ref)
		if err != nil || got.ID != a.ID {
			t.Fatalf("Resolve(%q) = %v, %v", ref, got, err)
		}
	}
}

func TestResolveAmbiguousDigestIsAnError(t *testing.T) {
	// Two artifacts sharing one deduped blob: a digest prefix cannot pick
	// one, and silently choosing would make `rm` delete the wrong record.
	ix := newIndex(t)
	ix.Put(art(1, "feed", "first.txt"))
	ix.Put(art(2, "feed", "second.txt"))
	_, err := ix.Resolve("feed")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}

func TestResolveUnknownRefFailsCleanly(t *testing.T) {
	ix := newIndex(t)
	ix.Put(art(1, "ab", "x"))
	for _, ref := range []string{"", "zzzz", "sha256:ffff"} {
		if _, err := ix.Resolve(ref); err == nil {
			t.Fatalf("Resolve(%q) should fail", ref)
		}
	}
}

func TestGetCorruptRecordFailsLoudly(t *testing.T) {
	ix := newIndex(t)
	a := art(1, "ab", "x")
	ix.Put(a)
	if err := os.WriteFile(ix.path(a.ID), []byte("{truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Get(a.ID); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corruption error, got %v", err)
	}
}

func TestListSkipsTempAndForeignFiles(t *testing.T) {
	ix := newIndex(t)
	ix.Put(art(1, "ab", "x"))
	// A crashed writer's temp file and a stray README must not break List.
	os.WriteFile(filepath.Join(ix.dir, ".put-12345"), []byte("partial"), 0o644)
	os.WriteFile(filepath.Join(ix.dir, "README"), []byte("hi"), 0o644)
	arts, err := ix.List()
	if err != nil || len(arts) != 1 {
		t.Fatalf("got %d artifacts, %v", len(arts), err)
	}
}
