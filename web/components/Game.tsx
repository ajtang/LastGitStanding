"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { useGame } from "@/lib/useGame";
import { CityView } from "./city/CityView";
import { Hud } from "./Hud";
import { Feed } from "./Feed";
import { GameOver } from "./GameOver";
import { GitOpsPanel } from "./panels/GitOpsPanel";
import { IncidentsPanel } from "./panels/IncidentsPanel";
import { TelemetryPanel } from "./panels/TelemetryPanel";
import { CompanyPanel } from "./panels/CompanyPanel";

type Tab = "gitops" | "incidents" | "telemetry" | "company";

export function Game({ gameId, onExit }: { gameId: string; onExit: () => void }) {
  const { frame, connected, speed, setSpeed, feed, toast, error } = useGame(gameId);
  const [tab, setTab] = useState<Tab>("gitops");
  const [lastSpeed, setLastSpeed] = useState(1);
  const [missing, setMissing] = useState(false);
  const [noteDismissed, setNoteDismissed] = useState(false);

  // A run expired or never existed: say so, rather than a console full of reconnects.
  useEffect(() => {
    api.frame(gameId).catch(() => setMissing(true));
  }, [gameId]);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement;
      if (el.tagName === "TEXTAREA" || el.tagName === "INPUT" || el.tagName === "SELECT" || el.tagName === "BUTTON") return;
      if (e.code === "Space") {
        e.preventDefault();
        if (speed > 0) {
          setLastSpeed(speed);
          setSpeed(0);
        } else {
          setSpeed(lastSpeed);
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [speed, lastSpeed, setSpeed]);

  const open = frame?.incidents?.filter((i) => i.resolvedTick === 0).length ?? 0;
  const firing = frame?.alerts?.length ?? 0;
  const over = frame?.ledger?.gameOverReason;

  if (missing) {
    return (
      <main className="landing">
        <div className="landing-col">
          <h1>Gone</h1>
          <p className="lede">This run isn&apos;t in the checkpoint store any more. That&apos;s what you get for an in-memory dev setup, or for leaving it a week.</p>
          <button className="btn primary big" onClick={onExit}>Start over</button>
        </div>
      </main>
    );
  }

  const tabs: { id: Tab; label: string; badge?: number }[] = [
    { id: "gitops", label: "GitOps" },
    { id: "incidents", label: "Incidents", badge: open + firing },
    { id: "telemetry", label: "Telemetry" },
    { id: "company", label: "Company" },
  ];

  return (
    <div className="game">
      <Hud frame={frame} speed={speed} setSpeed={setSpeed} connected={connected} onExit={onExit} />
      <div className="stage">
        <CityView frame={frame} />
        <Feed items={feed} tick={frame?.tick ?? 0} />
        {(frame?.tick ?? 0) === 0 && !noteDismissed && (
          <aside className="handover">
            <h2>Handover note</h2>
            <p>Welcome to the rotation. Sorry about the timing.</p>
            <ul>
              <li><strong>Nothing changes without a commit.</strong> Edit platform.yaml in the GitOps tab and push. Pipeline, then Flux, then physics. Plan ahead.</li>
              <li><strong>Reading the city:</strong> each building is a pod. Blue is calm, amber is busy, red is in trouble. Towers are load balancers, the big block is Postgres, the green drum is Redis. Red cars are failed requests.</li>
              <li><strong>You can&apos;t fix what you can&apos;t see.</strong> Metrics, logs and traces all cost money. You have the cheap ones.</li>
              <li>I never got round to automating the TLS cert. It&apos;s probably fine.</li>
            </ul>
            <p className="muted small">Space pauses. Pausing works here, unlike in real life. A page drops you back to 1x.</p>
            <div className="row">
              <button className="btn primary" onClick={() => { setNoteDismissed(true); setSpeed(1); }}>Start the clock</button>
              <button className="btn" onClick={() => setNoteDismissed(true)}>Read the config first</button>
            </div>
          </aside>
        )}
        {error && <p className="error floating" role="alert">{error}</p>}
      </div>
      <aside className="dock">
        <div className="tabs" role="tablist">
          {tabs.map((t) => (
            <button key={t.id} role="tab" aria-selected={tab === t.id} className={tab === t.id ? "tab on" : "tab"} onClick={() => setTab(t.id)}>
              {t.label}
              {t.badge ? <span className="badge">{t.badge}</span> : null}
            </button>
          ))}
        </div>
        {/* Panels stay mounted so a half-written commit survives checking the graphs. */}
        <div className="panel" hidden={tab !== "gitops"}><GitOpsPanel gameId={gameId} frame={frame} toast={toast} /></div>
        <div className="panel" hidden={tab !== "incidents"}><IncidentsPanel gameId={gameId} frame={frame} toast={toast} /></div>
        <div className="panel" hidden={tab !== "telemetry"}>{tab === "telemetry" && <TelemetryPanel gameId={gameId} frame={frame} />}</div>
        <div className="panel" hidden={tab !== "company"}><CompanyPanel gameId={gameId} frame={frame} toast={toast} /></div>
      </aside>
      {over && frame?.ledger && <GameOver frame={frame} onExit={onExit} />}
    </div>
  );
}
