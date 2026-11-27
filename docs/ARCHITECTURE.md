# unlucky-agent — architecture

Document version: 0.2
Implementation language: Go.

---

## 1. Architecture goals

1. **Determinism.** The same input must give the same verdict. Everything non-deterministic is
   pushed into isolated adapters and marked in the report.
2. **Isolation.** Destructive commands run only inside a container; the bench core must have no
   code path that can execute them on the host.
3. **Extension without rebuilding the core.** A new scenario is YAML. A new tool is a separate
   adapter process.
4. **Streaming.** A manifest of 50k files is written as a stream, without loading the tree into
   memory.
5. **Provability.** Every verdict can be reconstructed from the run artifacts.

## 2. Stack

- **Go 1.27+**, static build, CGO disabled by default (`CGO_ENABLED=0`).
- **CLI:** `spf13/cobra` (subcommands, flags, generated help). The current skeleton uses the
  standard `flag` package; migration is a separate task.
- **Scenario config:** `gopkg.in/yaml.v3` with strict mode (`KnownFields(true)`), so a typo in a
  scenario field is an error instead of a silent default.
- **Filesystem primitives:** `io/fs`, `path/filepath`, `golang.org/x/sys/unix` (`Lstat`,
  `Stat_t`, `Listxattr`, `Flock`, `Ftruncate`).
- **Hashes:** `crypto/sha256` for identity, `golang.org/x/crypto/blake2b` for throughput on large
  files. The choice is recorded in the manifest header.
- **Docker:** for the MVP, the `docker` CLI is invoked through `os/exec` with a context and a
  timeout. Rationale: fewer dependencies, a stable contract, easier debugging. The
  `sandbox.Driver` interface allows a later switch to `github.com/docker/docker/client` without
  touching the core.
- **Report templates:** `text/template`. JSON uses `encoding/json` with a schema.
- **Tests:** standard `testing`, table-driven, golden files, `go test -race`.
- **Build:** Makefile plus GoReleaser for releases.

Forbidden dependencies: heavy ORMs, and any library that requires cgo for the base scenario.

## 3. Component diagram

```
                        ┌──────────────────────────────┐
                        │            CLI               │
                        │  run | scenarios | reproduce │
                        │  adapter | doctor | report   │
                        └───────────────┬──────────────┘
                                        │
        ┌───────────────────────────────┼───────────────────────────────┐
        │                               │                               │
┌───────▼────────┐            ┌─────────▼──────────┐          ┌─────────▼─────────┐
│   scenario     │            │       runner       │          │      report       │
│ parse/validate │───────────▶│ plan → execute     │─────────▶│ json + markdown   │
└────────────────┘            └────┬──────────┬────┘          └───────────────────┘
                                   │          │
                    ┌──────────────▼──┐   ┌───▼───────────────┐
                    │    sandbox      │   │      oracle       │
                    │ docker driver   │   │ manifest/capture  │
                    │ guard, canary   │   │ diff/compare      │
                    │ limits, timeouts│   │ policy            │
                    └───────┬─────────┘   └───┬───────────────┘
                            │                 │
                    ┌───────▼─────────────────▼────────┐
                    │   container: workspace + home    │
                    │   /sandbox/{work,home,scratch,   │
                    │             evidence}            │
                    └───────┬──────────────────────────┘
                            │ exec / copy
                    ┌───────▼─────────┐
                    │    adapters     │
                    │ stdio JSON-RPC  │
                    │ tool-under-test │
                    └─────────────────┘
```

## 4. Repository layout

