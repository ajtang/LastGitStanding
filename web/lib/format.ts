export const TICKS_PER_DAY = 96;

export function clock(tick: number): string {
  const day = Math.floor(tick / TICKS_PER_DAY) + 1;
  const minutes = Math.floor(((tick % TICKS_PER_DAY) / TICKS_PER_DAY) * 1440);
  const hh = String(Math.floor(minutes / 60)).padStart(2, "0");
  const mm = String(minutes % 60).padStart(2, "0");
  return `Day ${day} ${hh}:${mm}`;
}

export function money(v: number): string {
  const sign = v < 0 ? "-" : "";
  const a = Math.abs(v);
  if (a >= 1e6) return `${sign}$${(a / 1e6).toFixed(2)}M`;
  if (a >= 1e4) return `${sign}$${(a / 1e3).toFixed(1)}k`;
  return `${sign}$${Math.round(a).toLocaleString("en-US")}`;
}

export function count(v: number): string {
  if (v >= 1e6) return `${(v / 1e6).toFixed(2)}M`;
  if (v >= 1e4) return `${(v / 1e3).toFixed(1)}k`;
  return Math.round(v).toLocaleString("en-US");
}

export function pct(v: number, digits = 2): string {
  return `${(v * 100).toFixed(digits)}%`;
}

export type Status = "good" | "warning" | "critical";

export function statusOf(value: number, warn: number, crit: number): Status {
  return value >= crit ? "critical" : value >= warn ? "warning" : "good";
}

// Status never travels by color alone: every badge also gets one of these and a word.
export const STATUS_ICON: Record<Status, string> = { good: "●", warning: "▲", critical: "◆" };
