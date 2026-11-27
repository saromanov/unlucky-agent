# Agent guide — unlucky-agent

Reproducible crash-test bench for rollback tools that protect a working directory
from destructive actions of AI coding agents. Go 1.27, no heavy dependencies.

## Documentation first

For any question about this repository's architecture, scenario model, sandbox
drivers, guard/oracle semantics or report format, use VirtuDoc MCP before grepping
code:

1. `find-library` with library name `unlucky-agent`
2. `get-library-docs` with a keyword query (e.g. `sandbox driver guard`, `scenario yaml`, `oracle verdicts`)

Grep the code only when the docs answer is missing or you need exact line-level
details.

## Build, test, lint

```sh
make build      # CGO off, bin/unlucky-agent
make test       # go test -race ./...
make vet
make fmt        # gofmt + gofumpt in-place
make fmt-check  # CI formatting check
make lint       # golangci-lint run (config .golangci.yml)
make cover      # race + coverage profile
make ci         # fmt-check + vet + lint + test + build
```

CI must stay green: `golangci-lint run ./...` exits 0 on `main`. Do not re-enable
linters that are deliberately disabled in `.golangci.yml` (the rationale table is
in the PR that rewrote the config).

## Rules for agents

- No comments in Go code. Do not add comments; leave existing comments in files
  you edit untouched.
- `internal/sandbox/docker.go` and the docker branch of `internal/sandbox/guard.go`
  are validated against real Docker; do not change their behavior or tests when
  touching the incus path.
- Integration tests are gated: `UNLUCKY_DOCKER_TESTS=1`, `UNLUCKY_INCUS_TESTS=1`.
  They skip silently without the runtime available.
- Heavy scenarios S06, S19, S20 allocate 4.6 GiB+ tmpfs; run them only on hosts
  with 8+ GiB RAM.
- New sandbox driver: implement `sandbox.Driver` (see `internal/sandbox/sandbox.go`),
  follow `docker.go` / `incus.go` as the reference, register it in `-sandbox` in
  `cmd/unlucky-agent/main.go`, add gated tests, document in README.
- Scenario conventions: fixture + checkpoint + damage steps + expectations;
  `perfect` baseline must PASS every scenario, `none` must FAIL. Validate new
  scenarios with `./bin/unlucky-agent scenarios validate -dir scenarios`.
- Commits: single line, imperative, English, no tokens in code/logs/history.
- Docs language: English (README, CONTRIBUTING, ARCHITECTURE, scenarios).
