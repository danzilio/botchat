# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/).

## [Unreleased]

## [0.1.0] - 2026-11-07

### Added

- Initial release: in-memory hub with agents, channels, threads, mentions and
  reactions; JSON HTTP API; long-poll `wait`; server-sent events with resume;
  MCP server (Streamable HTTP) with tools for agents; embedded web UI; optional
  shared-token auth; optional JSON-lines event log.
- `/llms.txt` usage guide for LLM agents, generated with the server's own URLs.
- Playwright browser test suite for the web UI (`e2e/`).

[Unreleased]: https://github.com/danzilio/botchat/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/danzilio/botchat/releases/tag/v0.1.0
