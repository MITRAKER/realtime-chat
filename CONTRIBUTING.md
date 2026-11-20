# Contributing

Thanks for helping! This guide explains how to set up the project, how changes
are made, and what every pull request must pass.

## Set up

You need [Go 1.26+](https://go.dev/dl/), Docker (for PostgreSQL), `make`, and
[golangci-lint](https://golangci-lint.run/) (`make tools` installs it). The
pre-commit hooks also need Python, for [pre-commit](https://pre-commit.com/).

```sh
git clone https://github.com/MITRAKER/realtime-chat
cd realtime-chat
make tools        # installs golangci-lint
make hooks        # installs the pre-commit hooks (needs: pip install pre-commit)
make db-up        # starts PostgreSQL in Docker
cp .env.example .env   # then set JWT_SECRET, e.g. openssl rand -hex 32
make run          # http://localhost:8080
```

`make help` lists every task.

## Making a change

1. **Open or pick an issue** so work isn't duplicated.
2. **Branch from `main`** with a short descriptive name: `feat/typing-indicator`,
   `fix/history-paging`, `docs/learning-ch3`.
3. **Write the code and its tests together.** Bug fixes start with a test that
   fails without the fix.
4. **Run `make check`** before pushing. It runs CI's main checks: tidy
   modules, `go vet`, `golangci-lint`, the JPL rule checker and all tests with
   the race detector. Run `make test-db` to include the database tests,
   `make cover` for the coverage report, and `make fuzz` after changing code
   that reads outside input.
5. **Open a pull request** and fill in the template. CI must be green and the
   code owner must approve before merging.

## Commit messages

We use [Conventional Commits](https://www.conventionalcommits.org/), so the
history reads well and release notes can be generated from it:

```text
feat(chat): add typing indicators
fix(auth): reject tokens without an expiry
docs: explain keyset pagination
test(postgres): cover concurrent migrations
ci: run fuzz tests nightly
```

Types: `feat`, `fix`, `docs`, `test`, `refactor`, `perf`, `ci`, `build`, `chore`.
Add `!` after the type for a breaking change (`feat!: ...`).

## Code standards

- Follow [docs/CODING_STANDARDS.md](docs/CODING_STANDARDS.md): NASA/JPL's
  Power of Ten rules adapted to Go. The short version: no recursion, every loop
  bounded or documented, functions under 60 lines, every error handled, every
  input validated, zero linter findings.
- Code is formatted by `gofmt` and `goimports` (`make fmt`).
- Exported names have doc comments. Comments explain *why*, not *what*.
- Never commit secrets. `.env` is ignored by git; GitHub's secret scanning and
  the `gitleaks` pre-commit hook catch mistakes.

## What CI checks

| Check | Tool | Fails when |
|-------|------|------------|
| Tidy modules | `go mod tidy -diff` | `go.mod` or `go.sum` needs changes |
| Formatting, bugs, style, security, complexity | `go vet`, `golangci-lint` | any finding |
| NASA/JPL rules 1 and 2 | `jplcheck` | recursion, `goto`, or an undocumented unbounded loop |
| Shell scripts | ShellCheck | any finding |
| Correctness | `go test -race` with PostgreSQL, on the two supported Go releases | a test fails or a data race is found |
| Coverage | `scripts/coverage.sh` | total coverage is below 80% |
| Known vulnerabilities | `govulncheck` | the code calls a vulnerable function |
| Security bugs | CodeQL | the change adds an alert of severity "error", or a security alert rated high or critical |
| Risky dependency changes | Dependency review | a PR adds a dependency with a known vulnerability of moderate severity or worse |
| Builds | `go build`, `goreleaser check`, Docker | a release platform or the image fails to build, or the release configuration is invalid |
| Fuzz tests (daily) | `scripts/fuzz.sh` | a fuzz test finds an input that breaks its property |

**CI OK** passes only when every job of the CI workflow passed, so it is the
one CI check to require in branch protection, together with **CodeQL** and
**Dependency review**, which run as separate workflows. Dependency review
needs the repository's dependency graph, which the owner switches on under
Settings, Advanced Security.

## Releases

Versions follow [Semantic Versioning](https://semver.org/): `MAJOR.MINOR.PATCH`.
To release:

1. Move the `Unreleased` entries in `CHANGELOG.md` under a new version heading
   with today's date, and merge that.
2. Tag the merge commit and push the tag:

   ```sh
   git tag -a v0.3.0 -m "v0.3.0"
   git push origin v0.3.0
   ```

3. The Release workflow tests the tag, builds binaries for Linux, macOS and
   Windows, publishes a GitHub release with checksums, and pushes the Docker
   image `ghcr.io/mitraker/realtime-chat`.

## Reporting security problems

Please don't open a public issue. See [SECURITY.md](SECURITY.md).
