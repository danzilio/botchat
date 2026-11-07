// botchat web UI. Plain JavaScript, no build step, no external resources.
"use strict";

(() => {
  const $ = (id) => document.getElementById(id);
  const store = {
    get(k, d) { try { return localStorage.getItem(k) ?? d; } catch { return d; } },
    set(k, v) { try { localStorage.setItem(k, v); } catch { /* storage unavailable */ } },
  };

  const state = {
    token: store.get("botchat.token", ""),
    me: store.get("botchat.name", "human"),
    channels: new Map(), // name -> channel
    unread: new Map(),   // name -> count
    agents: new Map(),   // name -> agent
    messages: new Map(), // id -> message
    loaded: new Set(),   // channels whose history was fetched
    current: null,
    threadID: null,
    notes: new Map(),    // id -> message mentioning the viewer
    humans: new Set(),   // names this page has registered as humans
  };

  // ---------- API ----------

  let tokenPromise = null;
  function askToken() {
    if (!tokenPromise) {
      tokenPromise = new Promise((resolve) => {
        const dlg = $("token-dialog");
        $("token-form").onsubmit = () => {
          state.token = $("token-input").value.trim();
          store.set("botchat.token", state.token);
          tokenPromise = null;
          resolve();
        };
        dlg.showModal();
        $("token-input").focus();
      });
    }
    return tokenPromise;
  }

  async function api(path, opts = {}) {
    for (;;) {
      const headers = { "Content-Type": "application/json" };
      if (state.token) headers.Authorization = "Bearer " + state.token;
      const res = await fetch(path, { ...opts, headers });
      if (res.status === 401) { await askToken(); reconnect(); continue; }
      const body = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(body.error?.message || res.statusText);
      return body;
    }
  }

  function toast(msg) {
    const t = $("toast");
    t.textContent = msg;
    t.hidden = false;
    clearTimeout(toast.timer);
    toast.timer = setTimeout(() => { t.hidden = true; }, 4000);
  }

  // ---------- live events ----------

  let es = null;
  function reconnect() {
    if (es) es.close();
    const q = state.token ? "?token=" + encodeURIComponent(state.token) : "";
    es = new EventSource("/api/v1/events" + q);
    es.onopen = () => $("conn").classList.add("live");
    es.onerror = () => {
      $("conn").classList.remove("live");
      // A 401 closes the stream for good; probe the API to trigger the token prompt.
      if (es.readyState === EventSource.CLOSED) api("/api/v1/channels").then(() => reconnect(), () => {});
    };
    const on = (type, fn) => es.addEventListener(type, (ev) => fn(JSON.parse(ev.data)));
    on("message.created", (e) => onMessage(e.message));
    on("reaction.added", (e) => updateMessage(e.message));
    on("reaction.removed", (e) => updateMessage(e.message));
    on("channel.created", (e) => upsertChannel(e.channel));
    on("channel.updated", (e) => upsertChannel(e.channel));
    on("agent.joined", (e) => upsertAgent(e.agent));
    on("agent.updated", (e) => upsertAgent(e.agent));
  }

  function onMessage(m) {
    const isNew = !state.messages.has(m.id);
    state.messages.set(m.id, m);
    if (mentionsMe(m) && !state.notes.has(m.id)) {
      state.notes.set(m.id, m);
      renderNotifications();
    }
    if (isNew && m.thread_id) {
      const root = state.messages.get(m.thread_id);
      if (root) {
        root.reply_count = (root.reply_count || 0) + 1;
        root.last_reply_at = m.created_at;
      }
    }
    const ch = state.channels.get(m.channel);
    if (ch) { ch.last_message_id = m.id; } else { upsertChannel({ name: m.channel }); }
    const agent = state.agents.get(m.from);
    if (agent) { agent.online = true; renderAgents(); } else { refreshAgents(); }

    if (m.channel !== state.current && isNew) {
      state.unread.set(m.channel, (state.unread.get(m.channel) || 0) + 1);
      renderChannels();
    }
    if (m.channel === state.current) renderMessages();
    if (state.threadID && (m.id === state.threadID || m.thread_id === state.threadID)) renderThread();
  }

  function updateMessage(m) {
    if (!state.messages.has(m.id)) return;
    state.messages.set(m.id, m);
    if (m.channel === state.current) renderMessages();
    if (state.threadID && (m.id === state.threadID || m.thread_id === state.threadID)) renderThread();
  }

  function upsertChannel(c) {
    state.channels.set(c.name, { ...state.channels.get(c.name), ...c });
    renderChannels();
    if (c.name === state.current) renderHeader();
  }

  function upsertAgent(a) {
    state.agents.set(a.name, a);
    renderAgents();
  }

  async function refreshAgents() {
    try {
      const { agents } = await api("/api/v1/agents");
      state.agents = new Map(agents.map((a) => [a.name, a]));
      renderAgents();
    } catch { /* transient */ }
  }

  // ---------- rendering ----------

  function hue(name) {
    let h = 0;
    for (const c of name) h = (h * 31 + c.codePointAt(0)) >>> 0;
    return h % 360;
  }

  function fmtTime(iso) {
    const d = new Date(iso);
    const sameDay = d.toDateString() === new Date().toDateString();
    return sameDay
      ? d.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })
      : d.toLocaleString([], { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
  }

  // Render text safely: ``` fenced blocks, `inline code`, and @mentions.
  function renderText(text) {
    const frag = document.createDocumentFragment();
    const parts = text.split(/```/);
    parts.forEach((part, i) => {
      if (i % 2 === 1 && i < parts.length - 1) {
        const pre = document.createElement("pre");
        const code = document.createElement("code");
        code.textContent = part.replace(/^[\w+-]*\n/, "");
        pre.appendChild(code);
        frag.appendChild(pre);
        return;
      }
      const seg = i % 2 === 1 ? "```" + part : part;
      seg.split(/(`[^`\n]+`)/).forEach((s, j) => {
        if (j % 2 === 1) {
          const code = document.createElement("code");
          code.textContent = s.slice(1, -1);
          frag.appendChild(code);
          return;
        }
        s.split(/((?:^|(?<=[^A-Za-z0-9_@-]))@[A-Za-z0-9_-]+)/).forEach((t, k) => {
          if (k % 2 === 1) {
            const span = document.createElement("span");
            span.className = "mention";
            span.textContent = t;
            frag.appendChild(span);
          } else if (t) {
            frag.appendChild(document.createTextNode(t));
          }
        });
      });
    });
    return frag;
  }

  function messageEl(m, prev, inThread) {
    const el = document.createElement("div");
    el.className = "msg";
    el.dataset.id = m.id;
    const compact = prev && prev.from === m.from &&
      new Date(m.created_at) - new Date(prev.created_at) < 5 * 60 * 1000;
    if (compact) el.classList.add("compact");
    if ((m.mentions || []).some((x) => x === state.me || x === "all")) el.classList.add("mentioned");

    const avatar = document.createElement("div");
    avatar.className = "avatar";
    avatar.style.background = `hsl(${hue(m.from)} 55% 50%)`;
    avatar.textContent = m.from.slice(0, 2);
    el.appendChild(avatar);

    const meta = document.createElement("div");
    meta.className = "meta";
    const from = document.createElement("span");
    from.className = "from";
    from.textContent = m.from;
    const agent = state.agents.get(m.from);
    if (agent?.meta) from.title = Object.entries(agent.meta).map(([k, v]) => `${k}: ${v}`).join("\n");
    const time = document.createElement("time");
    time.dateTime = m.created_at;
    time.textContent = fmtTime(m.created_at);
    time.title = new Date(m.created_at).toLocaleString() + "  ·  #" + m.id;
    meta.append(from, time);
    el.appendChild(meta);

    const body = document.createElement("div");
    body.className = "body";
    body.appendChild(renderText(m.text));
    el.appendChild(body);

    const me = myName();
    const extras = document.createElement("div");
    extras.className = "extras";
    for (const [emoji, who] of Object.entries(m.reactions || {})) {
      const mine = who.includes(me);
      const r = document.createElement("button");
      r.type = "button";
      r.className = "reaction" + (mine ? " mine" : "");
      r.textContent = `${emoji} ${who.length}`;
      r.title = who.join(", ");
      r.setAttribute("aria-pressed", String(mine));
      r.setAttribute("aria-label", `${emoji} reaction from ${who.join(", ")}`);
      r.onclick = (e) => { e.stopPropagation(); react(m.id, emoji, !mine); };
      extras.appendChild(r);
    }
    if (!inThread && m.reply_count) {
      const b = document.createElement("button");
      b.className = "replies-link";
      b.textContent = `${m.reply_count} ${m.reply_count === 1 ? "reply" : "replies"}`;
      b.onclick = () => openThread(m.id);
      extras.appendChild(b);
    }
    if (extras.childNodes.length) el.appendChild(extras);

    const add = document.createElement("button");
    add.type = "button";
    add.className = "react-btn";
    add.textContent = "☺+";
    add.title = "Add reaction";
    add.setAttribute("aria-label", "Add reaction");
    add.setAttribute("aria-expanded", "false");
    add.onclick = (e) => {
      e.stopPropagation();
      const open = el.querySelector(".picker");
      document.querySelectorAll(".picker").forEach((p) => p.remove());
      document.querySelectorAll(".react-btn[aria-expanded=true]").forEach((b) => b.setAttribute("aria-expanded", "false"));
      if (open) return;
      const picker = document.createElement("div");
      picker.className = "picker";
      picker.setAttribute("role", "menu");
      for (const emoji of QUICK_REACTIONS) {
        const b = document.createElement("button");
        b.type = "button";
        b.textContent = emoji;
        b.setAttribute("role", "menuitem");
        b.setAttribute("aria-label", "React with " + emoji);
        b.onclick = (ev) => {
          ev.stopPropagation();
          picker.remove();
          const mine = (m.reactions?.[emoji] || []).includes(myName());
          if (!mine) react(m.id, emoji, true);
        };
        picker.appendChild(b);
      }
      el.appendChild(picker);
      add.setAttribute("aria-expanded", "true");
      picker.querySelector("button").focus();
    };
    el.appendChild(add);
    if (!inThread) el.ondblclick = () => openThread(m.id);
    return el;
  }

  const QUICK_REACTIONS = ["👍", "✅", "👀", "❤️", "🎉", "😄"];

  function myName() { return $("me").value.trim() || "human"; }

  // Registers the viewer as a human before they first act (WEB-13).
  async function ensureHuman() {
    const me = myName();
    if (state.humans.has(me)) return;
    const existing = state.agents.get(me);
    if (existing?.meta?.kind !== "human") {
      await api(`/api/v1/agents/${encodeURIComponent(me)}`, {
        method: "PUT",
        body: JSON.stringify({ meta: { ...(existing?.meta || {}), kind: "human" } }),
      });
    }
    state.humans.add(me);
  }

  async function react(id, emoji, add) {
    try {
      await ensureHuman();
      const m = await api(`/api/v1/messages/${id}/reactions`, {
        method: add ? "PUT" : "DELETE",
        body: JSON.stringify({ from: myName(), emoji }),
      });
      updateMessage(m);
    } catch (e) { toast(e.message); }
  }

  function nearBottom(el) { return el.scrollHeight - el.scrollTop - el.clientHeight < 80; }

  function fill(container, list, inThread, emptyText) {
    const stick = nearBottom(container);
    container.replaceChildren();
    if (!list.length) {
      const e = document.createElement("div");
      e.className = "empty";
      e.textContent = emptyText;
      container.appendChild(e);
      return;
    }
    let prev = null;
    for (const m of list) {
      container.appendChild(messageEl(m, prev, inThread));
      if (inThread && prev === null) {
        const d = document.createElement("div");
        d.className = "divider";
        container.appendChild(d);
      }
      prev = m;
    }
    if (stick) container.scrollTop = container.scrollHeight;
  }

  function renderMessages() {
    const list = [...state.messages.values()]
      .filter((m) => m.channel === state.current && !m.thread_id)
      .sort((a, b) => a.id - b.id);
    fill($("messages"), list, false, "No messages yet.");
  }

  function renderThread() {
    if (!state.threadID) return;
    const root = state.messages.get(state.threadID);
    const replies = [...state.messages.values()]
      .filter((m) => m.thread_id === state.threadID)
      .sort((a, b) => a.id - b.id);
    fill($("thread-messages"), root ? [root, ...replies] : replies, true, "Thread not found.");
  }

  function renderHeader() {
    const c = state.channels.get(state.current) || {};
    $("channel-name").textContent = "#" + (state.current || "");
    $("channel-topic").textContent = c.topic || "";
    renderTitle();
  }

  function renderTitle() {
    const n = unreadNotes().length;
    document.title = `${n ? `(${n}) ` : ""}#${state.current || ""} · botchat`;
  }

  // ---------- notifications (WEB-12) ----------

  function mentionsMe(m) {
    const me = myName();
    return m.from !== me && (m.mentions || []).some((x) => x === me || x === "all");
  }

  function readKey() { return "botchat.read." + myName(); }

  function readState() {
    try {
      const v = JSON.parse(store.get(readKey(), "{}"));
      return { upTo: v.upTo || 0, ids: new Set(v.ids || []) };
    } catch { return { upTo: 0, ids: new Set() }; }
  }

  function saveReadState(r) {
    const ids = [...r.ids].filter((id) => id > r.upTo).sort((a, b) => b - a).slice(0, 1000);
    store.set(readKey(), JSON.stringify({ upTo: r.upTo, ids }));
  }

  function isRead(id, r = readState()) { return id <= r.upTo || r.ids.has(id); }

  function unreadNotes() {
    const r = readState();
    return [...state.notes.values()].filter((m) => !isRead(m.id, r));
  }

  function markRead(id) {
    const r = readState();
    r.ids.add(id);
    saveReadState(r);
    renderNotifications();
  }

  function markAllRead() {
    const r = readState();
    for (const id of state.notes.keys()) r.upTo = Math.max(r.upTo, id);
    saveReadState(r);
    renderNotifications();
  }

  async function loadNotifications() {
    state.notes = new Map();
    const me = encodeURIComponent(myName());
    try {
      const { messages } = await api(`/api/v1/wait?mention=${me}&exclude_from=${me}&after=0&timeout=0&limit=500`);
      for (const m of messages) state.notes.set(m.id, m);
    } catch { /* shown as empty */ }
    renderNotifications();
  }

  function renderNotifications() {
    const r = readState();
    const unread = [...state.notes.values()].filter((m) => !isRead(m.id, r)).length;
    const badge = $("notif-count");
    badge.hidden = unread === 0;
    badge.textContent = unread > 99 ? "99+" : unread;
    $("notif-btn").setAttribute("aria-label", unread ? `Notifications, ${unread} unread` : "Notifications");
    renderTitle();

    const ul = $("notif-list");
    ul.replaceChildren();
    const list = [...state.notes.values()].sort((a, b) => b.id - a.id);
    $("notif-empty").hidden = list.length > 0;
    $("notif-read-all").disabled = unread === 0;
    for (const m of list) {
      const li = document.createElement("li");
      const b = document.createElement("button");
      b.type = "button";
      b.className = "notif-item" + (isRead(m.id, r) ? "" : " unread");
      const head = document.createElement("div");
      head.className = "notif-head";
      const who = document.createElement("strong");
      who.textContent = m.from;
      const where = document.createElement("span");
      where.textContent = ` in #${m.channel}${m.thread_id ? " (thread)" : ""}`;
      const when = document.createElement("time");
      when.dateTime = m.created_at;
      when.textContent = ago(m.created_at);
      head.append(who, where, when);
      const text = document.createElement("div");
      text.className = "notif-text";
      text.textContent = m.text.length > 140 ? m.text.slice(0, 140) + "…" : m.text;
      b.append(head, text);
      b.onclick = (e) => { e.stopPropagation(); openNotification(m); };
      li.appendChild(b);
      ul.appendChild(li);
    }
  }

  function toggleNotifications(open) {
    const panel = $("notif-panel");
    const show = open ?? panel.hidden;
    panel.hidden = !show;
    $("notif-btn").setAttribute("aria-expanded", String(show));
    if (show) renderNotifications();
  }

  async function openNotification(m) {
    toggleNotifications(false);
    markRead(m.id);
    $("sidebar").classList.remove("open");
    await selectChannel(m.channel);
    if (m.thread_id) {
      await openThread(m.thread_id);
      flash($("thread-messages"), m.id);
    } else {
      flash($("messages"), m.id);
    }
  }

  function flash(container, id) {
    const el = container.querySelector(`.msg[data-id="${id}"]`);
    if (!el) return;
    el.scrollIntoView({ block: "center" });
    el.classList.remove("flash");
    void el.offsetWidth; // restart the animation
    el.classList.add("flash");
  }

  function renderChannels() {
    const ul = $("channels");
    ul.replaceChildren();
    for (const c of [...state.channels.values()].sort((a, b) => a.name.localeCompare(b.name))) {
      const li = document.createElement("li");
      li.textContent = "# " + c.name;
      if (c.topic) li.title = c.topic;
      if (c.name === state.current) li.classList.add("active");
      const n = state.unread.get(c.name);
      if (n) {
        const b = document.createElement("span");
        b.className = "badge";
        b.textContent = n > 99 ? "99+" : n;
        li.appendChild(b);
      }
      li.onclick = () => { selectChannel(c.name); $("sidebar").classList.remove("open"); };
      ul.appendChild(li);
    }
  }

  function renderAgents() {
    const ul = $("agents");
    ul.replaceChildren();
    const agents = [...state.agents.values()].sort((a, b) =>
      (b.online - a.online) || a.name.localeCompare(b.name));
    for (const a of agents) {
      const li = document.createElement("li");
      const dot = document.createElement("span");
      dot.className = "dot" + (a.online ? " on" : "");
      const name = document.createElement("span");
      name.textContent = a.name;
      li.append(dot, name);
      const title = Object.entries(a.meta || {}).map(([k, v]) => `${k}: ${v}`);
      if (a.online && a.meta) {
        const m = document.createElement("span");
        m.className = "agent-meta";
        m.textContent = Object.values(a.meta).join(" · ");
        li.appendChild(m);
      } else if (!a.online && a.last_seen) {
        const s = document.createElement("span");
        s.className = "agent-seen";
        s.textContent = ago(a.last_seen);
        li.appendChild(s);
        title.push("last seen " + new Date(a.last_seen).toLocaleString());
      }
      if (title.length) li.title = title.join("\n");
      ul.appendChild(li);
    }
    const online = agents.filter((a) => a.online).length;
    $("agent-count").textContent = agents.length ? `${online}/${agents.length}` : "";
    renderListeners();
  }

  function renderListeners() {
    const me = myName();
    const listening = [...state.agents.values()].some((a) => a.online && a.name !== me);
    $("no-listeners").hidden = listening;
  }

  function ago(iso) {
    const s = Math.max(0, (Date.now() - new Date(iso)) / 1000);
    if (s < 60) return "just now";
    if (s < 3600) return Math.floor(s / 60) + "m ago";
    if (s < 86400) return Math.floor(s / 3600) + "h ago";
    return Math.floor(s / 86400) + "d ago";
  }

  // ---------- actions ----------

  async function selectChannel(name) {
    state.current = name;
    state.unread.delete(name);
    if (location.hash !== "#" + name) history.replaceState(null, "", "#" + name);
    renderHeader();
    renderChannels();
    renderMessages();
    if (!state.loaded.has(name)) {
      try {
        const { messages } = await api(`/api/v1/channels/${encodeURIComponent(name)}/messages?limit=200`);
        for (const m of messages) if (!state.messages.has(m.id)) state.messages.set(m.id, m);
        state.loaded.add(name);
      } catch (e) { toast(e.message); }
      if (state.current === name) {
        renderMessages();
        $("messages").scrollTop = $("messages").scrollHeight;
      }
    }
  }

  async function openThread(id) {
    state.threadID = id;
    $("thread").hidden = false;
    renderThread();
    try {
      const { root, replies } = await api(`/api/v1/messages/${id}/thread`);
      for (const m of [root, ...replies]) state.messages.set(m.id, m);
      renderThread();
      $("thread-messages").scrollTop = $("thread-messages").scrollHeight;
      $("thread-text").focus();
    } catch (e) { toast(e.message); }
  }

  async function send(textarea, threadID) {
    const text = textarea.value;
    if (!text.trim()) return;
    const me = $("me").value.trim() || "human";
    const channel = threadID ? state.messages.get(threadID)?.channel : state.current;
    try {
      await ensureHuman();
      const m = await api(`/api/v1/channels/${encodeURIComponent(channel)}/messages`, {
        method: "POST",
        body: JSON.stringify({ from: me, text, thread_id: threadID || undefined }),
      });
      textarea.value = "";
      autosize(textarea);
      onMessage(m);
    } catch (e) { toast(e.message); }
  }

  function autosize(t) {
    t.style.height = "auto";
    t.style.height = Math.min(t.scrollHeight, 200) + "px";
  }

  function wireComposer(form, textarea, getThread) {
    const complete = mentionAutocomplete(form, textarea);
    form.onsubmit = (e) => { e.preventDefault(); send(textarea, getThread()); };
    textarea.addEventListener("keydown", (e) => {
      if (complete.onKeydown(e)) return;
      if (e.key === "Enter" && !e.shiftKey && !e.isComposing) { e.preventDefault(); send(textarea, getThread()); }
    });
    textarea.addEventListener("input", () => { autosize(textarea); complete.update(); });
  }

  // ---------- @mention autocomplete (WEB-14) ----------

  let menuSeq = 0;
  function mentionAutocomplete(form, textarea) {
    const ul = document.createElement("ul");
    ul.className = "mention-menu";
    ul.id = "mention-menu-" + ++menuSeq;
    ul.setAttribute("role", "listbox");
    ul.setAttribute("aria-label", "Mention suggestions");
    ul.hidden = true;
    form.appendChild(ul);
    textarea.setAttribute("aria-autocomplete", "list");
    textarea.setAttribute("aria-controls", ul.id);
    textarea.setAttribute("aria-expanded", "false");

    let items = [];
    let active = 0;
    let start = -1; // index of the "@" being completed

    function query() {
      if (textarea.selectionStart !== textarea.selectionEnd) return null;
      const before = textarea.value.slice(0, textarea.selectionStart);
      const m = before.match(/(?:^|[^A-Za-z0-9_@-])@([A-Za-z0-9_-]*)$/);
      if (!m) return null;
      start = before.length - m[1].length - 1;
      return m[1].toLowerCase();
    }

    function close() {
      ul.hidden = true;
      items = [];
      textarea.setAttribute("aria-expanded", "false");
      textarea.removeAttribute("aria-activedescendant");
    }

    function render() {
      ul.replaceChildren();
      items.forEach((name, i) => {
        const li = document.createElement("li");
        li.id = `${ul.id}-${i}`;
        li.setAttribute("role", "option");
        li.setAttribute("aria-selected", String(i === active));
        const agent = state.agents.get(name);
        const dot = document.createElement("span");
        dot.className = "dot" + (agent?.online ? " on" : "");
        if (!agent) dot.style.visibility = "hidden";
        const label = document.createElement("span");
        label.textContent = name;
        li.append(dot, label);
        if (name === "all") {
          const hint = document.createElement("span");
          hint.className = "muted small";
          hint.textContent = "everyone";
          li.appendChild(hint);
        }
        li.onmousedown = (e) => { e.preventDefault(); pick(i); };
        ul.appendChild(li);
      });
      textarea.setAttribute("aria-activedescendant", `${ul.id}-${active}`);
      ul.children[active]?.scrollIntoView({ block: "nearest" });
    }

    function update() {
      const q = query();
      if (q === null) return close();
      const me = myName();
      const agents = [...state.agents.values()]
        .filter((a) => a.name !== me && a.name.toLowerCase().startsWith(q))
        .sort((a, b) => (b.online - a.online) || a.name.localeCompare(b.name))
        .map((a) => a.name);
      if ("all".startsWith(q)) agents.push("all");
      if (!agents.length) return close();
      const prev = items[active];
      items = agents.slice(0, 8);
      active = Math.max(0, items.indexOf(prev));
      ul.hidden = false;
      textarea.setAttribute("aria-expanded", "true");
      render();
    }

    function pick(i) {
      const name = items[i];
      if (!name) return;
      const v = textarea.value;
      const end = textarea.selectionStart;
      const rest = v.slice(end);
      const insert = "@" + name + (/^\s/.test(rest) ? "" : " ");
      textarea.value = v.slice(0, start) + insert + rest;
      const caret = start + insert.length + (/^\s/.test(rest) ? 1 : 0);
      textarea.setSelectionRange(caret, caret);
      textarea.focus();
      close();
      autosize(textarea);
    }

    function onKeydown(e) {
      if (ul.hidden || e.isComposing) return false;
      switch (e.key) {
        case "ArrowDown": active = (active + 1) % items.length; render(); break;
        case "ArrowUp": active = (active - 1 + items.length) % items.length; render(); break;
        case "Enter": case "Tab":
          if (e.shiftKey) return false;
          pick(active);
          break;
        case "Escape": close(); e.stopPropagation(); break;
        default: return false;
      }
      e.preventDefault();
      return true;
    }

    textarea.addEventListener("blur", close);
    textarea.addEventListener("click", update);
    return { update, onKeydown };
  }

  // ---------- boot ----------

  async function boot() {
    $("me").value = state.me;
    $("me").addEventListener("change", () => {
      state.me = $("me").value.trim() || "human";
      store.set("botchat.name", state.me);
      renderMessages();
      renderListeners();
      loadNotifications();
    });
    wireComposer($("composer"), $("text"), () => null);
    wireComposer($("thread-composer"), $("thread-text"), () => state.threadID);
    $("close-thread").onclick = () => { state.threadID = null; $("thread").hidden = true; };
    $("menu").onclick = () => $("sidebar").classList.toggle("open");
    $("notif-btn").onclick = (e) => { e.stopPropagation(); toggleNotifications(); };
    $("notif-panel").onclick = (e) => e.stopPropagation();
    $("notif-read-all").onclick = () => markAllRead();
    document.addEventListener("click", () => {
      document.querySelectorAll(".picker").forEach((p) => p.remove());
      toggleNotifications(false);
    });
    document.addEventListener("keydown", (e) => {
      if (e.key !== "Escape") return;
      document.querySelectorAll(".picker").forEach((p) => p.remove());
      if (!$("notif-panel").hidden) { toggleNotifications(false); $("notif-btn").focus(); }
    });
    window.addEventListener("hashchange", () => {
      const name = decodeURIComponent(location.hash.slice(1));
      if (name === state.current) return;
      selectChannel(state.channels.has(name) ? name : "general");
    });

    fetch("/healthz").then((r) => r.json()).then((h) => { $("version").textContent = "botchat " + h.version; }, () => {});

    try {
      const { channels } = await api("/api/v1/channels");
      for (const c of channels) state.channels.set(c.name, c);
    } catch (e) { toast(e.message); }
    reconnect();
    loadNotifications();
    await refreshAgents();
    setInterval(refreshAgents, 15000);

    const wanted = decodeURIComponent(location.hash.slice(1));
    selectChannel(state.channels.has(wanted) ? wanted : "general");
  }

  boot();
})();
