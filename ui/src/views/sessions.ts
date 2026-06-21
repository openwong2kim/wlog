// Sessions view (DESIGN §4 Sessions).
// 1st: MM-DD HH:mm · cost · duration · accept-rate (right-aligned numerics)
// 2nd: total tokens (input+output) · model abbrev (secondary size)
// 3rd: session id (8 chars) · source badge.
// sort: last_seen|cost|tokens; row click -> Timeline. Row height 44px.

import {
  getSessions,
  type SessionRow,
  type SessionsSort,
  type SortOrder,
  type SessionsQuery,
} from '../api.js';
import { el, clear } from '../util/dom.js';
import {
  formatCost,
  formatTokens,
  formatDurationSec,
  formatListTime,
  formatPercent,
} from '../util/format.js';
import { onboardingView } from '../components/onboarding.js';
import { getSetup, type SetupResponse } from '../api.js';
import { navigate } from '../router.js';
import { periodSelector, periodToSince, getPeriod } from '../components/filters.js';
import type { ViewHandle } from './now.js';

export function mountSessions(root: HTMLElement): ViewHandle {
  clear(root);
  const view = el('div', { class: 'view', role: 'main' });
  root.appendChild(view);

  const ctrl = new AbortController();
  let destroyed = false;
  let sort: SessionsSort = 'last_seen';
  let order: SortOrder = 'desc';
  let query = '';

  view.appendChild(el('h1', { text: 'Sessions' }));

  const toolbar = el('div', { class: 'toolbar' });
  const search = el('input', {
    type: 'search',
    placeholder: 'ID prefix 또는 날짜',
    'aria-label': '세션 검색',
  }) as HTMLInputElement;
  let searchTimer: number | null = null;
  search.addEventListener('input', () => {
    if (searchTimer != null) window.clearTimeout(searchTimer);
    searchTimer = window.setTimeout(() => {
      query = search.value.trim();
      void load();
    }, 250);
  });
  toolbar.append(el('span', { class: 'caption', text: '검색:' }), search);
  toolbar.append(
    el('span', { class: 'caption', text: '기간:' }),
    periodSelector(() => void load()),
  );
  view.appendChild(toolbar);

  const container = el('div');
  view.appendChild(container);

  function header(label: string, key: SessionsSort, numeric: boolean): HTMLElement {
    const th = el('th', {
      class: (numeric ? 'num ' : '') + 'sortable',
      scope: 'col',
      tabindex: '0',
      role: 'button',
      text: label,
    });
    if (sort === key) {
      th.setAttribute('aria-sort', order === 'asc' ? 'ascending' : 'descending');
    }
    const apply = () => {
      if (sort === key) {
        order = order === 'asc' ? 'desc' : 'asc';
      } else {
        sort = key;
        order = 'desc';
      }
      void load();
    };
    th.addEventListener('click', apply);
    th.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        apply();
      }
    });
    return th;
  }

  function renderRows(rows: SessionRow[]): HTMLElement {
    const table = el('table', { class: 'data' });
    const thead = el('thead');
    const trh = el('tr');
    trh.append(
      header('시각', 'last_seen', false),
      header('비용', 'cost', true),
      el('th', { class: 'num', scope: 'col', text: '소요' }),
      el('th', { class: 'num col-accept-rate', scope: 'col', text: '수락률' }),
      header('토큰', 'tokens', true),
      el('th', { class: 'col-secondary', scope: 'col', text: '모델' }),
      el('th', { scope: 'col', text: 'ID / source' }),
    );
    thead.appendChild(trh);
    const tbody = el('tbody');
    for (const s of rows) {
      const tr = el('tr', {
        class: 'row-link',
        tabindex: '0',
        role: 'button',
        'aria-label': `세션 ${s.id.slice(0, 8)} 타임라인 열기`,
      });
      const go = () => navigate('timeline', { session: s.id });
      tr.addEventListener('click', go);
      tr.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' || e.key === ' ') {
          e.preventDefault();
          go();
        }
      });
      const totalTokens = s.tokens.input + s.tokens.output;
      tr.append(
        el('td', { class: 'mono', text: formatListTime(s.last_seen) }),
        el('td', { class: 'num', text: formatCost(s.cost_usd) }),
        el('td', { class: 'num', text: formatDurationSec(s.duration_sec) }),
        el('td', {
          class: 'num col-accept-rate',
          text: formatPercent(s.tool_accept_rate),
        }),
        el('td', { class: 'num col-secondary', text: formatTokens(totalTokens) }),
        el('td', { class: 'sec-text col-secondary', text: s.model_set || '—' }),
        el('td', {}, [
          el('span', { class: 'mono caption', text: s.id.slice(0, 8) }),
          document.createTextNode(' '),
          el('span', { class: 'badge', text: s.token_source }),
        ]),
      );
      tbody.appendChild(tr);
    }
    table.append(thead, tbody);
    return table;
  }

  async function load(): Promise<void> {
    try {
      const q: SessionsQuery = { sort, order };
      if (query) q.q = query;
      const since = periodToSince(getPeriod());
      if (since !== undefined) q.since = since;
      const res = await getSessions(q, ctrl.signal);
      if (destroyed) return;
      clear(container);
      if (res.sessions.length === 0) {
        // Empty -> onboarding (no_data) unless server says otherwise.
        let setup: SetupResponse | null = null;
        try {
          setup = await getSetup(ctrl.signal);
        } catch {
          setup = null;
        }
        if (destroyed) return;
        container.appendChild(onboardingView(setup));
        return;
      }
      container.appendChild(renderRows(res.sessions));
    } catch (err) {
      if (destroyed || ctrl.signal.aborted) return;
      clear(container);
      container.appendChild(
        el('div', { class: 'error-box', text: '세션을 불러오지 못했습니다: ' + msg(err) }),
      );
    }
  }

  void load();

  return {
    destroy() {
      destroyed = true;
      if (searchTimer != null) window.clearTimeout(searchTimer);
      ctrl.abort();
    },
  };
}

function msg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
