# unlucky-agent

A reproducible crash-test bench for rollback tools that protect a working directory from
destructive actions of AI coding agents.

The bench builds a deterministic fixture (project directory plus a user home), takes a
checkpoint, replays a destructive scenario, performs a rollback, captures a file-level manifest
of the tree and compares it with the reference state using formal rules.

Tools are compared inside their class (git-shadow, filesystem snapshot, staging), together with
mandatory baseline controls (`git stash`, `cp -r`, `tar`, filesystem snapshot), so it is visible
what a tool adds over the obvious answer.

## Status

v0.1.0-dev. Early skeleton: scenario parser, manifest capture, oracle, report, runner with
`none`, `cp-r`, `tar` and `perfect` baselines, plus an optional container sandbox with a pre-flight
guard and a host canary. Scenarios cover git shadowing of untracked files (S01-S04), files outside
the project (S05), large and sparse files (S06), special files with hardlinks, setuid, fifos and
sockets (S07), path-name edge cases (S09), concurrent sessions, torn state, an interrupted
restore and a restore overlapping a live writer (S13, S14, S15, S18), a corrupted `.git` and
shadow repository (S16), a checkpoint under ~95% disk pressure (S17), rollback safety (S21) and
the cost of rollback on a 50k-file repository (S19, S20).
The adapter protocol is next.

Working name: the repository name may change before the first public release.

## Quick start

```sh
make build
./bin/unlucky-agent scenarios list
./bin/unlucky-agent run -all -dir scenarios -baseline cp-r -out out/cp-r
./bin/unlucky-agent run -all -dir scenarios -baseline tar -out out/tar
./bin/unlucky-agent run -all -dir scenarios -baseline perfect -out out/perfect
./bin/unlucky-agent run -all -dir scenarios -baseline none -out out/none
./bin/unlucky-agent run -scenario scenarios/S13.yaml -baseline cp-r -repeat 5 -out out/s13
```

`-repeat N` runs each scenario N times with a fresh runner and aggregates the verdicts: identical
verdicts pass through, any divergence is reported as `FLAKY`.

Output: `out/<baseline>/report.json` and `out/<baseline>/report.md`. Manifests for every state
(`base`, `damaged`, `post_edit`, `restored`) are written next to the report. Checkpoint, restore
and manifest-capture failures carry `recovery_path` in JSON and a "Recovery paths" section in
Markdown.

Run against your own rollback command:

```sh
./bin/unlucky-agent run -scenario scenarios/S02.yaml \
  -restore 'tar -xpf /tmp/snapshot.tar' -out out/custom
```

## Sandbox

By default commands run through the `local` driver: a plain subprocess on the host. The `docker`
driver runs every damage and restore command inside a hardened container:

```sh
make image-sandbox
./bin/unlucky-agent run -all -dir scenarios -baseline cp-r -sandbox docker -out out/docker
```

The container uses `--network none`, uid 1000, `--cap-drop ALL`, `--security-opt
no-new-privileges`, memory and pid limits. The work, home and scratch trees are tmpfs; the work
and scratch sizes follow `resources.disk_mb` of each scenario (work is `disk_mb + 64`, scratch is
`disk_mb`, with 64m/256m floors); home stays at 64m. The container memory limit is
`max(-memory, resources.memory_mb)`, so a scenario can raise it: tmpfs pages count against the
container memory limit, and a large fixture plus its checkpoint needs more than the 512m default.
The evidence directory (`<out>/evidence`) is the only host bind. Before the first damage step a
pre-flight guard verifies the uid, `$HOME`, the `/.dockerenv` marker and every mount in
`/proc/self/mountinfo`; a disk-backed mount outside `/sandbox/evidence` (and the docker
`/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf` files) refuses the run with `GUARD_FAILED`.

A custom image must provide `sh`, `find`, `tar` and `du`. `du` feeds the cost metrics
(`disk_work_bytes`, `disk_snapshot_bytes`, `peak_disk_bytes`); when it is missing the run fails
with a `disk measurement` error instead of reporting zeros.

A host canary writes marker files into the temp directory, the output directory and `$HOME` and
re-hashes them after the run. Any change or removal fails the run with `CANARY_FAILED`, even when
all scenarios passed.

## Model

- Scenario (`scenarios/*.yaml`) describes a fixture, a checkpoint point, damage steps and
  expectations.
- `checkpoint.pre` steps run before the checkpoint is taken (used by S17 to fill the disk to
  ~95% of the work tmpfs in the docker sandbox; a bounded fill on the local driver). The local
  run additionally holds the snapshot copy of the fill (~2x121 MB peak); the pressure itself
  applies to the docker sandbox.
