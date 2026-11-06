"use client";

import { useState } from "react";
import { api } from "@/lib/api";
import { CAUSES, causeLabel } from "@/lib/causes";
import { clock } from "@/lib/format";
import { usePolled } from "@/lib/useGame";
import type { Frame, Incident } from "@/lib/types";

interface Props { gameId: string; frame: Frame | null; toast: (m: string, s?: string) => void }

function Card({ inc, gameId, tick, toast }: { inc: Incident; gameId: string; tick: number; toast: Props["toast"] }) {
  const [cause, setCause] = useState("");
  const live = inc.resolvedTick === 0;

  const diagnose = async () => {
    try {
      const r = await api.diagnose(gameId, inc.id, cause);
      toast(`${inc.id}: ${r.message}`, r.ok ? "info" : "warn");
    } catch (e) { toast(String(e), "warn"); }
  };
  const postmortem = async () => {
    try {
      const r = await api.postmortem(gameId, inc.id);
      toast(r.message, r.ok ? "info" : "warn");
    } catch (e) { toast(String(e), "warn"); }
  };

  return (
    <article className={`incident ${live ? "live" : ""}`}>
      <header>
        <code>{inc.id}</code>
        <span className={`pill ${live ? "critical" : "good"}`}>{inc.status}</span>
      </header>
      <h4>{inc.title}</h4>
      <p className="small muted">
        {live
          ? `Open ${tick - inc.openedTick} ticks. Noticed via ${inc.detectedBy} after ${inc.detectedTick - inc.openedTick}.`
          : `Lasted ${inc.resolvedTick - inc.openedTick} ticks. Time to detect: ${inc.detectedTick - inc.openedTick}.`}
      </p>
      {inc.kind && <p className="small"><strong>Cause:</strong> {causeLabel(inc.kind)}</p>}
      {inc.runbook && <p className="small runbook">{inc.runbook}</p>}
      {live && !inc.kind && (
        <div className="diagnose">
          <label className="small">
            <span className="muted">What&apos;s your theory? A right answer unlocks the runbook. A wrong one costs two hours.</span>
            <select value={cause} onChange={(e) => setCause(e.target.value)}>
              <option value="">Pick a cause…</option>
              {CAUSES.map((c) => <option key={c.value} value={c.value}>{c.label}</option>)}
            </select>
          </label>
          <button className="btn" disabled={!cause} onClick={diagnose}>Call it</button>
        </div>
      )}
      {!live && !inc.postmortemDone && (
        <button className="btn" onClick={postmortem}>Write postmortem (4h)</button>
      )}
      {inc.postmortemDone && <p className="small muted">Postmortem written. This should be less likely to happen again.</p>}
    </article>
  );
}

export function IncidentsPanel({ gameId, frame, toast }: Props) {
  const { data: timeline, failed } = usePolled(() => api.timeline(gameId), 3000, [gameId]);
  const incidents = [...(frame?.incidents ?? [])].sort((a, b) => Number(a.resolvedTick !== 0) - Number(b.resolvedTick !== 0) || b.openedTick - a.openedTick);
  const alerts = frame?.alerts ?? [];
  const alerting = frame?.topology?.observability?.alerting;

  return (
    <div className="stack">
      <section>
        <div className="section-head"><h3>Alerts firing</h3></div>
        {alerting === "none" && <p className="callout warning small">Alerting is off, so your first signal is customers. They&apos;re slower than a pager, and less polite.</p>}
        {alerts.length === 0 && alerting !== "none" && <p className="muted small">Quiet. Enjoy it, and don&apos;t say so out loud.</p>}
        <ul className="alerts">
          {alerts.map((a) => (
            <li key={a.name} className={a.severity === "page" ? "critical" : "warning"}>
              <span className={`pill ${a.severity === "page" ? "critical" : "warning"}`}>{a.severity}</span>
              <span><strong>{a.name}</strong> {a.summary}</span>
            </li>
          ))}
        </ul>
      </section>
      <section>
        <div className="section-head"><h3>Incidents</h3></div>
        {incidents.length === 0 && <p className="muted small">None that you know about.</p>}
        {incidents.map((i) => <Card key={i.id} inc={i} gameId={gameId} tick={frame?.tick ?? 0} toast={toast} />)}
      </section>
      <section>
        <div className="section-head"><h3>Timeline</h3></div>
        {failed && <p className="muted small">Timeline is lagging. The game isn&apos;t; it will catch up.</p>}
        <ol className="timeline">
          {(timeline?.events ?? []).filter((e) => e.kind !== "incident_opened").slice(0, 60).map((e, i) => (
            <li key={`${e.tick}-${e.kind}-${e.ref}-${i}`} className={e.kind === "suspect_change" ? "suspect" : ""}>
              <span className="feed-time">{clock(e.tick).replace("Day ", "D")}</span>
              <span>{e.kind === "suspect_change" ? `What changed? ${e.message}` : e.message}</span>
            </li>
          ))}
        </ol>
      </section>
    </div>
  );
}
