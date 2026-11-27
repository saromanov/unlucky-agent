# Security policy

## Reporting a vulnerability

Use GitHub private vulnerability reporting: open the **Security** tab of this repository and
choose **Report a vulnerability**. It creates a private GitHub Security Advisory visible only to
you and the maintainers.

Do not open a public issue for anything that lets damage escape the sandbox, bypasses the
pre-flight guard, defeats the host canary or silently corrupts a report.

## Scope

Reports that are in scope:

- A sandbox driver (`docker`, `incus`) that lets a scenario command read or write outside the
  sandbox trees.
- A `GUARD_FAILED` or `CANARY_FAILED` condition that is missed, or a guard that a scenario can
  pass while it should refuse the run.
- A verdict that reports `PASS` for a run that did not restore the tree.
- Secret or host data leaked into reports, manifests or logs.

The `local` driver is not a security boundary by design: it runs commands as your user. Reports
against it are treated as documentation bugs, not vulnerabilities.

## Expectations

This is a small project maintained on a best-effort basis. There is no SLA and no guaranteed
response time. Fixes are released in the next tagged version when possible.

## Versions

Only the latest release and the current `main` branch receive fixes. Older tags are not
maintained.

## License

The project is released under the MIT License, which includes the usual "as is" warranty
disclaimer. See [`LICENSE`](LICENSE).
