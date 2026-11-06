"use client";

import { useEffect, useRef, useState } from "react";
import type { Frame } from "@/lib/types";
import type { CityWorld } from "./world";

export function CityView({ frame }: { frame: Frame | null }) {
  const host = useRef<HTMLDivElement>(null);
  const labels = useRef<HTMLDivElement>(null);
  const world = useRef<CityWorld | null>(null);
  const latest = useRef(frame);
  const [unsupported, setUnsupported] = useState(false);
  latest.current = frame;

  useEffect(() => {
    let cancelled = false;
    // Three touches window at import time, so it stays out of the server bundle.
    import("./world").then(({ CityWorld }) => {
      if (cancelled || !host.current || !labels.current) return;
      try {
        world.current = new CityWorld(host.current, labels.current);
        if (latest.current) world.current.sync(latest.current);
      } catch {
        setUnsupported(true);
      }
    });
    return () => {
      cancelled = true;
      world.current?.dispose();
      world.current = null;
    };
  }, []);

  useEffect(() => {
    if (frame) world.current?.sync(frame);
  }, [frame]);

  return (
    <div className="city" role="img" aria-label="Three-dimensional map of your platform: each building is a pod, towers are load balancers, and the large block is the database.">
      <div ref={host} className="city-canvas" />
      <div ref={labels} className="city-labels" aria-hidden />
      {unsupported && (
        <p className="city-fallback">WebGL isn&apos;t available here, so no city. The panels still tell you everything, in the way a pager does.</p>
      )}
    </div>
  );
}
