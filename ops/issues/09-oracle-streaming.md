# Oracle: merge-join diff and external sort for large trees

Current implementation loads manifests into memory. Fine for 50k entries, not for millions.

## Scope

- Merge-join over two sorted manifest streams.
- External sort when the entry count exceeds a threshold.
- Preserve the public `Diff` API for tests and the `diff` command.

## Acceptance

- Memory stays below 512 MiB on a 3M-entry manifest.
- Golden tests produce identical output before and after the change.
