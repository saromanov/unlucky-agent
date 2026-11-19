# Scenarios S19, S20: cost on a 50k-file repository

## Scope

- Fixture: 50k files, 1.2 GiB, depth 8.
- Measure p50/p95 added latency per agent tool call over 100 calls.
- Measure checkpoint time, restore time, disk delta, peak disk.
- S20: 200 consecutive checkpoints; measure degradation and space leak.

## Acceptance

- Numbers are reproducible within 10% across five runs on the same machine.
- Baseline `none` used as the latency reference.