```
unlucky-agent/
  cmd/unlucky-agent/main.go      # thin main, delegates to internal/cli
  internal/
    cli/                         # cobra commands
    scenario/                    # model, parser, validator, versions
    fixture/                     # fixture generators, seed
    sandbox/                     # driver, guard, canary, limits, exec
    runner/                      # run plan, execution, retries, sharding
    adapter/                     # protocol client, adapter registry
    oracle/                      # manifest, capture, diff, policy
    metrics/                     # metric computation
    report/                      # json, markdown, schema
    shim/                        # cmdshim and barriers for scenarios
    version/                     # version, run fingerprint
  adapters/                      # built-in adapters (subcommands)
    gitstash/
    cpr/
    tar/
    fssnapshot/
    perfect/                     # reference stub that validates the oracle
    gitcheckpoint/               # shadow-git tools (T1)
  scenarios/                     # S01..S22 in YAML
  schema/                        # JSON Schemas for report and manifest (go:embed)
  testdata/                      # golden files, synthetic trees
  docker/                        # Dockerfile.base, Dockerfile.tools
  docs/
  Makefile
  go.mod
  go.sum
```

## 5. Core data model

```go
type Scenario struct {
    ID          string
    Title       string
    Group       Group          // loss | meta | semantics | failure | cost | safety
    SpecVersion int
    Tiers       []Tier
    Platforms   []Platform
    Fixture     string
    Seed        int64
    Checkpoint  CheckpointSpec
    Damage      []DamageStep
    Restore     RestoreSpec
    Expect      ExpectSpec
    Limits      Limits
    Resources   Resources
}

type DamageStep struct {
    Kind    StepKind // shell | post_edit | toolcall | barrier | signal | fill_disk
    Shell   string
    Tool    *ToolCall
    Barrier string
    Timeout time.Duration
}

type ExpectSpec struct {
    Restore  []string
    Preserve []string
    Exclude  []string
    Require  MatchLevel // content | meta
}

type Verdict string // PASS | FAIL | SKIPPED_PLATFORM | SKIPPED_UNSUPPORTED | ERROR | FLAKY
```

A manifest is never held in memory as a whole:

```go
type Entry struct {
    Root    string            `json:"root"`
    Path    string            `json:"path"`
    Type    EntryType         `json:"type"`
    Mode    string            `json:"mode"`
    UID     uint32            `json:"uid"`
    GID     uint32            `json:"gid"`
    Size    int64             `json:"size"`
    Blocks  int64             `json:"blocks"`
    NLink   uint64            `json:"nlink"`
    Hash    string            `json:"hash,omitempty"`
    MtimeNS int64             `json:"mtime_ns"`
    Xattr   map[string]string `json:"xattr,omitempty"`
    Link    string            `json:"link,omitempty"`
}
```

Capture writes into a `bufio.Writer` over a temporary file, entries are sorted by path. For very
large trees the plan is an external sort; the switch threshold is 1 million entries, in-memory
buffering is used below it (50k paths take roughly 10 MB).

## 6. Adapter protocol

An adapter is a separate executable, discovered by name in `adapters.yaml` or in `PATH` as
`crashtest-adapter-<id>`. Built-in adapters are also available as `unlucky-agent adapter <id>`.

Transport: line-delimited JSON-RPC 2.0 over stdin/stdout, UTF-8, one object per line. stderr is
for logs and is never parsed.

Methods:

- `handshake` → `{protocol: 1, id, tier, version, caps: ["checkpoint","restore","attribution","outside_project"]}`
- `prepare` `{ctx: SandboxCtx}` → `{}` — the adapter initializes its storage inside the sandbox.
- `checkpoint` `{label}` → `{checkpoint_id, at_unix_nano}` — create a restore point.
- `restore` `{checkpoint_id, mode}` → `{restored: bool, notes}` — perform the rollback.
- `attribution` `{since_checkpoint}` → `[{path, level, command}]`.
- `teardown` `{}` — release resources and remove service directories.

Rules:

- the adapter has no network access;
- the adapter must be idempotent when `restore` is repeated for the same `checkpoint_id`,
  otherwise scenario S15 records a failure;
- the adapter must never be interactive: reading stdin outside the protocol is forbidden;
- an incompatible protocol version is an explicit `ERROR`, never a silent skip.

`SandboxCtx` contains only in-container paths: `WorkDir`, `HomeDir`, `ScratchDir`,
`EvidenceDir`, `ToolchainDirs`.

Registry:

