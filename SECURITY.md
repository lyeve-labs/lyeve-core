# Security Policy

## Supported versions

LyEve Core is pre-1.0. Security fixes land on the latest release.

| Version | Supported |
|---------|-----------|
| Latest `0.x` | Yes |
| Older `0.x` | No |

## Reporting a vulnerability

**Please do not open a public issue for security vulnerabilities.**

Email **security@lyeve.com** with:

- a description of the vulnerability,
- steps to reproduce,
- the potential impact, and
- any suggested fix.

We acknowledge reports within **48 hours** and aim to share a remediation
timeline within **7 days**. We follow coordinated disclosure and will credit
reporters in the release notes unless you prefer to remain anonymous.

## Scope

**In scope:** authentication or authorization bypass, license-verification
bypass, SQL injection, remote code execution, and cross-tenant data access.

**Out of scope:** volumetric denial of service (rate limiting is configurable),
not-yet-patched upstream dependency issues, and social engineering.
