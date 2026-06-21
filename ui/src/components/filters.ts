// Shared dashboard filters.
//
// A single period (time-range) selector that every data view (Cost, Sessions,
// Tools, Daily) uses. The choice is persisted in localStorage so it sticks
// across view navigation and reloads — pick "30d" once and the whole dashboard
// is scoped to the last 30 days until you change it. periodToSince() converts
// the selection into the API's `since` epoch-ms (undefined = all time).

import { el } from '../util/dom.js';

export type Period = '7d' | '30d' | '90d' | '1y' | 'all';

const DAY_MS = 86_400_000;
const KEY = 'wlog.period';

const PERIODS: { id: Period; label: string; days: number | null }[] = [
  { id: '7d', label: '7d', days: 7 },
  { id: '30d', label: '30d', days: 30 },
  { id: '90d', label: '90d', days: 90 },
  { id: '1y', label: '1y', days: 365 },
  { id: 'all', label: 'All', days: null },
];

/** Current period from localStorage, defaulting to 30d. */
export function getPeriod(): Period {
  const v = localStorage.getItem(KEY) as Period | null;
  return v && PERIODS.some((p) => p.id === v) ? v : '30d';
}

function setPeriodValue(p: Period): void {
  try {
    localStorage.setItem(KEY, p);
  } catch {
    /* private mode / quota — period just won't persist */
  }
}

/** Days for a period, or null for "all time". */
export function periodDays(p: Period): number | null {
  return PERIODS.find((x) => x.id === p)?.days ?? null;
}

/** `since` epoch-ms for the API, or undefined for "all time". */
export function periodToSince(p: Period): number | undefined {
  const days = periodDays(p);
  if (days == null) return undefined;
  return Date.now() - days * DAY_MS;
}

/**
 * A segmented period selector (7d · 30d · 90d · 1y · All). Persists the choice
 * and calls onChange with the new period. Reuses the `.seg` button styling.
 */
export function periodSelector(onChange: (p: Period) => void): HTMLElement {
  const seg = el('div', { class: 'seg', role: 'group', 'aria-label': '기간 필터' });
  let current = getPeriod();
  const buttons = new Map<Period, HTMLButtonElement>();
  for (const p of PERIODS) {
    const b = el('button', {
      type: 'button',
      text: p.label,
      class: current === p.id ? 'active' : '',
      'aria-pressed': current === p.id,
    }) as HTMLButtonElement;
    b.addEventListener('click', () => {
      if (current === p.id) return;
      current = p.id;
      setPeriodValue(p.id);
      for (const [id, btn] of buttons) {
        const on = id === p.id;
        btn.classList.toggle('active', on);
        btn.setAttribute('aria-pressed', String(on));
      }
      onChange(p.id);
    });
    buttons.set(p.id, b);
    seg.appendChild(b);
  }
  return seg;
}

/**
 * A labeled filter bar wrapper. Pass a label and the controls to lay out in a
 * single horizontal row.
 */
export function filterBar(...controls: (HTMLElement | false | null | undefined)[]): HTMLElement {
  const bar = el('div', { class: 'filter-bar' });
  for (const c of controls) if (c) bar.appendChild(c);
  return bar;
}

/** A small "label: control" group for the filter bar. */
export function filterGroup(label: string, control: HTMLElement): HTMLElement {
  return el('div', { class: 'filter-group' }, [
    el('span', { class: 'caption', text: label }),
    control,
  ]);
}
