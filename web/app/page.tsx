"use client";

import dynamic from "next/dynamic";
import { useEffect, useState } from "react";
import { Landing } from "@/components/Landing";

const Game = dynamic(() => import("@/components/Game").then((m) => m.Game), { ssr: false });

export default function Page() {
  const [gameId, setGameId] = useState<string | null>(null);
  const [ready, setReady] = useState(false);

  // The game id lives in the URL so a reload picks the same run back up, paused.
  useEffect(() => {
    setGameId(new URLSearchParams(window.location.search).get("game"));
    setReady(true);
  }, []);

  const enter = (id: string | null) => {
    window.history.replaceState(null, "", id ? `?game=${id}` : "/");
    setGameId(id);
  };

  if (!ready) return null;
  return gameId ? <Game gameId={gameId} onExit={() => enter(null)} /> : <Landing onStart={enter} />;
}
