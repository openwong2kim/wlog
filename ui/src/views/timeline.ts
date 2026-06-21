// Timeline view (DESIGN §4 Timeline, M2).
// Vertical stack (ts asc). No color coding -> type text labels.
// Indent: tool_* under api_request +16px; 2nd api under tool +16px (max 2 levels,
//   beyond -> chronological fallback). Node click = inline expand.
// logs-off -> "상세 로깅 꺼짐" placeholder. No session -> "세션을 선택하세요".
// Sticky header shows session cost total.

import {
  getTimeline,
  getSession,
  type TimelineEvent,
  type TimelineResponse,
  type SessionDetail,
} from '../api.js';
import { el, clear } from '../util/dom.js';
import {
  formatCost,
  formatDurationMs,
  formatTimelineTime,
} from '../util/format.js';
import type { ViewHandle } from './now.js';

export function mountTimeline(root: HTMLElement, sessionId?: string): ViewHandle {
  clear(root);
  const view = el('div', { class: 'view', role: 'main' });
  root.appendChild(view);
  view.appendChild(el('h1', { text: 'Timeline' }));

  const ctrl = new AbortController();
  let destroyed = false;

  if (!sessionId) {
    view.appendChild(
      el('div', {
        class: 'muted',
        style: 'padding:32px 0',
        text: '세션을 선택하세요',
      }),
    );
    return { destroy() {} };
  }

  const header = el('div', { class: 'tl-header' });
  const body = el('div');
  view.append(header, body);

  function renderHeader(detail: SessionDetail | null): void {
    clear(header);
    header.append(
      el('span', {
        class: 'mono caption',
        text: `session ${sessionId!.slice(0, 8)}`,
      }),
      el('span', {
        class: 'mono',
        style: 'margin-left:16px',
        text: detail ? `total ${formatCost(detail.cost_usd)}` : 'total —',
      }),
    );
  }

  // Indent assignment: walk events ts-asc. api_request resets to base-level
  // anchor; subsequent tool_*/tool_decision get +1 level; a follow-up
  // api_request after a tool gets +1 more (max 2). Beyond 2 -> clamp (fallback).
  function indentLevel(events: TimelineEvent[]): number[] {
    const levels: number[] = [];
    let curApiLevel = 0; // level of most recent api anchor
    let sawToolSinceApi = false;
    for (const ev of events) {
      if (ev.kind === 'api_request' || ev.kind === 'api_error') {
        if (sawToolSinceApi && curApiLevel < 2) {
          curApiLevel = Math.min(curApiLevel + 1, 2);
        } else if (!sawToolSinceApi) {
          // consecutive api at same anchor (no nesting trigger)
        }
        sawToolSinceApi = false;
        levels.push(curApiLevel);
      } else if (ev.kind === 'prompt') {
        curApiLevel = 0;
        sawToolSinceApi = false;
        levels.push(0);
      } else {
        // tool_decision / tool_result -> nested under current api anchor
        levels.push(Math.min(curApiLevel + 1, 2));
        sawToolSinceApi = true;
      }
    }
    return levels;
  }

  function nodeDetail(ev: TimelineEvent): string {
    switch (ev.kind) {
      case 'prompt':
        // Core metric only (prompt_length); the text stays hidden until expand
        // for privacy. Flag the not-stored case so the user knows expanding the
        // row will not reveal any text.
        return ev.prompt == null
          ? `prompt_length ${ev.prompt_length} · 미저장`
          : `prompt_length ${ev.prompt_length}`;
      case 'api_request':
        return `${ev.model} · ${formatCost(ev.cost_usd)} · ${formatDurationMs(ev.duration_ms)}`;
      case 'api_error':
        return `${ev.model} · error ${ev.status_code}`;
      case 'tool_decision':
        return `${ev.tool_name} · ${ev.decision === 'reject' ? '✕ reject' : 'accept'} (${ev.source})`;
      case 'tool_result':
        return `${ev.tool_name} · ${ev.success ? 'ok' : '✕ fail'}`;
    }
  }

  function typeLabel(ev: TimelineEvent): string {
    switch (ev.kind) {
      case 'prompt':
        return 'prompt';
      case 'api_request':
      case 'api_error':
        return 'api';
      case 'tool_decision':
        return 'decision';
      case 'tool_result':
        return 'tool';
    }
  }

  function expandedText(ev: TimelineEvent): string {
    switch (ev.kind) {
      case 'prompt': {
        // Privacy: the prompt text is sensitive (code / business context), so it
        // is never shown in the collapsed row — only here, on explicit expand.
        // prompt is null/absent when stored with --no-store-prompts; show a muted
        // marker (length is still available) rather than an empty block.
        const head = `kind: prompt\nprompt_length: ${ev.prompt_length}`;
        const text = ev.prompt;
        if (text == null) {
          return `${head}\nprompt: (미저장 — --no-store-prompts)`;
        }
        return `${head}\n\n${text}`;
      }
      case 'api_request':
        return [
          `kind: api_request`,
          `model: ${ev.model}`,
          `input: ${ev.input}  output: ${ev.output}`,
          `cache_read: ${ev.cache_read}  cache_creation: ${ev.cache_creation}`,
          `cost: ${formatCost(ev.cost_usd)}  duration: ${formatDurationMs(ev.duration_ms)}`,
        ].join('\n');
      case 'api_error':
        return `kind: api_error\nmodel: ${ev.model}\nstatus_code: ${ev.status_code}\nerror: ${ev.error}`;
      case 'tool_decision':
        return `kind: tool_decision\ntool_name: ${ev.tool_name}\ndecision: ${ev.decision}\nsource: ${ev.source}`;
      case 'tool_result': {
        // logs-off: bash_command null -> "-- 상세 꺼짐"
        const cmd =
          ev.bash_command === null
            ? '-- 상세 꺼짐'
            : ev.bash_command;
        const mcp = ev.mcp_server ?? '—';
        return [
          `kind: tool_result`,
          `tool_name: ${ev.tool_name}`,
          `success: ${ev.success}`,
          `duration: ${formatDurationMs(ev.duration_ms)}`,
          `bash_command: ${cmd}`,
          `mcp_server: ${mcp}`,
        ].join('\n');
      }
    }
  }

  function renderEvents(res: TimelineResponse): void {
    clear(body);

    if (res.meta?.empty_reason === 'logs_disabled') {
      // api_request nodes shown; tool_* slots replaced by placeholder.
      // (Backend still returns the events array; we annotate.)
    }

    if (res.events.length === 0) {
      body.appendChild(
        el('div', {
          class: 'muted',
          style: 'padding:32px 0',
          text:
            res.meta?.empty_reason === 'logs_disabled'
              ? '상세 로깅이 꺼져 있습니다.'
              : '이벤트가 없습니다.',
        }),
      );
      return;
    }

    const levels = indentLevel(res.events);
    res.events.forEach((ev, i) => {
      const level = levels[i] ?? 0;
      const node = el('div', {
        class: 'tl-node',
        style: `padding-left:calc(8px + ${level} * var(--tl-indent))`,
        tabindex: '0',
        role: 'button',
        'aria-expanded': 'false',
        'aria-label': `${typeLabel(ev)} 노드, 확장하려면 Space`,
      });
      const expanded = el('div', {
        class: 'tl-expanded',
        style: `display:none;padding-left:calc(8px + ${level} * var(--tl-indent))`,
      });
      expanded.appendChild(el('pre', { class: 'mono', text: expandedText(ev) }));

      node.append(
        el('span', { class: 'tl-ts', text: formatTimelineTime(ev.ts) }),
        el('span', { class: 'tl-type', text: typeLabel(ev) }),
        el('span', { class: 'tl-detail', text: nodeDetail(ev) }),
      );

      let open = false;
      const toggle = () => {
        open = !open;
        expanded.style.display = open ? 'block' : 'none';
        node.setAttribute('aria-expanded', String(open));
      };
      node.addEventListener('click', toggle);
      node.addEventListener('keydown', (e) => {
        if (e.key === ' ' || e.key === 'Enter') {
          e.preventDefault();
          toggle();
        }
      });

      body.append(node, expanded);
    });
  }

  async function load(): Promise<void> {
    try {
      const [detail, tl] = await Promise.all([
        getSession(sessionId!, ctrl.signal).catch(() => null),
        getTimeline(sessionId!, ctrl.signal),
      ]);
      if (destroyed) return;
      renderHeader(detail);
      renderEvents(tl);
    } catch (err) {
      if (destroyed || ctrl.signal.aborted) return;
      renderHeader(null);
      clear(body);
      body.appendChild(
        el('div', { class: 'error-box', text: '타임라인을 불러오지 못했습니다: ' + msg(err) }),
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

function msg(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}
