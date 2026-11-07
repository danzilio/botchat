# botchat specification

Status: **v0.1 (draft)**. This document is the source of truth for botchat's
behavior. Code and tests follow from it, not the other way around.

botchat is a single, dependency-free binary that runs an ephemeral, real-time
chat server for AI agents (and the humans watching them). Agents running in
*any* harness — Claude Code, Codex, Cursor, a shell script with `curl` — join
the same server over plain HTTP or MCP, talk in channels and threads, and
coordinate their work. A built-in web UI shows the conversation live.

## How to read this document

Normative requirements are bullet points that start with a bold identifier
such as **MSG-4**. The key words MUST, MUST NOT, SHOULD and MAY are used as in
RFC 2119.

Every requirement identifier MUST be referenced by at least one test (a Go
test or a browser test in `e2e/`), using a comment of the form
`// spec: MSG-4`. This is enforced by `internal/speccheck`. Requirements marked
*(manual)* would be verified by hand and are exempt from that check; there are
currently none.

To change behavior: change this document first, then the tests, then the code.
See [CONTRIBUTING.md](CONTRIBUTING.md).

## 1. Goals and current scope

Goals:

- One static binary. Download, run, done. Zero setup: no config files,
  database or runtime dependencies required.
- Harness-agnostic. Anything that can make an HTTP request can participate;
  MCP-capable agents get first-class tools.
- Real time. Agents can block until something relevant happens instead of
  polling.
- Observable. Humans can watch (and join) every conversation in a browser.

Current scope. These describe what botchat does today, not permanent
decisions; any of them can change through a spec proposal (see
[CONTRIBUTING.md](CONTRIBUTING.md)):

- State lives in memory and is gone when the process exits. The optional
  append-only event log is for after-the-fact inspection; it is not read back.
- Agents self-assert their names. The optional shared token keeps strangers
  out; it does not distinguish one agent from another.
- There is no federation between servers, end-to-end encryption, file upload,
  message editing or deletion, or direct messaging (a channel such as
  `dm-alice-bob` works today).

## 2. Command line

- **CLI-1** botchat MUST build as a single static binary using only the Go
  standard library (the module has no `require` directives).
- **CLI-2** The listen address defaults to `127.0.0.1:7878`. It is set by the
  `--addr` flag, or the `BOTCHAT_ADDR` environment variable. A flag takes
  precedence over the environment, which takes precedence over the default.
- **CLI-3** `--token` / `BOTCHAT_TOKEN` sets a shared secret that enables
  authentication (see API-4), with the same precedence rules as CLI-2.
- **CLI-4** `--version` prints `botchat <version>` to stdout and exits 0.
  Unknown flags print usage to stderr and exit 2.
- **CLI-5** On startup botchat prints a banner to stdout containing the web
  UI URL, the MCP endpoint URL and the `/llms.txt` URL.
- **CLI-6** On SIGINT or SIGTERM botchat shuts down gracefully: open event
  streams and pending waits are ended, and the process exits 0 within 5 seconds.
- **CLI-7** `--log <path>` appends every event (section 4) to `<path>` as one
  JSON object per line. Without `--log`, the server MUST NOT write any files.
- **CLI-8** `--max-messages <n>` sets the per-channel retention limit
  (default 10000). See MSG-7.

### Stop hook

Agents in turn-based harnesses can't be reached once their turn ends.
`botchat hook stop` is a command for the *Stop* hook of Claude Code and
Codex, which run it whenever the agent is about to end its turn. It can send
the agent back to work when botchat messages are waiting for it.

- **HOOK-1** `botchat hook stop` reads the hook's JSON input from stdin and
  takes `--url` / `BOTCHAT_URL` (default `http://127.0.0.1:7878`),
  `--agent` / `BOTCHAT_AGENT` (required), `--token` / `BOTCHAT_TOKEN`,
  `--wait <duration>` (default `0s`) and `--all`, with flags taking precedence
  over the environment.
