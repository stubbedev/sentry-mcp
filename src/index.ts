#!/usr/bin/env node
import 'dotenv/config';
import { readFileSync } from 'fs';
import { fileURLToPath } from 'url';
import { dirname, join } from 'path';
import { Server } from '@modelcontextprotocol/sdk/server/index.js';
import { StdioServerTransport } from '@modelcontextprotocol/sdk/server/stdio.js';
import {
  CallToolRequestSchema,
  ListToolsRequestSchema,
  McpError,
  ErrorCode,
} from '@modelcontextprotocol/sdk/types.js';
import { loadConfig } from './config.js';
import { SentryClient } from './sentry.js';

const _pkg = JSON.parse(
  readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../package.json'), 'utf-8'),
) as { version: string };

const config = loadConfig();
const sentry = config.sentry
  ? new SentryClient(config.sentry.url, config.sentry.token, config.sentry.org)
  : null;

if (!sentry) {
  console.error('[sentry-mcp] No Sentry configuration found. Set sentry.{url,token,org} in ~/.sentry-mcp.json or SENTRY_URL/SENTRY_AUTH_TOKEN/SENTRY_ORG_SLUG env vars. Server will start with no tools registered.');
}

async function buildInstructions(): Promise<string> {
  const lines: string[] = [];
  lines.push('# sentry-mcp');
  lines.push('');
  lines.push('Self-hosted Sentry tooling for triaging issues, inspecting events, and managing assignments. Prefer these tools over shelling out to `curl` or guessing endpoint paths.');
  lines.push('');
  if (!sentry || !config.sentry) {
    lines.push('## Configured instance');
    lines.push('- (not configured — set SENTRY_URL, SENTRY_AUTH_TOKEN, SENTRY_ORG_SLUG)');
    return lines.join('\n');
  }

  const me = await sentry.whoami().catch(() => null);

  lines.push('## Configured instance');
  lines.push(`- URL:  ${config.sentry.url}`);
  lines.push(`- Org:  ${config.sentry.org}`);
  if (me) {
    const ident = me.username ?? me.email ?? me.name ?? '?';
    lines.push(`- You:  ${ident}${me.email && me.email !== me.username ? ` <${me.email}>` : ''}`);
  }

  // Best-effort project listing so the model knows what slugs exist
  try {
    const projects = await sentry.fetchProjects(20);
    if (projects.length) {
      lines.push('');
      lines.push(`## Projects (top ${projects.length})`);
      for (const proj of projects) {
        lines.push(`- ${proj.slug ?? '?'}${proj.platform ? ` (${proj.platform})` : ''}`);
      }
    }
  } catch {
    // Skip project listing if instance is unreachable at startup
  }

  lines.push('');
  lines.push('## Use these tools — do NOT guess');
  lines.push('- "what am I working on / show me the context" → call `sentry_get_dev_context` first.');
  lines.push('- Looking up a person\'s username (for `assignedTo`) → ALWAYS use `sentry_search resource=users`. NEVER guess from git authors or email prefixes — the wrong username silently breaks `sentry_mutate_issue`.');
  lines.push('- Reading an issue → `sentry_get_issue`. Stack traces only → `sentry_stack_frames`. Full event → `sentry_get_event`.');
  lines.push('- Mutating an issue → `sentry_mutate_issue`. Comments → `sentry_comment`.');
  lines.push('- Missing iOS/macOS/Android symbols → `sentry_check_dsym` returns the UUIDs to upload.');
  lines.push('- Anything else → `sentry_raw_api`. Always pass `grepPattern` or `maxChars`/`charOffset` for endpoints that may return large events.');
  lines.push('');
  lines.push('IMPORTANT: do NOT resolve, ignore, or reassign issues without an explicit user instruction. Read tools are safe; mutation tools are not.');

  return lines.join('\n');
}

const _instructions = await buildInstructions();

const server = new Server(
  { name: 'sentry-mcp', version: _pkg.version },
  { capabilities: { tools: {} }, instructions: _instructions },
);

server.onerror = (error) => console.error('[MCP Error]', error);

function normalizeArgs(args: unknown): Record<string, unknown> {
  const src = (args && typeof args === 'object') ? (args as Record<string, unknown>) : {};
  const out: Record<string, unknown> = { ...src };
  if (typeof out.project === 'string' && typeof out.projectSlug !== 'string') out.projectSlug = out.project;
  return out;
}

