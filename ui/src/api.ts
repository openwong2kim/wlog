// API client — PLAN §10 frozen HTTP JSON contract.
// fetch for REST, EventSource for /api/now SSE.
// Units: ts = unix ms, cost = USD, duration = ms, tokens = integers.

export type EmptyReason = 'no_data' | 'logs_disabled' | 'no_active_session';

export interface Meta {
  empty_reason?: EmptyReason;
}

export interface ApiError {
  error: { code: 'bad_request' | 'not_found' | 'internal'; message: string };
}

export type TokenSource = 'events' | 'metrics' | 'mixed';

// ---- /api/health ----
export interface Health {
  status: string;
  version: string;
  uptime_sec: number;
  db: { path: string; size_bytes: number };
  ingest: {
    received: number;
    accepted: number;
    rejected_permanent: number;
    retry_signaled: number;
    bus_dropped: number;
  };
  retention: { policy: string };
  // Non-contract optional hint: backend may surface bind safety / privacy
  // state to drive banners. Treated as optional.
  bind?: { non_local?: boolean; auth?: boolean };
  logs_enabled?: boolean;
}

// ---- /api/sessions ----
export interface Tokens {
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
}

export interface SessionRow {
  id: string;
  first_seen: number;
  last_seen: number;
  cost_usd: number;
  tokens: Tokens;
  duration_sec: number;
  tool_accept: number;
  tool_reject: number;
  tool_accept_rate: number; // 0..1
  model_set: string;
  token_source: TokenSource;
  has_events: boolean;
}

export interface SessionsResponse {
  sessions: SessionRow[];
  total: number;
  meta?: Meta;
}

export type SessionsSort = 'last_seen' | 'cost' | 'tokens';
export type SortOrder = 'asc' | 'desc';

export interface SessionsQuery {
  limit?: number;
  offset?: number;
  since?: number;
  sort?: SessionsSort;
  order?: SortOrder;
  q?: string;
}

// ---- /api/sessions/:id ----
export interface SessionModel {
  model: string;
  cost_usd: number;
  tokens: number;
}

export interface SessionDetail {
  id: string;
  first_seen: number;
  last_seen: number;
  cost_usd: number;
  tokens: Tokens;
  duration_sec: number;
  tool_accept: number;
  tool_reject: number;
  tool_accept_rate: number;
  models: SessionModel[];
  token_source: TokenSource;
  has_events: boolean;
  meta?: Meta;
}

// ---- /api/sessions/:id/timeline ----
export type TimelineKind =
  | 'prompt'
  | 'api_request'
  | 'api_error'
  | 'tool_decision'
  | 'tool_result';

interface TimelineBase {
  ts: number;
  kind: TimelineKind;
}
export interface TLPrompt extends TimelineBase {
  kind: 'prompt';
  prompt_length: number;
  // Full prompt text. null when the prompt was not stored (--no-store-prompts);
  // older backends may omit the key entirely, so treat undefined as null too.
  prompt?: string | null;
}
export interface TLApiRequest extends TimelineBase {
  kind: 'api_request';
  model: string;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  cost_usd: number;
  duration_ms: number;
}
export interface TLApiError extends TimelineBase {
  kind: 'api_error';
  model: string;
  status_code: number;
  error: string;
}
export interface TLToolDecision extends TimelineBase {
  kind: 'tool_decision';
  decision: 'accept' | 'reject';
  source: string;
  tool_name: string;
}
export interface TLToolResult extends TimelineBase {
  kind: 'tool_result';
  tool_name: string;
  success: boolean;
  duration_ms: number;
  bash_command: string | null;
  mcp_server: string | null;
}
export type TimelineEvent =
  | TLPrompt
  | TLApiRequest
  | TLApiError
  | TLToolDecision
  | TLToolResult;

export interface TimelineResponse {
  events: TimelineEvent[];
  meta?: Meta;
}

// ---- /api/cost ----
export interface CostSeriesPoint {
  ts: number;
  cost_usd: number;
  input: number;
  output: number;
  cache_read: number;
  cache_creation: number;
  // Cumulative spend up to and including this bucket (USD). Optional:
  // older backends omit it -> view falls back gracefully.
  cum_cost_usd?: number;
}
export interface CostByModel {
  model: string;
  cost_usd: number;
  tokens: number;
  pct: number; // 0..1
}
export interface CostResponse {
  series: CostSeriesPoint[];
  by_model: CostByModel[];
  cache_hit_ratio: number; // 0..1
  token_source: TokenSource;
  // Money-centric fields (frozen contract addition). All optional so the
  // view renders safely when the backend has not yet been updated.
  total_cost_usd?: number;
  burn_rate_usd_per_min?: number;
  burn_rate_tok_per_min?: number;
  cache_saved_usd?: number | null;
  cache_saved_estimated?: boolean;
  meta?: Meta;
}
export interface CostQuery {
  bucket?: 'hour' | 'day';
  since?: number;
  model?: string;
  session?: string;
}