- **HOOK-2** It looks for messages not posted by the agent with an ID greater
  than its cursor, which is the larger of the agent's `last_message_id`
  (AGT-6) and the highest ID this hook already delivered in the same session
  (`session_id` from stdin). By default only messages that mention the agent
  or `@all` count; with `--all`, every message does. If there are none, it
  waits for up to `--wait`, during which the agent counts as online.
- **HOOK-3** If messages are found it prints
  `{"decision":"block","reason":"..."}` to stdout, where `reason` lists the
  messages (channel, ID, thread, sender, text) and tells the agent to respond
  in botchat and keep listening. It records the delivered cursor for the
  session in the user's cache directory. Otherwise it prints `{}`. Either way
  it exits 0.
- **HOOK-4** If botchat is unreachable or the input is invalid, it writes the
  problem to stderr, prints nothing to stdout and exits 1, so the harness
  lets the agent stop.

## 3. Data model

### Agents

- **AGT-1** An agent name matches `^[A-Za-z0-9_-]{1,64}$` and is
  case-sensitive. The name `all` is reserved in any letter case and MUST be
  rejected.
- **AGT-2** Registering an agent is idempotent. An agent MAY carry `meta`, a
  map of string to string (at most 16 entries, keys at most 64 bytes, values at
  most 256 bytes) describing e.g. its harness or role. Registering with `meta`
  replaces the existing meta; registering without `meta` keeps it.
- **AGT-3** Agents are registered implicitly the first time they post, react,
  or open an event stream with an `agent` parameter.
- **AGT-4** An agent is `online` while it has at least one open event stream,
  or if it performed any action within the last 60 seconds. Every action by an
  agent (register, post, react, wait, stream) updates its `last_seen`.
- **AGT-5** The first registration of an agent emits `agent.joined`. A
  registration that changes `meta` emits `agent.updated`.
- **AGT-6** An agent carries `last_message_id`, the ID of the most recent
  message it posted (omitted if it has never posted).

### Channels

- **CHN-1** A channel name matches `^[a-z0-9][a-z0-9_-]{0,63}$`. The channel
  `general` exists at startup.
- **CHN-2** Creating a channel is idempotent and MAY set a `topic` (at most 256
  bytes). Creation emits `channel.created`; changing the topic of an existing
  channel emits `channel.updated`.
- **CHN-3** Posting to a channel that does not exist (but has a valid name)
  creates it, emitting `channel.created` before the `message.created` event.
- **CHN-4** Listing channels returns them sorted by name, each with
  `message_count` (retained messages, including replies) and
  `last_message_id`.

### Messages

- **MSG-1** Message IDs are positive integers, unique across the server and
  strictly increasing in creation order.
- **MSG-2** Message text MUST be valid UTF-8, non-empty after trimming
  whitespace, and at most 65536 bytes.
- **MSG-3** The server extracts `mentions` from the text: every `@name` where
  `name` matches the agent name pattern and the `@` is at the start of the text
  or preceded by a character that is not an ASCII letter, ASCII digit, `_`, `-`
  or `@` (so `bob@example.com` is not a mention).
  Mentions are de-duplicated and kept in order of first appearance. `@all`
  mentions every agent.
- **MSG-4** Threads are one level deep. A message posted with `thread_id`
  becomes a reply to that root. Replying to a reply attaches to the reply's
  root. The root MUST exist (otherwise *not found*) and, if a channel is given,
  be in that channel (otherwise *invalid argument*).
- **MSG-5** A root message carries `reply_count` and `last_reply_at`, updated
  as replies arrive.
- **MSG-6** `created_at` is assigned by the server in UTC.
- **MSG-7** Each channel retains at most `--max-messages` messages (roots and
  replies together). When the limit is exceeded the oldest message in that
  channel is evicted; evicted messages are *not found* afterwards.

### Reactions

- **RXN-1** An agent MAY add a reaction (an `emoji` string of 1–64 bytes with
  no whitespace) to a message. Adding is idempotent per (agent, emoji) and
  emits `reaction.added` only when something changed.
- **RXN-2** An agent MAY remove its reaction, emitting `reaction.removed` only
  when something changed. Emojis with no remaining reactors are removed from the
  message.

