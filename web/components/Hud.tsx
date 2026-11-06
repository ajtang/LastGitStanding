"use client";

import { clock, count, money, pct, statusOf, STATUS_ICON, type Status } from "@/lib/format";
import type { Frame } from "@/lib/types";

function Stat({ label, value, sub, status }: { label: string; value: string; sub?: string; status?: Status }) {
  return (
    <div className="stat">
      <span className="stat-label">{label}</span>
      <span className="stat-value">
        {status && status !== "good" && <span className={`icon ${status}`} aria-label={status}>{STATUS_ICON[status]}</span>}
        {value}
      </span>
      {sub && <span className="stat-sub">{sub}</span>}
    </div>
  );
}

const SPEEDS = [1, 2, 4, 8];

export function Hud({ frame, speed, setSpeed, connected, onExit }: {
  frame: Frame | null; speed: number; setSpeed: (s: number) => void; connected: boolean; onExit: () => void;
}) {
  const l = frame?.ledger;
  const h = frame?.health;
  const net = l ? l.revenuePerDay - l.costPerDay : 0;
  const availability = h ? 1 - h.errorRate : 1;

  return (
    <header className="hud">
      <button className="wordmark" onClick={onExit} title="Back to the title screen. Your run stays where it is.">LGS</button>
      <div className="clock">
        <span className="clock-time">{clock(frame?.tick ?? 0)}</span>
        <span className="stat-sub">{l?.stage ?? "Seed"}{connected ? "" : " · reconnecting"}</span>
      </div>
      <div className="speed" role="group" aria-label="Simulation speed">
        <button className={speed === 0 ? "sp on" : "sp"} onClick={() => setSpeed(0)} aria-pressed={speed === 0}>Pause</button>
        {SPEEDS.map((s) => (
          <button key={s} className={speed === s ? "sp on" : "sp"} onClick={() => setSpeed(s)} aria-pressed={speed === s}>{s}x</button>
        ))}
      </div>
      <div className="stats">
        <Stat label="Availability" value={pct(availability)} sub={`SLO ${pct(l?.sloTarget ?? 0.99, 1)}`}
          status={statusOf(1 - availability, 1 - (l?.sloTarget ?? 0.99), 0.05)} />
        <Stat label="p95" value={`${Math.round(h?.latencyP95Ms ?? 0)}ms`} status={statusOf(h?.latencyP95Ms ?? 0, 400, 800)} />
        <Stat label="Users" value={count(l?.users ?? 0)} sub={`next round ${count(l?.nextStageUsers ?? 0)}`} />
        <Stat label="Budget" value={money(l?.budget ?? 0)} sub={`${net >= 0 ? "+" : ""}${money(net)}/day`} status={(l?.budget ?? 1) < 0 ? "critical" : undefined} />
        <Stat label="Eng hours" value={Math.floor(l?.engineerHours ?? 0).toString()} sub={`${l?.engineers ?? 0} engineers`} />
        <Stat label="Debt" value={Math.round(l?.techDebt ?? 0).toString()} status={statusOf(l?.techDebt ?? 0, 25, 60)} />
      </div>
    </header>
  );
}
