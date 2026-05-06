type ToolResult = { content: Array<{ type: 'text'; text: string }> };

interface SentryErrorPayload {
  detail?: string;
  message?: string;
  errors?: unknown;
}

function text(t: string): ToolResult {
  return { content: [{ type: 'text', text: t }] };
}

function json(obj: unknown): ToolResult {
  return text(JSON.stringify(obj, null, 2));
}

function parseSentryErrorDetails(errText: string): string {
  const trimmed = errText.trim();
  if (!trimmed) return '';
  try {
    const parsed = JSON.parse(trimmed) as SentryErrorPayload;
    if (parsed.detail) return parsed.detail;
    if (parsed.message) return parsed.message;
    if (parsed.errors) return JSON.stringify(parsed.errors);
  } catch {
    // fall through to raw text
  }
  return trimmed.length > 500 ? `${trimmed.slice(0, 500)}...` : trimmed;
}

function formatSentryError(status: number, method: string, path: string, details: string): string {
  const prefix = `Sentry ${status} ${method} ${path}`;
  if (status === 400) return `${prefix}. Invalid request. ${details}`.trim();
  if (status === 401) return `${prefix}. Authentication failed. Check SENTRY_AUTH_TOKEN.`;
  if (status === 403) return `${prefix}. Permission denied. Check token scopes (need org:read, project:read, event:read, etc.).`;
  if (status === 404) return `${prefix}. Resource not found. Verify org slug, project slug, issue/event ID.`;
  return details ? `${prefix}. ${details}` : prefix;
}

// Extract issue ID from a numeric string or a Sentry issue URL
export function extractIssueId(input: string): string | null {
  try {
    const url = new URL(input);
    const parts = url.pathname.split('/').filter(Boolean);
    const idx = parts.indexOf('issues');
    if (idx !== -1 && parts.length > idx + 1) {
      const candidate = parts[idx + 1];
      if (/^\d+$/.test(candidate)) return candidate;
    }
  } catch {
    if (/^\d+$/.test(input)) return input;
  }
  return null;
}

// Filter object fields based on include/exclude lists. Supports dot notation.
export function filterFields(obj: unknown, includeFields?: string[], excludeFields?: string[]): unknown {
  if (!obj || typeof obj !== 'object') return obj;
  if (Array.isArray(obj)) return obj.map((item) => filterFields(item, includeFields, excludeFields));

  const src = obj as Record<string, unknown>;
  let result: Record<string, unknown> = {};

  if (includeFields && includeFields.length > 0) {
    for (const field of includeFields) {
      if (field.includes('.')) {
        const [parent, ...rest] = field.split('.');
        if (src[parent] !== undefined) {
          if (!result[parent]) result[parent] = {};
          result[parent] = filterFields(src[parent], [rest.join('.')], undefined);
        }
      } else if (src[field] !== undefined) {
        result[field] = src[field];
      }
    }
  } else {
    result = { ...src };
    if (excludeFields && excludeFields.length > 0) {
      for (const field of excludeFields) {
        if (field.includes('.')) {
          const [parent, ...rest] = field.split('.');
          if (result[parent]) {
            result[parent] = filterFields(result[parent], undefined, [rest.join('.')]);
          }
        } else {
          delete result[field];
        }
      }
    }
  }
  return result;
}

// Apply grep pattern filtering to JSON content. Returns matching lines with surrounding context.
export function grepFilter(data: unknown, pattern: string): unknown {
  const regex = new RegExp(pattern, 'gi');
  const jsonStr = JSON.stringify(data, null, 2);
  const lines = jsonStr.split('\n');
  const matched: string[] = [];
  for (let i = 0; i < lines.length; i++) {
    if (regex.test(lines[i])) {
      if (i > 0) matched.push(lines[i - 1]);
      matched.push(lines[i]);
      if (i < lines.length - 1) matched.push(lines[i + 1]);
    }
  }
  const filtered = matched.join('\n');
  try {
    return JSON.parse(filtered);
  } catch {
    return { grep_results: matched, original_pattern: pattern };
  }
}

