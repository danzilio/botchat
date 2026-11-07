// Test fixtures: every test gets its own fresh botchat process on a random
// port, so tests are isolated and can run in parallel.
import { test as base, expect } from "@playwright/test";
import { spawn } from "node:child_process";
import { fileURLToPath } from "node:url";
import path from "node:path";

const here = path.dirname(fileURLToPath(import.meta.url));
export const repoRoot = path.resolve(here, "..");
export const binaryPath = path.join(here, ".bin", process.platform === "win32" ? "botchat.exe" : "botchat");

async function startBotchat(args = []) {
  const proc = spawn(binaryPath, ["--addr", "127.0.0.1:0", ...args], { stdio: ["ignore", "pipe", "pipe"] });
  let output = "";
  const url = await new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error("botchat did not start:\n" + output)), 10_000);
    const onData = (chunk) => {
      output += chunk;
      const m = output.match(/listening on (\S+)/);
      if (m) {
        clearTimeout(timer);
        resolve("http://" + m[1]);
      }
    };
    proc.stdout.on("data", onData);
    proc.stderr.on("data", onData);
    proc.on("exit", (code) => reject(new Error(`botchat exited with ${code}:\n${output}`)));
  });
  return {
    url,
    async stop() {
      if (proc.exitCode !== null) return;
      const exited = new Promise((r) => proc.once("exit", r));
      proc.kill("SIGINT");
      await exited;
    },
  };
}

/** A tiny client for seeding state through the HTTP API. */
function apiClient(url, token) {
  return async function api(method, p, body) {
    const headers = { "Content-Type": "application/json" };
    if (token) headers.Authorization = "Bearer " + token;
    const res = await fetch(url + "/api/v1" + p, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
    });
    const json = await res.json();
    if (!res.ok) throw new Error(`${method} ${p}: ${res.status} ${JSON.stringify(json)}`);
    return json;
  };
}

export const test = base.extend({
  // Extra command-line flags for the server; override with test.use().
  botchatArgs: [[], { option: true }],
  token: [undefined, { option: true }],

  botchat: async ({ botchatArgs, token }, use) => {
    const args = token ? [...botchatArgs, "--token", token] : botchatArgs;
    const server = await startBotchat(args);
    const api = apiClient(server.url, token);
    await use({
      url: server.url,
      api,
      post: (channel, from, text, threadID) =>
        api("POST", `/channels/${channel}/messages`, { from, text, thread_id: threadID }),
    });
    await server.stop();
  },
});

export { expect };
