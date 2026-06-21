// Onboarding / empty-state full screen (DESIGN §3 / §4 screen 0).
// Shown on Now & Sessions when no data. Setup snippet + copy + "~30s" copy.

import { el, copyText } from '../util/dom.js';
import type { SetupResponse } from '../api.js';

/** Reusable code block with a copy button. */
export function codeBlock(snippet: string): HTMLElement {
  const block = el('div', { class: 'codeblock' });
  const btn = el('button', {
    class: 'copy-btn',
    type: 'button',
    text: '복사',
    'aria-label': '스니펫 복사',
  });
  btn.addEventListener('click', async () => {
    const ok = await copyText(snippet);
    btn.textContent = ok ? '복사됨' : '복사 실패';
    window.setTimeout(() => {
      btn.textContent = '복사';
    }, 1500);
  });
  block.append(el('pre', { text: snippet }), btn);
  return block;
}

/**
 * Full onboarding view. `setup` may be null while loading; we still render
 * the guidance copy and a placeholder code block.
 */
export function onboardingView(setup: SetupResponse | null): HTMLElement {
  const snippet =
    setup?.snippet ??
    '# Claude Code OTel setup for wlog\n# (loading setup snippet…)';

  const portsLine = setup
    ? `OTLP grpc :${setup.ports.grpc} · http :${setup.ports.http}`
    : '';

  return el('div', { class: 'empty-state fade-in' }, [
    el('h1', { text: '아직 수신된 세션이 없습니다.' }),
    el('p', {
      text: '아래 설정을 Claude Code에서 켜면 텔레메트리가 수신됩니다. OTLP를 활성화하세요.',
    }),
    codeBlock(snippet),
    portsLine ? el('div', { class: 'caption mono', text: portsLine }) : null,
    el('p', { class: 'secondary', text: '첫 API 호출까지 약 30초 정도 걸립니다.' }),
  ]);
}