// Truncate stack traces inside event entries down to the most recent N frames.
export function truncateStackFrames(data: unknown, maxFrames: number): unknown {
  if (!data || typeof data !== 'object') return data;
  if (Array.isArray(data)) return data.map((item) => truncateStackFrames(item, maxFrames));

  const result = { ...(data as Record<string, unknown>) };

  const entries = result.entries;
  if (Array.isArray(entries)) {
    result.entries = entries.map((entry: Record<string, unknown>) => {
      if (entry.type === 'exception') {
        const entryData = entry.data as { values?: Array<Record<string, unknown>> } | undefined;
        if (entryData?.values) {
          entryData.values = entryData.values.map((value) => {
            const stacktrace = value.stacktrace as { frames?: unknown[] } | undefined;
            if (stacktrace?.frames && Array.isArray(stacktrace.frames) && stacktrace.frames.length > maxFrames) {
              const omitted = stacktrace.frames.length - maxFrames;
              stacktrace.frames = stacktrace.frames.slice(-maxFrames);
              (stacktrace as Record<string, unknown>).frames_omitted = omitted;
            }
            return value;
          });
        }
      }
      return entry;
    });
  }
  return result;
}

// Pull out only essential issue metadata to keep responses compact by default.
function essentialIssueFields(issue: Record<string, unknown>): Record<string, unknown> {
  return {
    id: issue.id,
    shortId: issue.shortId,
    title: issue.title,
    culprit: issue.culprit,
    permalink: issue.permalink,
    logger: issue.logger,
    level: issue.level,
    status: issue.status,
    type: issue.type,
    platform: issue.platform,
    project: issue.project,
    count: issue.count,
    userCount: issue.userCount,
    firstSeen: issue.firstSeen,
    lastSeen: issue.lastSeen,
    assignedTo: issue.assignedTo,
    metadata: issue.metadata,
  };
}

function essentialEventEntry(entry: Record<string, unknown>): Record<string, unknown> {
  if (entry.type === 'exception') {
    const data = entry.data as { values?: Array<Record<string, unknown>> } | undefined;
    return {
      type: entry.type,
      data: {
        values: (data?.values ?? []).map((exc) => {
          const stacktrace = exc.stacktrace as { frames?: Array<Record<string, unknown>> } | undefined;
          return {
            type: exc.type,
            value: exc.value,
            mechanism: exc.mechanism,
            stacktrace: stacktrace
              ? {
                  frames: stacktrace.frames?.slice(-5).map((frame) => ({
                    filename: frame.filename,
                    function: frame.function,
                    lineNo: frame.lineNo,
                    colNo: frame.colNo,
                    absPath: frame.absPath,
                    inApp: frame.in_app,
                    context: Array.isArray(frame.context) ? frame.context.slice(0, 7) : undefined,
                  })),
                }
              : undefined,
          };
        }),
      },
    };
  }
  if (entry.type === 'message') return entry;
  if (entry.type === 'breadcrumbs') {
    const data = entry.data as { values?: unknown[] } | undefined;
    return { type: entry.type, data: { values: (data?.values ?? []).slice(-10) } };
  }
  return { type: entry.type, _truncated: true };
}

export class SentryClient {
  private baseUrl: string;
  public readonly orgSlug: string;
  private headers: Record<string, string>;

  constructor(baseUrl: string, token: string, orgSlug: string) {
    this.baseUrl = baseUrl.replace(/\/$/, '');
    this.orgSlug = orgSlug;
    this.headers = {
      Authorization: `Bearer ${token}`,
      'Content-Type': 'application/json',
      Accept: 'application/json',
    };
  }

  private async request<T>(method: string, path: string, opts?: { params?: Record<string, unknown>; body?: unknown }): Promise<{ data: T; linkHeader: string | null; status: number }> {
    let qs = '';
    if (opts?.params) {
      const search = new URLSearchParams();
      for (const [k, v] of Object.entries(opts.params)) {
        if (v === undefined || v === null) continue;
        search.append(k, String(v));
      }
      const str = search.toString();
      if (str) qs = `?${str}`;
    }
    const cleanPath = path.startsWith('/') ? path : `/${path}`;
    const url = `${this.baseUrl}/api/0${cleanPath}${qs}`;
    const init: RequestInit = { method, headers: this.headers, signal: AbortSignal.timeout(30_000) };
    if (opts?.body !== undefined) init.body = JSON.stringify(opts.body);
    const res = await fetch(url, init);
    if (!res.ok) {
      const errText = await res.text();
      throw new Error(formatSentryError(res.status, method, cleanPath, parseSentryErrorDetails(errText)));
    }
    if (res.status === 204) return { data: undefined as T, linkHeader: res.headers.get('link'), status: res.status };
    const data = (await res.json()) as T;
    return { data, linkHeader: res.headers.get('link'), status: res.status };
  }

