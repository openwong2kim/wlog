// Hash router. Routes: #now #sessions #cost #daily #tools #timeline (+?session=ID).
// nav order: Now · Sessions · Cost · Daily · Tools · Timeline.

export type RouteName = 'now' | 'sessions' | 'cost' | 'daily' | 'tools' | 'timeline';

export interface Route {
  name: RouteName;
  params: URLSearchParams;
}

export const ROUTES: { name: RouteName; label: string }[] = [
  { name: 'now', label: 'Now' },
  { name: 'sessions', label: 'Sessions' },
  { name: 'cost', label: 'Cost' },
  { name: 'daily', label: 'Daily' },
  { name: 'tools', label: 'Tools' },
  { name: 'timeline', label: 'Timeline' },
];

const VALID = new Set<RouteName>(ROUTES.map((r) => r.name));

export function parseHash(hash: string): Route {
  // strip leading '#'
  const raw = hash.startsWith('#') ? hash.slice(1) : hash;
  const [path, query = ''] = raw.split('?');
  const name = (path || 'now') as RouteName;
  return {
    name: VALID.has(name) ? name : 'now',
    params: new URLSearchParams(query),
  };
}

export function currentRoute(): Route {
  return parseHash(window.location.hash);
}

export function navigate(name: RouteName, params?: Record<string, string>): void {
  let hash = '#' + name;
  if (params) {
    const sp = new URLSearchParams(params);
    const q = sp.toString();
    if (q) hash += '?' + q;
  }
  if (window.location.hash === hash) {
    // force a re-render even if hash unchanged
    window.dispatchEvent(new HashChangeEvent('hashchange'));
  } else {
    window.location.hash = hash;
  }
}

export function onRouteChange(cb: (r: Route) => void): () => void {
  const handler = () => cb(currentRoute());
  window.addEventListener('hashchange', handler);
  return () => window.removeEventListener('hashchange', handler);
}
