# Contributing to stashd

Issues, discussions and pull requests are all welcome.

## Getting started

You need Go ≥1.22; nothing else — no services, no databases, no network.

```bash
git clone https://github.com/JaydenCJ/stashd && cd stashd
go build ./...
go test ./...
bash scripts/smoke.sh
```

`scripts/smoke.sh` builds the binary and drives a full artifact lifecycle
(put, dedup, tag, pin, policy install, gc, verify) against a temp store,
asserting on real CLI output; it must finish by printing `SMOKE OK`.

## Before you open a pull request

1. `gofmt -l .` reports nothing (formatting is enforced).
2. `go vet ./...` passes with no findings.
3. `go test ./...` passes (91 deterministic tests, offline).
4. `bash scripts/smoke.sh` prints `SMOKE OK`.
5. Add tests for behavior changes; keep logic in pure, unit-testable
   modules (retention decisions live in `internal/policy` and never touch
   the filesystem — keep it that way).

## Ground rules

- Keep dependencies at zero; adding one needs strong justification in the PR.
- No network calls, ever — stashd's whole interface is the local filesystem.
  No telemetry.
- Deletion must stay explainable: every code path that removes data goes
  through a `policy.Decision` with a quotable reason, honors pins, and is
  covered by a `--dry-run` test.
- On-disk formats (`objects/` layout, `meta/*.json`, `policy.json`) are
  documented in `docs/store-layout.md`; a change there needs a doc update
  and a migration note in the PR.
- Code comments and doc comments are written in English.
- Determinism first: identical store state must produce byte-identical
  output, including all orderings.

## Reporting bugs

Include the output of `stashd version`, the full command you ran, and — for
retention surprises — the installed policy (`stashd policy show`), the
`stashd ls --json` listing, and the `stashd gc --dry-run` plan, since those
three are exactly what the evaluator saw.

## Security

Please do not open public issues for security problems; use GitHub's
private vulnerability reporting on this repository instead.
