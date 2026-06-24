# sentry-mcp

A [Model Context Protocol](https://modelcontextprotocol.io) (MCP) server for **self-hosted Sentry**, written in Go. Exposes tools for natural-language workflows around issues, events, stack traces, and debug-symbol triage.

Distributed two ways: a zero-dependency **prebuilt binary** (run directly or via `go install`) and an **npm wrapper** (`npx @stubbedev/sentry-mcp`) that downloads the matching binary — so every config below works unchanged.

> **Note:** This server targets self-hosted Sentry installs. It will also work against sentry.io, but the official Sentry MCP is a better fit there.

---

## Tools

### Workflow

| Tool | Description |
|---|---|
| `sentry_get_dev_context` | Master entry point: configured instance + org, your Sentry identity, unresolved issues assigned to you, and recent unresolved issues across the org |

### Discovery & read

| Tool | Description |
|---|---|
| `sentry_search` | Discover resources: `issues` (default), `projects`, `teams`, or `users` via `resource` param. Use `users` to look up valid usernames before assignment. |
| `sentry_get_issue` | Full details for one issue (by ID or URL) with field/grep/stack-frame filtering to keep responses small |
| `sentry_get_event` | Full details for one event with smart entry prioritisation and pagination |
| `sentry_stack_frames` | Structured stack-trace frames only (function/file/line/inApp) — best for debug analysis |
| `sentry_check_dsym` | Check whether iOS/macOS/Android debug symbols are missing for an event |
| `sentry_raw_api` | Raw call to any Sentry API endpoint with optional `grepPattern` or `maxChars`/`charOffset` paging |

### Mutation

| Tool | Description |
|---|---|
| `sentry_mutate_issue` | Update status, assign, and/or add a comment on an issue in one call |
| `sentry_comment` | Add, update, or delete a comment on an issue (`action`: `add` / `update` / `delete`) |

Many tools accept `project` as an alias for `projectSlug`.

### Natural language examples

- "what am I working on?" → `sentry_get_dev_context`
- "list projects in this org" → `sentry_search` with `resource=projects`
- "find user alice" → `sentry_search` with `resource=users`, `query=alice`
- "show unresolved issues in my-web-app" → `sentry_search` with `projectSlug`, `status=unresolved`
- "what's issue 5217" → `sentry_get_issue` with `issueIdOrUrl=5217`
- "give me the stack trace for event abc123 in apple-ios" → `sentry_stack_frames`
- "are dSYMs missing on this crash?" → `sentry_check_dsym`
- "resolve issue 5217 and leave a comment" → `sentry_mutate_issue` with `status=resolved`, `comment=...`
- "list releases for my org" → `sentry_raw_api` with `endpoint=organizations/<org>/releases/`

---

## Setup

### 1. Create a config file

Create `~/.sentry-mcp.json`:

```json
{
  "$schema": "https://raw.githubusercontent.com/stubbedev/sentry-mcp/master/sentry-mcp.schema.json",
  "sentry": {
    "url": "https://sentry.example.com",
    "token": "your-sentry-auth-token",
    "org": "your-org-slug"
  }
}
```

The `$schema` field is optional but enables editor autocomplete and validation.

The token needs at minimum: `org:read`, `project:read`, `event:read`. Add `project:write` and `event:admin` if you want to mutate issues or comment.

Alternatively, use environment variables (or a `.env` file in this directory):

```env
SENTRY_URL=https://sentry.example.com
SENTRY_AUTH_TOKEN=your-sentry-auth-token
SENTRY_ORG_SLUG=your-org-slug
```

Config is resolved in this order: `--config <path>` CLI arg → `SENTRY_MCP_CONFIG` env var → `~/.sentry-mcp.json` → `$XDG_CONFIG_HOME/sentry-mcp/config.json` (defaults to `~/.config/sentry-mcp/config.json`) → `.sentry-mcp.json` in cwd → environment variables.

### 2. Connect to your AI tool

No cloning or building required — just point your tool at `npx @stubbedev/sentry-mcp@latest` and it will install and run automatically.

> Note: `--prefer-online` can break MCP startup in some clients. Keep the command simple and use the update steps below when you want to refresh.

---

#### Claude Code

```bash
claude mcp add sentry -- npx -y @stubbedev/sentry-mcp@latest --config ~/.sentry-mcp.json
```

---

#### Cursor

Add to `~/.cursor/mcp.json` (global) or `.cursor/mcp.json` (project-only):

```json
{
  "mcpServers": {
    "sentry": {
      "command": "npx",
      "args": ["-y", "@stubbedev/sentry-mcp@latest", "--config", "/Users/you/.sentry-mcp.json"]
    }
  }
}
```

---

#### Windsurf

Add to `~/.codeium/windsurf/mcp_config.json`:

```json
{
  "mcpServers": {
    "sentry": {
      "command": "npx",
      "args": ["-y", "@stubbedev/sentry-mcp@latest", "--config", "/Users/you/.sentry-mcp.json"]
    }
  }
}
```

---

#### Zed

Add to `~/.config/zed/settings.json`:

```json
{
  "context_servers": {
    "sentry": {
      "command": {
        "path": "npx",
        "args": ["-y", "@stubbedev/sentry-mcp@latest", "--config", "/home/you/.sentry-mcp.json"]
      }
    }
  }
}
```

---

#### OpenCode

Add to `opencode.json` in your project root (or `~/.config/opencode/opencode.json` for global):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "sentry": {
      "type": "local",
      "command": ["npx", "-y", "@stubbedev/sentry-mcp@latest", "--config", "/home/you/.sentry-mcp.json"]
    }
  }
}
```

---

#### Codex CLI

Add to `~/.codex/config.yaml`:

```yaml
mcpServers:
  sentry:
    command: npx
    args:
      - -y
      - @stubbedev/sentry-mcp@latest
      - --config
      - /home/you/.sentry-mcp.json
