"use client";

import { clock } from "@/lib/format";
import type { FeedItem } from "@/lib/useGame";

export function Feed({ items, tick }: { items: FeedItem[]; tick: number }) {
  return (
    <ol className="feed" aria-live="polite" aria-label="Recent events">
      {items.map((ev) => (
        <li key={ev.key} className={`feed-item ${ev.severity}`}>
          <span className="feed-time">{ev.tick < 0 ? "you" : clock(ev.tick).replace(/^Day \d+ /, "")}</span>
          <span>{ev.message}</span>
        </li>
      ))}
      {tick === 0 && items.length === 0 && (
        <li className="feed-item info"><span className="feed-time">00:00</span><span>Paused. Nothing is on fire yet. Press space when you&apos;re ready.</span></li>
      )}
    </ol>
  );
}
