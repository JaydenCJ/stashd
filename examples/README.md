# stashd examples

Runnable, self-contained demonstrations. Both need only Go ≥1.22 and bash;
neither touches the network or your real `~/.stashd` unless you ask it to.

## `agent-run.sh`

The integration pattern: an agent harness exports `STASHD_RUN` once, then
stashes every output it produces with `kind=` tags, pins the final report,
installs the sample retention policy, and previews `gc --dry-run`.

```bash
bash examples/agent-run.sh              # throwaway temp store
bash examples/agent-run.sh /tmp/mystore # or point it somewhere persistent
```

## `retention-policy.json`

The sample policy the README and the agent-run script install:
screenshots live 72 hours (at most 20 per name), everything else lives
30 days, and the whole store is capped at 2 GiB — oldest unpinned
artifacts evicted first, deduplicated blobs counted once. Adapt the rules,
then install with:

```bash
stashd policy set examples/retention-policy.json
stashd gc --dry-run   # always preview before the first real gc
```
