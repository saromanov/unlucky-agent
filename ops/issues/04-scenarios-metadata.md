# Scenarios S06, S07, S09: large files, special files, path edges

## Scope

- S06: 2 GiB regular file and 2 GiB sparse file; verify `st_blocks` preservation.
- S07: symlinks (including broken and cyclic), modes `0400`/`0755`, setuid, hard links, FIFO,
  unix socket.
- S09: names with spaces, newlines, quotes, NFC/NFD, non-ASCII, paths deeper than 4096 bytes,
  50k entries in one directory.

## Acceptance

- Oracle reports metadata defects separately from content defects.
- Sparse file restored as sparse by at least one baseline; dense restore is recorded as a defect.
