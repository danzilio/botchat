# Contributing to botchat

Thanks for helping! botchat is small on purpose. This guide explains how the
project is organized and how to get a change merged.

## Ground rules

- **Standard library only.** botchat ships as one dependency-free binary.
  `go.mod` has no `require` lines, and a test enforces that. If you think a
  dependency is needed, open an issue first.
- **Ephemeral by design.** Persistence, accounts and federation are non-goals
  (see [SPEC.md §1](SPEC.md#1-goals-and-non-goals)).
- **Small, focused PRs** are much easier to review than large ones.

## Spec-driven development

[SPEC.md](SPEC.md) is the source of truth for behavior. Every normative
requirement has an ID such as **MSG-4**, and every ID must be covered by at
least one test that cites it:

```go
// spec: MSG-4
func TestThreads(t *testing.T) { ... }
```

`internal/speccheck` runs as part of `go test ./...` and fails if a
requirement has no test, or if a test cites an ID that doesn't exist.
Browser tests in `e2e/` count too.

The workflow for a behavior change is:

1. **Spec.** Edit SPEC.md. Add a requirement with the next free ID in its
   section, or reword an existing one. Never reuse a deleted ID.
2. **Tests.** Write or update tests citing the ID. They should fail.
3. **Code.** Make them pass.
4. **Changelog.** Add a line under "Unreleased" in CHANGELOG.md.

Pure refactors, bug fixes that bring code in line with the spec, docs, and
tests need no spec change.

For larger features, open an issue or a draft PR containing only the spec
change first. Agreeing on behavior before writing code saves everyone time.

## Layout

```
cmd/botchat/        main package: signal handling, version stamping
internal/app/       flag parsing, wiring, lifecycle (CLI-*)
internal/hub/       in-memory state, events, wait; no I/O (AGT, CHN, MSG, RXN, EVT)
internal/server/    HTTP API, SSE, auth, origin checks (API-*, SSE-*)
internal/mcp/       MCP Streamable HTTP server and tools (MCP-*)
internal/hook/      `botchat hook stop` for Claude Code / Codex (HOOK-*)
internal/web/       embedded single-page UI, no build step (WEB-*)
internal/speccheck/ spec ↔ test coverage enforcement
e2e/                Playwright browser tests for the web UI (dev-only)
```

The hub has no knowledge of transports. The server and mcp packages are thin
adapters over it, so most logic (and most tests) belong in `internal/hub`.

## Development

Requires Go 1.24 or newer.

```sh
make test      # go test -race ./...
make e2e       # browser tests (needs Node.js; installs Playwright + Chromium)
make lint      # gofmt + go vet
make cover     # coverage report
make run       # build and run on 127.0.0.1:7878
make build     # ./bin/botchat
```

The web UI is plain HTML, CSS and JavaScript in `internal/web/static`, with
no build step and no external resources. Edit the files, then rebuild and
reload. Please also look at UI changes in both light and dark mode.

### Browser tests

`e2e/` is a separate, dev-only Node project using
[Playwright](https://playwright.dev). It is never part of the binary, and Go
contributors who don't touch the UI can ignore it (CI runs it for you).

```sh
cd e2e
npm ci
npx playwright install chromium
npx playwright test            # headless
npx playwright test --ui       # interactive runner, great for debugging
```

The suite builds the binary once, then each test starts its own botchat
process on a random port, so tests are isolated and run in parallel. Seed
state through the `botchat.api` / `botchat.post` fixtures rather than through
the UI unless the UI interaction is what you're testing. Cite requirement IDs
with `// spec: WEB-N` just like Go tests.

### Testing guidelines

- Tests must be deterministic. Use `hub.Options.Now` to control time instead
  of sleeping where possible, and keep any sleeps short.
- Run with `-race`. CI does.
- Prefer black-box tests through the public API of a package.
- Integration tests in `internal/app` start a real server on `127.0.0.1:0`.

## Commit messages and PRs

- Use the imperative mood ("Add thread filter to wait") and reference spec IDs
  where relevant.
- Fill in the PR template checklist.
- CI must be green: tests (with race detector) on Linux, macOS and Windows,
  browser tests, plus `gofmt` and `go vet`.

## Releasing (maintainers)

1. Move "Unreleased" entries in CHANGELOG.md under a new version heading.
2. Tag: `git tag -a v0.2.0 -m v0.2.0 && git push origin v0.2.0`.
3. The release workflow builds binaries with GoReleaser and publishes them.

## Code of conduct

Be kind and assume good intent. Harassment or personal attacks are not
tolerated. Maintainers may remove comments or contributors that violate this.
