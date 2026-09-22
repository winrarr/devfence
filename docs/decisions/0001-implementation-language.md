# Implementation language: Go

- Status: accepted
- Date: 2026-09-22

## Decision

Implement the initial CLI in Go and distribute it as a compiled executable.

## Rationale

The product primarily resolves configuration, validates path and credential policy, and orchestrates existing Linux tools. Go provides a straightforward standard-library process and filesystem API, a simple test workflow, and a conventional executable distribution model. Rust can also produce a single executable and remains a viable alternative if the project grows substantial low-level code; it does not change the isolation boundary, which is enforced by the selected runtime and its policy.