- Damage steps are either `shell` (mutations that bypass agent hooks) or `toolcall` (mutations
  through an agent tool), plus `post_edit` for edits made after the checkpoint and `barrier`
  steps (`signal`, `wait`, `wait-held`, `wait-free`) for concurrent branches. A `parallel` step
  runs its branches simultaneously and joins before the damage manifest is captured.
- `repeat: N` on a damage step executes it N times and times every execution into `steps` in
  microseconds (`us`); the `{{i}}` placeholder is replaced with the 0-based iteration index in
  the `shell`, `post_edit` and `toolcall` command text. `repeat: N` on the checkpoint takes N
  checkpoints into the same storage slot and records `checkpoint_times_us` and
  `disk_snapshot_bytes` per checkpoint.
- `checkpoint.pre` steps run before the checkpoint is taken (used by S17 to fill the disk to
  ~95% of the work tmpfs in the docker sandbox; a bounded fill on the local driver). The local
  run additionally holds the snapshot copy of the fill (~2x121 MB peak); the pressure itself
  applies to the docker sandbox.
- The oracle compares three levels: content, metadata, strict metadata. Only content and
  metadata are hard gates. `post_edit` paths are checked for clobbering.
- Metadata covers mode (including setuid/setgid), ownership, xattrs, hardlink counts of files
  and allocated blocks. Block counts are compared with a 32 KiB tolerance: filesystem allocation
  noise is ignored, but a densified sparse file produces millions of extra blocks.
- A scenario passes only when `recall == 1` and `clobbered == 0`.

Supported baselines: `none` (no rollback), `cp-r` (whole-tree copy), `tar` (GNU tar with
`--sparse`) and `perfect`.
`cp-r` duplicates hardlinks and densifies sparse files; both are recorded as metadata defects.
`tar` keeps hardlinks and holes, but it is host-only: with `-sandbox docker` use `cp-r` instead.
`perfect` is the reference rollback used to validate the oracle itself: it snapshots both work
and home at the checkpoint, restores byte-for-byte, and preserves post-T edits, so it must pass
every scenario, while `none` must fail every scenario.

## Cost

The `cost` group measures what rollback costs on a 50k-file, ~1.2 GiB repository (`big_tree`
fixture; every file at directory depth 8). S19 measures the added latency per agent tool call
over 100 calls and the checkpoint/restore path; S20 measures 200 consecutive checkpoints for
degradation and space leaks. The report gains a `Cost` section and JSON gains
`checkpoint_times_us`, `disk_snapshot_bytes`, `disk_work_bytes`, `peak_disk_bytes` and `steps`.

- Tool-call latency: p50/p95 of the per-call duration over the repeats, in microseconds, with
  the `none` baseline as the reference (no checkpoint, no rollback).
- Checkpoint cost: p50/p95 over the checkpoint series, plus the number of checkpoints.
- Restore time, work-tree allocated bytes, snapshot allocated bytes and peak disk (work + home
  + snapshot); with the docker driver the sizes are read inside the container.
- For the local driver, checkpoint and restore durations include an fsync of the snapshot area
  and of the restored trees: a rollback is not complete until its data is durable, and a durable
  measurement keeps consecutive runs independent of leftover writeback. Container storage is
  tmpfs, so it has no writeback to wait for and the sandbox driver skips the flush.
- Numbers are reproducible within 10% across five runs on one machine; every checkpoint reuses
  one storage slot, so a growing snapshot size in S20 is a space leak.

## Layout

```
cmd/unlucky-agent      CLI
internal/scenario      strict YAML parsing and validation
internal/manifest      file-level manifest capture (JSON Lines)
internal/oracle        diff, policy, verdicts
internal/fixture       deterministic fixtures
internal/baseline      reference rollback strategies
internal/runner        scenario execution
internal/sandbox       local and docker drivers, guard, canary
internal/shim          barrier scripts (files + flock)
internal/report        JSON and Markdown reports
scenarios              S01-S09, S13-S21
docker                 sandbox image
docs                   design documents
```

## Docs

- [Architecture](docs/ARCHITECTURE.md)
- Scenario catalog: `scenarios/` and the issue tracker.

The technical requirements document is maintained outside the repository.

## Safety

The default `local` driver executes damage commands as a local subprocess inside a temporary
directory. It does not isolate the host filesystem at the kernel level, so only run scenarios you
have read. Use `-sandbox docker` for kernel-level isolation together with the pre-flight guard
and the host canary.

## License

MIT.
