# Scenarios S13, S14, S15, S18: concurrency and interrupted restore

## Scope

- S13: two parallel agent sessions in one workspace.
- S14: file written while a checkpoint is taken (torn state).
- S15: `kill -9` in the middle of a restore, then restore again.
- S18: restore while the agent keeps writing.

## Acceptance

- Barrier primitive on files plus `flock`; no sleep-based synchronization.
- Five consecutive runs produce the same verdict, otherwise the scenario is marked `FLAKY`.
- S15 documents whether a tool is idempotent.
