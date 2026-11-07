// Builds the botchat binary once for the whole run.
import { execFileSync } from "node:child_process";
import { binaryPath, repoRoot } from "./fixtures.js";

export default function globalSetup() {
  execFileSync("go", ["build", "-o", binaryPath, "./cmd/botchat"], {
    cwd: repoRoot,
    stdio: "inherit",
    env: { ...process.env, CGO_ENABLED: "0" },
  });
}
