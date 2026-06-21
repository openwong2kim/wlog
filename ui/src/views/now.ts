// Now view (DESIGN §4 Now). Top 40% summary card (cost 20px mono max emphasis),
// mid 20% active tool 1-line, bottom 40% live tail (SSE, max 50 rows,
// new rows fade-in 150ms, auto-scroll pauses when user scrolls up).
// tick display throttled to 2s. No count-up / spinner / blink.

import {
  connectNow,
  getSetup,
  getHealth,
  type NowConnection,
  type NowTick,
  type NowLog,
  type SetupResponse,
} from '../api.js';
import { el, clear } from '../util/dom.js';
import {
  formatCost,
  formatTokens,
  formatDurationSec,
  formatTimelineTime,
} from '../util/format.js';
import { onboardingView } from '../components/onboarding.js';
import { logsOffNotice, oneTimeAmberNotice } from '../components/privacy.js';

const MAX_TAIL = 50;
const TICK_THROTTLE_MS = 2000;
// Rolling window for client-side burn-rate (Δcost/Δt, Δtok/Δt). ~60s smooths
// the 2s tick cadence so the rate doesn't jitter on instantaneous deltas.
const BURN_WINDOW_MS = 60_000;
// Samples older than this without a fresh tick -> treat as idle, show "—/min".
const BURN_STALE_MS = 15_000;

interface BurnSample {
  ts: number; // tick wall-clock arrival (ms)
  cost: number; // session cumulative cost at sample
  tokens: number; // session cumulative input+output at sample
}

export interface ViewHandle {
  destroy(): void;
}

