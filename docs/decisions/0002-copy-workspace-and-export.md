# Copied workspace and explicit export

- Status: accepted
- Date: 2026-09-22

## Decision

Copy-mode sessions receive an allowlisted copy of workspace files, not a Git clone or host `.git` metadata. Changes return to the source only through a separate preview/apply export that checks for host-side conflicts and never applies deletions.

## Rationale

The copied file set is the confidentiality boundary. Exposing `.git` can disclose commit history and remotes beyond the files the user explicitly selected. Export keeps the review boundary on the host and avoids silently overwriting concurrent changes.

## Consequences

Git history, branch operations, and pushing are not available inside a copied session. Users review and push from the host after export. Exported additions and edits remain ordinary host worktree changes; the copied session itself must still be deleted explicitly.
