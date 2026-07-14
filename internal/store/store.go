// Package store composes the CAS and the metadata index into the artifact
// lifecycle stashd exposes: put with dedup, addressable get, tags, pins,
// policy-driven gc, integrity verification, and dedup statistics.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/JaydenCJ/stashd/internal/cas"
	"github.com/JaydenCJ/stashd/internal/index"
	"github.com/JaydenCJ/stashd/internal/policy"
)

// Store is one artifact store on disk.
type Store struct {
	Root  string
	CAS   *cas.Store
	Index *index.Index

	// Now is injectable for deterministic tests; defaults to time.Now.
	Now func() time.Time
	// LockWait bounds how long mutating operations wait for the store lock.
	LockWait time.Duration
}

// Open creates or opens a store rooted at dir.
func Open(dir string) (*Store, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	c, err := cas.New(filepath.Join(abs, "objects"))
	if err != nil {
		return nil, err
	}
	ix, err := index.New(filepath.Join(abs, "meta"))
	if err != nil {
		return nil, err
	}
	return &Store{
		Root:     abs,
		CAS:      c,
		Index:    ix,
		Now:      time.Now,
		LockWait: 2 * time.Second,
	}, nil
}

// PolicyPath is where the retention policy document lives.
func (s *Store) PolicyPath() string {
	return filepath.Join(s.Root, "policy.json")
}

