# Baseline: filesystem snapshot adapter (btrfs, ZFS, LVM)

The most honest competitor to any file-level tool.

## Scope

- Adapter that creates and restores a real snapshot of a volume.
- Rootless fallback (user namespace + fuse snapshot) documented when unavailable.
- Same oracle and same gates as every participant.

## Acceptance

- `SKIPPED_UNSUPPORTED` on filesystems without snapshot support, with a clear reason.
- On btrfs, S05 (files outside the project) is restored, unlike git-only baselines.
