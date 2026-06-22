# Security Policy

## Scope

wlog is a **local-only** tool. By default it binds the loopback interface
(`127.0.0.1`) only, makes no outbound network connections, and stores data in a
local SQLite file created with `0600` permissions. A non-loopback bind requires
`--unsafe` or `--auth-token`, and includes a Host-header check to guard against
DNS rebinding.

Prompts and tool arguments are **not** collected unless you opt in on the Claude
Code side (`OTEL_LOG_USER_PROMPTS=1`); `--no-store-prompts` drops them even when
sent.

## Supported versions

The latest released version receives security fixes.

| Version | Supported |
|---------|-----------|
| 0.1.x   | ✅ |

## Reporting a vulnerability

Please **do not** open a public issue for security problems.

Use **GitHub's private vulnerability reporting**:
[Report a vulnerability](https://github.com/openwong2kim/wlog/security/advisories/new).

Include a description, affected version (`wlog version`), reproduction steps, and
impact. We aim to acknowledge within a few days and will coordinate a fix and
disclosure timeline with you.