```yaml
# adapters.yaml
adapters:
  - id: cline-checkpoints
    tier: git-shadow
    binary: cline
    version_pin: "3.4.1"
    image_digest: "sha256:..."
  - id: btrfs-snapshot
    tier: fs-snapshot
    binary: unlucky-agent-adapter-btrfs
    requires: {fs: btrfs, root: true}
```

The adapter version is part of the run fingerprint. A mismatch between `version_pin` and the
actual version is an `ERROR`, not a warning.

## 7. Sandbox

### 7.1. Layout inside the container

```
/sandbox/work       # project, a working copy of the fixture
/sandbox/home       # ephemeral HOME (tmpfs), holds ~/.config, ~/.ssh and similar
/sandbox/scratch    # large files, a separate volume with a quota
/sandbox/evidence   # the only host bind mount, rw, created by the bench
/opt/tools          # images of the tools under test, read-only
```

Host mounts: only `/sandbox/evidence`. Everything else is tmpfs or a named volume removed after
the run.

### 7.2. Run parameters

```
docker run --rm
  --network none
  --user 1000:1000
  --cap-drop ALL
  --security-opt no-new-privileges
  --pids-limit 4096
  --memory <resources.memory_mb>
  --cpus 2
  --tmpfs /sandbox/home:rw,size=512m
  --mount type=bind,src=<out>/evidence,dst=/sandbox/evidence
  --mount type=bind,src=<fixture>,dst=/sandbox/work
```

`scratch` is a separate volume with `size=` (tmpfs), or a directory inside `work` when the
scenario does not need a quota.

### 7.3. Guard

Before a scenario starts, `sandbox.Guard` runs these checks inside the container:

1. the filesystem root is a container root (`/.dockerenv` exists, or `/proc/self/mountinfo` shows
   no host rootfs);
2. `$HOME` points inside `/sandbox/home`;
3. mountinfo contains no host paths except `/sandbox/evidence`;
4. the process uid is 1000;
5. `work` and `home` are not symlinks to host paths.

Any failure aborts the run with `GUARD_FAILED`. The guard runs before the first destructive
command, never after.

### 7.4. Canary

Before a run the bench creates a set of markers on the host (in `$HOME`, `/tmp`, `<out>/canary`)
with known content and hashes. After the run it verifies they are unchanged. A failure is a
critical error in the report and a non-zero exit code, even if every scenario passed.

### 7.5. Timeouts and kill

Every damage step and every restore runs under `context.WithTimeout`. On expiry the bench issues
`docker kill`, collects logs and reports `ERROR` with reason `timeout`. The adapter process
receives SIGKILL; the bench does not wait for a graceful shutdown.

### 7.6. Incus driver

A second container backend implements the same `sandbox.Driver` interface: `-sandbox incus`
creates one unprivileged Incus container per scenario from `images:debian/12/cloud`. The driver
removes every network device, sets `limits.memory`, `limits.processes` and `security.guestapi=false`, bind-mounts only the evidence directory, and installs the work, home and
scratch tmpfs trees through `raw.lxc` entries at the same sizes as the docker driver. Exec, the
cp-r and perfect baselines, the size metrics and the fixture copy-in/out all follow the docker
implementation; the tar streams go through `incus exec` with a piped stdin. Like the docker
driver, it does not implement `Interruptible`, so interrupt scenarios are reported as skipped.

The guard is that driver's own variant: the container marker is a `fuse.lxcfs` mount instead of
`/.dockerenv`, `/dev/incus` must be absent (the probe fails when the guest API socket is present
or its marker is missing), the tmpfs requirement covers `/sandbox/work`, `/sandbox/home` and
`/sandbox/scratch`, only the loopback interface may be present, and the container rootfs (`/`)
counts as a permitted disk-backed mount. A tmpfs-backed `/sandbox/evidence` is permitted, matching
the docker branch. The `Driver` interface plus the optional `CPBaseline`,
`PerfectBaseline`, `SnapshotSizer` and `StorageSizer` interfaces let the runner pick up the new
backend without a single change to the runner itself.

