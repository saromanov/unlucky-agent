# Adapter protocol: JSON-RPC over stdio and registry

Tools under test are external processes. The core must not link against them.

## Scope

- Line-delimited JSON-RPC: `handshake`, `prepare`, `checkpoint`, `restore`, `attribution`,
  `teardown`.
- `handshake` reports id, tier, version and capabilities.
- Registry file `adapters.yaml` with pinned versions and optional image digest.
- Built-in adapters exposed as `unlucky-agent adapter <id>`.
- Protocol version mismatch is an explicit `ERROR`, never a silent skip.

## Acceptance

- A minimal reference adapter (snapshot into a directory) passes S02, S04, S05, S21.
- Adapter that fails `restore` idempotency is caught by S15.
- Adapter crash mid-call yields `ERROR` with a readable reason, not a hang.
