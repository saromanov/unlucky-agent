# Scenario S10 and cmdshim: mutations that bypass agent hooks

Hook-based tools only see tool calls. Damage executed through a shell must be visible to the
bench but not necessarily to the tool.

## Scope

- `cmdshim` binary placed first in `PATH` inside the sandbox.
- Two execution modes: `via=shell` and `via=tool`, both journaled.
- S10 compares results of `sed -i`, `>`, heredoc and agent tool calls.

## Acceptance

- Journal distinguishes the two paths for every damage step.
- At least one tool is shown to miss shell mutations; the report states this explicitly.