## 8. Scenario execution

The sequence for one scenario × tool pair:

1. **Plan.** Validate the scenario, check the platform and the adapter capabilities from
   `handshake`. An incompatibility yields `SKIPPED_UNSUPPORTED` with a reason.
2. **Materialize.** Build the fixture from the seed in an empty directory and capture the expected
   manifest `M0`.
3. **Start.** Start the container, run the guard, establish the canary baseline.
4. **Prepare.** `adapter.prepare`.
5. **Checkpoint.** `adapter.checkpoint("T")` and capture the reference manifest `MT`.
6. **Damage.** Execute the damage steps in order. Every step is journaled with timing.
7. **Damage manifest.** Capture `MD`, confirming the state was actually destroyed. If
   `MD == MT`, the scenario is defective and yields `ERROR(no_damage)`. This protects against a
   green benchmark that destroys nothing.
8. **Restore.** `adapter.restore(cp, mode)` with a timeout, measuring duration.
9. **Final manifest.** Capture `MF`.
10. **Compare.** The oracle diffs `MT → MF` and checks `preserve`.
11. **Attribution.** When the scenario requires it, `adapter.attribution`.
12. **Teardown.** `adapter.teardown`, stop the container, export evidence.
13. **Record.** Persist the result and the metrics.

For concurrent scenarios the steps between 6 and 7 are coordinated through barrier files. Damage
branches of a `parallel` group run in separate goroutines and the group joins before the damage
manifest is captured. A restore may also overlap a concurrent writer driven by
`restore.concurrent_write`: the writer holds the advisory lock on
`work/.barriers/writer.lock`, signals readiness with `work/.barriers/writer-ready`, the runner
restores the tree, then drops `work/.barriers/writer-stop`, the marker the writer watches for to
exit.

## 9. Damage and determinism

### 9.1. Two modes

- **scripted** (default): steps are defined in YAML. Deterministic and reproducible; used for the
  rating.
- **agentic**: a real LLM agent performs a free-form task. Marked `non_reproducible`, excluded
  from the rating, used to demonstrate realism.

Even in scripted mode, damage goes through the same interface as an agent's: either `shell` (a
shell inside the container) or `toolcall` (through `cmdshim`).

### 9.2. cmdshim

`cmdshim` is a bench binary placed first in `PATH` inside the container. Modes:

- `cmdshim exec -- <cmd>` — executes the command as a shell invocation and journals it;
- `cmdshim tool <name> --json <args>` — executes an "agent tool" call and journals it with
  `via=tool`.

The `via=shell` versus `via=tool` distinction is the basis of scenario S10: tools that only
listen to agent hooks must fail mutations issued through a shell.

### 9.3. Barriers

Barriers are files under `work/.barriers/` plus `flock`. The runner turns a `barrier` damage step
into a small POSIX-sh script (`internal/shim`) and executes it through `sandbox.Driver.Exec`, so
the local and the container drivers synchronize identically. Operations:

- `signal <name>` — create or refresh `.barriers/<name>`;
- `wait <name>` — wait for the marker file, polling every 20 ms, exit 1 on timeout;
- `wait-held <name>` — wait until the marker exists and its advisory lock is held
  (`flock -n <file> -c :` fails), polling every 20 ms;
- `wait-free <name>` — wait until the lock is acquirable with a blocking
  `flock -w <seconds> <file> -c :`; a missing marker counts as free.

Ordering is guaranteed by the marker files and the advisory locks; the sleep only paces the
polling loop, so two-session and torn-state scenarios stay deterministic instead of becoming
FLAKY. Barrier names are restricted to `[A-Za-z0-9][A-Za-z0-9_.-]*` so a scenario cannot inject
shell code. The runner adds `.barriers/**` to the allowed-added policy: barrier service files are
never reported as restore defects.

### 9.4. Time and environment control

Fixed values: `TZ=UTC`, `LANG=C.UTF-8`, `LC_ALL=C.UTF-8`, `SOURCE_DATE_EPOCH`, and the fixture
seed. Fixture file modification times are set explicitly (`unix.UtimesNanoAt`) so that
`meta_strict` is stable across runs.

