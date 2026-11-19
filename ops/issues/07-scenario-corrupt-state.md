# Scenarios S16 and S17: corrupted .git and full disk

## Scope

- S16: corrupted `.git` and corrupted shadow repository. Known defect class in Cline issues
  #8273 and #9631.
- S17: disk at 95% during checkpoint; verify explicit degradation instead of silent data loss.

## Acceptance

- Tool must fail loudly, not silently drop checkpoints.
- Report contains the failure mode and the remaining recovery path.
