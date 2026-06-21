// Daily usage view — a GitHub contribution-graph ("잔디") heatmap of daily
// token / cost usage. This is the codex `/usage daily` analogue: one square per
// calendar day, color-stepped by how much was spent that day.
//
// Data source: the existing /api/cost series, fetched at HOUR granularity and
// re-bucketed into LOCAL calendar days here. We deliberately do NOT use the
// backend's bucket=day, because that truncates on UTC midnight — for a +09:00
// user every day would be shifted 9h and land on the wrong square. Re-folding
// hour buckets with the browser's local Date keeps each square on the user's
// own wall-clock day.
//
// Pure DOM, no chart lib: a 7-row × 53-col CSS grid (column = week, row =
// weekday, Sun..Sat), each cell shaded by quartile. A token/cost toggle re-
// shades without refetching. Matches the minimal, text-centric DESIGN palette.

import { getCost, type CostResponse } from '../api.js';
import { el, clear } from '../util/dom.js';
import { formatCost, formatTokens } from '../util/format.js';
import {
  periodSelector,
  periodDays,
  getPeriod,
  filterBar,
  filterGroup,
} from '../components/filters.js';
import type { ViewHandle } from './now.js';

// The grid width adapts to the shared period filter: up to 53 weeks (≈1 year,
// the GitHub width) for 1y/All, down to MIN_WEEKS so a short window still reads
// as a heatmap rather than a sliver. DAY_MS is used only for stepping the cursor
// date; each step is re-normalized to local midnight so DST shifts (a 23/25h
// local day) never drift the square.
const MAX_WEEKS = 53;
const MIN_WEEKS = 4;
const DAY_MS = 86_400_000;

// weeksForPeriod maps the shared period to a heatmap column count.
function weeksForPeriod(): number {
  const d = periodDays(getPeriod()); // null = all time
  if (d == null) return MAX_WEEKS;
  // +1 week of context, clamped to [MIN_WEEKS, MAX_WEEKS].
  return Math.min(MAX_WEEKS, Math.max(MIN_WEEKS, Math.ceil(d / 7) + 1));
}

type Metric = 'tokens' | 'cost';

interface DayAgg {
  tokens: number; // input + output (the work done that day)
  cost: number; // USD
  cacheRead: number; // surfaced in the tooltip, not used for shading
}

interface DayCell extends DayAgg {
  date: Date;
  key: string; // local YYYY-MM-DD
  future: boolean; // beyond today -> rendered empty/transparent
}

const WEEKDAY_LABELS = ['', 'Mon', '', 'Wed', '', 'Fri', '']; // rows Sun..Sat
const MONTHS = [
  'Jan', 'Feb', 'Mar', 'Apr', 'May', 'Jun',
  'Jul', 'Aug', 'Sep', 'Oct', 'Nov', 'Dec',
];

