# Changelog

All notable changes to this project are documented in this file.
The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [0.1.0] - 2026-11-27

First working release of the crash-test bench for rollback tools.

### Added

- Scenario engine: deterministic YAML scenarios (`scenarios/S01-S09`, `S13-S21`) —
  git shadowing of untracked files, files outside the project, large and sparse files,
  special files (hardlinks, setuid, fifos, sockets), path-name edge cases,
  concurrent sessions, torn state, interrupted restore, corrupted `.git`,
  checkpoint under ~95% disk pressure, rollback safety, cost on a 50k-file repository.
- Formal oracle: recall (content), metadata (mode, setuid/setgid, ownership, xattrs,
  hardlinks, allocated blocks) and clobbering of post-checkpoint edits. A scenario
  passes only when `recall == 1` and `clobbered == 0`.
- Reference baselines: `none` (must fail all), `cp-r`, `tar --sparse`, `perfect`
  (must pass all; validates the oracle itself).
- Sandbox drivers: `local` (subprocess in a temp directory), `docker` (hardened
  container, pre-flight guard, host canary, per-scenario tmpfs sizing), `incus`
  (unprivileged instance, no network, `security.guestapi=false`, lxcfs guard marker,
  tmpfs-only sandbox layout).
- Custom rollback commands via `-restore 'cmd'`.
- Cost metrics: per-call latency p50/p95, checkpoint/restore time, disk footprint,
  space-leak detection over repeated checkpoints.
- Reports: JSON and Markdown with recovery paths for checkpoint/restore failures.
- Release automation: goreleaser artifacts (linux amd64/arm64), checksums, GitHub
  Release on `v*` tags.

### Known limitations

- Interrupt scenarios (S15-class) are SKIPPED on `docker` and `incus` drivers.
- S06, S19, S20 allocate 4.6 GiB+ tmpfs and need a host with 8+ GiB RAM.
- The `local` driver is not a security boundary: run scenarios you have read,
  or use `-sandbox docker` / `-sandbox incus`.
