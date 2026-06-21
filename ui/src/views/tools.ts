// Tools (Tool Decisions) view (DESIGN §4 Tools). Page h1 = "Tool Decisions".
// Top headline: "auto-approved: N". Then accept/reject + reject-rate.
// source table (no bars). hotspots top5 with optional single-color bar.
// hotspot click -> Timeline drilldown (session filter when applicable).

import { getTools, type ToolsResponse, type ToolsQuery } from '../api.js';
import { el, clear } from '../util/dom.js';
import { formatPercent } from '../util/format.js';
import { navigate } from '../router.js';
import {
  periodSelector,
  periodToSince,
  getPeriod,
  filterBar,
  filterGroup,
} from '../components/filters.js';
import type { ViewHandle } from './now.js';

export function mountTools(root: HTMLElement, sessionId?: string): ViewHandle {
  clear(root);
  const view = el('div', { class: 'view', role: 'main' });
  root.appendChild(view);
  view.appendChild(el('h1', { text: 'Tool Decisions' }));

  const ctrl = new AbortController();
  let destroyed = false;

  view.appendChild(
    filterBar(filterGroup('기간', periodSelector(() => void load()))),
  );

  const container = el('div');
  view.appendChild(container);

  function render(res: ToolsResponse): void {
    clear(container);

    const total = res.accept + res.reject;
    if (total === 0 && res.auto_approved === 0) {
      container.appendChild(
        el('div', {
          class: 'muted',
          style: 'padding:32px 0',
          text: 'tool_decision 이벤트가 없습니다. 로그 수신을 확인하세요.',
        }),
      );
      return;
    }

    // headline
    const headline = el('div', { class: 'card', style: 'margin-bottom:16px' });
    headline.append(
      el('div', { class: 'metric-label', text: 'auto-approved' }),
      el('div', { class: 'headline-num', text: String(res.auto_approved) }),
    );
    container.appendChild(headline);

    // accept / reject / reject-rate
    const stats = el('div', { class: 'metric-row', style: 'margin-bottom:24px' });
    stats.append(
      metricBlock('accept', String(res.accept)),
      metricBlock('reject', `✕ ${res.reject}`, true),
      metricBlock('reject rate', formatPercent(res.reject_rate)),
    );
    container.appendChild(stats);

    // by-source table
    container.appendChild(el('h2', { text: 'source별' }));
    const table = el('table', { class: 'data' });
    table.appendChild(
      el('thead', {}, [
        el('tr', {}, [
          el('th', { scope: 'col', text: 'source' }),
          el('th', { class: 'num', scope: 'col', text: 'accept' }),
          el('th', { class: 'num', scope: 'col', text: 'reject' }),
        ]),
      ]),
    );
    const tbody = el('tbody');
    for (const s of res.by_source) {
      tbody.appendChild(
        el('tr', {}, [
          el('td', { class: 'mono', text: s.source }),
          el('td', { class: 'num', text: String(s.accept) }),
          el('td', {
            class: 'num' + (s.reject > 0 ? ' reject' : ''),
            text: s.reject > 0 ? `✕ ${s.reject}` : '0',
          }),
        ]),
      );
    }
    table.appendChild(tbody);
    container.appendChild(table);

    // hotspots top5
    container.appendChild(
      el('h2', { text: '거절 핫스팟 top5', style: 'margin-top:24px' }),
    );
    // Sort by reject desc; ties broken by tool_name asc (stable, deterministic).
    const sorted = res.hotspots
      .filter((h) => h.reject > 0)
      .slice()
      .sort((a, b) =>
        b.reject - a.reject || a.tool_name.localeCompare(b.tool_name),
      );
    const top = sorted.slice(0, 5);
    if (top.length === 0) {
      container.appendChild(el('div', { class: 'muted', text: '거절 없음' }));
    } else {
      // Bar width strictly proportional to reject count: width = reject/max * 100%
      // inside a fixed-width track, so equal counts render equal widths.
      const maxReject = Math.max(...top.map((h) => h.reject), 1);
      const list = el('div');
      top.forEach((h, i) => {
        const widthPct = (h.reject / maxReject) * 100;
        const row = el('div', {
          class: 'hotspot',
          tabindex: '0',
          role: 'button',
          'aria-label': `${h.tool_name} 거절 ${h.reject}건 타임라인 보기`,
        });
        const go = () =>
          sessionId
            ? navigate('timeline', { session: sessionId })
            : navigate('timeline');
        row.addEventListener('click', go);
        row.addEventListener('keydown', (e) => {
          if (e.key === 'Enter' || e.key === ' ') {
            e.preventDefault();
            go();
          }
        });
        // fixed-width track holds the proportional bar + right-aligned count.
        const track = el('span', { class: 'bar-track' }, [
          el('span', {
            class: 'bar',
            style: `width:${widthPct}%`,
            'aria-hidden': 'true',
          }),
          el('span', {
            class: 'mono reject caption bar-count',
            text: String(h.reject),
          }),
        ]);
        row.append(
          el('span', {
            class: 'mono caption',
            style: 'width:16px;flex-shrink:0',
            text: `${i + 1}.`,
          }),
          el('span', { class: 'label', text: h.tool_name }),
          track,
        );
        list.appendChild(row);
      });
      container.appendChild(list);
    }
  }

  async function load(): Promise<void> {
    try {
      const q: ToolsQuery = {};
      if (sessionId) q.session = sessionId;
      const since = periodToSince(getPeriod());
      if (since !== undefined) q.since = since;
      const res = await getTools(q, ctrl.signal);
      if (destroyed) return;
      render(res);
    } catch (err) {
      if (destroyed || ctrl.signal.aborted) return;
      clear(container);
      container.appendChild(
        el('div', { class: 'error-box', text: 'Tools 데이터를 불러오지 못했습니다: ' + msg(err) }),
      );
    }
  }

  void load();

  return {
    destroy() {
      destroyed = true;
      ctrl.abort();
    },
  };
}

function metricBlock(label: string, value: string, reject = false): HTMLElement {
  return el('div', { class: 'metric-block' }, [
    el('span', { class: 'metric-label', text: label }),
    el('span', {
      class: 'metric-value' + (reject ? ' reject' : ''),
      text: value,
    }),
  ]);
}

function msg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
