"use client";

import { useState } from "react";
import { api } from "@/lib/api";
import { clock, pct, statusOf, STATUS_ICON } from "@/lib/format";
import { usePolled } from "@/lib/useGame";
import type { ComponentHealth, Frame, Series, Trace } from "@/lib/types";
import { LineChart } from "./LineChart";

const TITLES: Record<string, string> = {
  availability: "Availability", latency_mean: "Mean latency", traffic: "Traffic", latency_p50: "Latency p50",
  latency_p99: "Latency p99", app_saturation: "App saturation", db_saturation: "Database saturation",
  tower_saturation: "Load balancer saturation", cache_hit_ratio: "Cache hit ratio", memory: "Worst pod memory",
  storage: "Disk used", retry_amplification: "Retry amplification",
};

type View = "metrics" | "logs" | "traces" | "map";

function Upsell({ field, value, why }: { field: string; value: string; why: string }) {
  return <p className="callout warning small">{why} To see more, set <code>observability.{field}: {value}</code> in platform.yaml and push it.</p>;
}

function Metrics({ gameId, frame }: { gameId: string; frame: Frame | null }) {
  const { data } = usePolled(() => api.metrics(gameId), 1500, [gameId]);
  const [table, setTable] = useState(false);
  const series = data?.series ?? [];
  const slo = (frame?.ledger?.sloTarget ?? 0.99) * 100;
  return (
    <>
      <div className="section-head">
        <h3>Last two days</h3>
        <label className="check"><input type="checkbox" checked={table} onChange={(e) => setTable(e.target.checked)} /> table</label>
      </div>
      {data?.tier !== "full" && <Upsell field="metrics" value="full" why="Basic metrics are three five-tick averages. A short failure is a slight bump, and you can't see saturation at all." />}
      {table ? (
        <table className="table">
          <thead><tr><th>Metric</th><th className="num">Now</th><th className="num">Peak</th></tr></thead>
          <tbody>
            {series.map((s: Series) => (
              <tr key={s.name}>
                <td>{TITLES[s.name] ?? s.name}</td>
                <td className="num">{(s.values[s.values.length - 1] ?? 0).toFixed(1)} {s.unit}</td>
                <td className="num">{Math.max(0, ...s.values).toFixed(1)} {s.unit}</td>
              </tr>
            ))}
          </tbody>
        </table>
      ) : series.map((s) => (
        <LineChart key={s.name} series={s} title={TITLES[s.name] ?? s.name}
          reference={s.name === "availability" ? { value: slo, label: `SLO ${slo.toFixed(1)}%` } : undefined} />
      ))}
    </>
  );
}

function Logs({ gameId }: { gameId: string }) {
  const [query, setQuery] = useState("");
  const { data } = usePolled(() => api.logs(gameId, query), 1500, [gameId, query]);
  const tier = data?.tier ?? "";
  return (
    <>
      <div className="section-head"><h3>Logs</h3></div>
      {(tier === "none" || tier === "") && <Upsell field="logs" value="sampled" why="You aren't collecting logs, which is cheap right up until you need one." />}
      {tier === "sampled" && <p className="muted small">Sampled. About three quarters of these lines never made it here. Full logs cost more, and the bill grows with your traffic.</p>}
      <input className="search" aria-label="Filter logs" placeholder="Filter: error, postgres, timeout…" value={query} onChange={(e) => setQuery(e.target.value)} />
      <div className="logs" role="log">
        {(data?.lines ?? []).map((l, i) => (
          <div key={i} className={`log ${l.level}`}>
            <span className="feed-time">{clock(l.tick).replace(/^Day \d+ /, "")}</span>
            <span className="log-level">{l.level}</span>
            <span className="log-svc">{l.service}</span>
            <span className="log-msg">{l.message}</span>
          </div>
        ))}
        {tier !== "none" && tier !== "" && (data?.lines ?? []).length === 0 && <p className="muted small">Nothing matching.</p>}
      </div>
    </>
  );
}

const SPAN_SLOT: Record<string, string> = { ingress: "s1", app: "s2", redis: "s3", postgres: "s4" };