// LoadPolicy reads the installed policy; a missing file is an empty policy,
// not an error — a fresh store retains everything.
func (s *Store) LoadPolicy() (*policy.Policy, error) {
	data, err := os.ReadFile(s.PolicyPath())
	if os.IsNotExist(err) {
		return &policy.Policy{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	return policy.Parse(data)
}

// InstallPolicy validates and atomically installs a policy document.
func (s *Store) InstallPolicy(data []byte) (*policy.Policy, error) {
	p, err := policy.Parse(data)
	if err != nil {
		return nil, err
	}
	tmp := s.PolicyPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	if err := os.Rename(tmp, s.PolicyPath()); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("store: %w", err)
	}
	return p, nil
}

// PutOptions carries the metadata attached to a new artifact.
type PutOptions struct {
	Name   string
	Media  string
	Run    string
	Tags   map[string]string
	Pinned bool
}

// Put streams content into the store and records an artifact. The second
// return reports whether the blob already existed (content dedup).
func (s *Store) Put(r io.Reader, opt PutOptions) (*index.Artifact, bool, error) {
	if opt.Name == "" {
		return nil, false, fmt.Errorf("store: artifact name is required")
	}
	l, err := acquireLock(s.Root, s.LockWait)
	if err != nil {
		return nil, false, err
	}
	defer l.release()

	digest, size, existed, err := s.CAS.Write(r)
	if err != nil {
		return nil, false, err
	}
	seq, err := s.Index.NextSeq()
	if err != nil {
		return nil, false, err
	}
	media := opt.Media
	if media == "" {
		media = SniffMedia(opt.Name)
	}
	a := &index.Artifact{
		ID:      index.NewID(digest, seq),
		Seq:     seq,
		Digest:  digest,
		Name:    opt.Name,
		Size:    size,
		Media:   media,
		Run:     opt.Run,
		Tags:    opt.Tags,
		Pinned:  opt.Pinned,
		Created: s.Now().UTC(),
	}
	if err := s.Index.Put(a); err != nil {
		return nil, false, err
	}
	return a, existed, nil
}

// Get resolves a reference and opens its content. The returned reader
// verifies the SHA-256 digest as it is consumed: io.EOF is only delivered
// if the bytes still match the artifact's digest.
func (s *Store) Get(ref string) (*index.Artifact, io.ReadCloser, error) {
	a, err := s.Index.Resolve(ref)
	if err != nil {
		return nil, nil, err
	}
	rc, _, err := s.CAS.Open(a.Digest)
	if err != nil {
		return nil, nil, err
	}
	return a, &verifyingReader{rc: rc, want: a.Digest, h: sha256.New()}, nil
}

type verifyingReader struct {
	rc   io.ReadCloser
	want string
	h    interface {
		io.Writer
		Sum([]byte) []byte
	}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	n, err := v.rc.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
	}
	if err == io.EOF {
		got := cas.Algorithm + ":" + hex.EncodeToString(v.h.Sum(nil))
		if got != v.want {
			return n, fmt.Errorf("store: blob %s is corrupt (content hashes to %s)", v.want, got)
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error { return v.rc.Close() }

// Update applies fn to a resolved artifact under the store lock and
// persists the result — the shared machinery behind tag/untag/pin/unpin.
func (s *Store) Update(ref string, fn func(*index.Artifact) error) (*index.Artifact, error) {
	l, err := acquireLock(s.Root, s.LockWait)
	if err != nil {
		return nil, err
	}
	defer l.release()
	a, err := s.Index.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if err := fn(a); err != nil {
		return nil, err
	}
	if err := s.Index.Put(a); err != nil {
		return nil, err
	}
	return a, nil
}

// RemoveResult reports what `stashd rm` did.
type RemoveResult struct {
	Artifact      *index.Artifact
	BlobRemoved   bool
	BytesFreed    int64
	OtherRefCount int
}

// Remove deletes an artifact record; the blob is swept immediately when no
// other artifact references it. Pinned artifacts require force.
func (s *Store) Remove(ref string, force bool) (*RemoveResult, error) {
	l, err := acquireLock(s.Root, s.LockWait)
	if err != nil {
		return nil, err
	}
	defer l.release()
	a, err := s.Index.Resolve(ref)
	if err != nil {
		return nil, err
	}
	if a.Pinned && !force {
		return nil, fmt.Errorf("store: artifact %s is pinned (unpin it or pass --force)", a.ID)
	}
	if err := s.Index.Delete(a.ID); err != nil {
		return nil, err
	}
	arts, err := s.Index.List()
	if err != nil {
		return nil, err
	}
	res := &RemoveResult{Artifact: a}
	for _, other := range arts {
		if other.Digest == a.Digest {
			res.OtherRefCount++
		}
	}
	if res.OtherRefCount == 0 {
		if err := s.CAS.Remove(a.Digest); err != nil {
			return nil, err
		}
		res.BlobRemoved = true
		res.BytesFreed = a.Size
	}
	return res, nil
}

// GCResult reports one garbage-collection run.
type GCResult struct {
	DryRun         bool              `json:"dry_run"`
	PolicyEmpty    bool              `json:"policy_empty"`
	Expired        []policy.Decision `json:"expired"`
	BlobsRemoved   int               `json:"blobs_removed"`
	BytesReclaimed int64             `json:"bytes_reclaimed"`
}

// GC applies the retention policy, deletes expired artifact records, and
// sweeps blobs no surviving artifact references. With dryRun it computes
// the identical result without touching disk. Even with an empty policy a
// gc run is useful: it sweeps blobs orphaned by interrupted operations.
func (s *Store) GC(p *policy.Policy, dryRun bool) (*GCResult, error) {
	l, err := acquireLock(s.Root, s.LockWait)
	if err != nil {
		return nil, err
	}
	defer l.release()

	arts, err := s.Index.List()
	if err != nil {
		return nil, err
	}
	res := &GCResult{DryRun: dryRun, PolicyEmpty: p.Empty()}
	res.Expired = policy.Evaluate(arts, p, s.Now().UTC())

	gone := make(map[string]bool, len(res.Expired))
	for _, d := range res.Expired {
		gone[d.ID] = true
	}
	if !dryRun {
		for _, d := range res.Expired {
			if err := s.Index.Delete(d.ID); err != nil {
				return nil, err
			}
		}
	}
	// Sweep: any blob with zero surviving references goes.
	refs := map[string]bool{}
	for _, a := range arts {
		if !gone[a.ID] {
			refs[a.Digest] = true
		}
	}
	blobs, err := s.CAS.List()
	if err != nil {
		return nil, err
	}
	for _, b := range blobs {
		if refs[b.Digest] {
			continue
		}
		if !dryRun {
			if err := s.CAS.Remove(b.Digest); err != nil {
				return nil, err
			}
		}
		res.BlobsRemoved++
		res.BytesReclaimed += b.Size
	}
	return res, nil
}

// MissingRef is an artifact whose blob has vanished.
type MissingRef struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Digest string `json:"digest"`
}

// VerifyResult reports a full integrity check.
type VerifyResult struct {
	BlobsChecked int          `json:"blobs_checked"`
	Corrupt      []string     `json:"corrupt"`
	Missing      []MissingRef `json:"missing"`
	Orphans      int          `json:"orphans"`
}

// OK reports whether the store passed verification. Orphan blobs are not
// a failure — they are reclaimable garbage, and gc's job.
func (v *VerifyResult) OK() bool {
	return len(v.Corrupt) == 0 && len(v.Missing) == 0
}

// Verify re-hashes every blob and checks that every artifact's blob exists.
func (s *Store) Verify() (*VerifyResult, error) {
	res := &VerifyResult{}
	blobs, err := s.CAS.List()
	if err != nil {
		return nil, err
	}
	arts, err := s.Index.List()
	if err != nil {
		return nil, err
	}
	refs := map[string]bool{}
	for _, a := range arts {
		refs[a.Digest] = true
		if !s.CAS.Exists(a.Digest) {
			res.Missing = append(res.Missing, MissingRef{ID: a.ID, Name: a.Name, Digest: a.Digest})
		}
	}
	for _, b := range blobs {
		res.BlobsChecked++
		if err := s.CAS.Verify(b.Digest); err != nil {
			res.Corrupt = append(res.Corrupt, b.Digest)
		}
		if !refs[b.Digest] {
			res.Orphans++
		}
	}
	return res, nil
}

// StatsResult reports store totals and the dedup win.
type StatsResult struct {
	Store         string  `json:"store"`
	Artifacts     int     `json:"artifacts"`
	Pinned        int     `json:"pinned"`
	Runs          int     `json:"runs"`
	Blobs         int     `json:"blobs"`
	LogicalBytes  int64   `json:"logical_bytes"`
	PhysicalBytes int64   `json:"physical_bytes"`
	DedupRatio    float64 `json:"dedup_ratio"`
}

// Stats aggregates counts and byte totals. LogicalBytes is what callers
// stored; PhysicalBytes is what disk actually holds after dedup.
func (s *Store) Stats() (*StatsResult, error) {
	arts, err := s.Index.List()
	if err != nil {
		return nil, err
	}
	blobs, err := s.CAS.List()
	if err != nil {
		return nil, err
	}
	res := &StatsResult{Store: s.Root, Artifacts: len(arts), Blobs: len(blobs)}
	runs := map[string]bool{}
	for _, a := range arts {
		res.LogicalBytes += a.Size
		if a.Pinned {
			res.Pinned++
		}
		if a.Run != "" {
			runs[a.Run] = true
		}
	}
	res.Runs = len(runs)
	for _, b := range blobs {
		res.PhysicalBytes += b.Size
	}
	if res.PhysicalBytes > 0 {
		res.DedupRatio = float64(res.LogicalBytes) / float64(res.PhysicalBytes)
	} else {
		res.DedupRatio = 1.0
	}
	return res, nil
}

// mediaByExt maps common agent-output extensions to media types. Content
// is never inspected — names are cheap and this is a hint, not a contract.
var mediaByExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg",
	".gif": "image/gif", ".svg": "image/svg+xml", ".webp": "image/webp",
	".txt": "text/plain", ".log": "text/plain", ".md": "text/markdown",
	".html": "text/html", ".css": "text/css", ".csv": "text/csv",
	".json": "application/json", ".jsonl": "application/jsonl",
	".yaml": "application/yaml", ".yml": "application/yaml",
	".pdf": "application/pdf", ".zip": "application/zip",
	".tar": "application/x-tar", ".gz": "application/gzip",
	".diff": "text/x-diff", ".patch": "text/x-diff",
	".mp4": "video/mp4", ".webm": "video/webm",
}

// SniffMedia guesses a media type from the artifact name's extension.
func SniffMedia(name string) string {
	ext := filepath.Ext(name)
	if mt, ok := mediaByExt[ext]; ok {
		return mt
	}
	return "application/octet-stream"
}

// SortNewestFirst orders artifacts newest first (ties broken by sequence),
// the display order used by `stashd ls`.
func SortNewestFirst(arts []*index.Artifact) {
	sort.Slice(arts, func(i, j int) bool {
		if !arts[i].Created.Equal(arts[j].Created) {
			return arts[i].Created.After(arts[j].Created)
		}
		return arts[i].Seq > arts[j].Seq
	})
}
