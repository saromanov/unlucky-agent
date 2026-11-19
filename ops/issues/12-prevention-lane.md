# Prevention lane: dangerous command corpus with false-positive metric

Separate mode `unlucky-agent prevent`, after v1.0.

## Scope

- Dangerous corpus: `rm -rf` outside the project, `git push --force`, `DROP TABLE`,
  `chmod -R 000`, `.env` overwrite.
- Benign corpus: `rm -rf node_modules`, `git push --force-with-lease`, `DROP TABLE IF EXISTS tmp_*`.
- Metrics: detection rate and false positive rate.

## Acceptance

- A detector without an FP metric is not reported.
- Corpus is versioned and derived from public incident reports.
