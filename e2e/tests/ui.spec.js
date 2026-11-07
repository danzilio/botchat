// Browser tests for the web UI (SPEC.md section 7).
import { test, expect } from "../fixtures.js";

const channels = (page) => page.locator("#channels li");
const channelItem = (page, name) => channels(page).filter({ hasText: new RegExp(`^# ${name}(\\d+|99\\+)?$`) });
const messages = (page) => page.locator("#messages .msg");
const threadMessages = (page) => page.locator("#thread-messages .msg");

// spec: WEB-1
test("loads nothing from other origins", async ({ page, botchat }) => {
  const foreign = [];
  page.on("request", (req) => {
    const u = req.url();
    if (!u.startsWith(botchat.url) && !u.startsWith("data:")) foreign.push(u);
  });
  const cspErrors = [];
  page.on("console", (msg) => {
    if (msg.type() === "error") cspErrors.push(msg.text());
  });
  await botchat.post("general", "alice", "hello");
  await page.goto(botchat.url);
  await expect(messages(page)).toHaveCount(1);
  expect(foreign).toEqual([]);
  expect(cspErrors).toEqual([]);
});

// spec: WEB-2
test("lists channels and selects them via click and URL fragment", async ({ page, botchat }) => {
  await botchat.api("PUT", "/channels/zeta", { topic: "last" });
  await botchat.api("PUT", "/channels/alpha", {});
  const first = await botchat.post("general", "alice", "first");
  await botchat.post("general", "bob", "second");
  await botchat.post("general", "carol", "a reply", first.id);
  await botchat.post("alpha", "alice", "in alpha");

  await page.goto(botchat.url);
  await expect(channels(page)).toHaveText(["# alpha", "# general", "# zeta"]);
  await expect(page).toHaveURL(/#general$/);
  await expect(page.locator("#channel-name")).toHaveText("#general");
  // Thread roots only, oldest first.
  await expect(messages(page).locator(".body")).toHaveText(["first", "second"]);

  await channelItem(page, "alpha").click();
  await expect(page).toHaveURL(/#alpha$/);
  await expect(channelItem(page, "alpha")).toHaveClass(/active/);
  await expect(messages(page).locator(".body")).toHaveText(["in alpha"]);

  await page.goto(botchat.url + "/#zeta");
  await expect(page.locator("#channel-name")).toHaveText("#zeta");
  await expect(page.locator("#channel-topic")).toHaveText("last");

  await page.goto(botchat.url + "/#does-not-exist");
  await expect(page.locator("#channel-name")).toHaveText("#general");
});

// spec: WEB-3
test("shows new messages live and tracks unread counts", async ({ page, botchat }) => {
  await botchat.api("PUT", "/channels/ops", {});
  await page.goto(botchat.url);
  await expect(page.locator("#conn")).toHaveClass(/live/);

  await botchat.post("general", "alice", "live one");
  await expect(messages(page).locator(".body")).toHaveText(["live one"]);

  const ops = channelItem(page, "ops");
  await expect(ops.locator(".badge")).toHaveCount(0);
  await botchat.post("ops", "bob", "ops 1");
  await expect(ops.locator(".badge")).toHaveText("1");
  await botchat.post("ops", "bob", "ops 2");
  await expect(ops.locator(".badge")).toHaveText("2");
  // Messages in the selected channel never count as unread.
  await expect(channelItem(page, "general").locator(".badge")).toHaveCount(0);

  await ops.click();
  await expect(ops.locator(".badge")).toHaveCount(0);
  await expect(messages(page).locator(".body")).toHaveText(["ops 1", "ops 2"]);

  await botchat.api("PUT", "/channels/brand-new", {});
  await expect(channelItem(page, "brand-new")).toBeVisible();
  await botchat.post("auto-created", "carol", "hi");
  await expect(channelItem(page, "auto-created")).toBeVisible();
});

// spec: WEB-4
test("lists agents with presence, online first", async ({ page, botchat }) => {
  // Presence depends on a 60s window, so serve a fixed agent list instead of
  // waiting for an agent to go offline.
  const now = new Date().toISOString();
  const fiveMinutesAgo = new Date(Date.now() - 5 * 60 * 1000 - 5000).toISOString();
  await page.route("**/api/v1/agents", (route) =>
    route.fulfill({
      json: {
        agents: [
          { name: "aaa-offline", joined_at: now, last_seen: fiveMinutesAgo, online: false, meta: { harness: "claude-code" } },
          { name: "zzz-online", joined_at: now, last_seen: now, online: true, meta: { harness: "codex" } },
          { name: "mmm-online", joined_at: now, last_seen: now, online: true },
        ],
      },
    }),
  );
  await page.goto(botchat.url);
  const agents = page.locator("#agents li");
  await expect(agents).toHaveCount(3);
  await expect(agents.locator("span:nth-child(2)")).toHaveText(["mmm-online", "zzz-online", "aaa-offline"]);
  await expect(agents.nth(0).locator(".dot")).toHaveClass(/\bon\b/);
  await expect(agents.nth(2).locator(".dot")).not.toHaveClass(/\bon\b/);
  await expect(agents.nth(1)).toContainText("codex");
  await expect(agents.nth(2)).toContainText("5m ago");
  await expect(agents.nth(2)).not.toContainText("claude-code"); // meta moves to the tooltip
  await expect(agents.nth(2)).toHaveAttribute("title", /harness: claude-code[\s\S]*last seen/);
  await expect(page.locator("#agent-count")).toHaveText("2/3");
});

// spec: WEB-4
test("shows real agents joining live", async ({ page, botchat }) => {
  await botchat.api("PUT", "/agents/early", {});
  await page.goto(botchat.url);
  await expect(page.locator("#agents li")).toHaveText(["early"]);
  await botchat.post("general", "late-joiner", "hi");
  await expect(page.locator("#agents li")).toHaveText(["early", "late-joiner"]);
  await expect(page.locator("#agent-count")).toHaveText("2/2");
});

// spec: WEB-10
test("warns when nobody is listening", async ({ page, botchat }) => {
  const notice = page.locator("#no-listeners");
  await page.goto(botchat.url);
  await expect(notice).toBeVisible();

  // The viewer's own presence doesn't count as someone listening.
  await page.locator("#text").fill("anyone there?");
  await page.locator("#text").press("Enter");
  await expect(messages(page)).toHaveCount(1);
  await expect(notice).toBeVisible();

  await botchat.post("general", "claude-worker", "I'm here");
  await expect(notice).toBeHidden();
});

// spec: WEB-10
test("hides the warning when another agent is online at load", async ({ page, botchat }) => {
  await botchat.api("PUT", "/agents/listener", {});
  await page.goto(botchat.url);
  await expect(page.locator("#agents li")).toHaveCount(1);
  await expect(page.locator("#no-listeners")).toBeHidden();
  // Taking the listener's name makes it "me" again.
  await page.locator("#me").fill("listener");
  await page.locator("#me").blur();
  await expect(page.locator("#no-listeners")).toBeVisible();
});

// spec: WEB-11
test("reacts to messages and toggles reactions", async ({ page, botchat }) => {
  await page.addInitScript(() => localStorage.setItem("botchat.name", "dana"));
  const root = await botchat.post("general", "alice", "ship it?");
  await botchat.post("general", "bob", "reply", root.id);
  await page.goto(botchat.url);
  const msg = messages(page).first();

  // Add a reaction from the picker.
  await msg.hover();
  await msg.locator(".react-btn").click();
  await expect(msg.locator(".picker button")).toHaveCount(6);
  await msg.getByRole("menuitem", { name: "React with 👍" }).click();
  await expect(msg.locator(".picker")).toHaveCount(0);
  const thumbs = msg.locator(".reaction", { hasText: "👍" });
  await expect(thumbs).toHaveText("👍 1");
  await expect(thumbs).toHaveClass(/mine/);
  await expect(thumbs).toHaveAttribute("aria-pressed", "true");
  let m = await botchat.api("GET", `/messages/${root.id}`);
  expect(m.reactions).toEqual({ "👍": ["dana"] });

  // Someone else's reaction appears live and isn't marked as mine.
  await botchat.api("PUT", `/messages/${root.id}/reactions`, { from: "bob", emoji: "🎉" });
  const party = msg.locator(".reaction", { hasText: "🎉" });
  await expect(party).toHaveText("🎉 1");
  await expect(party).not.toHaveClass(/mine/);

  // Clicking someone else's reaction adds mine; clicking again removes it.
  await party.click();
  await expect(party).toHaveText("🎉 2");
  await expect(party).toHaveClass(/mine/);
  await party.click();
  await expect(party).toHaveText("🎉 1");
  await thumbs.click();
  await expect(msg.locator(".reaction", { hasText: "👍" })).toHaveCount(0);
  m = await botchat.api("GET", `/messages/${root.id}`);
  expect(m.reactions).toEqual({ "🎉": ["bob"] });

  // Escape closes the picker.
  await msg.hover();
  await msg.locator(".react-btn").click();
  await expect(msg.locator(".picker")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(msg.locator(".picker")).toHaveCount(0);

  // Reacting works in the thread pane too.
  await msg.locator(".replies-link").click();
  const reply = threadMessages(page).nth(1);
  await reply.hover();
  await reply.locator(".react-btn").click();
  await reply.getByRole("menuitem", { name: "React with ✅" }).click();
  await expect(reply.locator(".reaction")).toHaveText("✅ 1");
  const thread = await botchat.api("GET", `/messages/${root.id}/thread`);
  expect(thread.replies[0].reactions).toEqual({ "✅": ["dana"] });
});

// spec: WEB-12
test("lists mentions as notifications and jumps to them", async ({ page, botchat }) => {
  await page.addInitScript(() => localStorage.setItem("botchat.name", "dana"));
  await botchat.api("PUT", "/channels/ops", {});
  const early = await botchat.post("ops", "claude", "@dana should we ship on Friday?");
  await botchat.post("general", "codex", "no mention here");
  await botchat.post("general", "dana", "@dana note to self"); // own messages don't count

  await page.goto(botchat.url);
  const btn = page.locator("#notif-btn");
  const count = page.locator("#notif-count");
  await expect(count).toHaveText("1"); // includes mentions from before the page loaded
  await expect(page).toHaveTitle(/^\(1\) #general/);

  const root = await botchat.post("general", "opencode", "tests are flaky");
  await botchat.post("general", "codex", "@all heads up: CI is red", root.id);
  await expect(count).toHaveText("2");

  await btn.click();
  const items = page.locator("#notif-list .notif-item");
  await expect(items).toHaveCount(2);
  await expect(items.nth(0)).toContainText("codex in #general (thread)");
  await expect(items.nth(0)).toContainText("@all heads up: CI is red");
  await expect(items.nth(1)).toContainText("claude in #ops");
  await expect(items.nth(0)).toHaveClass(/unread/);

  // Selecting a channel-level mention opens the channel and highlights it.
  await items.nth(1).click();
  await expect(page.locator("#notif-panel")).toBeHidden();
  await expect(page).toHaveURL(/#ops$/);
  await expect(page.locator(`#messages .msg[data-id="${early.id}"]`)).toHaveClass(/flash/);
  await expect(count).toHaveText("1");

  // A reply opens its thread.
  await btn.click();
  await items.nth(0).click();
  await expect(page).toHaveURL(/#general$/);
  await expect(page.locator("#thread")).toBeVisible();
  await expect(page.locator("#thread-messages .msg.flash")).toContainText("CI is red");
  await expect(count).toBeHidden();
  await expect(page).toHaveTitle(/^#general/);

  // Read state survives a reload; mark all read clears new ones.
  await page.reload();
  await expect(page.locator("#notif-count")).toBeHidden();
  await botchat.post("ops", "claude", "@dana one more thing");
  await botchat.post("ops", "claude", "@dana and another");
  await expect(page.locator("#notif-count")).toHaveText("2");
  await page.locator("#notif-btn").click();
  await page.locator("#notif-read-all").click();
  await expect(page.locator("#notif-count")).toBeHidden();
  await expect(page.locator("#notif-list .notif-item.unread")).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(page.locator("#notif-panel")).toBeHidden();
});

// spec: WEB-12
test("notifications follow the viewer's name", async ({ page, botchat }) => {
  await botchat.post("general", "claude", "@sam can you check this?");
  await page.goto(botchat.url);
  await expect(page.locator("#notif-count")).toBeHidden(); // default name is "human"
  await page.locator("#notif-btn").click();
  await expect(page.locator("#notif-empty")).toBeVisible();
  await page.keyboard.press("Escape");
  await page.locator("#me").fill("sam");
  await page.locator("#me").blur();
  await expect(page.locator("#notif-count")).toHaveText("1");
});

// spec: WEB-13
test("registers the viewer as a human before acting", async ({ page, botchat }) => {
  await page.addInitScript(() => localStorage.setItem("botchat.name", "dana"));
  const m = await botchat.post("general", "claude", "hello");
  await page.goto(botchat.url);
  await expect(page.locator("#messages .msg")).toHaveCount(1);
  await expect(botchat.api("GET", "/agents/dana")).rejects.toThrow(/404/); // not just for viewing

  await page.locator("#text").fill("hi!");
  await page.locator("#text").press("Enter");
  await expect(page.locator("#messages .msg")).toHaveCount(2);
  expect((await botchat.api("GET", "/agents/dana")).meta).toEqual({ kind: "human" });

  // Reacting under a new name registers that one too.
  await page.locator("#me").fill("lee");
  await page.locator("#me").blur();
  const first = page.locator("#messages .msg").first();
  await first.hover();
  await first.locator(".react-btn").click();
  await first.getByRole("menuitem", { name: "React with 👍" }).click();
  await expect(first.locator(".reaction")).toHaveText("👍 1");
  expect((await botchat.api("GET", "/agents/lee")).meta).toEqual({ kind: "human" });
  expect((await botchat.api("GET", `/messages/${m.id}`)).reactions).toEqual({ "👍": ["lee"] });
});

// spec: WEB-5
test("opens threads in a side pane that updates live", async ({ page, botchat }) => {
  const root = await botchat.post("general", "alice", "root message");
  await botchat.post("general", "bob", "reply one", root.id);
  await botchat.post("general", "carol", "reply two", root.id);
  await botchat.post("general", "dave", "lonely root");

  await page.goto(botchat.url);
  await expect(page.locator("#thread")).toBeHidden();
  const rootEl = messages(page).filter({ hasText: "root message" });
  await expect(rootEl.locator(".replies-link")).toHaveText("2 replies");
  await expect(messages(page).filter({ hasText: "lonely root" }).locator(".replies-link")).toHaveCount(0);

  await rootEl.locator(".replies-link").click();
  await expect(page.locator("#thread")).toBeVisible();
  await expect(threadMessages(page).locator(".body")).toHaveText(["root message", "reply one", "reply two"]);

  await botchat.post("general", "erin", "reply three", root.id);
  await expect(threadMessages(page).locator(".body")).toHaveText(["root message", "reply one", "reply two", "reply three"]);
  await expect(rootEl.locator(".replies-link")).toHaveText("3 replies");

  await page.locator("#close-thread").click();
  await expect(page.locator("#thread")).toBeHidden();
});

// spec: WEB-6
test("posts and replies as a named human", async ({ page, botchat }) => {
  await page.goto(botchat.url);
  await expect(page.locator("#me")).toHaveValue("human");
  await page.locator("#me").fill("dana");
  await page.locator("#me").blur();

  const text = page.locator("#text");
  await text.click();
  await text.pressSequentially("line one");
  await text.press("Shift+Enter");
  await text.pressSequentially("line two");
  await text.press("Enter");
  await expect(messages(page).locator(".body")).toHaveText(["line one\nline two"]);
  await expect(text).toHaveValue("");

  const { messages: sent } = await botchat.api("GET", "/channels/general/messages");
  expect(sent).toHaveLength(1);
  expect(sent[0]).toMatchObject({ from: "dana", text: "line one\nline two" });

  // The send button works too, and the thread composer replies in-thread.
  await text.fill("via button");
  await page.locator("#composer button").click();
  await expect(messages(page)).toHaveCount(2);

  await messages(page).first().dblclick();
  await expect(page.locator("#thread")).toBeVisible();
  await page.locator("#thread-text").fill("threaded reply");
  await page.locator("#thread-text").press("Enter");
  await expect(threadMessages(page).locator(".body")).toHaveText(["line one\nline two", "threaded reply"]);
  const thread = await botchat.api("GET", `/messages/${sent[0].id}/thread`);
  expect(thread.replies).toEqual([expect.objectContaining({ from: "dana", text: "threaded reply" })]);

  // The name is remembered across reloads.
  await page.reload();
  await expect(page.locator("#me")).toHaveValue("dana");
});

// spec: WEB-6
test("shows server errors instead of failing silently", async ({ page, botchat }) => {
  await page.goto(botchat.url);
  await page.locator("#me").fill("bad name!");
  await page.locator("#me").blur();
  await page.locator("#text").fill("hello");
  await page.locator("#text").press("Enter");
  await expect(page.locator("#toast")).toBeVisible();
  await expect(page.locator("#toast")).toContainText("invalid agent name");
  await expect(page.locator("#text")).toHaveValue("hello");
});

test.describe("with a token", () => {
  test.use({ token: "e2e-secret" });

  // spec: WEB-7
  test("asks for the token once and remembers it", async ({ page, botchat }) => {
    await botchat.post("general", "alice", "secret history");
    await page.goto(botchat.url);
    const dialog = page.locator("#token-dialog");
    await expect(dialog).toBeVisible();
    await page.locator("#token-input").fill("e2e-secret");
    await page.locator("#token-input").press("Enter");
    await expect(dialog).toBeHidden();
    await expect(messages(page).locator(".body")).toHaveText(["secret history"]);

    // The live stream is authenticated too.
    await expect(page.locator("#conn")).toHaveClass(/live/);
    await botchat.post("general", "bob", "live and authenticated");
    await expect(messages(page)).toHaveCount(2);

    await page.reload();
    await expect(messages(page)).toHaveCount(2);
    await expect(dialog).toBeHidden();
  });

  // spec: WEB-7
  test("asks again when the token is wrong", async ({ page, botchat }) => {
    await page.goto(botchat.url);
    await page.locator("#token-input").fill("wrong");
    await page.locator("#token-input").press("Enter");
    await expect(page.locator("#token-dialog")).toBeVisible();
    await page.locator("#token-input").fill("e2e-secret");
    await page.locator("#token-input").press("Enter");
    await expect(page.locator("#token-dialog")).toBeHidden();
    await expect(page.locator("#channel-name")).toHaveText("#general");
  });
});

// spec: WEB-8
test("renders text safely with code and mentions", async ({ page, botchat }) => {
  await page.addInitScript(() => localStorage.setItem("botchat.name", "dana"));
  const evil = '<img src=x onerror="window.__xss=1"><b>not bold</b> <script>window.__xss=2</script>';
  await botchat.post("general", "mallory", evil);
  await botchat.post("general", "alice", "run `make test` then ping @dana");
  await botchat.post("general", "alice", "```go\nfmt.Println(\"<hi>\")\n```");
  await botchat.post("general", "alice", "heads up @all");
  await botchat.post("general", "alice", "for @bob only, email bob@example.com");

  await page.goto(botchat.url);
  const bodies = messages(page).locator(".body");
  await expect(bodies).toHaveCount(5);

  await expect(bodies.nth(0)).toHaveText(evil);
  await expect(bodies.nth(0).locator("img, b, script")).toHaveCount(0);
  expect(await page.evaluate(() => window.__xss)).toBeUndefined();

  await expect(bodies.nth(1).locator("code")).toHaveText("make test");
  await expect(bodies.nth(1).locator(".mention")).toHaveText("@dana");
  await expect(bodies.nth(2).locator("pre code")).toHaveText('fmt.Println("<hi>")\n');
  await expect(bodies.nth(4).locator(".mention")).toHaveText(["@bob"]);

  const msgs = messages(page);
  await expect(msgs.nth(1)).toHaveClass(/mentioned/);
  await expect(msgs.nth(3)).toHaveClass(/mentioned/);
  await expect(msgs.nth(0)).not.toHaveClass(/mentioned/);
  await expect(msgs.nth(4)).not.toHaveClass(/mentioned/);
});

test.describe("on a phone", () => {
  test.use({ viewport: { width: 375, height: 740 } });

  // spec: WEB-9
  test("fits the screen and reaches channels through the menu", async ({ page, botchat }) => {
    await botchat.api("PUT", "/channels/ops", {});
    await botchat.post("general", "alice", "x".repeat(300) + " " + "a-very-long-unbroken-token-".repeat(20));
    await page.goto(botchat.url);
    await expect(messages(page)).toHaveCount(1);

    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth);
    expect(overflow).toBeLessThanOrEqual(0);

    const ops = channelItem(page, "ops");
    await expect(ops).not.toBeInViewport();
    await page.locator("#menu").click();
    await expect(ops).toBeInViewport();
    await ops.click();
    await expect(page).toHaveURL(/#ops$/);
    await expect(ops).not.toBeInViewport();
  });
});
