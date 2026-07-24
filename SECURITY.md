# Security Policy

## Supported Versions

The latest release of Coding Hermes Scheduler is supported with security updates.

| Version | Supported          |
| ------- | ------------------ |
| latest  | :white_check_mark: |

## Reporting a Vulnerability

Do NOT open a public issue for security vulnerabilities.

Email: [REDACTED]

Expect acknowledgment within 48 hours and a fix timeline within 7 days.

## Architecture

The Scheduler manages agent spawn lifecycles via the Hermes Gateway API.
It stores project/enabled/cooldown state in a local SQLite database.
API keys are passed via environment variables and never logged or stored in git.

## Dependencies

This project uses Go modules. Dependencies are pinned in `go.sum`.
Run `go list -u -m all` to check for available updates.