  // Parse Sentry's Link header to extract the `next` cursor when results=true.
  private parseNextCursor(linkHeader: string | null): string | null {
    if (!linkHeader) return null;
    // Format: <url>; rel="next"; results="true"; cursor="0:100:0"
    const segments = linkHeader.split(',');
    for (const seg of segments) {
      if (!/rel="next"/.test(seg)) continue;
      if (!/results="true"/.test(seg)) return null;
      const m = seg.match(/cursor="([^"]+)"/);
      return m ? m[1] : null;
    }
    return null;
  }

  // ── Discovery ───────────────────────────────────────────────────────────
  async listProjects(): Promise<ToolResult> {
    const { data } = await this.request<unknown[]>('GET', `/organizations/${this.orgSlug}/projects/`);
    if (!Array.isArray(data) || data.length === 0) return text('No projects found.');
    const summary = data.map((p) => {
      const proj = p as Record<string, unknown>;
      return `  • ${proj.slug} — ${proj.name}${proj.platform ? ` (${proj.platform})` : ''}`;
    });
    return text(`Projects in "${this.orgSlug}":\n${summary.join('\n')}`);
  }

  async listTeams(): Promise<ToolResult> {
    const { data } = await this.request<unknown[]>('GET', `/organizations/${this.orgSlug}/teams/`);
    if (!Array.isArray(data) || data.length === 0) return text('No teams found.');
    return json(data);
  }

  async listIssues(args: { projectSlug: string; query?: string; status?: string; limit?: number; cursor?: string }): Promise<ToolResult> {
    const params: Record<string, unknown> = {};
    let q = args.query ?? '';
    if (args.status) q = q ? `${q} is:${args.status}` : `is:${args.status}`;
    if (q) params.query = q;
    params.limit = Math.min(Math.max(args.limit ?? 25, 1), 100);
    if (args.cursor) params.cursor = args.cursor;

    const { data, linkHeader } = await this.request<unknown[]>('GET', `/projects/${this.orgSlug}/${args.projectSlug}/issues/`, { params });
    const issues = Array.isArray(data) ? data.map((i) => essentialIssueFields(i as Record<string, unknown>)) : [];
    const nextCursor = this.parseNextCursor(linkHeader);
    const payload: Record<string, unknown> = { issues, count: issues.length };
    if (nextCursor) payload.next_cursor = nextCursor;
    return json(payload);
  }

  // ── Issue read ──────────────────────────────────────────────────────────
  async getIssue(args: {
    issueIdOrUrl: string;
    includeLatestEvent?: boolean;
    includeFields?: string[];
    excludeFields?: string[];
    grepPattern?: string;
    maxStackFrames?: number;
  }): Promise<ToolResult> {
    const issueId = extractIssueId(args.issueIdOrUrl);
    if (!issueId) throw new Error(`Could not extract issue ID from "${args.issueIdOrUrl}". Pass a numeric ID or full issue URL.`);

    const { data: issueRaw } = await this.request<Record<string, unknown>>('GET', `/issues/${issueId}/`);
    let combined: Record<string, unknown> = { ...essentialIssueFields(issueRaw), latest_event: null };

    if (args.includeLatestEvent) {
      try {
        const { data: ev } = await this.request<Record<string, unknown>>('GET', `/organizations/${this.orgSlug}/issues/${issueId}/events/latest/`);
        const entries = Array.isArray(ev.entries) ? ev.entries.slice(0, 3).map((e) => essentialEventEntry(e as Record<string, unknown>)) : [];
        combined.latest_event = {
          id: ev.id,
          eventID: ev.eventID,
          dateCreated: ev.dateCreated,
          entries,
          _note: 'Event truncated. Use sentry_get_event for full data.',
        };
      } catch (err) {
        combined.latest_event = { _error: (err as Error).message };
      }
    }

    if (args.maxStackFrames !== undefined) combined = truncateStackFrames(combined, args.maxStackFrames) as Record<string, unknown>;
    if (args.includeFields || args.excludeFields) combined = filterFields(combined, args.includeFields, args.excludeFields) as Record<string, unknown>;
    let out: unknown = combined;
    if (args.grepPattern) out = grepFilter(combined, args.grepPattern);
    return json(out);
  }

  async getEvent(args: { projectSlug: string; eventId: string; limit?: number; offset?: number; entryType?: string }): Promise<ToolResult> {
    const { data: ev } = await this.request<Record<string, unknown>>('GET', `/projects/${this.orgSlug}/${args.projectSlug}/events/${args.eventId}/`);
    const offset = args.offset ?? 0;
    const limit = args.limit ?? 5;

    const out: Record<string, unknown> = { id: ev.id, eventID: ev.eventID, dateCreated: ev.dateCreated, message: ev.message, title: ev.title, platform: ev.platform };

    if (Array.isArray(ev.entries)) {
      const entries = ev.entries as Array<Record<string, unknown>>;
      const total = entries.length;
      let selected: Array<Record<string, unknown>>;

      if (args.entryType) {
        selected = entries.filter((e) => e.type === args.entryType).slice(offset, offset + limit);
      } else {
        const priority = ['exception', 'message', 'breadcrumbs', 'request'];
        const top: Array<Record<string, unknown>> = [];
        for (const t of priority) {
          const entry = entries.find((e) => e.type === t);
          if (entry && top.length < limit) top.push(entry);
        }
        if (top.length < limit) {
          const others = entries.filter((e) => !priority.includes(e.type as string)).slice(0, limit - top.length);
          top.push(...others);
        }
        selected = top;
      }

      out.entries = selected.map(essentialEventEntry);
      out.pagination_info = {
        total_entries: total,
        showing: selected.length,
        available_types: [...new Set(entries.map((e) => e.type))],
        tip: args.entryType
          ? `Showing only "${args.entryType}" entries. Remove entryType to see prioritized entries.`
          : 'Showing prioritized entries. Use entryType="exception" to see only stack traces.',
      };
    }
    return json(out);
  }

  // ── Issue mutation ──────────────────────────────────────────────────────
  async updateIssueStatus(args: { issueId: string; status: 'resolved' | 'unresolved' | 'ignored' }): Promise<ToolResult> {
    const { data } = await this.request<Record<string, unknown>>('PUT', `/issues/${args.issueId}/`, { body: { status: args.status } });
    return text(`Issue ${args.issueId} → ${data.status}`);
  }

  async assignIssue(args: { issueId: string; assignedTo: string | null }): Promise<ToolResult> {
    const body: Record<string, unknown> = { assignedTo: args.assignedTo === null ? '' : args.assignedTo };
    const { data } = await this.request<Record<string, unknown>>('PUT', `/issues/${args.issueId}/`, { body });
    const assignedTo = data.assignedTo as Record<string, unknown> | null | undefined;
    const label = assignedTo?.username ?? assignedTo?.name ?? assignedTo?.email ?? '(unassigned)';
    return text(`Issue ${args.issueId} assignee → ${label}`);
  }

  async mutateIssue(args: { issueId: string; status?: 'resolved' | 'unresolved' | 'ignored'; assignedTo?: string | null; comment?: string }): Promise<ToolResult> {
    const lines: string[] = [];
    if (args.status !== undefined) {
      const r = await this.updateIssueStatus({ issueId: args.issueId, status: args.status });
      lines.push(r.content[0].text);
    }
    if (args.assignedTo !== undefined) {
      const r = await this.assignIssue({ issueId: args.issueId, assignedTo: args.assignedTo });
      lines.push(r.content[0].text);
    }
    if (args.comment !== undefined && args.comment.trim()) {
      const r = await this.addComment({ issueId: args.issueId, body: args.comment });
      lines.push(r.content[0].text);
    }
    if (lines.length === 0) return text('No mutations specified. Provide status, assignedTo, or comment.');
    return text(lines.join('\n'));
  }

  // ── Comments ────────────────────────────────────────────────────────────
  async addComment(args: { issueId: string; body: string }): Promise<ToolResult> {
    const trimmed = args.body.trim();
    if (!trimmed) throw new Error('Comment body must not be empty.');
    const { data } = await this.request<Record<string, unknown>>('POST', `/issues/${args.issueId}/comments/`, { body: { text: trimmed } });
    return text(`Added comment ${data.id} on issue ${args.issueId}.`);
  }

  async editComment(args: { issueId: string; commentId: string; body: string }): Promise<ToolResult> {
    const trimmed = args.body.trim();
    if (!trimmed) throw new Error('Comment body must not be empty.');
    const { data } = await this.request<Record<string, unknown>>('PUT', `/issues/${args.issueId}/comments/${args.commentId}/`, { body: { text: trimmed } });
    return text(`Updated comment ${data.id} on issue ${args.issueId}.`);
  }

  async deleteComment(args: { issueId: string; commentId: string }): Promise<ToolResult> {
    await this.request('DELETE', `/issues/${args.issueId}/comments/${args.commentId}/`);
    return text(`Deleted comment ${args.commentId} on issue ${args.issueId}.`);
  }

  // ── Specialized debug tools ─────────────────────────────────────────────
  async getStackFrames(args: { projectSlug: string; eventId: string; inAppOnly?: boolean; maxFrames?: number }): Promise<ToolResult> {
    const { data: ev } = await this.request<Record<string, unknown>>('GET', `/projects/${this.orgSlug}/${args.projectSlug}/events/${args.eventId}/`);
    const frames: Array<Record<string, unknown>> = [];
    if (Array.isArray(ev.entries)) {
      for (const entry of ev.entries as Array<Record<string, unknown>>) {
        if (entry.type !== 'exception') continue;
        const entryData = entry.data as { values?: Array<Record<string, unknown>> } | undefined;
        for (const exc of entryData?.values ?? []) {
          const stacktrace = exc.stacktrace as { frames?: Array<Record<string, unknown>> } | undefined;
          for (const frame of stacktrace?.frames ?? []) {
            if (args.inAppOnly && !frame.in_app) continue;
            frames.push({
              function: frame.function ?? frame.rawFunction ?? '<unknown>',
              filename: frame.filename ?? frame.absPath ?? null,
              lineNo: frame.lineNo ?? null,
              colNo: frame.colNo ?? null,
              inApp: frame.in_app ?? false,
              module: frame.module ?? null,
              package: frame.package ?? null,
              instructionAddr: frame.instructionAddr ?? null,
              symbolAddr: frame.symbolAddr ?? null,
            });
          }
        }
      }
    }
    const max = args.maxFrames ?? 50;
    const limited = frames.slice(-max);
    return json({
      eventId: args.eventId,
      totalFrames: frames.length,
      returnedFrames: limited.length,
      inAppOnly: args.inAppOnly ?? false,
      frames: limited,
    });
  }

  async checkDsymStatus(args: { projectSlug: string; eventId?: string }): Promise<ToolResult> {
    let ev: Record<string, unknown>;
    if (args.eventId) {
      const r = await this.request<Record<string, unknown>>('GET', `/projects/${this.orgSlug}/${args.projectSlug}/events/${args.eventId}/`);
      ev = r.data;
    } else {
      const r = await this.request<unknown[]>('GET', `/projects/${this.orgSlug}/${args.projectSlug}/issues/`, { params: { limit: 1 } });
      const issues = r.data;
      if (!Array.isArray(issues) || issues.length === 0) return text('No recent issues found in project. Cannot check dSYM status.');
      const issueId = (issues[0] as Record<string, unknown>).id;
      const evRes = await this.request<Record<string, unknown>>('GET', `/organizations/${this.orgSlug}/issues/${issueId}/events/latest/`);
      ev = evRes.data;
    }

    const missing: Array<Record<string, unknown>> = [];
    if (Array.isArray(ev.errors)) {
      for (const err of ev.errors as Array<Record<string, unknown>>) {
        if (err.type === 'native_missing_dsym' || err.type === 'proguard_missing_mapping') {
          const errData = err.data as Record<string, unknown> | undefined;
          missing.push({
            type: err.type,
            message: err.message,
            imagePath: errData?.image_path,
            imageUuid: errData?.image_uuid,
            imageName: errData?.image_name,
          });
        }
      }
    }
    return json({
      project: args.projectSlug,
      eventId: ev.eventID ?? args.eventId,
      hasMissingSymbols: missing.length > 0,
      missingCount: missing.length,
      missingSymbols: missing,
      recommendation: missing.length > 0
        ? 'Upload missing dSYM files to Sentry — sentry-cli upload-dif <path> — to see function names instead of addresses.'
        : 'All debug symbols are present for this event.',
    });
  }

  async rawApi(args: { endpoint: string; method?: string; params?: Record<string, unknown>; body?: unknown; grepPattern?: string }): Promise<ToolResult> {
    const method = (args.method ?? 'GET').toUpperCase();
    if (!['GET', 'POST', 'PUT', 'DELETE'].includes(method)) throw new Error(`Unsupported HTTP method: ${method}`);
    // Strip optional /api/0/ prefix so callers can copy URLs from docs.
    const endpoint = args.endpoint.replace(/^\/?api\/0\//, '/').replace(/^\/+/, '/');
    const path = endpoint.startsWith('/') ? endpoint : `/${endpoint}`;

    const { data } = await this.request<unknown>(method, path, { params: args.params, body: args.body });

    let result: unknown = data;
    const jsonStr = JSON.stringify(result, null, 2);
    const estimatedTokens = Math.ceil(jsonStr.length / 4);
    if (estimatedTokens > 20000 && !args.grepPattern) {
      return text([
        `WARNING: Response is approximately ${estimatedTokens} tokens.`,
        '',
        'This endpoint returns a lot of data. Re-run with grepPattern to filter.',
        'Suggested patterns:',
        '  - Stack frames: \'"function":|"filename":|"in_app":\'',
        '  - Breadcrumbs:  \'"breadcrumbs"\'',
        '  - Tags:         \'"tags"\'',
      ].join('\n'));
    }
    if (args.grepPattern) result = grepFilter(result, args.grepPattern);
    return json(result);
  }
}
