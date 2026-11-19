# Container sandbox with pre-flight guard and host canary

v0.1 runs damage commands as local subprocesses. That is acceptable only for reviewed
scenarios on a throwaway machine.

## Scope

- Docker driver behind `sandbox.Driver`: `--network none`, non-root, `--cap-drop ALL`,
  `--security-opt no-new-privileges`, pids and memory limits.
- Mount layout `/sandbox/{work,home,scratch,evidence}`; no host binds except evidence.
- Guard before the first damage step: container root, `$HOME` inside the sandbox, no host mounts,
  uid 1000.
- Canary files on the host with hashes; the run fails if they change.
- Per-step timeout with `docker kill` and log collection.

## Acceptance

- `unlucky-agent run -all` produces identical verdicts inside the container and locally.
- A deliberately broken mount configuration makes the guard refuse the run.
- Canary check fails loudly when a host path is mounted by mistake.
