# Scenarios S01 and S03: tracked directory loss and reset --hard

## Scope

- S01: `rm -rf` over a tracked directory, `git` fixture.
- S03: `git reset --hard` with uncommitted edits in tracked files.
- A correct git-shadow tool restores both; the `none` baseline must fail with an exact missing set.

## Acceptance

- Scenarios are valid YAML and pass `scenarios validate`.
- `perfect` reference adapter passes; `none` fails with the expected missing set.
