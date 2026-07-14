# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2026-07-13

### Added

- Content-addressed blob store: SHA-256 addressing under `objects/`, atomic
  temp-file-and-rename writes, read-only blobs, two-level fan-out, and
  dedup by construction (identical content is stored once, reported as
  `dedup` on the second `put`).
- Artifact metadata index: one atomic JSON record per artifact under
  `meta/` with name, size, media type (extension-sniffed), run ID
  (`--run` or `$STASHD_RUN`), `key=value` tags, pin bit, and creation time;
  stable 12-hex IDs with docker-style unique-prefix resolution for both
  artifact IDs and blob digests, strict on ambiguity.
- Retention policies as validated data (`policy.json`, unknown fields
  rejected): first-match-wins rules with tag/name/run matchers,
  `max_age` (`72h`/`7d`/`2w` durations), `keep_last` N grouped by name or
  run, and a store-wide dedup-aware `max_total_bytes` budget; pinned
  artifacts exempt from everything.
- `gc` with `--dry-run` and ad-hoc `--max-age` / `--keep-last` /
  `--max-bytes` overrides; every expiry quotes its rule and reason;
  unreferenced blobs are swept and reclaimed bytes reported.
- Integrity guarantees: `get` re-hashes content while streaming and fails
  on mismatch; `verify` re-hashes every blob, reports corrupt blobs,
  missing references, and orphans, exiting 1 on findings.
- Lifecycle commands `tag`, `untag`, `pin`, `unpin`, `rm` (`--force` for
  pinned; blobs freed only when unreferenced), plus `ls` filters
  (`--tag`, `--run`, `--name` globs, `--pinned`), `info`, `stats` with
  logical/physical bytes and dedup ratio, and `--json` on every reader.
- Store-level advisory lock so concurrent agent processes cannot
  interleave a gc sweep with a put.
- Runnable example (`examples/agent-run.sh`), a sample policy
  (`examples/retention-policy.json`), and the on-disk format reference
  (`docs/store-layout.md`).
- 91 deterministic offline tests (unit + in-process CLI integration with
  injected clocks) and `scripts/smoke.sh`.

[0.1.0]: https://github.com/JaydenCJ/stashd/releases/tag/v0.1.0