export function mountDaily(root: HTMLElement): ViewHandle {
  clear(root);
  const view = el('div', { class: 'view', role: 'main' });
  root.appendChild(view);
  view.appendChild(el('h1', { text: 'Daily usage' }));

  const ctrl = new AbortController();
  let destroyed = false;

  // metric toggle state (token intensity by default — "토큰을 얼마나 썼는지")
  let metric: Metric = 'tokens';
  let cells: DayCell[] = [];
  let loaded = false;

  const filterSlot = el('div');
  const headlineSlot = el('div', { style: 'margin-bottom:16px' });
  const toggleSlot = el('div', { style: 'margin-bottom:12px' });
  const heatmapSlot = el('div');
  const legendSlot = el('div');
  view.append(filterSlot, headlineSlot, toggleSlot, heatmapSlot, legendSlot);

  filterSlot.appendChild(
    filterBar(filterGroup('기간', periodSelector(() => void load()))),
  );

  // ---- metric toggle (Tokens | Cost) ----
  function renderToggle(): void {
    clear(toggleSlot);
    const seg = el('div', { class: 'seg', role: 'group', 'aria-label': '색상 기준' });
    const mk = (m: Metric, label: string) => {
      const b = el('button', {
        type: 'button',
        text: label,
        class: metric === m ? 'active' : '',
        'aria-pressed': metric === m,
      });
      b.addEventListener('click', () => {
        if (metric === m) return;
        metric = m;
        renderToggle();
        renderHeatmap();
        renderLegend();
      });
      return b;
    };
    seg.append(mk('tokens', 'Tokens'), mk('cost', 'Cost'));
    toggleSlot.appendChild(seg);
  }

  function valueOf(c: DayAgg): number {
    return metric === 'tokens' ? c.tokens : c.cost;
  }

  function fmtValue(v: number): string {
    return metric === 'tokens' ? formatTokens(v) + ' tokens' : formatCost(v);
  }

  // ---- summary headline ----
  function renderHeadline(): void {
    clear(headlineSlot);
    if (!loaded) return;

    let totalTokens = 0;
    let totalCost = 0;
    let activeDays = 0;
    let busiest: DayCell | null = null;
    for (const c of cells) {
      if (c.future) continue;
      totalTokens += c.tokens;
      totalCost += c.cost;
      if (c.tokens > 0 || c.cost > 0) activeDays++;
      if (!busiest || valueOf(c) > valueOf(busiest)) busiest = c;
    }

    const block = (label: string, value: string) =>
      el('div', { class: 'metric-block' }, [
        el('div', { class: 'metric-label', text: label }),
        el('div', { class: 'metric-value', text: value }),
      ]);

    const row = el('div', { class: 'metric-row' }, [
      block('total tokens', formatTokens(totalTokens)),
      block('total spend', formatCost(totalCost)),
      block('active days', String(activeDays)),
      block(
        'busiest day',
        busiest && valueOf(busiest) > 0
          ? `${busiest.key} · ${fmtValue(valueOf(busiest))}`
          : '—',
      ),
    ]);
    headlineSlot.appendChild(row);
  }

  // ---- the heatmap grid ----
  function renderHeatmap(): void {
    clear(heatmapSlot);

    if (loaded && cells.every((c) => c.tokens === 0 && c.cost === 0)) {
      heatmapSlot.appendChild(
        el('div', {
          class: 'muted',
          style: 'padding:32px 0',
          text: '아직 사용 기록이 없습니다. Claude Code 세션을 실행하면 채워집니다.',
        }),
      );
      return;
    }
    if (!loaded) {
      heatmapSlot.appendChild(
        el('div', { class: 'muted', style: 'padding:32px 0', text: '불러오는 중…' }),
      );
      return;
    }

    // quartile thresholds over the active (>0) day values for the current metric
    const th = quartiles(cells.filter((c) => !c.future).map(valueOf));

    const wrap = el('div', { class: 'hm-wrap' });

    // month labels: one slot per week column; label when the column's first
    // (Sunday) cell starts a new month vs the previous column.
    const months = el('div', { class: 'hm-months', 'aria-hidden': 'true' });
    let prevMonth = -1;
    const cols = cells.length / 7;
    for (let col = 0; col < cols; col++) {
      const first = cells[col * 7];
      let label = '';
      if (first) {
        const m = first.date.getMonth();
        if (m !== prevMonth) {
          label = MONTHS[m]!;
          prevMonth = m;
        }
      }
      months.appendChild(el('span', { text: label }));
    }

    const body = el('div', { class: 'hm-body' });

    // weekday labels (Mon/Wed/Fri), aligned to grid rows Sun..Sat
    const days = el('div', { class: 'hm-days', 'aria-hidden': 'true' });
    for (const lbl of WEEKDAY_LABELS) days.appendChild(el('span', { text: lbl }));

    // the cells, column-major (grid-auto-flow: column). cells[] is already in
    // i = col*7 + row order, matching the auto-flow fill.
    const grid = el('div', {
      class: 'hm-grid',
      role: 'img',
      'aria-label': `일별 ${metric === 'tokens' ? '토큰' : '비용'} 사용량 히트맵`,
    });
    for (const c of cells) {
      const attrs: Record<string, string> = { class: c.future ? 'hm-cell future' : 'hm-cell' };
      if (!c.future) {
        const lvl = levelOf(valueOf(c), th);
        attrs['data-level'] = String(lvl);
        const v = valueOf(c);
        attrs['title'] =
          v > 0 ? `${c.key} · ${fmtValue(v)}` : `${c.key} · 사용 없음`;
      }
      grid.appendChild(el('div', attrs));
    }

    body.append(days, grid);
    wrap.append(months, body);
    heatmapSlot.appendChild(wrap);
  }

  // ---- legend (Less ▢▢▢▢▢ More) ----
  function renderLegend(): void {
    clear(legendSlot);
    if (!loaded) return;
    const legend = el('div', { class: 'hm-legend' });
    legend.appendChild(el('span', { text: 'Less' }));
    for (let lvl = 0; lvl <= 4; lvl++) {
      legend.appendChild(el('div', { class: 'hm-cell', 'data-level': String(lvl) }));
    }
    legend.appendChild(el('span', { text: 'More' }));
    legendSlot.appendChild(legend);
  }

  async function load(): Promise<void> {
    // fetch ~ the grid window + a little slack; hour buckets so we can fold to
    // local days. The window width follows the shared period filter.
    const weeks = weeksForPeriod();
    const since = Date.now() - (weeks * 7 + 7) * DAY_MS;
    try {
      const res = await getCost({ bucket: 'hour', since }, ctrl.signal);
      if (destroyed) return;
      cells = buildCells(res, weeks);
      loaded = true;
      renderHeadline();
      renderHeatmap();
      renderLegend();
    } catch (err) {
      if (destroyed || ctrl.signal.aborted) return;
      clear(heatmapSlot);
      heatmapSlot.appendChild(
        el('div', {
          class: 'error-box',
          text: '사용량 데이터를 불러오지 못했습니다: ' + msg(err),
        }),
      );
    }
  }

  renderToggle();
  renderHeatmap(); // shows the loading placeholder
  void load();

  return {
    destroy() {
      destroyed = true;
      ctrl.abort();
    },
  };
}

