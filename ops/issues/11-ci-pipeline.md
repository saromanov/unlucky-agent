# CI: fmt, vet, race, e2e matrix, canary verification

## Scope

- GitHub Actions: `gofmt -l`, `go vet`, `go test -race ./...` on linux and macOS.
- E2E job runs the MVP matrix with `none` and `cp-r`, five repetitions.
- Artifacts: reports, manifests, logs.
- Any verdict divergence across the five repetitions fails the build.

## Acceptance

- Pipeline is green on a clean clone.
- A deliberately flaky scenario turns the build red and names the scenario.
