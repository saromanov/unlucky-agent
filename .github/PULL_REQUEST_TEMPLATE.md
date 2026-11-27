## What

<!-- One or two sentences: what changes. -->

## Why

<!-- The defect, scenario or measurement gap this closes. Link the issue. -->

## Checklist

- [ ] Tests added or updated for the change (a scenario counts as a test).
- [ ] `make ci` passes and `golangci-lint run` is clean.
- [ ] Docs updated when a flag, verdict field or isolation boundary changed.
- [ ] New or changed scenario is idempotent (`-repeat N` gives the same verdict).
- [ ] `scenarios/*.yaml` not related to this change are untouched.
