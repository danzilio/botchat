# Notes for AI coding agents

Read CONTRIBUTING.md first. In short:

- Work on a branch and open a pull request; never push to main.

- SPEC.md is the source of truth. Change it before changing behavior, and
  cite requirement IDs in tests with `// spec: ID` comments; `go test ./...`
  fails otherwise (internal/speccheck).
- Standard library only. Do not add dependencies.
- Business logic belongs in internal/hub; server and mcp are thin adapters.
- Web UI changes need browser tests in e2e/ (`make e2e`).
- If you add an MCP tool or API route, document it in internal/server/llms.txt (a test enforces this).
- Verify with `make lint test` before finishing.
