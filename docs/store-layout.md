# stashd on-disk format and gc semantics

Everything stashd knows lives in one directory (default `~/.stashd`,
overridable with `$STASHD_DIR` or `--store`). The layout is deliberately
boring: plain files, atomic renames, no database, greppable with standard
tools. This document is the contract; changing anything here requires a
version bump and a migration note.

## Layout

```
<store>/
├── objects/
│   ├── sha256/
│   │   └── ab/cdef…            # blob, named by the rest of its digest
│   └── tmp/                    # staging area for atomic writes
├── meta/
│   └── <artifact-id>.json      # one metadata record per artifact
├── policy.json                 # retention policy (absent = retain all)
└── lock                        # advisory lock, present only mid-operation
```

## Blobs (`objects/`)

- A blob's path is derived from its canonical digest
  `sha256:<64 lowercase hex>`: first two hex characters become the fan-out
  directory, the remaining 62 the file name.
- Writes stream through SHA-256 into `objects/tmp/`, then `rename(2)` into
  place — a crash can leave stale temp files (swept by `gc`) but never a
  half-written blob.
- Blobs are chmodded read-only (`0444`) at publish time. They are immutable:
  stashd never rewrites a blob, only creates and deletes.
- Two artifacts with equal content share one blob; that is the entire dedup
  mechanism. A blob is deleted only when its last referencing artifact
  record is removed (`rm` sweeps eagerly, `gc` sweeps the rest).

## Artifact records (`meta/`)

One JSON document per artifact, written via temp-file-and-rename:

```json
{
  "id": "de9774e7f2a5",
  "seq": 3,
  "digest": "sha256:0b272145c019…",
  "name": "report.md",
  "size": 36,
  "media": "text/markdown",
  "run": "run-014",
  "tags": { "kind": "report" },
  "created": "2026-07-13T12:38:19.872217251Z"
}
```

- `id` is the first 12 hex characters of `sha256("<digest>#<seq>")` —
  stable for a given store history, distinct for deduped duplicates.
- `seq` is a store-local monotonic counter; `ls` sorts by it to break
  timestamp ties, so ordering is total and deterministic.
- `run`, `tags`, and `pinned` are omitted when empty or false, so records
  stay minimal and greppable.
- References (`get`, `rm`, `tag`, …) resolve in strict order: exact `id`,
  unique `id` prefix, unique digest prefix (`sha256:…` or bare hex). Any
  ambiguity is an error listing the candidates — never a silent guess.

## Retention policy (`policy.json`)

```json
{
  "rules": [
    { "name": "screenshots",
      "match": { "tags": { "kind": "screenshot" } },
      "max_age": "72h", "keep_last": 20 },
    { "name": "everything-else", "max_age": "30d" }
  ],
  "max_total_bytes": "2GiB"
}
```

Evaluation (`internal/policy`, pure, fully unit-tested) runs three passes:

1. **Claim** — each unpinned artifact is claimed by its *first* matching
   rule (empty/omitted `match` matches everything). Later rules never see
   claimed artifacts.
2. **Rules** — per rule: artifacts strictly older than `max_age` expire;
   survivors are grouped by `group_by` (`name` default, or `run`), sorted
   newest first, and everything past `keep_last` expires. `keep_last`
   ranks only age-survivors, so the two constraints compose.
3. **Budget** — physical usage is summed per unique digest (dedup counted
   once). While over `max_total_bytes`, the oldest unpinned survivor is
   evicted; bytes are freed only when the last reference to a blob goes,
   and the loop accounts for that.

Pinned artifacts are exempt from all three passes. Every decision carries
the rule label and a human-readable reason, printed by `gc` and emitted in
`gc --json`. `gc --dry-run` computes the identical plan without deleting.

Durations accept Go syntax plus `d` (24h) and `w` (7d). Sizes are binary:
`KB`/`KiB` = 1024, `MB`/`MiB` = 1024², and so on.

## Locking

Mutating commands (`put`, `rm`, `tag`, `pin`, `gc`, …) take an advisory
lock — an `O_EXCL`-created `lock` file — polling up to two seconds before
failing with a message that names the file. Reads are lock-free: blobs are
immutable and record writes are atomic renames. If a process dies holding
the lock, remove the file once you have confirmed nothing is running.
