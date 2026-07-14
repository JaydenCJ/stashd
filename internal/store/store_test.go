// Tests for the composed store: put with dedup, verifying reads, remove
// semantics, policy-driven gc, verification, stats, and locking. The clock
// is injected everywhere so every retention outcome is deterministic.
package store

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/JaydenCJ/stashd/internal/index"
	"github.com/JaydenCJ/stashd/internal/policy"
)

var t0 = time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)

// newStore opens a store in a temp dir with a controllable clock.
func newStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	clock := t0
	s.Now = func() time.Time { return clock }
	s.LockWait = 0 // single attempt: contention tests stay instant
	return s, &clock
}

func put(t *testing.T, s *Store, content string, opt PutOptions) *index.Artifact {
	t.Helper()
	a, _, err := s.Put(strings.NewReader(content), opt)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestPutStoresAndDescribes(t *testing.T) {
	s, _ := newStore(t)
	a := put(t, s, "screenshot bytes", PutOptions{
		Name: "login.png", Run: "run-7", Tags: map[string]string{"kind": "screenshot"},
	})
	if a.Size != int64(len("screenshot bytes")) || a.Media != "image/png" {
		t.Fatalf("bad artifact: %+v", a)
	}
	if a.Run != "run-7" || a.Tags["kind"] != "screenshot" || a.Seq != 1 {
		t.Fatalf("metadata lost: %+v", a)
	}
	if !a.Created.Equal(t0) {
		t.Fatalf("created should come from the injected clock: %v", a.Created)
	}
	// A nameless artifact would be unfindable and unretainable: reject it.
	if _, _, err := s.Put(strings.NewReader("x"), PutOptions{}); err == nil {
		t.Fatal("nameless put should fail")
	}
}

func TestPutDedupsContentButKeepsBothRecords(t *testing.T) {
	s, _ := newStore(t)
	a1, dedup1, _ := s.Put(strings.NewReader("identical"), PutOptions{Name: "a.txt"})
	a2, dedup2, _ := s.Put(strings.NewReader("identical"), PutOptions{Name: "b.txt"})
	if dedup1 || !dedup2 {
		t.Fatalf("dedup flags wrong: %v %v", dedup1, dedup2)
	}
	if a1.Digest != a2.Digest || a1.ID == a2.ID {
		t.Fatalf("want same blob, distinct artifacts: %+v %+v", a1, a2)
	}
	st, _ := s.Stats()
	if st.Artifacts != 2 || st.Blobs != 1 {
		t.Fatalf("stats: %+v", st)
	}
}

func TestGetVerifiesContentWhileStreaming(t *testing.T) {
	s, _ := newStore(t)
	a := put(t, s, "trusted bytes", PutOptions{Name: "out.txt"})
	_, rc, err := s.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(rc)
	rc.Close()
	if err != nil || string(data) != "trusted bytes" {
		t.Fatalf("got %q, %v", data, err)
	}
}

func TestGetDetectsCorruptBlob(t *testing.T) {
	s, _ := newStore(t)
	a := put(t, s, "will be corrupted", PutOptions{Name: "x.txt"})
	// Corrupt in place, preserving length so only the hash can tell.
	blobs, _ := s.CAS.List()
	p := blobPath(t, s, blobs[0].Digest)
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("was be corrupted!"), 0o644)
	_, rc, err := s.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	if _, err := io.ReadAll(rc); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corruption error at EOF, got %v", err)
	}
}

// blobPath digs the on-disk path out via the object layout, keeping the
// test honest about the documented layout in docs/store-layout.md.
func blobPath(t *testing.T, s *Store, digest string) string {
	t.Helper()
	h := strings.TrimPrefix(digest, "sha256:")
	return s.Root + "/objects/sha256/" + h[:2] + "/" + h[2:]
}

