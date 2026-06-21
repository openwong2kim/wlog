// Privacy banners (DESIGN §8, 3 stages):
//  1. logs off (default): muted 1-line in Now tail, "[켜는 법 보기]" inline expand.
//  2. logs on: one-time amber notice; recorded in localStorage afterward.
//  3. non-local bind: persistent red, top of app, NOT dismissible.

import { el } from '../util/dom.js';

const AMBER_SEEN_KEY = 'wlog.privacy.amberSeen';

/** Stage 3: persistent red banner. Returns null if bind is local/safe. */
export function persistentBindBanner(nonLocal: boolean, auth: boolean): HTMLElement | null {
  if (!nonLocal) return null;
  return el('div', { class: 'banner-danger', role: 'alert' }, [
    el('span', { text: 'wlog가 외부 네트워크에 열려 있습니다.' }),
    el('span', { class: 'mono', text: `[인증=${auth ? 'on' : 'off'}]` }),
  ]);
}

/**
 * Stage 1: logs-off muted notice for the Now tail.
 * setupSnippet is shown inline when the user clicks "[켜는 법 보기]".
 */
export function logsOffNotice(setupSnippet: string): HTMLElement {
  const wrap = el('div', { class: 'notice-muted' });
  const expand = el('div', { class: 'codeblock', style: 'display:none' }, [
    el('pre', { text: setupSnippet }),
  ]);
  const link = el('button', {
    type: 'button',
    text: '켜는 법 보기',
    'aria-expanded': 'false',
  });
  let open = false;
  link.addEventListener('click', () => {
    open = !open;
    expand.style.display = open ? 'block' : 'none';
    link.setAttribute('aria-expanded', String(open));
  });
  wrap.append(
    document.createTextNode('상세 이벤트 수집이 꺼져 있습니다. '),
    link,
    expand,
  );
  return wrap;
}

/**
 * Stage 2: one-time amber notice shown after first data arrives with logs on.
 * Returns null if already acknowledged (localStorage). Click [닫기] persists.
 */
export function oneTimeAmberNotice(): HTMLElement | null {
  let seen = false;
  try {
    seen = localStorage.getItem(AMBER_SEEN_KEY) === '1';
  } catch {
    /* storage may be unavailable */
  }
  if (seen) return null;

  const banner = el('div', { class: 'notice-amber fade-in', role: 'status' });
  const close = el('button', { type: 'button', text: '닫기' });
  close.addEventListener('click', () => {
    try {
      localStorage.setItem(AMBER_SEEN_KEY, '1');
    } catch {
      /* ignore */
    }
    banner.remove();
  });
  banner.append(
    el('span', {
      text: '⚠ 툴 인자·프롬프트가 로컬 DB에 저장됩니다. 공용 머신 주의.',
    }),
    close,
  );
  return banner;
}
