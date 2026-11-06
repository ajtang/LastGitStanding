"use client";

import { useEffect, useMemo, useRef, useState } from "react";
import { api } from "@/lib/api";
import { money } from "@/lib/format";
import type { CommitRecord, Frame, Plan } from "@/lib/types";
import { addRegion, bumpNumber, diffLines, getValue, setValue } from "./manifest";

interface Props { gameId: string; frame: Frame | null; toast: (m: string, s?: string) => void }

const STAGES = ["lint", "policy", "plan", "merge"];

export function GitOpsPanel({ gameId, frame, toast }: Props) {
  const [commits, setCommits] = useState<CommitRecord[]>([]);
  const [draft, setDraft] = useState<string | null>(null);
  const [message, setMessage] = useState("");
  const [plan, setPlan] = useState<Plan | null>(null);
  const [baseCost, setBaseCost] = useState<number | null>(null);
  const [showDiff, setShowDiff] = useState(false);
  const [pushing, setPushing] = useState(false);
  const baseRef = useRef<string | null>(null);

  const headSha = frame?.gitops?.headSha ?? "";
  const refresh = () => api.commits(gameId).then((r) => setCommits(r.commits ?? [])).catch(() => {});

  // Refetch when anything about the history could have moved.
  const latestRunKey = frame?.gitops?.pipelines?.map((p) => p.sha + p.status).join() ?? "";
  const appliedSha = frame?.gitops?.flux?.appliedSha ?? "";
  useEffect(() => { refresh(); /* eslint-disable-next-line react-hooks/exhaustive-deps */ }, [gameId, headSha, latestRunKey, appliedSha]);

  const head = commits.find((c) => c.sha === headSha) ?? commits.find((c) => c.status === "applied") ?? commits[commits.length - 1];
  const base = head?.yaml ?? null;

  // Follow main until you start typing; after that the draft is yours to rebase by hand.
  useEffect(() => {
    if (base === null) return;
    setDraft((d) => (d === null || d === baseRef.current ? base : d));
    baseRef.current = base;
  }, [base]);

  useEffect(() => {
    if (base === null) return;
    api.plan(gameId, base).then((p) => setBaseCost(p.quote?.costPerDay ?? null)).catch(() => {});
  }, [base, gameId]);

  useEffect(() => {
    if (draft === null) return;
    const id = setTimeout(() => { api.plan(gameId, draft).then(setPlan).catch(() => {}); }, 300);
    return () => clearTimeout(id);
  }, [draft, gameId, frame?.gitops?.versions?.length]);

  const dirty = draft !== null && base !== null && draft !== base;
  const diff = useMemo(() => (showDiff && draft !== null && base !== null ? diffLines(base, draft) : []), [showDiff, draft, base]);
  const delta = plan?.quote && baseCost !== null ? plan.quote.costPerDay - baseCost : null;
  const edit = (fn: (y: string) => string) => setDraft((d) => (d === null ? d : fn(d)));

  const push = async () => {
    if (draft === null) return;
    setPushing(true);
    try {
      const res = await api.push(gameId, draft, message.trim());
      toast(`${res.ref.slice(0, 7)}: ${res.message}`);
      setMessage("");
      baseRef.current = draft; // treat what we pushed as clean, so merging it doesn't look like a conflict
      refresh();
    } catch (e) {
      toast(String(e), "warn");
    } finally {
      setPushing(false);
    }
  };

  const ship = async (kind: string) => {
    try {
      const res = await api.ship(gameId, kind);
      toast(res.message, res.ok ? "info" : "warn");
    } catch (e) {
      toast(String(e), "warn");
    }
  };

  const flux = frame?.gitops?.flux;
  const runs = [...(frame?.gitops?.pipelines ?? [])].reverse();
  const run = runs[0];
  const versions = frame?.gitops?.versions ?? [];
  const inManifest = draft ? getValue(draft, "version") : null;
  const lines = (draft ?? "").split("\n").length;

  return (
    <div className="stack">
      <section>
        <div className="section-head">
          <h3>Delivery</h3>
          <span className={`pill ${flux?.status === "stalled" ? "critical" : flux?.status === "reconciling" ? "warning" : "good"}`}>
            Flux {flux?.status ?? "synced"}
          </span>
        </div>
        <ol className="pipeline" aria-label="Latest pipeline">
          {STAGES.map((s) => {
            const i = STAGES.indexOf(s);
            const at = run ? STAGES.indexOf(run.stage) : -1;
            let state = "idle";
            if (run) {
              if (run.status === "passed" || i < at) state = "done";
              else if (i === at) state = run.status === "failed" ? "failed" : "active";
            }
            return <li key={s} className={`step ${state}`}><span>{s}</span></li>;
          })}
          <li className={`step ${!flux ? "idle" : flux.status === "synced" ? "done" : flux.appliedSha === flux.targetSha ? "active" : "idle"}`}>
            <span>{flux && flux.appliedSha !== flux.targetSha ? `flux in ${flux.nextSyncIn}` : "flux"}</span>
          </li>
        </ol>
        <p className="muted small">{flux?.detail ?? "Paused on day one. Flux has nothing to do yet."}</p>
        {run?.status === "failed" && (
          <div className="callout critical" role="alert">
            <strong>{run.stage} failed on {run.sha.slice(0, 7)}</strong>
            <ul>{run.errors.map((e) => <li key={e}>{e}</li>)}</ul>
          </div>
        )}
      </section>

      <section>
        <div className="section-head">
          <h3>platform.yaml</h3>
          <label className="check">
            <input type="checkbox" checked={showDiff} onChange={(e) => setShowDiff(e.target.checked)} /> diff against main
          </label>
        </div>
        <div className="chips" aria-label="Quick edits to the draft">
          <button className="chip" onClick={() => edit((y) => bumpNumber(y, "replicas", 1, 1))}>+ replica</button>
          <button className="chip" onClick={() => edit((y) => bumpNumber(y, "nodes", 1, 1))}>+ node</button>
          <button className="chip" onClick={() => edit((y) => bumpNumber(y, "towers", 1, 1))}>+ tower</button>
          <button className="chip" onClick={() => edit((y) => bumpNumber(y, "storageGb", 100))}>+ 100GB</button>
          <button className="chip" onClick={() => edit((y) => bumpNumber(y, "certRenewal", 1))}>renew cert</button>
          <button className="chip" onClick={() => edit((y) => addRegion(y, "westeurope"))}>add westeurope</button>
          <button className="chip" onClick={() => setDraft(base)} disabled={!dirty}>reset</button>
        </div>
        {showDiff ? (
          <pre className="diff" aria-label="Changes against main">
            {diff.length === 0 || !dirty ? <span className="muted">No changes yet.</span> : diff.map((d, i) => (
              <span key={i} className={`diff-${d.kind}`}>{d.kind === "add" ? "+ " : d.kind === "del" ? "- " : "  "}{d.text}{"\n"}</span>
            ))}
          </pre>
        ) : (
          <div className="editor">
            <pre className="gutter" aria-hidden>{Array.from({ length: lines }, (_, i) => i + 1).join("\n")}</pre>
            <textarea
              aria-label="platform.yaml"
              spellCheck={false}
              rows={lines}
              wrap="off"
              value={draft ?? ""}
              onChange={(e) => setDraft(e.target.value)}
            />
          </div>
        )}

        <div className="plan" aria-live="polite">
          {plan && !plan.valid && (
            <div className="callout warning">
              <strong>This wouldn&apos;t pass the pipeline</strong>
              <ul>{plan.errors.map((e) => <li key={e}>{e}</li>)}</ul>
            </div>
          )}
          {plan?.valid && (
            <p className="small">
              Plan OK. Infrastructure {money(plan.quote?.costPerDay ?? 0)}/day
              {delta !== null && Math.abs(delta) > 0.5 && <strong> ({delta > 0 ? "+" : ""}{money(delta)}/day)</strong>}
            </p>
          )}
        </div>

        <form className="commit" onSubmit={(e) => { e.preventDefault(); push(); }}>
          <input aria-label="Commit message" value={message} onChange={(e) => setMessage(e.target.value)} placeholder="Commit message. Future you is reading this." />
          <button className="btn primary" disabled={!dirty || pushing}>git push</button>
        </form>
      </section>

      <section>
        <div className="section-head"><h3>Releases</h3></div>
        <p className="small muted">Shipping costs hours and only puts an image in the registry. Users see nothing until you deploy it. New features grow the business, and some of them are cursed.</p>
        <div className="row">
          <button className="btn" onClick={() => ship("feature")}>Ship feature ({Math.round(frame?.ledger?.featureCostHours ?? 0)}h)</button>
          <button className="btn" onClick={() => ship("hotfix")}>Cut hotfix (5h)</button>
        </div>
        <ul className="versions">
          {[...versions].reverse().slice(0, 6).map((v) => (
            <li key={v}>
              <code>{v}</code>
              {v === inManifest
                ? <span className="muted small">in draft</span>
                : <button className="link" onClick={() => edit((y) => setValue(y, "version", v))}>use in draft</button>}
            </li>
          ))}
        </ul>
      </section>

      <section>
        <div className="section-head"><h3>History</h3></div>
        <ul className="history">
          {commits.slice(0, 12).map((c) => (
            <li key={c.sha}>
              <code>{c.sha.slice(0, 7)}</code>
              <span className="history-msg" title={c.message}>{c.message}</span>
              <span className={`state ${c.status}`}>{c.status}</span>
              {c.status !== "failed" && c.sha !== headSha && (
                <button className="link" onClick={() => { setDraft(c.yaml); setMessage(`Revert to ${c.sha.slice(0, 7)}`); setShowDiff(true); }}>revert to</button>
              )}
            </li>
          ))}
        </ul>
      </section>
    </div>
  );
}
