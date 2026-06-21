// Cost & Tokens view (DESIGN §4 Cost) — money-centric.
// Primary: cumulative spend step line (single --text-primary, 1 line, no gap
// interpolation, minimal axes/grid) + total / burn-rate as mono TEXT (not chart).
// Secondary (demoted, "tokens"): input/output 2 lines (input=primary solid,
// output=secondary 50%). cache NOT a line -> cache_hit_ratio number card + saved $.
// model breakdown = text table with inline single-color proportional bar (no swatch).
// metrics/mixed -> accuracy banner.

import uPlot from 'uplot';
import 'uplot/dist/uPlot.min.css';
import { getCost, type CostResponse, type CostQuery } from '../api.js';
import {
  periodSelector,
  periodToSince,
  getPeriod,
  filterBar,
  filterGroup,
} from '../components/filters.js';
import { el, clear } from '../util/dom.js';
import {
  formatCost,
  formatTokens,
  formatPercent,
  formatAxisTime,
  formatTokensAxis,
  formatCostAxis,
} from '../util/format.js';
import type { ViewHandle } from './now.js';

// Resolve CSS token to a concrete color for the canvas (uPlot needs rgb/hex).
function cssVar(name: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim();
}

export function mountCost(root: HTMLElement): ViewHandle {
  clear(root);
  const view = el('div', { class: 'view', role: 'main' });
  root.appendChild(view);
  view.appendChild(el('h1', { text: 'Cost & Tokens' }));

  const ctrl = new AbortController();
  let destroyed = false;
  let cumPlot: uPlot | null = null;
  let tokPlot: uPlot | null = null;

  // --- filters: shared period + bucket (hour/day) + model ---
  let bucket: 'hour' | 'day' = 'hour';
  let model = ''; // '' = all models
  const knownModels = new Set<string>();

  const modelSelect = el('select', {
    class: 'filter',
    'aria-label': '모델 필터',
  }) as HTMLSelectElement;
  modelSelect.addEventListener('change', () => {
    model = modelSelect.value;
    void load();
  });

  const bucketSeg = el('div', { class: 'seg', role: 'group', 'aria-label': '버킷' });
  const mkBucket = (b: 'hour' | 'day', label: string) => {
    const btn = el('button', {
      type: 'button',
      text: label,
      class: bucket === b ? 'active' : '',
      'aria-pressed': bucket === b,
    }) as HTMLButtonElement;
    btn.addEventListener('click', () => {
      if (bucket === b) return;
      bucket = b;
      for (const c of Array.from(bucketSeg.children)) c.classList.remove('active');
      btn.classList.add('active');
      btn.setAttribute('aria-pressed', 'true');
      void load();
    });
    return btn;
  };
  bucketSeg.append(mkBucket('hour', 'Hour'), mkBucket('day', 'Day'));

  view.appendChild(
    filterBar(
      filterGroup('기간', periodSelector(() => void load())),
      filterGroup('버킷', bucketSeg),
      filterGroup('모델', modelSelect),
    ),
  );

  // Accumulate model names seen across loads so the dropdown stays populated even
  // when a model filter narrows by_model to a single row. Selection is preserved.
  function syncModelOptions(res: CostResponse): void {
    for (const m of res.by_model) if (m.model) knownModels.add(m.model);
    clear(modelSelect);
    modelSelect.appendChild(el('option', { value: '', text: '전체 모델' }));
    for (const m of Array.from(knownModels).sort()) {
      modelSelect.appendChild(el('option', { value: m, text: m }));
    }
    modelSelect.value = model;
  }

  const bannerSlot = el('div');
  // money headline (text, not chart)
  const moneySlot = el('div', { style: 'margin-bottom:16px' });
  // primary cumulative-spend chart
  const cumSlot = el('div', { style: 'min-height:200px;margin-bottom:32px' });
  // cache card
  const cacheSlot = el('div', { style: 'margin-bottom:32px' });
  // demoted tokens chart
  const tokSlot = el('div', { style: 'min-height:220px;margin-bottom:24px' });
  const tableSlot = el('div');
  view.append(bannerSlot, moneySlot, cumSlot, cacheSlot, tokSlot, tableSlot);

  function renderAccuracyBanner(src: CostResponse['token_source']): void {
    clear(bannerSlot);
    if (src === 'metrics' || src === 'mixed') {
      bannerSlot.appendChild(
        el('div', {
          class: 'accuracy-banner',
          text:
            src === 'metrics'
              ? '토큰/비용이 메트릭 기반입니다. 이벤트보다 정밀도가 낮을 수 있습니다.'
              : '일부 세션은 메트릭 기반입니다(mixed). 정밀도가 혼재할 수 있습니다.',
        }),
      );
    }
  }

  // total $ + burn rate as monospace text (no chart). Falls back when the
  // backend has not yet provided the money-centric fields.
  function renderMoneyHeadline(res: CostResponse): void {
    clear(moneySlot);

    // total: prefer total_cost_usd; else last cumulative point; else sum of series.
    let total = res.total_cost_usd;
    if (total === undefined) {
      const last = res.series[res.series.length - 1];
      if (last?.cum_cost_usd !== undefined) total = last.cum_cost_usd;
    }
    if (total === undefined) {
      total = res.series.reduce((a, p) => a + (p.cost_usd || 0), 0);
    }

    moneySlot.append(
      el('div', { class: 'metric-label', text: 'total spend' }),
      el('div', { class: 'metric-cost mono', text: formatCost(total) }),
    );

    // burn rate line: "$X.XX/min · N tok/min", "—/min" when zero/absent.
    const usdMin = res.burn_rate_usd_per_min;
    const tokMin = res.burn_rate_tok_per_min;
    const usdPart =
      usdMin !== undefined && usdMin > 0 ? `${formatCost(usdMin)}/min` : '—/min';
    const tokPart =
      tokMin !== undefined && tokMin > 0
        ? `${formatTokens(tokMin)} tok/min`
        : '— tok/min';
    moneySlot.appendChild(
      el('div', {
        class: 'sec-text mono',
        style: 'margin-top:4px',
        text: `burn rate ${usdPart} · ${tokPart}`,
      }),
    );
  }

  // Primary chart: cumulative spend step line. Single color, 1 line.
  function renderCumChart(res: CostResponse): void {
    clear(cumSlot);
    cumPlot?.destroy();
    cumPlot = null;

    cumSlot.appendChild(el('div', { class: 'metric-label', text: 'cumulative spend' }));

    if (res.series.length === 0) {
      cumSlot.appendChild(
        el('div', {
          class: 'muted',
          style: 'padding:48px 0;text-align:center',
          text: '메트릭 수신 대기 중',
        }),
      );
      return;
    }

    // Build cumulative series. Prefer backend cum_cost_usd; else accumulate cost_usd.
    const xs = res.series.map((p) => p.ts / 1000);
    let running = 0;
    const cum = res.series.map((p) => {
      if (p.cum_cost_usd !== undefined) {
        running = p.cum_cost_usd;
        return p.cum_cost_usd;
      }
      running += p.cost_usd || 0;
      return running;
    });

    const spanMs = res.series[res.series.length - 1]!.ts - res.series[0]!.ts;
    const multiDay = spanMs > 24 * 3600 * 1000;

    const primary = cssVar('--text-primary') || '#1a1a1a';
    const gridColor = cssVar('--border') || '#e2e2e2';
    const axisColor = cssVar('--text-muted') || '#8a8a8a';
    const chartW = cumSlot.clientWidth || 720;

    const opts: uPlot.Options = {
      width: chartW,
      height: 180,
      cursor: { drag: { x: true, y: false } },
      legend: { show: false },
      scales: { x: { time: false } },
      axes: [
        {
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          ticks: { stroke: gridColor },
          values: (_self, splits) =>
            splits.map((v) => formatAxisTime(v * 1000, multiDay)),
        },
        {
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          ticks: { stroke: gridColor },
          values: (_self, splits) => splits.map((v) => formatCostAxis(v)),
        },
      ],
      series: [
        {},
        {
          label: 'cumulative $',
          stroke: primary,
          width: 1,
          points: { show: false },
          spanGaps: false, // gaps = line break, no interpolation
          paths: uPlot.paths.stepped!({ align: 1 }),
        },
      ],
    };

    const data: uPlot.AlignedData = [xs, cum];
    cumPlot = new uPlot(opts, data, cumSlot);
  }

  function renderCacheCard(res: CostResponse): void {
    clear(cacheSlot);
    const card = el('div', { class: 'card' });
    const children: (HTMLElement | false)[] = [
      el('div', { class: 'metric-label', text: 'cache hit ratio' }),
      el('div', { class: 'headline-num', text: formatPercent(res.cache_hit_ratio) }),
      el('div', {
        class: 'caption',
        text: 'cache_read / (input + cache_read)',
      }),
    ];

    // saved-$ subtitle: cache_saved_usd, null/absent -> dash. estimated -> "(est.)".
    const saved = res.cache_saved_usd;
    if (saved !== undefined && saved !== null) {
      const est = res.cache_saved_estimated ? ' (est.)' : '';
      children.push(
        el('div', {
          class: 'sec-text mono',
          style: 'margin-top:4px',
          text: `~${formatCost(saved)} saved${est}`,
        }),
      );
    } else if (saved === null) {
      children.push(
        el('div', {
          class: 'sec-text mono',
          style: 'margin-top:4px',
          text: '— saved',
        }),
      );
    }

    card.append(...children.filter((c): c is HTMLElement => c !== false));
    cacheSlot.appendChild(card);
  }

  // Demoted: input/output token lines under a small "tokens" heading.
  function renderTokenChart(res: CostResponse): void {
    clear(tokSlot);
    tokPlot?.destroy();
    tokPlot = null;

    tokSlot.appendChild(el('h2', { text: 'tokens', style: 'margin-bottom:8px' }));

    if (res.series.length === 0) {
      tokSlot.appendChild(
        el('div', { class: 'muted', text: '데이터 없음' }),
      );
      return;
    }

    const xs = res.series.map((p) => p.ts / 1000);
    const inputs = res.series.map((p) => p.input);
    const outputs = res.series.map((p) => p.output);

    const spanMs = res.series[res.series.length - 1]!.ts - res.series[0]!.ts;
    const multiDay = spanMs > 24 * 3600 * 1000;

    const primary = cssVar('--text-primary') || '#1a1a1a';
    const secondary = cssVar('--text-secondary') || '#5a5a5a';
    const gridColor = cssVar('--border') || '#e2e2e2';
    const axisColor = cssVar('--text-muted') || '#8a8a8a';
    const chartW = tokSlot.clientWidth || 720;

    const opts: uPlot.Options = {
      width: chartW,
      height: 200,
      cursor: { drag: { x: true, y: false } },
      legend: { show: false },
      scales: { x: { time: false } },
      axes: [
        {
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          ticks: { stroke: gridColor },
          values: (_self, splits) =>
            splits.map((v) => formatAxisTime(v * 1000, multiDay)),
        },
        {
          stroke: axisColor,
          grid: { stroke: gridColor, width: 1 },
          ticks: { stroke: gridColor },
          values: (_self, splits) => splits.map((v) => formatTokensAxis(v)),
        },
      ],
      series: [
        {},
        {
          label: 'input',
          stroke: primary,
          width: 1.5,
          points: { show: false },
          spanGaps: false,
        },
        {
          label: 'output',
          stroke: secondary,
          width: 1.5,
          alpha: 0.5,
          points: { show: false },
          spanGaps: false,
        },
      ],
    };

    const data: uPlot.AlignedData = [xs, inputs, outputs];
    tokPlot = new uPlot(opts, data, tokSlot);

    tokSlot.appendChild(
      el('div', { class: 'caption', style: 'margin-top:8px' }, [
        el('span', { text: '— input · ' }),
        el('span', { class: 'secondary', text: '— output' }),
        el('span', { text: '  (Y = tokens)' }),
      ]),
    );
  }

  function renderModelTable(res: CostResponse): void {
    clear(tableSlot);
    tableSlot.appendChild(el('h2', { text: '모델별' }));
    if (res.by_model.length === 0) {
      tableSlot.appendChild(el('div', { class: 'muted', text: '데이터 없음' }));
      return;
    }
    const table = el('table', { class: 'data' });
    const thead = el('thead');
    thead.appendChild(
      el('tr', {}, [
        el('th', { scope: 'col', text: '모델' }),
        el('th', { class: 'num', scope: 'col', text: '총비용' }),
        el('th', { class: 'num', scope: 'col', text: '총토큰' }),
        el('th', { scope: 'col', text: '비율' }),
      ]),
    );
    const tbody = el('tbody');
    for (const m of res.by_model) {
      const pct = Math.max(0, Math.min(1, m.pct));
      const widthPct = pct * 100;
      // inline single-color proportional bar (no swatch). bar width = ratio of a
      // fixed track, so equal ratios render equal widths. % text kept on right.
      const pctCell = el('td', { class: 'pct-cell' }, [
        el('span', { class: 'pct-track', 'aria-hidden': 'true' }, [
          el('span', { class: 'pct-bar', style: `width:${widthPct}%` }),
        ]),
        el('span', { class: 'pct-text mono', text: formatPercent(m.pct) }),
      ]);
      tbody.appendChild(
        el('tr', {}, [
          el('td', { class: 'mono', text: m.model }),
          el('td', { class: 'num', text: formatCost(m.cost_usd) }),
          el('td', { class: 'num', text: formatTokens(m.tokens) }),
          pctCell,
        ]),
      );
    }
    table.append(thead, tbody);
    tableSlot.appendChild(table);
  }

  async function load(): Promise<void> {
    try {
      const q: CostQuery = { bucket };
      const since = periodToSince(getPeriod());
      if (since !== undefined) q.since = since;
      if (model) q.model = model;
      const res = await getCost(q, ctrl.signal);
      if (destroyed) return;
      syncModelOptions(res);
      renderAccuracyBanner(res.token_source);
      renderMoneyHeadline(res);
      renderCumChart(res);
      renderCacheCard(res);
      renderTokenChart(res);
      renderModelTable(res);
    } catch (err) {
      if (destroyed || ctrl.signal.aborted) return;
      clear(cumSlot);
      cumSlot.appendChild(
        el('div', { class: 'error-box', text: '비용 데이터를 불러오지 못했습니다: ' + msg(err) }),
      );
    }
  }

  // resize charts to container
  const onResize = () => {
    if (cumPlot && cumSlot.clientWidth) {
      cumPlot.setSize({ width: cumSlot.clientWidth, height: 180 });
    }
    if (tokPlot && tokSlot.clientWidth) {
      tokPlot.setSize({ width: tokSlot.clientWidth, height: 200 });
    }
  };
  window.addEventListener('resize', onResize);

  void load();

  return {
    destroy() {
      destroyed = true;
      ctrl.abort();
      window.removeEventListener('resize', onResize);
      cumPlot?.destroy();
      tokPlot?.destroy();
      cumPlot = null;
      tokPlot = null;
    },
  };
}

function msg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
