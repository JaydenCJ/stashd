// Package cas implements the content-addressed blob store underneath
// stashd: immutable files named by their SHA-256 digest, written atomically,
// deduplicated by construction. It knows nothing about artifacts, tags, or
// retention — it stores bytes and can prove they are still the right bytes.
package cas

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Algorithm is the only digest algorithm stashd v0.1 writes or reads.
const Algorithm = "sha256"

// Blob describes one stored object.
type Blob struct {
	Digest string // "sha256:<64 hex>"
	Size   int64
}

// Store is a blob store rooted at a directory. Layout:
//
//	<root>/sha256/ab/cdef…   two-level fan-out keyed by digest prefix
//	<root>/tmp/              staging area for atomic writes
type Store struct {
	root string
}

// New returns a Store rooted at dir, creating the layout if needed.
func New(dir string) (*Store, error) {
	for _, d := range []string{dir, filepath.Join(dir, Algorithm), filepath.Join(dir, "tmp")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("cas: %w", err)
		}
	}
	return &Store{root: dir}, nil
}

// ValidateDigest checks the canonical "sha256:<64 lowercase hex>" form.
func ValidateDigest(digest string) error {
	hexPart, ok := strings.CutPrefix(digest, Algorithm+":")
	if !ok {
		return fmt.Errorf("cas: digest %q must start with %q", digest, Algorithm+":")
	}
	if len(hexPart) != sha256.Size*2 {
		return fmt.Errorf("cas: digest %q has wrong length", digest)
	}
	for _, r := range hexPart {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return fmt.Errorf("cas: digest %q is not lowercase hex", digest)
		}
	}
	return nil
}

// path maps a validated digest to its on-disk location.
func (s *Store) path(digest string) string {
	h := strings.TrimPrefix(digest, Algorithm+":")
	return filepath.Join(s.root, Algorithm, h[:2], h[2:])
}

// Write streams r into the store, hashing as it copies. It returns the
// canonical digest, the byte count, and whether the blob already existed
// (the dedup case: the new copy is discarded, the original kept).
func (s *Store) Write(r io.Reader) (digest string, size int64, existed bool, err error) {
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "put-*")
	if err != nil {
		return "", 0, false, fmt.Errorf("cas: %w", err)
	}
	defer func() {
		tmp.Close()
		os.Remove(tmp.Name()) // no-op after a successful rename
	}()

	h := sha256.New()
	size, err = io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		return "", 0, false, fmt.Errorf("cas: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", 0, false, fmt.Errorf("cas: %w", err)
	}
	digest = Algorithm + ":" + hex.EncodeToString(h.Sum(nil))

	dst := s.path(digest)
	if _, statErr := os.Stat(dst); statErr == nil {
		return digest, size, true, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", 0, false, fmt.Errorf("cas: %w", err)
	}
	// Blobs are immutable: drop write bits before publishing.
	if err := os.Chmod(tmp.Name(), 0o444); err != nil {
		return "", 0, false, fmt.Errorf("cas: %w", err)
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", 0, false, fmt.Errorf("cas: %w", err)
	}
	return digest, size, false, nil
}

// Open returns a reader over the blob plus its size on disk.
func (s *Store) Open(digest string) (io.ReadCloser, int64, error) {
	if err := ValidateDigest(digest); err != nil {
		return nil, 0, err
	}
	f, err := os.Open(s.path(digest))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, fmt.Errorf("cas: blob %s not found", digest)
		}
		return nil, 0, fmt.Errorf("cas: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("cas: %w", err)
	}
	return f, fi.Size(), nil
}

// Exists reports whether the blob is present.
func (s *Store) Exists(digest string) bool {
	if ValidateDigest(digest) != nil {
		return false
	}
	_, err := os.Stat(s.path(digest))
	return err == nil
}

// Remove deletes a blob. Removing an absent blob is not an error, so a
// gc sweep is idempotent.
func (s *Store) Remove(digest string) error {
	if err := ValidateDigest(digest); err != nil {
		return err
	}
	if err := os.Remove(s.path(digest)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("cas: %w", err)
	}
	return nil
}

// List enumerates every stored blob, sorted by digest for stable output.
func (s *Store) List() ([]Blob, error) {
	var blobs []Blob
	base := filepath.Join(s.root, Algorithm)
	prefixes, err := os.ReadDir(base)
	if err != nil {
		return nil, fmt.Errorf("cas: %w", err)
	}
	for _, p := range prefixes {
		if !p.IsDir() || len(p.Name()) != 2 {
			continue
		}
		entries, err := os.ReadDir(filepath.Join(base, p.Name()))
		if err != nil {
			return nil, fmt.Errorf("cas: %w", err)
		}
		for _, e := range entries {
			info, err := e.Info()
			if err != nil {
				return nil, fmt.Errorf("cas: %w", err)
			}
			digest := Algorithm + ":" + p.Name() + e.Name()
			if ValidateDigest(digest) != nil {
				continue // foreign file; never touch what we did not write
			}
			blobs = append(blobs, Blob{Digest: digest, Size: info.Size()})
		}
	}
	sort.Slice(blobs, func(i, j int) bool { return blobs[i].Digest < blobs[j].Digest })
	return blobs, nil
}

// Verify re-hashes a blob and fails if the content no longer matches its
// name — the on-disk corruption check behind `stashd verify`.
func (s *Store) Verify(digest string) error {
	f, _, err := s.Open(digest)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return fmt.Errorf("cas: %w", err)
	}
	got := Algorithm + ":" + hex.EncodeToString(h.Sum(nil))
	if got != digest {
		return fmt.Errorf("cas: blob %s is corrupt (content hashes to %s)", digest, got)
	}
	return nil
}
