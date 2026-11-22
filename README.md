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
`none` and `cp-r` baselines, plus an optional container sandbox with a pre-flight guard and a
host canary. The adapter protocol is next.

Working name: the repository name may change before the first public release.

## Quick start

```sh
make build
./bin/unlucky-agent scenarios list
./bin/unlucky-agent run -all -dir scenarios -baseline cp-r -out out/cp-r
./bin/unlucky-agent run -all -dir scenarios -baseline none -out out/none
```

Output: `out/<baseline>/report.json` and `out/<baseline>/report.md`. Manifests for every state
(`base`, `damaged`, `post_edit`, `restored`) are written next to the report.

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
no-new-privileges`, memory and pid limits. The work, home and scratch trees are tmpfs; the
evidence directory (`<out>/evidence`) is the only host bind. Before the first damage step a
pre-flight guard verifies the uid, `$HOME`, the `/.dockerenv` marker and every mount in
`/proc/self/mountinfo`; a disk-backed mount outside `/sandbox/evidence` (and the docker
`/etc/hosts`, `/etc/hostname`, `/etc/resolv.conf` files) refuses the run with `GUARD_FAILED`.

A host canary writes marker files into the temp directory, the output directory and `$HOME` and
re-hashes them after the run. Any change or removal fails the run with `CANARY_FAILED`, even when
all scenarios passed.

## Model

- Scenario (`scenarios/*.yaml`) describes a fixture, a checkpoint point, damage steps and
  expectations.
- Damage steps are either `shell` (mutations that bypass agent hooks) or `toolcall` (mutations
  through an agent tool), plus `post_edit` for edits made after the checkpoint.
- The oracle compares three levels: content, metadata, strict metadata. Only content and
  metadata are hard gates. `post_edit` paths are checked for clobbering.
- A scenario passes only when `recall == 1` and `clobbered == 0`.

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
internal/report        JSON and Markdown reports
scenarios              S02, S04, S05, S21
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
