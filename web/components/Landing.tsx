"use client";

import { useEffect, useState } from "react";
import { api, ApiError } from "@/lib/api";
import { clock, count } from "@/lib/format";
import { forgetGame, loadSavedGame } from "@/lib/savedGame";
import type { Frame, Run } from "@/lib/types";
import { WallAdmin } from "./WallAdmin";

export function Leaderboard({ runs }: { runs: Run[] }) {
  if (runs.length === 0) return <p className="muted">Nobody has finished a run yet. The wall is empty and very clean.</p>;
  return (
    <table className="table">
      <thead>
        <tr><th>On-call</th><th className="num">Score</th><th className="num">Peak users</th><th className="num">Days</th><th>Ended</th></tr>
      </thead>
      <tbody>
        {runs.map((r) => (
          <tr key={r.gameId}>
            <td>{r.player}</td>
            <td className="num">{Math.round(r.score).toLocaleString("en-US")}</td>
            <td className="num">{count(r.peakUsers)}</td>
            <td className="num">{Math.floor(r.ticks / 96)}</td>
            <td>{r.won ? "IPO" : "Fired or broke"}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export function Landing({ onStart }: { onStart: (id: string) => void }) {
  const [player, setPlayer] = useState("");
  const [seed, setSeed] = useState("");
  const [runs, setRuns] = useState<Run[]>([]);
  const [resume, setResume] = useState<Frame | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadRuns = () => api.leaderboard().then((r) => setRuns(r.runs ?? [])).catch(() => {});
  useEffect(() => {
    loadRuns();
  }, []);

  useEffect(() => {
    const id = loadSavedGame();
    if (!id) return;
    api.frame(id).then((f) => {
      // It may have ended in the couple of minutes it keeps running after the tab closes.
      if (f.ledger?.gameOverReason) forgetGame(id);
      else setResume(f);
    }).catch((err) => {
      // Only a 404 means the run is gone. A gateway blip mustn't lose it for good.
      if (err instanceof ApiError && err.status === 404) forgetGame(id);
    });
  }, []);

  const start = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      const frame = await api.createGame(player, seed);
      onStart(frame.gameId);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : `Couldn't reach the gateway. Is \`lgs dev\` running? (${err instanceof Error ? err.message : err})`);
      setBusy(false);
    }
  };

  return (
    <main className="landing">
      <div className="landing-col">
        <p className="eyebrow">incident 0 · you are now on call</p>
        <h1>Last Git<br />Standing</h1>
        <p className="lede">
          You inherit one cluster, one database, a few thousand users and a certificate nobody has
          looked at in a while. Keep it up, keep it funded, and get it to IPO.
        </p>
        <p className="lede muted">
          There is no console. Every change is a commit, through a pipeline, reconciled by Flux, and it
          all takes longer than you&apos;d like, especially at 3am.
        </p>
        {resume && (
          <div className="resume">
            <button type="button" className="btn primary big" onClick={() => onStart(resume.gameId)}>Resume your run</button>
            <span className="muted">{clock(resume.tick)} · {count(resume.ledger?.users ?? 0)} users</span>
          </div>
        )}
        <form className="landing-form" onSubmit={start}>
          <label>
            <span>Name for the postmortem</span>
            <input value={player} onChange={(e) => setPlayer(e.target.value)} maxLength={32} placeholder="on-call" autoFocus />
          </label>
          <label className="seed">
            <span>Seed (optional)</span>
            <input value={seed} onChange={(e) => setSeed(e.target.value.replace(/[^0-9]/g, ""))} inputMode="numeric" placeholder="random" />
          </label>
          <button className={resume ? "btn big" : "btn primary big"} disabled={busy}>{busy ? "Paging you…" : resume ? "Start a new run" : "Take the pager"}</button>
        </form>
        {error && <p className="error" role="alert">{error}</p>}
      </div>
      <aside className="landing-col wall">
        <h2>The wall</h2>
        <WallAdmin runs={runs} onChanged={loadRuns} />
      </aside>
    </main>
  );
}
