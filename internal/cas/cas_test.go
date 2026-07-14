// Tests for the content-addressed blob store: atomic writes, dedup by
// construction, immutability, and corruption detection.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func mustWrite(t *testing.T, s *Store, content string) string {
	t.Helper()
	digest, _, _, err := s.Write(strings.NewReader(content))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestWriteReturnsCanonicalSHA256Digest(t *testing.T) {
	s := newStore(t)
	digest := mustWrite(t, s, "hello artifacts")
	sum := sha256.Sum256([]byte("hello artifacts"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if digest != want {
		t.Fatalf("digest = %s, want %s", digest, want)
	}
}

func TestWriteSameContentDeduplicates(t *testing.T) {
	s := newStore(t)
	d1, size1, existed1, err := s.Write(strings.NewReader("same bytes"))
	if err != nil || existed1 {
		t.Fatalf("first write: existed=%v err=%v", existed1, err)
	}
	d2, size2, existed2, err := s.Write(strings.NewReader("same bytes"))
	if err != nil || !existed2 {
		t.Fatalf("second write should dedup: existed=%v err=%v", existed2, err)
	}
	if d1 != d2 || size1 != size2 {
		t.Fatalf("dedup mismatch: %s/%d vs %s/%d", d1, size1, d2, size2)
	}
	blobs, err := s.List()
	if err != nil || len(blobs) != 1 {
		t.Fatalf("want exactly 1 blob on disk, got %d (%v)", len(blobs), err)
	}
}

func TestOpenRoundTripsContent(t *testing.T) {
	s := newStore(t)
	digest := mustWrite(t, s, "round trip payload")
	rc, size, err := s.Open(digest)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil || string(data) != "round trip payload" {
		t.Fatalf("got %q, %v", data, err)
	}
	if size != int64(len("round trip payload")) {
		t.Fatalf("size = %d", size)
	}
}

func TestBlobsAreReadOnlyOnDisk(t *testing.T) {
	s := newStore(t)
	digest := mustWrite(t, s, "immutable")
	fi, err := os.Stat(s.path(digest))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o222 != 0 {
		t.Fatalf("blob should have no write bits, has %v", fi.Mode().Perm())
	}
}

func TestEmptyBlobIsStorable(t *testing.T) {
	// Agents emit empty logs; the empty blob must be a first-class citizen.
	s := newStore(t)
	digest, size, _, err := s.Write(strings.NewReader(""))
	if err != nil || size != 0 {
		t.Fatalf("size=%d err=%v", size, err)
	}
	if !s.Exists(digest) {
		t.Fatal("empty blob should exist")
	}
	if err := s.Verify(digest); err != nil {
		t.Fatalf("empty blob should verify: %v", err)
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	s := newStore(t)
	digest := mustWrite(t, s, "to be removed")
	if err := s.Remove(digest); err != nil {
		t.Fatal(err)
	}
	if s.Exists(digest) {
		t.Fatal("blob should be gone")
	}
	// A second remove (e.g. a retried gc) must not error.
	if err := s.Remove(digest); err != nil {
		t.Fatalf("second remove errored: %v", err)
	}
}

func TestListEnumeratesSorted(t *testing.T) {
	s := newStore(t)
	mustWrite(t, s, "one")
	mustWrite(t, s, "two")
	mustWrite(t, s, "three")
	blobs, err := s.List()
	if err != nil || len(blobs) != 3 {
		t.Fatalf("got %d blobs, %v", len(blobs), err)
	}
	for i := 1; i < len(blobs); i++ {
		if blobs[i-1].Digest >= blobs[i].Digest {
			t.Fatal("list is not sorted by digest")
		}
	}
}

func TestVerifyDetectsBitRot(t *testing.T) {
	s := newStore(t)
	digest := mustWrite(t, s, "pristine content")
	if err := s.Verify(digest); err != nil {
		t.Fatalf("fresh blob should verify: %v", err)
	}
	// Flip bytes behind the store's back, as disk corruption would.
	p := s.path(digest)
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("tampered content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(digest); err == nil {
		t.Fatal("corrupt blob must fail verification")
	}
}

func TestValidateDigestRejectsMalformedRefs(t *testing.T) {
	bad := []string{
		"",
		"sha256:",
		"sha256:short",
		"md5:" + strings.Repeat("a", 32),
		"sha256:" + strings.Repeat("A", 64), // uppercase hex is non-canonical
		"sha256:" + strings.Repeat("g", 64), // not hex at all
	}
	for _, d := range bad {
		if err := ValidateDigest(d); err == nil {
			t.Errorf("ValidateDigest(%q) should fail", d)
		}
	}
	if err := ValidateDigest("sha256:" + strings.Repeat("ab", 32)); err != nil {
		t.Fatalf("canonical digest rejected: %v", err)
	}
}

func TestOpenMissingBlobFailsCleanly(t *testing.T) {
	s := newStore(t)
	_, _, err := s.Open("sha256:" + strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a 'not found' error, got %v", err)
	}
}

func TestListIgnoresForeignFiles(t *testing.T) {
	// A stray editor file inside the object tree must never be treated as
	// (or deleted like) a blob.
	s := newStore(t)
	mustWrite(t, s, "real blob")
	junk := filepath.Join(s.root, Algorithm, "ab")
	if err := os.MkdirAll(junk, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(junk, "notes.txt"), []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	blobs, err := s.List()
	if err != nil || len(blobs) != 1 {
		t.Fatalf("foreign file leaked into List: %d blobs, %v", len(blobs), err)
	}
}
