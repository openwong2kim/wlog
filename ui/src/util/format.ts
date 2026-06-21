// Number / time formatting — DESIGN §7 (single forced module).
// All UI numeric rendering MUST route through these helpers.

/**
 * Cost (USD).
 *  - < $0.0001         -> "<$0.0001"
 *  - < $1              -> 4 decimals  "$0.0032"
 *  - $1 .. $999.99     -> 2 decimals  "$12.34"
 *  - >= $1000          -> grouped int "$1,234"
 */
export function formatCost(usd: number): string {
  if (!isFinite(usd) || usd < 0) return '$0.0000';
  if (usd === 0) return '$0.0000';
  if (usd < 0.0001) return '<$0.0001';
  if (usd < 1) return '$' + usd.toFixed(4);
  if (usd < 1000) return '$' + usd.toFixed(2);
  return '$' + Math.round(usd).toLocaleString('en-US');
}

/**
 * Token count.
 *  - < 1000   -> grouped int "1,420" wait: spec says "1,420 (<1000)"? -> grouped under 1000
 *               DESIGN: "1,420"(천단위 구분자, <1000) — group separator below 1000 too.
 *  - >= 1000  -> "1.4K"
 *  - >= 1e6   -> "1.2M"
 */
export function formatTokens(n: number): string {
  if (!isFinite(n) || n < 0) return '0';
  const v = Math.round(n);
  if (v < 1000) return v.toLocaleString('en-US');
  if (v < 1_000_000) return trim1(v / 1000) + 'K';
  return trim1(v / 1_000_000) + 'M';
}

/**
 * Duration from milliseconds.
 *  - < 1s      -> "234ms"
 *  - 1..60s    -> "2.1s"
 *  - 60s+      -> "3m 12s"
 *  - 3600s+    -> "1h 23m"
 */
export function formatDurationMs(ms: number): string {
  if (!isFinite(ms) || ms < 0) return '0ms';
  if (ms < 1000) return Math.round(ms) + 'ms';
  const s = ms / 1000;
  return formatDurationSec(s);
}

/** Duration from seconds (same buckets as ms variant above 1s). */
export function formatDurationSec(sec: number): string {
  if (!isFinite(sec) || sec < 0) return '0s';
  if (sec < 1) return Math.round(sec * 1000) + 'ms';
  if (sec < 60) return trim1(sec) + 's';
  if (sec < 3600) {
    const m = Math.floor(sec / 60);
    const s = Math.round(sec % 60);
    return `${m}m ${s}s`;
  }
  const h = Math.floor(sec / 3600);
  const m = Math.round((sec % 3600) / 60);
  return `${h}h ${m}m`;
}

/** Ratio 0..1 -> integer percent, no decimals. */
export function formatPercent(ratio: number): string {
  if (!isFinite(ratio)) return '0%';
  return Math.round(ratio * 100) + '%';
}

/** Already-percent integer (0..100) helper for rates expressed as fractions. */
export function formatRate(ratio: number): string {
  return formatPercent(ratio);
}

/** List timestamp: "MM-DD HH:mm" (local). tsMs = unix milliseconds. */
export function formatListTime(tsMs: number): string {
  const d = new Date(tsMs);
  return (
    pad2(d.getMonth() + 1) +
    '-' +
    pad2(d.getDate()) +
    ' ' +
    pad2(d.getHours()) +
    ':' +
    pad2(d.getMinutes())
  );
}

/** Timeline timestamp: "HH:mm:ss.mmm" (local). */
export function formatTimelineTime(tsMs: number): string {
  const d = new Date(tsMs);
  return (
    pad2(d.getHours()) +
    ':' +
    pad2(d.getMinutes()) +
    ':' +
    pad2(d.getSeconds()) +
    '.' +
    pad3(d.getMilliseconds())
  );
}

/** Chart x-axis short label: "HH:mm" same-day else "MM-DD". */
export function formatAxisTime(tsMs: number, multiDay: boolean): string {
  const d = new Date(tsMs);
  if (multiDay) return pad2(d.getMonth() + 1) + '-' + pad2(d.getDate());
  return pad2(d.getHours()) + ':' + pad2(d.getMinutes());
}

/** Short K/M label for token y-axis ("12K"). */
export function formatTokensAxis(n: number): string {
  if (n >= 1_000_000) return trim1(n / 1_000_000) + 'M';
  if (n >= 1000) return Math.round(n / 1000) + 'K';
  return Math.round(n).toString();
}

/**
 * Short cost label for a chart y-axis ("$0.50", "$12", "$1.2K"). Compact so
 * splits stay readable; this is axis-only, not a headline value.
 */
export function formatCostAxis(usd: number): string {
  if (!isFinite(usd) || usd <= 0) return '$0';
  if (usd >= 1000) return '$' + trim1(usd / 1000) + 'K';
  if (usd >= 1) return '$' + (Number.isInteger(usd) ? usd : usd.toFixed(2));
  return '$' + usd.toFixed(2);
}

// ---- internals ----

function trim1(v: number): string {
  // one decimal, drop trailing ".0"
  const s = v.toFixed(1);
  return s.endsWith('.0') ? s.slice(0, -2) : s;
}

function pad2(n: number): string {
  return n < 10 ? '0' + n : String(n);
}

function pad3(n: number): string {
  if (n < 10) return '00' + n;
  if (n < 100) return '0' + n;
  return String(n);
}