function TraceView({ tr }: { tr: Trace }) {
  const total = tr.totalMs || 1;
  return (
    <article className={`trace ${tr.error ? "failed" : ""}`}>
      <header>
        <code>{tr.id.slice(0, 10)}</code>
        <span className="small muted">{clock(tr.tick).replace("Day ", "D")}</span>
        <span className="small"><strong>{Math.round(tr.totalMs)}ms</strong>{tr.error ? " · failed" : ""}</span>
      </header>
      {tr.spans.map((s, i) => (
        <div key={i} className="span-row" title={s.note}>
          <span className="span-name">{s.service}</span>
          <div className="span-track">
            <div className={`span-bar ${SPAN_SLOT[s.service] ?? "s1"} ${s.error ? "err" : ""}`}
              style={{ left: `${(s.startMs / total) * 100}%`, width: `max(3px, ${(s.durationMs / total) * 100}%)` }} />
          </div>
          <span className="span-ms">{s.durationMs < 10 ? s.durationMs.toFixed(1) : Math.round(s.durationMs)}ms{s.error ? " err" : ""}</span>
          {s.note && <span className="span-note">{s.note}</span>}
        </div>
      ))}
    </article>
  );
}

function Traces({ gameId }: { gameId: string }) {
  const { data } = usePolled(() => api.traces(gameId), 2000, [gameId]);
  const tier = data?.tier ?? "";
  return (
    <>
      <div className="section-head"><h3>Traces</h3></div>
      {(tier === "none" || tier === "") && <Upsell field="traces" value="sampled" why="No tracing. Some failures look perfectly healthy from every other angle." />}
      {tier === "sampled" && <p className="muted small">Head sampled: you get a couple of recent requests, whichever ones the coin picked.</p>}
      {(data?.traces ?? []).map((t) => <TraceView key={t.id} tr={t} />)}
    </>
  );
}

function Hop({ name, detail, health, showNumbers, warn = 0.8, crit = 1 }: { name: string; detail: string; health: ComponentHealth | null; showNumbers: boolean; warn?: number; crit?: number }) {
  const util = health?.utilization ?? 0;
  const err = health?.errorRate ?? 0;
  const status = err > 0.1 ? "critical" : statusOf(util, warn, crit);
  return (
    <li className={`hop ${showNumbers ? status : ""}`}>
      <div>
        <strong>{name}</strong>
        <span className="small muted">{detail}</span>
      </div>
      {showNumbers ? (
        <div className="hop-nums">
          <span>{STATUS_ICON[status]} {status}</span>
          <span className="small muted">{pct(util, 0)} · {Math.round(health?.latencyMs ?? 0)}ms · {pct(err, 1)} err</span>
        </div>
      ) : <span className="small muted">not measured</span>}
    </li>
  );
}

function ServiceMap({ frame }: { frame: Frame | null }) {
  const t = frame?.topology, h = frame?.health;
  const full = t?.observability?.metrics === "full";
  const pods = (t?.regions ?? []).reduce((n, r) => n + r.pods.filter((p) => p.phase === "ready").length, 0);
  const towers = (t?.regions ?? []).reduce((n, r) => n + r.towers, 0);
  return (
    <>
      <div className="section-head"><h3>Request path</h3></div>
      {!full && <Upsell field="metrics" value="full" why="You can see the shape of your system, but nothing is measuring it." />}
      <ol className="hops">
        <Hop name="Load balancers" detail={`${towers} towers`} health={h?.ingress ?? null} showNumbers={full} />
        <Hop name="App" detail={`${pods} ready pods`} health={h?.app ?? null} showNumbers={full} />
        <Hop name="Redis" detail={`${t?.cache?.nodes ?? 0} nodes · ${pct(h?.cacheHitRatio ?? 0, 0)} hit`} health={h?.cache ?? null} showNumbers={full} warn={1} crit={1.6} />
        <Hop name="Postgres" detail={`${t?.database?.tier} · ${t?.database?.readReplicas} replicas · ${pct(h?.storageUtilization ?? 0, 0)} disk`} health={h?.database ?? null} showNumbers={full} />
      </ol>
    </>
  );
}

export function TelemetryPanel({ gameId, frame }: { gameId: string; frame: Frame | null }) {
  const [view, setView] = useState<View>("metrics");
  const views: View[] = ["metrics", "logs", "traces", "map"];
  return (
    <div className="stack">
      <div className="subtabs" role="tablist">
        {views.map((v) => (
          <button key={v} role="tab" aria-selected={view === v} className={view === v ? "sub on" : "sub"} onClick={() => setView(v)}>{v}</button>
        ))}
      </div>
      <section>
        {view === "metrics" && <Metrics gameId={gameId} frame={frame} />}
        {view === "logs" && <Logs gameId={gameId} />}
        {view === "traces" && <Traces gameId={gameId} />}
        {view === "map" && <ServiceMap frame={frame} />}
      </section>
    </div>
  );
}