// buildCells folds the hour-bucketed series into local-day aggregates, then lays
// out a `weeks`-wide grid ending on the column containing today.
function buildCells(res: CostResponse, weeks: number): DayCell[] {
  const byDay = new Map<string, DayAgg>();
  for (const p of res.series) {
    const key = localYMD(new Date(p.ts));
    const cur = byDay.get(key) ?? { tokens: 0, cost: 0, cacheRead: 0 };
    cur.tokens += (p.input || 0) + (p.output || 0);
    cur.cost += p.cost_usd || 0;
    cur.cacheRead += p.cache_read || 0;
    byDay.set(key, cur);
  }

  const today = new Date();
  today.setHours(0, 0, 0, 0);
  const dow = today.getDay(); // 0 = Sunday
  // first cell = the Sunday of the leftmost column. The grid is laid out
  // column-major (col*7 + row), so cell i steps i days forward from there.
  const firstOffset = (weeks - 1) * 7 + dow;

  const cells: DayCell[] = [];
  for (let i = 0; i < weeks * 7; i++) {
    const d = new Date(today.getTime() - (firstOffset - i) * DAY_MS);
    d.setHours(0, 0, 0, 0); // re-normalize against DST drift
    const key = localYMD(d);
    const agg = byDay.get(key) ?? { tokens: 0, cost: 0, cacheRead: 0 };
    cells.push({
      date: d,
      key,
      tokens: agg.tokens,
      cost: agg.cost,
      cacheRead: agg.cacheRead,
      future: d.getTime() > today.getTime(),
    });
  }
  return cells;
}

// quartiles returns the [q25, q50, q75, max] boundaries over the positive
// values. All zeros (or empty) yields zeros, so levelOf maps everything to 0.
function quartiles(values: number[]): [number, number, number, number] {
  const pos = values.filter((v) => v > 0).sort((a, b) => a - b);
  if (pos.length === 0) return [0, 0, 0, 0];
  const q = (p: number) => pos[Math.min(pos.length - 1, Math.floor(p * pos.length))]!;
  return [q(0.25), q(0.5), q(0.75), pos[pos.length - 1]!];
}

// levelOf maps a value to 0..4 using the quartile thresholds (0 -> level 0).
function levelOf(v: number, th: [number, number, number, number]): number {
  if (v <= 0) return 0;
  if (v <= th[0]) return 1;
  if (v <= th[1]) return 2;
  if (v <= th[2]) return 3;
  return 4;
}

function localYMD(d: Date): string {
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}`;
}

function pad2(n: number): string {
  return n < 10 ? '0' + n : String(n);
}

function msg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