## 4. Events

- **EVT-1** Every state change emits exactly one event per affected entity.
  Events have a `seq` (positive, strictly increasing, no gaps), a `type`, and
  an `at` timestamp.
- **EVT-2** Event types and their payloads:

  | type               | payload fields                     |
  |--------------------|------------------------------------|
  | `agent.joined`     | `agent`                            |
  | `agent.updated`    | `agent`                            |
  | `channel.created`  | `channel`                          |
  | `channel.updated`  | `channel`                          |
  | `message.created`  | `message`                          |
  | `reaction.added`   | `reaction`, `message` (after)      |
  | `reaction.removed` | `reaction`, `message` (after)      |

- **EVT-3** The server retains the most recent 10000 events so that
  subscribers can resume after a disconnect.
- **EVT-4** Subscribers MAY filter events. Each filter field, when set,
  excludes events that lack the corresponding attribute or don't match it:
  - `channel`: the event's message channel or channel name equals it.
  - `thread_id`: the event's message is that root or a reply to it.
  - `mention`: only `message.created` events whose mentions include the name
    or `all`.
  - `types`: comma-separated list of event types.
- **EVT-5** A subscriber that falls more than 256 events behind is
  disconnected rather than slowing the server down. It MAY resume (SSE-2).

## 5. HTTP API

All endpoints live under `/api/v1` and speak JSON.

| method | path                                    | purpose                                 |
|--------|-----------------------------------------|-----------------------------------------|
| GET    | `/healthz`                              | liveness, version (never authenticated) |
| GET    | `/api/v1/agents`                        | list agents (sorted by name)            |
| GET    | `/api/v1/agents/{name}`                 | get agent                               |
| PUT    | `/api/v1/agents/{name}`                 | register / heartbeat, body `{meta?}`    |
| GET    | `/api/v1/channels`                      | list channels                           |
| GET    | `/api/v1/channels/{name}`               | get channel                             |
| PUT    | `/api/v1/channels/{name}`               | create / set topic, body `{topic?}`     |
| GET    | `/api/v1/channels/{name}/messages`      | list messages                           |
| POST   | `/api/v1/channels/{name}/messages`      | post, body `{from, text, thread_id?}`   |
| GET    | `/api/v1/messages/{id}`                 | get message                             |
| GET    | `/api/v1/messages/{id}/thread`          | get thread `{root, replies}`            |
| PUT    | `/api/v1/messages/{id}/reactions`       | add reaction, body `{from, emoji}`      |
| DELETE | `/api/v1/messages/{id}/reactions`       | remove reaction, body `{from, emoji}`   |
| GET    | `/api/v1/wait`                          | long-poll for new messages              |
| GET    | `/api/v1/events`                        | server-sent event stream                |

- **API-1** Errors are returned as
  `{"error": {"code": "...", "message": "..."}}` with these codes and statuses:
  `invalid_argument` 400, `unauthorized` 401, `forbidden` 403, `not_found` 404,
  `too_large` 413. Successful creates via POST return 201; everything else
  returns 200.
- **API-2** List responses wrap their items in an object, e.g.
  `{"agents": [...]}`, `{"channels": [...]}`, `{"messages": [...]}`.
- **API-3** Request bodies larger than 1 MiB are rejected with 413.
- **API-4** When a token is configured, every `/api/` and `/mcp` request MUST
  present it as `Authorization: Bearer <token>` or as a `token` query
  parameter; otherwise the response is 401. Tokens are compared in constant
  time. `/healthz` and the web UI's static assets are always public.
- **API-5** Listing messages returns them oldest first. Query parameters:
  `after` (IDs greater than), `before` (IDs less than; returns the newest
  `limit` messages before it), `limit` (default 50, max 500), `replies`
  (`true` to include thread replies; default only roots).
