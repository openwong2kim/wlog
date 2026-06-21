// App shell: persistent banner + side nav + routed main. Hash router.
// nav order: Now · Sessions · Cost · Tools · Timeline.

import './styles.css';
import { el, clear } from './util/dom.js';
import {
  ROUTES,
  currentRoute,
  onRouteChange,
  navigate,
  type Route,
} from './router.js';
import { getHealth } from './api.js';
import { persistentBindBanner } from './components/privacy.js';
import { mountNow, type ViewHandle } from './views/now.js';
import { mountSessions } from './views/sessions.js';
import { mountCost } from './views/cost.js';
import { mountDaily } from './views/daily.js';
import { mountTools } from './views/tools.js';
import { mountTimeline } from './views/timeline.js';

const app = document.getElementById('app');
if (!app) throw new Error('#app missing');

// ---- shell ----
const bannerSlot = el('div');
const shell = el('div', { class: 'shell' });
const nav = el('nav', { class: 'sidenav', role: 'navigation', 'aria-label': '주 메뉴' });
const main = el('main', { class: 'content' });

nav.appendChild(el('div', { class: 'brand', text: 'wlog' }));
const navLinks = new Map<string, HTMLAnchorElement>();
for (const r of ROUTES) {
  const a = el('a', {
    href: '#' + r.name,
    text: r.label,
  }) as HTMLAnchorElement;
  navLinks.set(r.name, a);
  nav.appendChild(a);
}

shell.append(nav, main);
app.append(bannerSlot, shell);

// ---- persistent bind banner (stage 3) ----
void (async () => {
  try {
    const h = await getHealth();
    const banner = persistentBindBanner(
      h.bind?.non_local === true,
      h.bind?.auth === true,
    );
    if (banner) bannerSlot.appendChild(banner);
  } catch {
    /* health optional */
  }
})();

// ---- routing ----
let active: ViewHandle | null = null;

function setActiveNav(name: string): void {
  for (const [n, a] of navLinks) {
    a.classList.toggle('active', n === name);
    if (n === name) a.setAttribute('aria-current', 'page');
    else a.removeAttribute('aria-current');
  }
}

function render(route: Route): void {
  active?.destroy();
  active = null;
  clear(main);
  setActiveNav(route.name);

  switch (route.name) {
    case 'now':
      active = mountNow(main);
      break;
    case 'sessions':
      active = mountSessions(main);
      break;
    case 'cost':
      active = mountCost(main);
      break;
    case 'daily':
      active = mountDaily(main);
      break;
    case 'tools': {
      const session = route.params.get('session') ?? undefined;
      active = mountTools(main, session);
      break;
    }
    case 'timeline': {
      const session = route.params.get('session') ?? undefined;
      active = mountTimeline(main, session);
      break;
    }
  }
}

onRouteChange(render);

// default route
if (!window.location.hash) {
  navigate('now');
} else {
  render(currentRoute());
}