// ---- /api/tools ----
export interface ToolsBySource {
  source: string;
  accept: number;
  reject: number;
}
export interface ToolHotspot {
  tool_name: string;
  reject: number;
  accept: number;
}
export interface ToolsResponse {
  auto_approved: number;
  accept: number;
  reject: number;
  reject_rate: number; // 0..1
  by_source: ToolsBySource[];
  hotspots: ToolHotspot[];
  meta?: Meta;
}
export interface ToolsQuery {
  session?: string;
  since?: number;
}

// ---- /api/setup ----
export interface SetupResponse {
  snippet: string;
  ports: { grpc: number; http: number };
}

// ---- /api/now SSE payloads ----
export interface NowTick {
  ts: number;
  session_id: string | null;
  cost_usd: number;
  tokens: Tokens;
  active_time_sec: number;
  active_tool: string | null;
}
export interface NowLog {
  ts: number;
  kind: string;
  summary: string;
  [k: string]: unknown;
}
export interface NowDrop {
  dropped: number;
}
export interface NowStatus {
  state: 'live' | 'reconnecting';
}

// ---- core fetch helper ----

async function getJSON<T>(url: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(url, signal ? { signal } : undefined);
  if (!res.ok) {
    let msg = `HTTP ${res.status}`;
    try {
      const body = (await res.json()) as ApiError;
      if (body?.error?.message) msg = body.error.message;
    } catch {
      /* non-JSON error body */
    }
    throw new Error(msg);
  }
  return (await res.json()) as T;
}

function qs(params: Record<string, string | number | undefined>): string {
  const parts: string[] = [];
  for (const [k, v] of Object.entries(params)) {
    if (v === undefined || v === '') continue;
    parts.push(encodeURIComponent(k) + '=' + encodeURIComponent(String(v)));
  }
  return parts.length ? '?' + parts.join('&') : '';
}

// ---- REST methods ----

export function getHealth(signal?: AbortSignal): Promise<Health> {
  return getJSON<Health>('/api/health', signal);
}

export function getSessions(
  query: SessionsQuery = {},
  signal?: AbortSignal,
): Promise<SessionsResponse> {
  const url =
    '/api/sessions' +
    qs({
      limit: query.limit,
      offset: query.offset,
      since: query.since,
      sort: query.sort,
      order: query.order,
      q: query.q,
    });
  return getJSON<SessionsResponse>(url, signal);
}

export function getSession(
  id: string,
  signal?: AbortSignal,
): Promise<SessionDetail> {
  return getJSON<SessionDetail>(
    '/api/sessions/' + encodeURIComponent(id),
    signal,
  );
}

export function getTimeline(
  id: string,
  signal?: AbortSignal,
): Promise<TimelineResponse> {
  return getJSON<TimelineResponse>(
    '/api/sessions/' + encodeURIComponent(id) + '/timeline',
    signal,
  );
}

export function getCost(
  query: CostQuery = {},
  signal?: AbortSignal,
): Promise<CostResponse> {
  const url =
    '/api/cost' +
    qs({
      bucket: query.bucket,
      since: query.since,
      model: query.model,
      session: query.session,
    });
  return getJSON<CostResponse>(url, signal);
}

export function getTools(
  query: ToolsQuery = {},
  signal?: AbortSignal,
): Promise<ToolsResponse> {
  const url = '/api/tools' + qs({ session: query.session, since: query.since });
  return getJSON<ToolsResponse>(url, signal);
}

export function getSetup(signal?: AbortSignal): Promise<SetupResponse> {
  return getJSON<SetupResponse>('/api/setup', signal);
}

// ---- SSE: /api/now ----

export interface NowHandlers {
  onTick?: (t: NowTick) => void;
  onLog?: (l: NowLog) => void;
  onDrop?: (d: NowDrop) => void;
  onStatus?: (s: NowStatus) => void;
  /** transport-level open/error -> drives LIVE / RECONNECTING. */
  onOpen?: () => void;
  onError?: () => void;
}

export interface NowConnection {
  close(): void;
}

/**
 * Opens the /api/now SSE stream. EventSource auto-reconnects; we surface
 * open/error so the view can flip LIVE <-> RECONNECTING. Last-Event-ID is
 * handled natively by EventSource on reconnect.
 */
export function connectNow(handlers: NowHandlers): NowConnection {
  const es = new EventSource('/api/now');

  es.addEventListener('open', () => handlers.onOpen?.());
  es.addEventListener('error', () => handlers.onError?.());

  es.addEventListener('tick', (ev) => {
    try {
      handlers.onTick?.(JSON.parse((ev as MessageEvent).data) as NowTick);
    } catch {
      /* ignore malformed frame */
    }
  });
  es.addEventListener('log', (ev) => {
    try {
      handlers.onLog?.(JSON.parse((ev as MessageEvent).data) as NowLog);
    } catch {
      /* ignore */
    }
  });
  es.addEventListener('drop', (ev) => {
    try {
      handlers.onDrop?.(JSON.parse((ev as MessageEvent).data) as NowDrop);
    } catch {
      /* ignore */
    }
  });
  es.addEventListener('status', (ev) => {
    try {
      handlers.onStatus?.(JSON.parse((ev as MessageEvent).data) as NowStatus);
    } catch {
      /* ignore */
    }
  });

  return {
    close() {
      es.close();
    },
  };
}
