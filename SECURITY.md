# Security policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Please **do not open a public issue** for security problems.

Report it privately through GitHub instead:
[Report a vulnerability](https://github.com/MITRAKER/realtime-chat/security/advisories/new).

Include what you found, how to reproduce it, and what an attacker could do
with it. You'll get a reply within 7 days. Once a fix is released, the advisory
is published and you're credited, unless you'd rather not be.

## What this project does to stay secure

- Passwords are hashed with bcrypt; login tokens are signed JWTs with a pinned
  algorithm and an expiry. See LEARNING.md chapter 2.
- Every input from outside is validated where it enters, and fuzz tests check
  the code that reads it with millions of generated inputs every day.
- Every pull request runs CodeQL, `gosec`, `govulncheck` and dependency review.
  `gosec` and `govulncheck` run again every day, and CodeQL every week, so
  newly published vulnerabilities are found without waiting for a change.
- Dependabot proposes dependency updates every week.
- The `gitleaks` pre-commit hook stops secrets before they are committed.
  GitHub's secret scanning, free for public repositories, should stay on in
  the repository settings (Settings, Code security).
- Release images run as a non-root user on a minimal base image, and come
  with an SBOM (a list of everything inside) and build provenance.