function requireSentry(): SentryClient {
  if (!sentry) throw new McpError(ErrorCode.InvalidRequest, 'Sentry is not configured. Set sentry.{url,token,org} in ~/.sentry-mcp.json or SENTRY_URL/SENTRY_AUTH_TOKEN/SENTRY_ORG_SLUG env vars.');
  return sentry;
}

server.setRequestHandler(ListToolsRequestSchema, async () => ({
  tools: !sentry ? [] : [
    {
      name: 'sentry_get_dev_context',
      description: 'Master entry point. Use when asked "what am I working on?", "what\'s the status?", "show me the context", or before any triage task. Returns: configured instance + org, your Sentry identity, unresolved issues assigned to you, and recent unresolved issues across the org with actionable next-step hints.',
      inputSchema: {
        type: 'object',
        properties: {},
      },
    },
    {
      name: 'sentry_search',
      description: 'Discover Sentry resources. Use when asked "list projects", "show issues for X", "what teams exist", or "find user alice". Set resource:\n• "issues" (default) — list issues for a project (pass projectSlug or project); filter with query and/or status\n• "projects" — list all projects in the organization\n• "teams" — list all teams in the organization\n• "users" — find members by name/email (pass query). ALWAYS use this to look up valid usernames before passing to `sentry_mutate_issue assignedTo` — guessing from email prefixes or git authors silently breaks assignment.',
      inputSchema: {
        type: 'object',
        properties: {
          resource:    { type: 'string', enum: ['issues', 'projects', 'teams', 'users'], description: 'What to search (default: issues)' },
          projectSlug: { type: 'string', description: 'Project slug, e.g. "my-web-app" (required for resource=issues)' },
          project:     { type: 'string', description: 'Alias for projectSlug' },
          query:       { type: 'string', description: 'Sentry search query for issues (e.g. "is:unresolved environment:production"), or name/email filter for users' },
          status:      { type: 'string', enum: ['resolved', 'unresolved', 'ignored'], description: 'Status filter, appended to query (issues only)' },
          limit:       { type: 'number', description: 'Max results, 1-100 (default 25)', default: 25, minimum: 1, maximum: 100 },
          cursor:      { type: 'string', description: 'Pagination cursor from a previous response' },
        },
      },
    },
    {
      name: 'sentry_get_issue',
      description: 'Full details for one Sentry issue by ID or URL. Returns essential metadata by default to keep the response compact. Pass includeLatestEvent=true to also pull a trimmed version of the most recent event. Use the filtering knobs (includeFields, excludeFields, grepPattern, maxStackFrames) to slim the response further when stack traces are large.',
      inputSchema: {
        type: 'object',
        properties: {
          issueIdOrUrl:       { type: 'string', description: 'Sentry issue ID (e.g. "123456") or full issue URL' },
          includeLatestEvent: { type: 'boolean', description: 'Include latest event entries (default false)', default: false },
          includeFields:      { type: 'array', items: { type: 'string' }, description: 'Whitelist of fields to keep. Dot notation supported (e.g. "latest_event.entries").' },
          excludeFields:      { type: 'array', items: { type: 'string' }, description: 'Blacklist of fields to drop. Applied only when includeFields is unset.' },
          grepPattern:        { type: 'string', description: 'Regex to filter response content. Returns only matching lines with surrounding context.' },
          maxStackFrames:     { type: 'number', description: 'Trim stack traces to the last N frames (the most relevant ones).' },
        },
        required: ['issueIdOrUrl'],
      },
    },
    {
      name: 'sentry_get_event',
      description: 'Full details for a specific event in a project. Sentry events can be very large — by default returns at most 5 entries, prioritising exception → message → breadcrumbs → request. Pass entryType to filter to a single entry kind, or limit/offset to page through entries.',
      inputSchema: {
        type: 'object',
        properties: {
          projectSlug: { type: 'string', description: 'Project slug, e.g. "my-web-app"' },
          project:     { type: 'string', description: 'Alias for projectSlug' },
          eventId:     { type: 'string', description: 'Event ID' },
          limit:       { type: 'number', description: 'Max entries to return (default 5)', default: 5, minimum: 1 },
          offset:      { type: 'number', description: 'Entry pagination offset (default 0)', default: 0, minimum: 0 },
          entryType:   { type: 'string', enum: ['exception', 'message', 'breadcrumbs', 'request', 'threads', 'debugmeta', 'contexts'], description: 'Return only entries of this type' },
        },
        required: ['eventId'],
      },
    },
    {
      name: 'sentry_mutate_issue',
      description: 'Update status, change assignee, and/or add a comment on a Sentry issue in one call. Use ONLY when the user explicitly asks to "resolve issue 123", "ignore this issue", "assign 123 to alice", or "add a comment on 123". Do NOT pre-emptively resolve or reassign issues during exploration. For assignedTo, look the username up first via `sentry_search resource=users` — guessing from email prefixes silently fails.',
      inputSchema: {
        type: 'object',
        properties: {
          issueId:    { type: 'string', description: 'Sentry issue ID to mutate' },
          status:     { type: 'string', enum: ['resolved', 'unresolved', 'ignored'], description: 'New status (optional)' },
          assignedTo: { type: 'string', description: 'Username, email, or team:slug to assign to. Pass empty string to unassign.' },
          comment:    { type: 'string', description: 'Comment to add after other mutations (optional)' },
        },
        required: ['issueId'],
      },
    },
    {
      name: 'sentry_comment',
      description: 'Add, update, or delete a comment on a Sentry issue. action defaults to "add". Can only edit/delete your own comments.',
      inputSchema: {
        type: 'object',
        properties: {
          action:    { type: 'string', enum: ['add', 'update', 'delete'], description: 'Operation (default: add)' },
          issueId:   { type: 'string', description: 'Sentry issue ID' },
          commentId: { type: 'string', description: 'Comment ID (required for update/delete)' },
          body:      { type: 'string', description: 'Comment text (required for add/update)' },
        },
        required: ['issueId'],
      },
    },
    {
      name: 'sentry_stack_frames',
      description: 'Extract structured stack-trace frames from an event. Optimised for debugging — returns only function/file/line/inApp without the surrounding event noise. Much more compact than sentry_get_event for stack analysis.',
      inputSchema: {
        type: 'object',
        properties: {
          projectSlug: { type: 'string', description: 'Project slug' },
          project:     { type: 'string', description: 'Alias for projectSlug' },
          eventId:     { type: 'string', description: 'Event ID' },
          inAppOnly:   { type: 'boolean', description: 'Drop system/library frames (default false)', default: false },
          maxFrames:   { type: 'number', description: 'Max frames, keeping the most recent / bottom of stack (default 50)', default: 50 },
        },
        required: ['eventId'],
      },
    },
    {
      name: 'sentry_check_dsym',
      description: 'Check whether iOS / macOS / Android debug symbols (dSYM, ProGuard mapping) are missing for an event. Missing symbols cause stack traces to show addresses instead of function names. Returns the UUIDs to upload.',
      inputSchema: {
        type: 'object',
        properties: {
          projectSlug: { type: 'string', description: 'Project slug, e.g. "apple-ios"' },
          project:     { type: 'string', description: 'Alias for projectSlug' },
          eventId:     { type: 'string', description: 'Event ID (optional — if omitted, checks the most recent issue\'s latest event)' },
        },
      },
    },
    {
      name: 'sentry_raw_api',
      description: 'Make a raw call to any Sentry API endpoint. Use when the structured tools above don\'t cover what you need. Endpoint should be the path after /api/0/ (e.g. "organizations/my-org/releases/"). For endpoints that may return large events, pass grepPattern to filter inline OR maxChars/charOffset to page through.',
      inputSchema: {
        type: 'object',
        properties: {
          endpoint:    { type: 'string', description: 'API path, e.g. "projects/my-org/my-project/events/abc123/". Leading /api/0/ is stripped if present.' },
          method:      { type: 'string', enum: ['GET', 'POST', 'PUT', 'DELETE'], description: 'HTTP method (default GET)', default: 'GET' },
          params:      { type: 'object', description: 'Query parameters' },
          body:        { type: 'object', description: 'Request body for POST/PUT' },
          grepPattern: { type: 'string', description: 'Regex to filter response content inline (returns matching lines plus context).' },
          maxChars:    { type: 'number', description: 'Max characters of JSON response to return. Pair with charOffset to page through large responses.' },
          charOffset:  { type: 'number', description: 'Skip this many characters from the start of the JSON response (for paging).' },
        },
        required: ['endpoint'],
      },
    },
  ],
}));

