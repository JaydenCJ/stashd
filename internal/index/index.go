// Package index stores artifact metadata: one small JSON document per
// artifact under <store>/meta/. Every artifact references a blob in the
// CAS by digest; many artifacts may share one blob (that is the dedup).
// Records are written atomically so a crashed process can never leave a
// half-written entry.
package index

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Artifact is one stored output with its lifecycle metadata.
type Artifact struct {
	ID      string            `json:"id"`
	Seq     int               `json:"seq"`
	Digest  string            `json:"digest"`
	Name    string            `json:"name"`
	Size    int64             `json:"size"`
	Media   string            `json:"media"`
	Run     string            `json:"run,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
	Pinned  bool              `json:"pinned,omitempty"`
	Created time.Time         `json:"created"`
}

// Index is a metadata directory.
type Index struct {
	dir string
}

// New returns an Index rooted at dir, creating it if needed.
func New(dir string) (*Index, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return &Index{dir: dir}, nil
}

// NewID derives a stable 12-hex artifact ID from the blob digest and the
// store-local sequence number. Same content stored twice gets two distinct
// IDs (different seq) while remaining reproducible for a given store state.
func NewID(digest string, seq int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s#%d", digest, seq)))
	return hex.EncodeToString(h[:])[:12]
}

func (ix *Index) path(id string) string {
	return filepath.Join(ix.dir, id+".json")
}

// Put writes (or overwrites) an artifact record atomically.
func (ix *Index) Put(a *Artifact) error {
	if a.ID == "" {
		return fmt.Errorf("index: artifact has no id")
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	tmp, err := os.CreateTemp(ix.dir, ".put-*")
	if err != nil {
		return fmt.Errorf("index: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return fmt.Errorf("index: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	if err := os.Rename(tmp.Name(), ix.path(a.ID)); err != nil {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// Get loads one artifact by exact ID.
func (ix *Index) Get(id string) (*Artifact, error) {
	data, err := os.ReadFile(ix.path(id))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("index: artifact %q not found", id)
		}
		return nil, fmt.Errorf("index: %w", err)
	}
	var a Artifact
	if err := json.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("index: record %q is corrupt: %w", id, err)
	}
	return &a, nil
}

// Delete removes an artifact record. Deleting an absent record is not an
// error, so retries are safe.
func (ix *Index) Delete(id string) error {
	if err := os.Remove(ix.path(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("index: %w", err)
	}
	return nil
}

// List loads every artifact, sorted by sequence number ascending (i.e.
// creation order) so callers get deterministic output for free.
func (ix *Index) List() ([]*Artifact, error) {
	entries, err := os.ReadDir(ix.dir)
	if err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	var arts []*Artifact
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		a, err := ix.Get(strings.TrimSuffix(name, ".json"))
		if err != nil {
			return nil, err
		}
		arts = append(arts, a)
	}
	sort.Slice(arts, func(i, j int) bool { return arts[i].Seq < arts[j].Seq })
	return arts, nil
}

// NextSeq returns the next free sequence number (1-based).
func (ix *Index) NextSeq() (int, error) {
	arts, err := ix.List()
	if err != nil {
		return 0, err
	}
	max := 0
	for _, a := range arts {
		if a.Seq > max {
			max = a.Seq
		}
	}
	return max + 1, nil
}

// Resolve turns a user-supplied reference into exactly one artifact.
// Resolution order, strict on ambiguity because these refs feed rm and gc:
//
//  1. exact artifact ID
//  2. unique artifact-ID prefix
//  3. unique blob-digest prefix ("sha256:…" or bare hex) — unique meaning
//     it selects exactly one artifact, so a deduped blob shared by two
//     artifacts cannot be addressed this way.
func (ix *Index) Resolve(ref string) (*Artifact, error) {
	if ref == "" {
		return nil, fmt.Errorf("index: empty artifact reference")
	}
	if a, err := ix.Get(ref); err == nil {
		return a, nil
	}
	arts, err := ix.List()
	if err != nil {
		return nil, err
	}
	var byID []*Artifact
	for _, a := range arts {
		if strings.HasPrefix(a.ID, ref) {
			byID = append(byID, a)
		}
	}
	if len(byID) == 1 {
		return byID[0], nil
	}
	if len(byID) > 1 {
		return nil, ambiguous(ref, byID)
	}
	hexRef := strings.TrimPrefix(ref, "sha256:")
	var byDigest []*Artifact
	for _, a := range arts {
		if strings.HasPrefix(strings.TrimPrefix(a.Digest, "sha256:"), hexRef) {
			byDigest = append(byDigest, a)
		}
	}
	if len(byDigest) == 1 {
		return byDigest[0], nil
	}
	if len(byDigest) > 1 {
		return nil, ambiguous(ref, byDigest)
	}
	return nil, fmt.Errorf("index: no artifact matches %q", ref)
}

func ambiguous(ref string, matches []*Artifact) error {
	ids := make([]string, 0, len(matches))
	for _, a := range matches {
		ids = append(ids, a.ID)
	}
	return fmt.Errorf("index: reference %q is ambiguous (matches %s)", ref, strings.Join(ids, ", "))
}
