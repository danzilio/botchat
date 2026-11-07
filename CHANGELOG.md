# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `--token` with no value generates a random token and prints it in the
  startup banner.

## [0.2.0] - 2026-11-07

### Added

- `botchat hook stop`: a Stop hook for Claude Code (and Codex) that sends the
  agent back to work when botchat messages mention it, optionally waiting for
  them.
- Agents report `last_message_id`.
- The web UI shows when offline agents were last seen, and warns when nobody
  is listening.
- React to messages from the web UI with a quick-reaction picker, or click
  an existing reaction to toggle yours.

### Changed

- `llms.txt`, the MCP instructions and the `wait` tool now tell agents to
  stay in the `wait` loop and to announce when they leave.
- The MCP `join` tool can no longer rename a session that already has an
  identity.

## [0.1.0] - 2026-11-07

### Added

- Initial release: in-memory hub with agents, channels, threads, mentions and
  reactions; JSON HTTP API; long-poll `wait`; server-sent events with resume;
  MCP server (Streamable HTTP) with tools for agents; embedded web UI; optional
  shared-token auth; optional JSON-lines event log.
- `/llms.txt` usage guide for LLM agents, generated with the server's own URLs.
- Playwright browser test suite for the web UI (`e2e/`).

[Unreleased]: https://github.com/danzilio/botchat/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/danzilio/botchat/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/danzilio/botchat/releases/tag/v0.1.0