server.setRequestHandler(CallToolRequestSchema, async (request) => {
  const { name, arguments: rawArgs = {} } = request.params;
  const args = normalizeArgs(rawArgs);
  try {
    switch (name) {
      case 'sentry_get_dev_context':
        return await requireSentry().getDevContext();
      case 'sentry_search': {
        const a = args as { resource?: string; projectSlug?: string; query?: string; status?: string; limit?: number; cursor?: string };
        const client = requireSentry();
        const resource = a.resource ?? 'issues';
        if (resource === 'projects') return await client.listProjects({ limit: a.limit, cursor: a.cursor });
        if (resource === 'teams')    return await client.listTeams({ limit: a.limit, cursor: a.cursor });
        if (resource === 'users')    return await client.listUsers({ query: a.query, limit: a.limit, cursor: a.cursor });
        // issues (default)
        if (!a.projectSlug) throw new Error('projectSlug (or project) is required for resource=issues.');
        return await client.listIssues({ projectSlug: a.projectSlug, query: a.query, status: a.status, limit: a.limit, cursor: a.cursor });
      }
      case 'sentry_get_issue': {
        const a = args as { issueIdOrUrl: string; includeLatestEvent?: boolean; includeFields?: string[]; excludeFields?: string[]; grepPattern?: string; maxStackFrames?: number };
        return await requireSentry().getIssue(a);
      }
      case 'sentry_get_event': {
        const a = args as { projectSlug?: string; eventId: string; limit?: number; offset?: number; entryType?: string };
        if (!a.projectSlug) throw new Error('projectSlug (or project) is required.');
        return await requireSentry().getEvent({ projectSlug: a.projectSlug, eventId: a.eventId, limit: a.limit, offset: a.offset, entryType: a.entryType });
      }
      case 'sentry_mutate_issue': {
        const a = args as { issueId: string; status?: 'resolved' | 'unresolved' | 'ignored'; assignedTo?: string; comment?: string };
        return await requireSentry().mutateIssue({
          issueId: a.issueId,
          status: a.status,
          assignedTo: a.assignedTo === undefined ? undefined : a.assignedTo === '' ? null : a.assignedTo,
          comment: a.comment,
        });
      }
      case 'sentry_comment': {
        const a = args as { action?: string; issueId: string; commentId?: string; body?: string };
        const client = requireSentry();
        const action = a.action ?? 'add';
        if (action === 'update') {
          if (!a.commentId || !a.body) throw new Error('update requires commentId and body.');
          return await client.editComment({ issueId: a.issueId, commentId: a.commentId, body: a.body });
        }
        if (action === 'delete') {
          if (!a.commentId) throw new Error('delete requires commentId.');
          return await client.deleteComment({ issueId: a.issueId, commentId: a.commentId });
        }
        if (!a.body) throw new Error('add requires body.');
        return await client.addComment({ issueId: a.issueId, body: a.body });
      }
      case 'sentry_stack_frames': {
        const a = args as { projectSlug?: string; eventId: string; inAppOnly?: boolean; maxFrames?: number };
        if (!a.projectSlug) throw new Error('projectSlug (or project) is required.');
        return await requireSentry().getStackFrames({ projectSlug: a.projectSlug, eventId: a.eventId, inAppOnly: a.inAppOnly, maxFrames: a.maxFrames });
      }
      case 'sentry_check_dsym': {
        const a = args as { projectSlug?: string; eventId?: string };
        if (!a.projectSlug) throw new Error('projectSlug (or project) is required.');
        return await requireSentry().checkDsymStatus({ projectSlug: a.projectSlug, eventId: a.eventId });
      }
      case 'sentry_raw_api': {
        const a = args as { endpoint: string; method?: string; params?: Record<string, unknown>; body?: unknown; grepPattern?: string; maxChars?: number; charOffset?: number };
        return await requireSentry().rawApi(a);
      }
      default:
        throw new McpError(ErrorCode.MethodNotFound, `Unknown tool: ${name}`);
    }
  } catch (err) {
    if (err instanceof McpError) throw err;
    return {
      content: [{ type: 'text', text: `Error: ${(err as Error).message}` }],
      isError: true,
    };
  }
});

async function shutdown() {
  await server.close();
  process.exit(0);
}
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);

const transport = new StdioServerTransport();
await server.connect(transport);
