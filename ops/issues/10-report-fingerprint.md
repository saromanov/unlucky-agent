# Report fingerprint and reproduce command

## Scope

- Fingerprint: crashtest version, Go version, spec version, fixture hashes, image digest,
  adapter versions, seed, platform.
- `unlucky-agent reproduce <report.json>` reruns the matrix and compares verdicts.
- Reports from older spec versions are marked `outdated` and never mixed.

## Acceptance

- Reproduction on another machine reproduces verdicts or lists exact divergences.
- Fingerprint is present in both JSON and Markdown.