## 10. Oracle

### 10.1. Capture

```
Walk(root):
  Lstat (without following symlinks)
  for a regular file: streaming blake2b-256, 1 MiB buffer
  blocks = st.Blocks (sparseness)
  xattr = Listxattr/Getxattr; EOPNOTSUPP sets an unsupported flag
  mode = st.Mode, uid/gid from Stat_t
```

Concurrency: a worker pool sized by `GOMAXPROCS`, while writes stay strictly sequential and
sorted. Traversal order is deterministic: names are sorted by bytes.

Hashing a 2 GiB file must not consume memory: streaming buffers only.

### 10.2. Diff

`oracle.Diff(base, observed, policy)` returns:

```go
type Diff struct {
    Missing   []Path   // present in base, absent in observed
    Added     []Path   // appeared; not always an error
    Changed   []Path   // content hash or type differs
    MetaOnly  []Path   // content matches, metadata does not
    Clobbered []Path   // belongs to preserve and changed
}
```

Memory: a merge-join over two sorted streams, without building two maps of 50k+ entries. At 50k
this does not matter, but the algorithm is designed for millions from the start.

### 10.3. Policy

A policy is a set of rules derived from the scenario and the platform:

- `Exclude(glob)` — remove from comparison;
- `RequireMeta(bool)` — whether metadata is part of the gate;
- `AllowAddedUnder(glob)` — which new paths are allowed (service directories, for example);
- `Preserve(glob)` — what must survive.

A policy is pure data and is serialized into the report. This allows a verdict to be recomputed
from saved manifests without rerunning the scenario (`unlucky-agent report --from <artifacts>`).

## 11. Metrics

`internal/metrics` takes a `Diff` plus timings and returns the metric structure. Formulas live in
one place so the report and the JSON never disagree.

Aggregation over five runs: median for timings, consensus for the verdict. Divergent verdicts
yield `FLAKY`; the scenario is excluded from the rating and listed in a separate section. The
`-repeat N` flag of `unlucky-agent run` runs every scenario N times with a fresh runner and
aggregates the results: identical verdicts pass through, any divergence becomes `FLAKY` with the
verdict sequence in the reason.

Published metrics: recall, content precision, metadata score, strict metadata score, clobbered
count, coverage vector by action class, p50/p95 added latency per agent tool call, checkpoint and
restore time, disk delta and peak disk, attribution level, flaky rate.

There is no single aggregate score. A single number hides class boundaries and turns a
capability table into a leaderboard.

## 12. Report

- `report/json` — a struct serialized with `encoding/json`, validated against
  `schema/report.v1.json` (the schema is embedded via `go:embed`; a test validates a golden
  report).
- `report/markdown` — `text/template`, deterministic output (every list is sorted).
- `unlucky-agent reproduce <report.json>` — reads the fingerprint, checks that the image and the
  adapters are available, reruns the same matrix and compares verdicts. A divergence produces a
  "not reproduced" report naming exactly what diverged.

Run fingerprint:

```go
type Fingerprint struct {
    CrashtestVersion string
    GoVersion        string
    SpecVersion      int
    FixtureHashes    map[string]string
    ImageDigest      string
    Adapters         map[string]string // id -> version
    Seed             int64
    Platform         string
}
```

## 13. Build and delivery

```
make build     # go build -trimpath -ldflags "-s -w -X ...version"
make test      # go test -race ./...
make lint      # gofmt -l, go vet, staticcheck (optional)
make images    # docker buildx build base + tools
make e2e       # run the MVP matrix in a container
make release   # goreleaser
```

Release artifacts: `unlucky-agent` for linux/amd64, linux/arm64, darwin/arm64; SHA256SUMS; SBOM;
image `ghcr.io/<org>/unlucky-agent-base:<version>` with a pinned digest.

Versioning:

