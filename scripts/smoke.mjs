// Dev smoke test: drives the stdio MCP server through the real lifecycle
// (initialize -> initialized -> tools/list) and asserts the tool list is
// well-formed. Run via `npm run smoke`. Not published.
import { spawn } from "node:child_process";

const proc = spawn("./sentry-mcp", [], {
  env: {
    ...process.env,
    SENTRY_URL: "https://smoke.invalid",
    SENTRY_AUTH_TOKEN: "smoke",
    SENTRY_ORG_SLUG: "smoke",
  },
  stdio: ["pipe", "pipe", "inherit"],
});

const send = (msg) => proc.stdin.write(JSON.stringify(msg) + "\n");
const fail = (m) => {
  console.error("smoke FAIL:", m);
  proc.kill();
  process.exit(1);
};

const timer = setTimeout(() => fail("timed out waiting for tools/list response"), 10000);

send({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocolVersion: "2025-06-18", capabilities: {}, clientInfo: { name: "smoke", version: "0" } } });
send({ jsonrpc: "2.0", method: "notifications/initialized" });
send({ jsonrpc: "2.0", id: 2, method: "tools/list", params: {} });

let buf = "";
proc.stdout.on("data", (chunk) => {
  buf += chunk;
  let nl;
  while ((nl = buf.indexOf("\n")) >= 0) {
    const line = buf.slice(0, nl).trim();
    buf = buf.slice(nl + 1);
    if (!line) continue;
    let r;
    try { r = JSON.parse(line); } catch { continue; }
    if (r.id !== 2) continue;
    clearTimeout(timer);
    if (r.error) fail("server error: " + r.error.message);
    const t = r.result?.tools;
    if (!Array.isArray(t) || !t.length) fail("no tools in response");
    const bad = t.filter((x) => !x.name || !x.inputSchema);
    if (bad.length) fail("malformed tools: " + bad.map((x) => x.name ?? "(unnamed)").join(", "));
    console.log("smoke OK — " + t.length + " tool(s): " + t.map((x) => x.name).join(", "));
    proc.kill();
    process.exit(0);
  }
});

proc.on("exit", (code) => fail("server exited early (code " + code + ")"));