export function mountNow(root: HTMLElement): ViewHandle {
  clear(root);
  const view = el('div', { class: 'view', role: 'main' });
  root.appendChild(view);

  let conn: NowConnection | null = null;
  let destroyed = false;
  let pendingTick: NowTick | null = null;
  let throttleTimer: number | null = null;
  let lastApplied = 0;
  let setup: SetupResponse | null = null;
  let logsEnabled = true;
  let receivedAnyData = false;
  let autoScroll = true;
  let amberShown = false;
  // burn-rate rolling window + the session id it belongs to (reset on switch).
  let burnSamples: BurnSample[] = [];
  let burnSessionId: string | null = null;

  // ---- DOM scaffold ----
  const header = el('div', {
    style:
      'display:flex;justify-content:space-between;align-items:baseline;margin-bottom:16px',
  });
  const h1 = el('h1', { text: 'Now', style: 'margin:0' });
  const liveLabel = el('span', { class: 'live-label', text: 'CONNECTING' });
  header.append(h1, liveLabel);

  const summary = el('div', { class: 'card', style: 'margin-bottom:16px' });
  const costEl = el('div', { class: 'metric-cost mono', text: '$0.0000' });
  const burnEl = el('div', {
    class: 'mono secondary',
    style: 'font-size:14px;margin-top:2px',
    text: '—/min',
  });
  const tokensEl = el('div', { class: 'sec-text', text: '' });
  const activeTimeEl = el('div', { class: 'caption mono', text: '' });
  summary.append(
    el('div', { class: 'metric-label', text: 'session cost' }),
    costEl,
    burnEl,
    tokensEl,
    activeTimeEl,
  );

  const toolLine = el('div', {
    class: 'mono secondary',
    style: 'margin-bottom:16px',
    text: 'idle',
  });

  const tailWrap = el('div');
  const tailHeader = el('div'); // privacy notice / amber slot
  const tail = el('div', {
    class: 'tail',
    role: 'log',
    'aria-live': 'polite',
    'aria-atomic': 'false',
    'aria-label': '라이브 이벤트 테일',
  });
  tailWrap.append(
    el('div', { class: 'metric-label', text: 'live events' }),
    tailHeader,
    tail,
  );

  // onboarding container (shown when no data and not logs-off)
  const onboardSlot = el('div');

  view.append(header, summary, toolLine, tailWrap, onboardSlot);

  // pause auto-scroll when user scrolls away from bottom
  tail.addEventListener('scroll', () => {
    const nearBottom =
      tail.scrollHeight - tail.scrollTop - tail.clientHeight < 24;
    autoScroll = nearBottom;
  });

  function setLive(state: 'live' | 'reconnecting' | 'connecting'): void {
    if (state === 'live') {
      liveLabel.textContent = 'LIVE';
      liveLabel.className = 'live-label';
    } else if (state === 'reconnecting') {
      liveLabel.textContent = 'RECONNECTING';
      liveLabel.className = 'live-label reconnecting';
    } else {
      liveLabel.textContent = 'CONNECTING';
      liveLabel.className = 'live-label reconnecting';
    }
  }

  // Record a tick into the rolling window; reset when the active session
  // changes so cross-session deltas never leak into the rate.
  function sampleBurn(t: NowTick): void {
    if (t.session_id == null) return;
    if (t.session_id !== burnSessionId) {
      burnSessionId = t.session_id;
      burnSamples = [];
    }
    const now = Date.now();
    burnSamples.push({
      ts: now,
      cost: t.cost_usd,
      tokens: t.tokens.input + t.tokens.output,
    });
    // drop samples outside the window
    const cutoff = now - BURN_WINDOW_MS;
    while (burnSamples.length > 1 && burnSamples[0]!.ts < cutoff) {
      burnSamples.shift();
    }
  }

  // Window-averaged Δcost/Δt and Δtok/Δt -> "$X.XX/min · N tok/min".
  // Returns "—/min" text when data is insufficient or stale (idle).
  function burnText(): string {
    if (burnSamples.length < 2) return '—/min · — tok/min';
    const first = burnSamples[0]!;
    const last = burnSamples[burnSamples.length - 1]!;
    const dtMs = last.ts - first.ts;
    const stale = Date.now() - last.ts > BURN_STALE_MS;
    if (dtMs <= 0 || stale) return '—/min · — tok/min';
    const minutes = dtMs / 60_000;
    const dCost = Math.max(0, last.cost - first.cost);
    const dTok = Math.max(0, last.tokens - first.tokens);
    const usdPerMin = dCost / minutes;
    const tokPerMin = dTok / minutes;
    const usdPart = usdPerMin > 0 ? `${formatCost(usdPerMin)}/min` : '—/min';
    const tokPart = tokPerMin > 0 ? `${formatTokens(tokPerMin)} tok/min` : '— tok/min';
    return `${usdPart} · ${tokPart}`;
  }

  function applyTick(t: NowTick): void {
    if (t.session_id == null) {
      // no active session
      costEl.textContent = '$0.0000';
      burnEl.textContent = '—/min · — tok/min';
      tokensEl.textContent = '';
      activeTimeEl.textContent = '';
      toolLine.textContent = '현재 활성 세션이 없습니다 (idle)';
      burnSamples = [];
      burnSessionId = null;
      return;
    }
    receivedAnyData = true;
    hideOnboarding();
    // immediate replacement, no transition / interpolation
    costEl.textContent = formatCost(t.cost_usd);
    burnEl.textContent = burnText();
    tokensEl.textContent =
      `in ${formatTokens(t.tokens.input)} · out ${formatTokens(t.tokens.output)}`;
    activeTimeEl.textContent = `active ${formatDurationSec(t.active_time_sec)}`;
    toolLine.textContent = t.active_tool ? `${t.active_tool} running…` : 'idle';
    maybeShowAmber();
  }

  function scheduleTick(t: NowTick): void {
    // Sample on every raw tick (denser window than the 2s display throttle).
    sampleBurn(t);
    pendingTick = t;
    const now = Date.now();
    const elapsed = now - lastApplied;
    if (elapsed >= TICK_THROTTLE_MS) {
      lastApplied = now;
      applyTick(t);
      pendingTick = null;
    } else if (throttleTimer == null) {
      throttleTimer = window.setTimeout(
        () => {
          throttleTimer = null;
          if (pendingTick) {
            lastApplied = Date.now();
            applyTick(pendingTick);
            pendingTick = null;
          }
        },
        TICK_THROTTLE_MS - elapsed,
      );
    }
  }

  // Error = api_error kind OR a tool_result whose success flag is false.
  // (NowLog carries arbitrary extra fields via its index signature.)
  function isErrorLog(l: NowLog): boolean {
    if (l.kind === 'api_error') return true;
    if (l.kind === 'tool_result' && l.success === false) return true;
    return false;
  }

  function appendLog(l: NowLog): void {
    receivedAnyData = true;
    hideOnboarding();
    const ts = formatTimelineTime(l.ts);
    const err = isErrorLog(l);
    // DESIGN §5: color + symbol together (never color alone). Prefix ✕ and
    // amber --reject so failures are visually distinct from success.
    const line = el('div', {
      class: 'tail-line fade-in' + (err ? ' reject' : ''),
      text: err ? `${ts}  ✕ ${l.summary}` : `${ts}  ${l.summary}`,
    });
    tail.appendChild(line);
    while (tail.childElementCount > MAX_TAIL) {
      tail.firstElementChild?.remove();
    }
    if (autoScroll) tail.scrollTop = tail.scrollHeight;
    maybeShowAmber();
  }

  function appendDropMarker(dropped: number): void {
    const line = el('div', {
      class: 'tail-line muted fade-in',
      text: `— ${dropped} dropped —`,
    });
    tail.appendChild(line);
    while (tail.childElementCount > MAX_TAIL) {
      tail.firstElementChild?.remove();
    }
    if (autoScroll) tail.scrollTop = tail.scrollHeight;
  }

  function maybeShowAmber(): void {
    if (amberShown || !logsEnabled) return;
    amberShown = true;
    const banner = oneTimeAmberNotice();
    if (banner) tailHeader.prepend(banner);
  }

  function showLogsOffNotice(): void {
    clear(tailHeader);
    tailHeader.appendChild(logsOffNotice(setup?.snippet ?? ''));
    tail.appendChild(
      el('div', {
        class: 'tail-line muted',
        text: '상세 이벤트 수집이 꺼져 있습니다.',
      }),
    );
  }

  function showOnboarding(): void {
    summary.style.display = 'none';
    toolLine.style.display = 'none';
    tailWrap.style.display = 'none';
    clear(onboardSlot);
    onboardSlot.appendChild(onboardingView(setup));
  }
  function hideOnboarding(): void {
    if (onboardSlot.childElementCount === 0) return;
    summary.style.display = '';
    toolLine.style.display = '';
    tailWrap.style.display = '';
    clear(onboardSlot);
  }

  // ---- async init ----
  void (async () => {
    try {
      setup = await getSetup();
    } catch {
      setup = null;
    }
    if (destroyed) return;

    try {
      const health = await getHealth();
      logsEnabled = health.logs_enabled !== false;
    } catch {
      /* health optional for banners */
    }
    if (destroyed) return;

    // If logs are disabled, show the stage-1 muted notice in the tail.
    if (!logsEnabled) showLogsOffNotice();

    // Connect SSE.
    setLive('connecting');
    conn = connectNow({
      onOpen: () => setLive('live'),
      onError: () => setLive('reconnecting'),
      onStatus: (s) => setLive(s.state),
      onTick: (t) => {
        setLive('live');
        scheduleTick(t);
      },
      onLog: (l) => appendLog(l),
      onDrop: (d) => appendDropMarker(d.dropped),
    });

    // If after a short grace period we have no data, surface onboarding.
    window.setTimeout(() => {
      if (!destroyed && !receivedAnyData) showOnboarding();
    }, 1500);
  })();

  return {
    destroy() {
      destroyed = true;
      if (throttleTimer != null) window.clearTimeout(throttleTimer);
      conn?.close();
    },
  };
}