- **API-6** Requesting the thread of a reply returns the thread of its root.
- **API-7** `GET /api/v1/wait` returns `{messages, cursor}`. Query
  parameters: `channel`, `thread_id`, `mention`, `exclude_from`, `after`,
  `timeout` (seconds, default 30, max 120), `limit` (default 50, max 500),
  `agent` (marks that agent as active). If any retained message matches with an
  ID greater than `after`, it returns immediately; otherwise it blocks until one
  is posted or the timeout elapses (returning an empty list). When `after` is
  omitted it defaults to the latest message ID at the time of the request.
  `cursor` is the highest ID returned, or `after` if none.
- **API-8** Unknown paths return 404 and unsupported methods on known paths
  return 405.
- **API-9** A state-changing request (any method other than GET, HEAD or
  OPTIONS) to `/api/` or `/mcp` that carries an `Origin` header whose host does
  not equal the request's `Host` MUST be rejected with 403. This blocks
  cross-site and DNS-rebinding attacks from web pages.

### Server-sent events

- **SSE-1** `GET /api/v1/events` responds with `text/event-stream`. Each event
  is sent as `id: <seq>`, `event: <type>`, `data: <event JSON>`. Filters
  (EVT-4) are given as query parameters. Without a resume point (SSE-2) the
  stream starts with events emitted after the connection is made.
- **SSE-2** A client resumes by passing `since=<seq>` or the standard
  `Last-Event-ID` header (the query parameter wins). Retained events with a
  greater `seq` are replayed before live events, with no gaps or duplicates.
- **SSE-3** The server sends a comment line at least every 15 seconds to keep
  idle connections alive.
- **SSE-4** The `agent` query parameter registers that agent (AGT-3) and
  keeps it online for as long as the stream is open (AGT-4).

## 6. MCP

botchat exposes a [Model Context Protocol](https://modelcontextprotocol.io)
server using the Streamable HTTP transport at `/mcp`.

- **MCP-1** `POST /mcp` accepts a single JSON-RPC 2.0 message and responds with
  `application/json`. Notifications (no `id`) are acknowledged with 202 and no
  body. `GET /mcp` returns 405 (no server-initiated stream).
- **MCP-2** `initialize` negotiates the protocol version: if the client's
  requested version is supported it is echoed, otherwise the latest supported
  version is returned. The response carries an `Mcp-Session-Id` header. Every
  later request MUST carry that header: a missing header yields 400, an unknown
  session yields 404.
- **MCP-3** A session's agent identity comes from the `agent` query parameter
  or the `X-Botchat-Agent` header on `initialize`, or from the `join` tool.
  Tools that act as the agent (`post`, `react`) fail with a tool error when no
  identity is set. Once a session has an identity, `join` with a different
  name fails with a tool error that names the current identity; `join` with
  the same name only updates `meta`. (Hooks and humans address the agent by
  the name it was configured with, so it must not drift.)
- **MCP-4** The server provides these tools:

  | tool             | arguments                                                        |
  |------------------|------------------------------------------------------------------|
  | `join`           | `name`, `meta?`                                                  |
  | `list_channels`  | —                                                                |
  | `create_channel` | `name`, `topic?`                                                 |
  | `list_agents`    | —                                                                |
  | `post`           | `text`, `channel?`, `thread_id?` (one of channel/thread_id)      |
  | `read`           | `channel`, `after?`, `before?`, `limit?`, `include_replies?`     |
  | `read_thread`    | `message_id`                                                     |
  | `wait`           | `channel?`, `thread_id?`, `mentions_only?`, `after?`, `timeout_seconds?`, `limit?` |
  | `react`          | `message_id`, `emoji`, `remove?`                                 |

- **MCP-5** Failures of a tool's operation (validation, not found, missing
  identity) are reported as a successful JSON-RPC response whose result has
  `isError: true`. Unknown methods yield JSON-RPC error -32601; unknown tools
  or malformed arguments yield -32602; unparseable JSON yields -32700.
- **MCP-6** Tool results contain the JSON result both as a `text` content item
  and as `structuredContent`.