```

---

#### Go install (no npm)

If you have Go installed and prefer a native binary on your `PATH`:

```bash
go install github.com/stubbedev/sentry-mcp@latest
```

Then point your MCP client at the `sentry-mcp` command directly, e.g. for Claude Code:

```bash
claude mcp add sentry -- sentry-mcp --config ~/.sentry-mcp.json
```

Prebuilt binaries for every platform are also attached to each [GitHub release](https://github.com/stubbedev/sentry-mcp/releases) if you'd rather not build.

---

#### Nix (flake)

The repo is a flake exposing a `sentry-mcp` package, app, and dev shell. Run it directly:

```bash
nix run github:stubbedev/sentry-mcp -- --config ~/.sentry-mcp.json
```

Or consume it from your own flake:

```nix
{
  inputs.sentry-mcp.url = "github:stubbedev/sentry-mcp";
  # ... then use inputs.sentry-mcp.packages.${system}.default
}
```

For an MCP client, point the command at the built binary, e.g.:

```bash
claude mcp add sentry -- nix run github:stubbedev/sentry-mcp -- --config ~/.sentry-mcp.json
```

The package `version` is read from `package.json` (so it follows releases), and CI keeps `vendorHash` current automatically — see [Releases](#releases-maintainers).

---

#### Any other MCP-compatible tool

Most tools that support MCP accept the same JSON format. Use `npx` as the command with `["-y", "@stubbedev/sentry-mcp@latest", "--config", "/path/to/config.json"]` as the args — or the `sentry-mcp` binary directly.

### Updating existing installs

If your MCP client is already configured and you want the newest package version:

```bash
npx clear-npx-cache
```

Then restart your MCP client.

---

### Manual install (optional)

If you prefer to clone and build locally (requires Go 1.26+):

```bash
git clone git@github.com:stubbedev/sentry-mcp.git
cd sentry-mcp
go build -o sentry-mcp .
```

Then use `/path/to/sentry-mcp/sentry-mcp` instead of the `npx` command in the configs above.

---

## Output format (TOON)

Structured responses are serialized as [TOON](https://github.com/toon-format/toon) (Token-Oriented Object Notation) by default — a compact, LLM-friendly format that cuts ~30-60% of the tokens of equivalent JSON, with the biggest savings on uniform tables (issue lists, stack frames, users). Example:

```
users[2]{email,name,role,username}:
  alice@example.com,Alice,owner,alice
  bob@example.com,Bob,member,bob
