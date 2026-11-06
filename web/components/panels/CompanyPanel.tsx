"use client";

import { api } from "@/lib/api";
import { count, money, pct } from "@/lib/format";
import type { Frame } from "@/lib/types";

interface Props { gameId: string; frame: Frame | null; toast: (m: string, s?: string) => void }

export function CompanyPanel({ gameId, frame, toast }: Props) {
  const l = frame?.ledger;
  if (!l) return null;
  const net = l.revenuePerDay - l.costPerDay;
  const runway = net < 0 ? Math.floor(l.budget / -net) : null;
  const budgetLeft = Math.max(0, Math.min(1, l.errorBudgetRemaining));
  const toRound = Math.min(1, l.users / l.nextStageUsers);

  const run = async (fn: () => Promise<{ ok: boolean; message: string }>) => {
    try {
      const r = await fn();
      if (r.message) toast(r.message, r.ok ? "info" : "warn");
    } catch (e) { toast(String(e), "warn"); }
  };

  return (
    <div className="stack">
      <section>
        <div className="section-head"><h3>The money</h3></div>
        <dl className="facts two">
          <div><dt>Budget</dt><dd>{money(l.budget)}</dd></div>
          <div><dt>Runway</dt><dd>{runway === null ? "profitable" : `${runway} days`}</dd></div>
          <div><dt>Revenue</dt><dd>{money(l.revenuePerDay)}/day</dd></div>
          <div><dt>Costs</dt><dd>{money(l.costPerDay)}/day</dd></div>
        </dl>
      </section>
      <section>
        <div className="section-head"><h3>Next round: {count(l.nextStageUsers)} users</h3></div>
        <div className="meter" role="progressbar" aria-valuenow={Math.round(toRound * 100)} aria-valuemin={0} aria-valuemax={100} aria-label="Progress to the next funding round">
          <div style={{ width: `${toRound * 100}%` }} />
        </div>
        <p className="small muted">{count(l.users)} of {count(l.nextStageUsers)}. Investors also read the status page: three-day availability has to clear {pct(l.sloTarget, 1)} to close, and you&apos;re at {pct(l.rollingAvailability)}.</p>
      </section>
      <section>
        <div className="section-head"><h3>Error budget</h3></div>
        <div className="meter" role="progressbar" aria-valuenow={Math.round(budgetLeft * 100)} aria-valuemin={0} aria-valuemax={100} aria-label="Error budget remaining">
          <div style={{ width: `${budgetLeft * 100}%` }} />
        </div>
        <p className="small muted">{pct(budgetLeft, 0)} left over the rolling three days. At zero, you&apos;re out of SLO and the next round waits. Fall under 80% availability and the board stops waiting.</p>
      </section>
      <section>
        <div className="section-head"><h3>Team</h3></div>
        <p className="small muted">{l.engineers} engineers at $420/day each make about 6 useful hours per day apiece. The other two are meetings. Open incidents eat into that.</p>
        <div className="row">
          <button className="btn" onClick={() => run(() => api.hire(gameId, 1))}>Hire ($2,500 fee)</button>
          <button className="btn" onClick={() => run(() => api.hire(gameId, -1))}>Let one go</button>
        </div>
      </section>
      <section>
        <div className="section-head"><h3>Tech debt: {Math.round(l.techDebt)}</h3></div>
        <p className="small muted">Every feature adds debt. Debt makes features cost more ({Math.round(l.featureCostHours)}h now), releases go wrong more often, and surprises more frequent. Nobody will thank you for paying it down, but the graphs might.</p>
        <button className="btn" onClick={() => run(() => api.payDebt(gameId, 8))}>Spend 8h on debt</button>
      </section>
    </div>
  );
}