- `unlucky-agent` follows semver;
- the scenario specification uses an integer `spec_version`, bumped on any semantic change to
  scenarios or the oracle;
- the report and manifest schemas use separate integers, bumped on incompatible field changes.

An incompatible scenario change invalidates comparability: old reports are marked `outdated` and
are never mixed with new ones.

## 14. Test strategy

- **Unit.** Scenario parser (including errors: unknown field, unknown glob), diff (synthetic
  trees in `testdata`), policy, metrics, report templates (golden).
- **Integration.** Fixture generators → manifest → diff on a real filesystem, without Docker.
  Metadata is covered: symlinks, permissions, sparse files, xattr.
- **Sandbox.** Guard and canary on a container; the "guard refuses a host bind mount" test is
  implemented with a deliberately broken configuration.
- **E2E.** The MVP matrix against `cp-r` and `perfect`: `perfect` must pass everything, `none`
  must fail everything. This validates the oracle.
- **Race and flakiness.** `go test -race`, five E2E repetitions in CI, and any divergent verdict
  fails the build.

## 15. Observability

- Structured logs on stderr (`slog`), JSON format, `-v` for verbosity.
- Every run step is an event with a duration.
- The damage journal is stored in evidence (`damage.log`): command, time, exit code.
- Adapter logs go to a separate file per run and never mix with the core.

## 16. Extension points

1. **Scenario** — YAML, no code.
2. **Adapter** — external process, any language.
3. **Fixture** — a Go generator registered in `fixture` by name.
4. **Tier** — added as a `tier` value plus a comparison rule in the policy.
5. **Report** — a new format through the `report.Writer` interface.

## 17. Explicitly out of scope for v1.0

- A web UI and a database: results are files in a directory.
- A cloud service and accounts.
- Comparing tools inside one workspace simultaneously: runs are isolated.
- An automatic leaderboard: publication is manual and follows external reproduction.

## 18. Future work

### 18.1. KVM sandbox driver

A VM-backed `sandbox.Driver` (`-sandbox kvm`) gives a guest kernel of its own and
unlocks a block-level snapshot baseline that no container driver can provide.

Value over the incus driver:

- A separate kernel: a rollback tool or a damage step cannot reach the host
  even through a container-runtime escape.
- The `filesystem snapshot` tier of the README becomes testable without a
  btrfs/zfs pool on the host: a checkpoint is a qcow2 internal snapshot of the
  guest disk, a restore rolls the block device back. This is the only realistic
  path to the `vm-snap` baseline.
- Scenarios touching kernel state (mount, setuid, ioctls) run against a real
  guest kernel.

Design:

- Runtime: Firecracker (fast boot, minimal API) or QEMU; the `sandbox.Driver`
  interface stays unchanged.
- Guest image: a small image with `sh`, `find`, `tar`, `du` and a persistent
  in-guest exec agent (or SSH over a host-only socket with ControlMaster
  multiplexing). Without multiplexing the per-exec latency of S19/S20 measures
  the transport, not the rollback tool.
- `Prepare` boots a fresh VM per scenario: `resources.disk_mb` maps to the
  work/home/scratch tmpfs sizes, memory maps to the VM limit.
- `CaptureRoots` streams work and home out with tar over the exec channel,
  same as the container drivers; no 9p/virtiofs host share, to keep the
  isolation story intact.
- Guard: DMI/`systemd-detect-virt` marker, absence of host shares in
  `/proc/mounts`, only `lo` in the guest network namespace.
- New baseline `vm-snap`: snapshot/restore of the block device, restore is
  byte-exact across metadata, so the oracle checks it under the strict tier.

Cost: roughly 1.5–2k lines (image pipeline, exec channel, guard, qcow2
rollback, tests) plus a guest-image build target; integration tests need
`/dev/kvm` on the host. A cheaper partial path exists: run the existing
docker driver under Kata containers — VM isolation with no new driver, but no
`vm-snap` baseline either.

Sequencing: validate the incus driver on a real host first; the KVM driver is
worth its cost only when the block-snapshot tier is actually on the roadmap.