```

Every data tool accepts `format` (`toon` | `json`) to override per call, and the env var `SENTRY_MCP_FORMAT=json` flips the default process-wide. Plain-text tools (`sentry_get_dev_context`, project listings, mutation/comment confirmations) are unaffected.

---

## Filtering large responses

Sentry events can blow past LLM context limits — a single event with a long stack trace and many breadcrumbs is easily 100K+ tokens. On top of TOON, the tools have several knobs to keep responses small:

- `sentry_get_issue`: pass `maxStackFrames=5`, `excludeFields=["stats","annotations"]`, or `grepPattern="AttributeError|process_activity"` to slim things down. Use `includeFields=["id","title","latest_event.entries"]` for the absolute minimum.
- `sentry_get_event`: defaults to 5 prioritised entries; pass `entryType="exception"` to focus on a stack trace, or `limit`/`offset` to page through.
- `sentry_stack_frames`: returns just frames — ideal when all you need is the call site.
- `sentry_raw_api`: warns when responses exceed ~20K tokens and suggests grep patterns; pass `grepPattern` directly to filter inline.

---

## Releases (Maintainers)

Each release ships **both** prebuilt Go binaries (attached to the GitHub release) and the npm wrapper `@stubbedev/sentry-mcp`. `.github/workflows/publish.yml` runs on a pushed `v*` tag and: cross-compiles binaries for 14 targets — linux (amd64, arm64, arm/v7, 386, ppc64le, s390x, riscv64), darwin (amd64, arm64), windows (amd64, arm64, 386), and freebsd (amd64, arm64) — attaches them to the GitHub release, then publishes the npm package.

Use semantic versioning. Breaking tool-surface changes should bump the minor version while `<1.0.0` (for example `0.0.x` -> `0.1.0`).

Release flow:

```bash
# 1. bump the version in package.json (the npm wrapper version === the tag,
#    and the binary embeds it via -ldflags -X main.Version)
#    e.g. set "version": "0.1.5"

# 2. commit, tag, and push
git commit -am "0.1.5"
git tag v0.1.5
git push origin HEAD --follow-tags
```

The publish workflow verifies that `package.json` version matches the tag before publishing.

- The npm step uses npm Trusted Publisher (OIDC), so no `NPM_TOKEN` secret is required
- The release step uses the default `GITHUB_TOKEN`
- The **Nix flake auto-tracks releases**: `flake.nix` reads its `version` from `package.json`, so bumping the version updates the flake too. The `Flake` workflow (`.github/workflows/flake.yml`) recomputes `vendorHash` on any change to `go.mod`/`go.sum`/sources and commits it back, so `nix build` always works against `master`.

Required npm setup (one-time):

- In npm package settings, add this GitHub repo/workflow as a Trusted Publisher

---

## Creating a Sentry auth token

1. Log in to your Sentry instance.
2. Click your profile avatar → **User settings** → **Auth tokens** (or visit `/settings/account/api/auth-tokens/`).
3. Click **Create New Token**.
4. Give the token a name (e.g. `sentry-mcp`) and grant scopes:
   - `org:read`
   - `project:read`
   - `event:read`
   - `project:write` (only if you want to update issue status or assignee)
   - `event:admin` (only if you want to comment on issues)
5. Click **Create Token** and copy the value — it will only be shown once.

Paste the token as `sentry.token` in your config file.

---

## Development

Requires Go 1.26+. Zero third-party dependencies — the server uses only the standard library.

```bash
# Build
go build -o sentry-mcp .

# Run directly
./sentry-mcp

# Vet
go vet ./...

# Test the tool list (smoke)
echo '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}' | ./sentry-mcp
```

To use a specific config file:

```bash
./sentry-mcp --config /path/to/config.json
```

Tool schemas live in `tools.json` (embedded into the binary via `go:embed`). The npm wrapper lives in `bin/cli.mjs` + `scripts/`.
