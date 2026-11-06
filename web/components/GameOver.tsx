"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { count, money, pct } from "@/lib/format";
import type { Frame, Run } from "@/lib/types";
import { Leaderboard } from "./Landing";

export function GameOver({ frame, onExit }: { frame: Frame; onExit: () => void }) {
  const l = frame.ledger!;
  const [runs, setRuns] = useState<Run[]>([]);
  useEffect(() => {
    api.leaderboard().then((r) => setRuns(r.runs ?? [])).catch(() => {});
  }, []);

  return (
    <div className="overlay" role="dialog" aria-modal="true" aria-labelledby="over-title">
      <div className="sheet">
        <p className="eyebrow">{l.won ? "you are the last git standing" : "final postmortem"}</p>
        <h2 id="over-title">{l.won ? "IPO" : "It's over"}</h2>
        <p className="lede">{l.gameOverReason}</p>
        <dl className="facts">
          <div><dt>Score</dt><dd>{Math.round(l.score).toLocaleString("en-US")}</dd></div>
          <div><dt>Lasted</dt><dd>{Math.floor(frame.tick / 96)} days</dd></div>
          <div><dt>Users</dt><dd>{count(l.users)}</dd></div>
          <div><dt>Stage</dt><dd>{l.stage}</dd></div>
          <div><dt>Budget</dt><dd>{money(l.budget)}</dd></div>
          <div><dt>Availability</dt><dd>{pct(l.rollingAvailability)}</dd></div>
        </dl>
        <Leaderboard runs={runs} />
        <button className="btn primary big" onClick={onExit}>Take the pager again</button>
      </div>
    </div>
  );
}