func TestUpdateAppliesTagsAndPins(t *testing.T) {
	s, _ := newStore(t)
	a := put(t, s, "x", PutOptions{Name: "n.txt"})
	_, err := s.Update(a.ID, func(a *index.Artifact) error {
		a.Tags = map[string]string{"phase": "final"}
		a.Pinned = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := s.Index.Get(a.ID)
	if got.Tags["phase"] != "final" || !got.Pinned {
		t.Fatalf("update not persisted: %+v", got)
	}
}

func TestRemoveFreesBlobOnlyWhenUnreferenced(t *testing.T) {
	s, _ := newStore(t)
	a := put(t, s, "shared", PutOptions{Name: "a.txt"})
	b := put(t, s, "shared", PutOptions{Name: "b.txt"})
	res, err := s.Remove(a.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.BlobRemoved || res.OtherRefCount != 1 {
		t.Fatalf("blob freed too early: %+v", res)
	}
	res, err = s.Remove(b.ID, false)
	if err != nil || !res.BlobRemoved || res.BytesFreed != int64(len("shared")) {
		t.Fatalf("last remove should free the blob: %+v, %v", res, err)
	}
}

func TestRemovePinnedNeedsForce(t *testing.T) {
	s, _ := newStore(t)
	a := put(t, s, "precious", PutOptions{Name: "keep.md", Pinned: true})
	if _, err := s.Remove(a.ID, false); err == nil || !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("want pinned refusal, got %v", err)
	}
	if _, err := s.Remove(a.ID, true); err != nil {
		t.Fatalf("force remove failed: %v", err)
	}
}

func TestGCExpiresByAgeAndSweepsBlobs(t *testing.T) {
	s, clock := newStore(t)
	old := put(t, s, "old content", PutOptions{Name: "old.log"})
	*clock = t0.Add(100 * time.Hour)
	fresh := put(t, s, "fresh content", PutOptions{Name: "fresh.log"})
	p := &policy.Policy{Rules: []policy.Rule{{MaxAge: "72h"}}}
	res, err := s.GC(p, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Expired) != 1 || res.Expired[0].ID != old.ID {
		t.Fatalf("expired: %+v", res.Expired)
	}
	if res.BlobsRemoved != 1 || res.BytesReclaimed != int64(len("old content")) {
		t.Fatalf("sweep wrong: %+v", res)
	}
	if _, err := s.Index.Get(fresh.ID); err != nil {
		t.Fatalf("survivor lost: %v", err)
	}
	if _, err := s.Index.Get(old.ID); err == nil {
		t.Fatal("expired record still present")
	}
}

func TestGCDryRunTouchesNothing(t *testing.T) {
	s, clock := newStore(t)
	a := put(t, s, "doomed", PutOptions{Name: "d.log"})
	*clock = t0.Add(100 * time.Hour)
	p := &policy.Policy{Rules: []policy.Rule{{MaxAge: "1h"}}}
	res, err := s.GC(p, true)
	if err != nil || len(res.Expired) != 1 || res.BlobsRemoved != 1 {
		t.Fatalf("dry-run should report the full plan: %+v, %v", res, err)
	}
	if _, err := s.Index.Get(a.ID); err != nil {
		t.Fatal("dry-run deleted a record")
	}
	if !s.CAS.Exists(a.Digest) {
		t.Fatal("dry-run deleted a blob")
	}
}

func TestGCKeepsSharedBlobWhenOneReferenceExpires(t *testing.T) {
	s, clock := newStore(t)
	old := put(t, s, "shared bytes", PutOptions{Name: "old.txt"})
	*clock = t0.Add(100 * time.Hour)
	fresh := put(t, s, "shared bytes", PutOptions{Name: "fresh.txt"})
	p := &policy.Policy{Rules: []policy.Rule{{MaxAge: "72h"}}}
	res, err := s.GC(p, false)
	if err != nil || len(res.Expired) != 1 || res.Expired[0].ID != old.ID {
		t.Fatalf("gc plan wrong: %+v, %v", res, err)
	}
	if res.BlobsRemoved != 0 {
		t.Fatal("blob still referenced by the fresh artifact; must survive")
	}
	if !s.CAS.Exists(fresh.Digest) {
		t.Fatal("shared blob swept")
	}
}

func TestGCWithEmptyPolicySweepsOrphansOnly(t *testing.T) {
	s, _ := newStore(t)
	put(t, s, "kept", PutOptions{Name: "k.txt"})
	// Simulate an orphan: a blob written but whose record was hand-deleted.
	orphan := put(t, s, "orphan", PutOptions{Name: "o.txt"})
	if err := s.Index.Delete(orphan.ID); err != nil {
		t.Fatal(err)
	}
	res, err := s.GC(&policy.Policy{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.PolicyEmpty || len(res.Expired) != 0 || res.BlobsRemoved != 1 {
		t.Fatalf("orphan sweep wrong: %+v", res)
	}
}

func TestVerifyReportsCorruptMissingAndOrphans(t *testing.T) {
	s, _ := newStore(t)
	ok := put(t, s, "healthy", PutOptions{Name: "ok.txt"})
	bad := put(t, s, "will rot", PutOptions{Name: "bad.txt"})
	lost := put(t, s, "will vanish", PutOptions{Name: "lost.txt"})
	orphan := put(t, s, "no record", PutOptions{Name: "orphan.txt"})

	p := blobPath(t, s, bad.Digest)
	os.Chmod(p, 0o644)
	os.WriteFile(p, []byte("bit rot!"), 0o644)
	if err := s.CAS.Remove(lost.Digest); err != nil {
		t.Fatal(err)
	}
	if err := s.Index.Delete(orphan.ID); err != nil {
		t.Fatal(err)
	}

	res, err := s.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if res.OK() {
		t.Fatal("verify should fail")
	}
	if len(res.Corrupt) != 1 || res.Corrupt[0] != bad.Digest {
		t.Fatalf("corrupt: %v", res.Corrupt)
	}
	if len(res.Missing) != 1 || res.Missing[0].ID != lost.ID {
		t.Fatalf("missing: %+v", res.Missing)
	}
	if res.Orphans != 1 {
		t.Fatalf("orphans = %d", res.Orphans)
	}
	_ = ok
}

func TestStatsComputesDedupRatio(t *testing.T) {
	s, _ := newStore(t)
	content := strings.Repeat("x", 1000)
	put(t, s, content, PutOptions{Name: "a.txt"})
	put(t, s, content, PutOptions{Name: "b.txt", Run: "run-1"})
	put(t, s, strings.Repeat("y", 500), PutOptions{Name: "c.txt", Run: "run-2", Pinned: true})
	st, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if st.LogicalBytes != 2500 || st.PhysicalBytes != 1500 {
		t.Fatalf("bytes: %+v", st)
	}
	if st.DedupRatio < 1.66 || st.DedupRatio > 1.67 {
		t.Fatalf("ratio = %f", st.DedupRatio)
	}
	if st.Pinned != 1 || st.Runs != 2 {
		t.Fatalf("counts: %+v", st)
	}
	// An empty store must report a neutral 1.0 ratio, not divide by zero.
	empty, _ := newStore(t)
	st, err = empty.Stats()
	if err != nil || st.Artifacts != 0 || st.DedupRatio != 1.0 {
		t.Fatalf("empty stats: %+v, %v", st, err)
	}
}

func TestLockBlocksSecondWriter(t *testing.T) {
	s, _ := newStore(t)
	l, err := acquireLock(s.Root, 0)
	if err != nil {
		t.Fatal(err)
	}
	// With the lock held, any mutating operation must refuse.
	if _, _, err := s.Put(strings.NewReader("x"), PutOptions{Name: "n"}); err == nil ||
		!strings.Contains(err.Error(), "locked") {
		t.Fatalf("put should hit the lock: %v", err)
	}
	l.release()
	if _, _, err := s.Put(strings.NewReader("x"), PutOptions{Name: "n"}); err != nil {
		t.Fatalf("put after release failed: %v", err)
	}
}

func TestInstallPolicyRejectsInvalidAndKeepsOld(t *testing.T) {
	s, _ := newStore(t)
	// A fresh store has no policy file: that must read as empty, not error.
	p, err := s.LoadPolicy()
	if err != nil || !p.Empty() {
		t.Fatalf("fresh store policy: %+v, %v", p, err)
	}
	if _, err := s.InstallPolicy([]byte(`{"rules":[{"max_age":"7d"}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InstallPolicy([]byte(`{"rules":[{"max_age":"nope"}]}`)); err == nil {
		t.Fatal("invalid policy should be rejected")
	}
	p, err = s.LoadPolicy()
	if err != nil || len(p.Rules) != 1 || p.Rules[0].MaxAge != "7d" {
		t.Fatalf("old policy lost: %+v, %v", p, err)
	}
}

func TestSniffMediaKnowsAgentOutputs(t *testing.T) {
	cases := map[string]string{
		"shot.png":     "image/png",
		"trace.jsonl":  "application/jsonl",
		"changes.diff": "text/x-diff",
		"report.md":    "text/markdown",
		"mystery.bin":  "application/octet-stream",
		"no-extension": "application/octet-stream",
	}
	for name, want := range cases {
		if got := SniffMedia(name); got != want {
			t.Errorf("SniffMedia(%q) = %q, want %q", name, got, want)
		}
	}
}