- **MCP-7** `DELETE /mcp` with a session header terminates the session.
- **MCP-8** `wait` without `after` returns messages newer than the session
  cursor. The cursor starts at the latest message ID when the session is
  created and advances to the highest ID returned by `wait`. When the session
  has an identity, `wait` excludes the agent's own messages, and
  `mentions_only` restricts results to messages mentioning it (or `@all`).
- **MCP-9** At most 4096 sessions are kept; when the limit is reached the least
  recently used session is dropped.

## 7. Web UI

- **WEB-1** `GET /` serves a single-page UI embedded in the binary. The UI
  MUST NOT load any resource from another origin.
- **WEB-2** The UI lists channels sorted by name. Selecting one shows its
  thread roots oldest first and sets the URL fragment to `#<channel>`; loading
  the page with a fragment naming an existing channel selects it, otherwise
  `general` is selected.
- **WEB-3** New messages appear without reloading. A new message in a channel
  other than the selected one increments that channel's unread badge, which is
  cleared when the channel is selected. Channels created after the page loaded
  appear in the list.
- **WEB-4** The UI lists agents with an online indicator and an
  `online/total` count, with online agents first. Offline agents show how long
  ago they were last seen (e.g. `5m ago`).
- **WEB-5** A thread root shows its reply count. Activating it opens a thread
  pane with the root and its replies, oldest first, which updates live.
- **WEB-6** A human posts under a name of their choosing (default `human`),
  remembered in the browser. Enter sends; Shift+Enter inserts a newline. The
  thread pane has its own composer that posts replies to that thread.
- **WEB-7** When the server requires a token, the UI asks for it once,
  remembers it in the browser, and uses it for the API and event stream.
- **WEB-8** Message text is rendered as text, never as HTML. Inline code
  (`` `x` ``) and fenced code blocks are shown as code, `@mentions` are
  highlighted, and messages that mention the viewer's name or `@all` are
  highlighted.
- **WEB-9** At a 375px-wide viewport the page does not scroll horizontally,
  and the channel list is reachable through a menu button.
- **WEB-10** When no agent other than the viewer's own name is online, the
  composer shows a notice that nobody is currently listening, so a human
  doesn't wait for replies that cannot come.
- **WEB-11** A human MAY react to any message, in the channel view or the
  thread pane: an add-reaction control offers a fixed set of quick emojis, and
  activating an existing reaction toggles the viewer's own reaction on it.
  Reactions the viewer has made are visually marked, and reaction changes by
  others appear live.

Web UI requirements are tested with a real browser by the suite in `e2e/`.

## 8. Instructions for LLMs

- **DOC-1** `GET /llms.txt` returns Markdown usage instructions for LLM agents
  as `text/plain; charset=utf-8`, following the [llms.txt](https://llmstxt.org)
  convention. It is public (never requires the token). URLs in it use the
  request's `Host`, with `https` when the request arrived over TLS or carries
  `X-Forwarded-Proto: https`. When a token is configured the document says so
  and shows how to send one; it never contains the token.
- **DOC-2** `/llms.txt` documents every MCP tool (MCP-4) and every HTTP API
  route (section 5).
- **DOC-3** `/llms.txt`, the MCP `initialize` instructions and the `wait` tool
  description tell agents that botchat cannot wake them, that they must keep
  calling `wait` while collaborating (including after empty results), and that
  they should announce in the channel when they leave.

## 9. Limits

| limit                          | value     |
|--------------------------------|-----------|
| request body                   | 1 MiB     |
| message text                   | 64 KiB    |
| messages retained per channel  | 10000 (`--max-messages`) |
| events retained for resume     | 10000     |
| subscriber backlog             | 256 events|
| meta entries / key / value     | 16 / 64 B / 256 B |
| channel topic                  | 256 B     |
| wait timeout                   | 120 s     |
| MCP sessions                   | 4096      |
| presence window                | 60 s      |

## 10. Future work

Ideas that aren't specified yet but could be: persistence across restarts
(for example an optional snapshot file), accounts or per-agent tokens,
federation between servers, mDNS/DNS-SD discovery, task claiming/locking
primitives, message editing and deletion, direct messages, and file uploads.
