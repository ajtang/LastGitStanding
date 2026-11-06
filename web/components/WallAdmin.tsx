"use client";

import { useState } from "react";
import { api, ApiError } from "@/lib/api";
import type { Run } from "@/lib/types";
import { Leaderboard } from "./Landing";

function message(err: unknown): string {
  if (err instanceof ApiError && err.status === 404) return "That's gone, or admin is off on this server.";
  return err instanceof Error ? err.message : String(err);
}

function RunRow({ run, passcode, onChanged, onError }: {
  run: Run;
  passcode: string;
  onChanged: () => void;
  onError: (message: string | null) => void;
}) {
  const shownScore = String(Math.round(run.score));
  const [player, setPlayer] = useState(run.player);
  const [score, setScore] = useState(shownScore);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  const act = async (work: () => Promise<void>) => {
    setBusy(true);
    onError(null);
    try {
      await work();
      onChanged();
    } catch (err) {
      onError(message(err));
      setBusy(false);
    }
  };

  const save = (e: React.FormEvent) => {
    e.preventDefault();
    // The field shows a rounded score, so a rename alone mustn't round the real one.
    const next = score === shownScore ? run.score : Number(score);
    if (score.trim() === "" || !Number.isFinite(next)) {
      onError("The score must be a number.");
      return;
    }
    act(() => api.adminEditRun(passcode, run.gameId, player, next));
  };

  const dirty = player !== run.player || score !== shownScore;
  return (
    <form onSubmit={save}>
      <input className="name" aria-label={`Name for ${run.player}`} value={player} maxLength={32} onChange={(e) => setPlayer(e.target.value)} />
      <input className="score" aria-label={`Score for ${run.player}`} value={score} inputMode="numeric" onChange={(e) => setScore(e.target.value)} />
      <button className="btn" disabled={busy || !dirty}>Save</button>
      <button
        type="button"
        className="btn danger"
        disabled={busy}
        onBlur={() => setConfirming(false)}
        onClick={() => (confirming ? act(() => api.adminDeleteRun(passcode, run.gameId)) : setConfirming(true))}
      >
        {confirming ? "Really?" : "Delete"}
      </button>
    </form>
  );
}

export function WallAdmin({ runs, onChanged }: { runs: Run[]; onChanged: () => void }) {
  const [stage, setStage] = useState<"hidden" | "locked" | "open">("hidden");
  // In memory only, so a reload locks the panel again and nothing outlives the tab.
  const [passcode, setPasscode] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const close = () => {
    setStage("hidden");
    setPasscode("");
    setError(null);
  };

  const unlock = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await api.adminUnlock(passcode);
      setStage("open");
    } catch (err) {
      setError(message(err));
    }
    setBusy(false);
  };

  if (stage === "hidden") {
    return (
      <>
        <Leaderboard runs={runs} />
        <button type="button" className="wall-secret" aria-label="Admin" onClick={() => setStage("locked")}>·</button>
      </>
    );
  }

  if (stage === "locked") {
    return (
      <>
        <Leaderboard runs={runs} />
        <form className="wall-admin" onSubmit={unlock}>
          <input type="password" aria-label="Passcode" placeholder="Passcode" autoComplete="off" autoFocus value={passcode} onChange={(e) => setPasscode(e.target.value)} />
          <div style={{ display: "flex", gap: 8 }}>
            <button className="btn primary" disabled={busy || passcode === ""}>Unlock</button>
            <button type="button" className="btn" onClick={close}>Cancel</button>
          </div>
          {error && <p className="error" role="alert">{error}</p>}
        </form>
      </>
    );
  }

  return (
    <div className="wall-admin">
      {runs.length === 0 && <p className="muted">Nothing on the wall to edit.</p>}
      {runs.map((run) => (
        // Keyed on the values so a saved row picks up what the server actually stored.
        <RunRow key={`${run.gameId}:${run.player}:${run.score}`} run={run} passcode={passcode} onChanged={onChanged} onError={setError} />
      ))}
      {error && <p className="error" role="alert">{error}</p>}
      <div><button type="button" className="btn" onClick={close}>Lock</button></div>
    </div>
  );
}
