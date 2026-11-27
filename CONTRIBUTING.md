# Contributing to unlucky-agent

## Development

Requirements: Go 1.27+, `make`, `golangci-lint`, optionally Docker or Incus for the container
drivers.

```sh
git clone https://github.com/saromanov/unlucky-agent
cd unlucky-agent
make build      # build bin/unlucky-agent
make test       # go test ./...
make race       # go test -race ./...
make lint       # golangci-lint over .golangci.yml
make ci         # lint, test, build
```

Run `make race` before opening a pull request: the runner and manifest code is concurrency
sensitive. Run `golangci-lint run` as well; the full linter set lives in `.golangci.yml`.

Container driver tests are opt-in because they need a host runtime:

```sh
UNLUCKY_DOCKER_TESTS=1 go test ./internal/sandbox/
UNLUCKY_INCUS_TESTS=1 go test ./internal/sandbox/
```

Heavy fixtures are gated the same way with `UNLUCKY_BIG_TESTS=1`.

## Commits

One line, imperative mood, English, no trailing period: `Add incus sandbox driver with pre-flight
guard`. One logical change per commit; do not mix a driver fix with a scenario rewrite.

## Pull requests

- Keep pull requests small and focused on one problem.
- A behaviour fix needs a test that fails without the change. A new scenario in `scenarios/*.yaml`
  is the preferred way to pin a rollback defect.
- Do not touch `scenarios/*.yaml` you are not addressing: ids, fixtures and expectations are a
  shared contract, and editing one changes every report produced from it. A scenario must stay
  idempotent, so check a new one with `-repeat N`.
- Update `README.md` and `docs/ARCHITECTURE.md` when a flag, verdict field or isolation boundary
  changes.

## Adding a scenario

1. Copy the closest file in `scenarios/` and use the next free `Sxx` id.
2. Fill `fixture`, `checkpoint`, `damage`, `restore`, `expect`. Parsing is strict, so an unknown
   key is an error.
3. `perfect` must pass and `none` must fail; if `perfect` fails, the scenario or the oracle is
   wrong:

```sh
./bin/unlucky-agent scenarios validate -dir scenarios
./bin/unlucky-agent run -scenario scenarios/S22.yaml -baseline perfect -out out/s22
./bin/unlucky-agent run -scenario scenarios/S22.yaml -baseline none -out out/s22-none
```


## Adding a sandbox driver

1. Implement `sandbox.Driver` (declared in `internal/sandbox/sandbox.go`) in a new file under
   `internal/sandbox/`, following `docker.go` or `incus.go`.
2. `Prepare` stages the fixture, `Guard` verifies the boundary and returns `ErrGuardFailed`
   (`GUARD_FAILED`) on a failed check, `Exec` runs the command with a timeout, `Close` tears the
   sandbox down without touching the host.
3. Wire the driver into the `-sandbox` switch in `cmd/unlucky-agent/main.go`.
4. Add a gated integration test next to `docker_test.go`, with its own `UNLUCKY_*_TESTS=1` gate.
5. Document image contents, mounts and limits in the `Sandbox` section of `README.md`.

## Where things live

`docs/ARCHITECTURE.md` holds the design and invariants; `scenarios/` is the catalog; `internal/`
is the bench core, one package per concern; `docker/` is the sandbox image.

## Running the bench on your own rollback command

```sh
./bin/unlucky-agent run -scenario scenarios/S02.yaml \
  -restore 'tar -xpf /tmp/snapshot.tar' -out out/custom
```

Use `-sandbox docker` for scenarios you did not write: the `local` driver runs damage commands
directly on the host.
