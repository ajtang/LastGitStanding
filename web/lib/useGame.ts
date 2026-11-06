"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { api, GATEWAY } from "./api";
import type { Frame, GameEvent } from "./types";

const FEED_LENGTH = 7;

export interface FeedItem extends GameEvent { key: number }

export function useGame(gameId: string) {
  const [frame, setFrame] = useState<Frame | null>(null);
  const [connected, setConnected] = useState(false);
  const [speed, setLocalSpeed] = useState(0);
  const [feed, setFeed] = useState<FeedItem[]>([]);
  const [error, setError] = useState<string | null>(null);
  const seq = useRef(0);
  const speedRef = useRef(0);
  speedRef.current = speed;

  const toast = useCallback((message: string, severity = "info") => {
    setFeed((f) => [{ tick: -1, source: "you", kind: "note", severity, message, ref: "", key: seq.current++ }, ...f].slice(0, FEED_LENGTH));
  }, []);

  useEffect(() => {
    const source = new EventSource(`${GATEWAY}/api/v1/games/${gameId}/stream`);
    let first = true;
    source.onopen = () => {
      setConnected(true);
      setError(null);
    };
    source.onerror = () => setConnected(false); // EventSource retries by itself
    source.addEventListener("frame", (e) => {
      const f = JSON.parse((e as MessageEvent<string>).data) as Frame;
      setFrame(f);
      // Only the first frame: after that, local state wins, because a frame already
      // on the wire when you hit pause would otherwise un-pause the button.
      if (first) setLocalSpeed(f.speed);
      first = false;
      // The pager outranks the fast-forward button. At 8x, a page could otherwise end
      // the run in the time it takes to find the pause key.
      if (f.events?.some((ev) => ev.kind === "incident_detected") && speedRef.current > 1) {
        setLocalSpeed(1);
        api.speed(gameId, 1).catch(() => {});
      }
      if (f.events?.length) {
        setFeed((prev) =>
          [...f.events.map((ev) => ({ ...ev, key: seq.current++ })).reverse(), ...prev].slice(0, FEED_LENGTH),
        );
      }
    });
    return () => source.close();
  }, [gameId]);

  const setSpeed = useCallback(
    async (s: number) => {
      // The stream goes quiet when paused, so the button can't wait for a frame to agree.
      setLocalSpeed(s);
      try {
        await api.speed(gameId, s);
      } catch (e) {
        setError(String(e));
      }
    },
    [gameId],
  );

  return { frame, connected, speed, setSpeed, feed, toast, error };
}

// usePolled refetches on an interval, skipping the overlap if the gateway is slow.
export function usePolled<T>(load: () => Promise<T>, ms: number, deps: unknown[]): { data: T | null; failed: boolean } {
  const [data, setData] = useState<T | null>(null);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    let alive = true;
    let busy = false;
    const run = async () => {
      if (busy) return;
      busy = true;
      try {
        const next = await load();
        if (alive) {
          setData(next);
          setFailed(false);
        }
      } catch {
        if (alive) setFailed(true);
      } finally {
        busy = false;
      }
    };
    run();
    const id = setInterval(run, ms);
    return () => {
      alive = false;
      clearInterval(id);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, deps);
  return { data, failed };
}
