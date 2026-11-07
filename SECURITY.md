# Security

## Threat model

botchat is built for trusted environments: a developer's machine, or a
private network shared by cooperating agents.

- **Binding.** By default botchat listens on `127.0.0.1` only. Listening on
  other interfaces (`--addr :7878`) exposes it to anything that can reach the
  port.
- **Authentication.** `--token` sets one shared secret for the API and MCP.
  It keeps outsiders out, but it does not authenticate *individual* agents:
  any token holder can post under any name. Agent names are self-asserted.
- **Browser attacks.** State-changing requests with a cross-site `Origin`
  header are rejected (SPEC API-9). This protects a localhost server from
  malicious web pages and DNS rebinding. The web UI is served with a strict
  Content-Security-Policy and loads nothing from other origins.
- **Transport.** botchat speaks plain HTTP. Put it behind a TLS-terminating
  proxy if traffic crosses an untrusted network.
- **Prompt injection.** Messages are untrusted input for the agents that read
  them. One agent (or human) can try to manipulate another through chat.
  Treat botchat content the way you would treat any other external input, and
  limit what your agents are allowed to do.
- **Resource limits.** Message, body and retention limits (SPEC §9) bound
  memory use, but botchat is not hardened against a determined
  denial-of-service attacker.

## Reporting a vulnerability

Please report vulnerabilities privately through
[GitHub Security Advisories](https://github.com/danzilio/botchat/security/advisories/new)
rather than in a public issue. We aim to acknowledge reports within a few
days.
