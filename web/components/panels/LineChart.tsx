"use client";

import { useEffect, useRef, useState } from "react";
import { clock } from "@/lib/format";
import type { Series } from "@/lib/types";

const HEIGHT = 96;
const PAD = { top: 8, right: 8, bottom: 16, left: 38 };

function fmt(v: number, unit: string): string {
  const n = Math.abs(v) >= 100 ? Math.round(v).toString() : v.toFixed(unit === "x" ? 2 : 1);
  return `${n}${unit === "%" ? "%" : unit === "x" ? "x" : ` ${unit}`}`;
}

// One measure per chart, one axis per chart, always. Small multiples line up by tick,
// which is how you notice the database saturating two ticks before the errors start.
export function LineChart({ series, title, reference }: { series: Series; title: string; reference?: { value: number; label: string } }) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(400);
  const [hover, setHover] = useState<number | null>(null);

  useEffect(() => {
    if (!box.current) return;
    const ob = new ResizeObserver(([e]) => setWidth(e.contentRect.width));
    ob.observe(box.current);
    return () => ob.disconnect();
  }, []);

  const { values, ticks, unit } = series;
  const n = values.length;
  const latest = n ? values[n - 1] : 0;
  const innerW = Math.max(10, width - PAD.left - PAD.right);
  const innerH = HEIGHT - PAD.top - PAD.bottom;

  // Percent-of-availability zooms to where the action is; everything else starts at
  // zero so a chart can't make a 2% wobble look like a catastrophe.
  const zoomed = series.name === "availability";
  const hi = Math.max(...values, reference?.value ?? 0, zoomed ? 100 : 1) * (zoomed ? 1 : 1.1);
  const lo = zoomed ? Math.min(...values, 95) : 0;
  const span = hi - lo || 1;
  const x = (i: number) => PAD.left + (n <= 1 ? 0 : (i / (n - 1)) * innerW);
  const y = (v: number) => PAD.top + innerH - ((v - lo) / span) * innerH;
  const path = values.map((v, i) => `${i ? "L" : "M"}${x(i).toFixed(1)} ${y(v).toFixed(1)}`).join("");
  const gridlines = [lo, (lo + hi) / 2, hi];

  const onMove = (e: React.PointerEvent<SVGSVGElement>) => {
    const rect = e.currentTarget.getBoundingClientRect();
    const i = Math.round(((e.clientX - rect.left - PAD.left) / innerW) * (n - 1));
    setHover(n ? Math.max(0, Math.min(n - 1, i)) : null);
  };

  return (
    <figure className="chart" ref={box}>
      <figcaption>
        <span>{title}</span>
        <strong>{n ? fmt(latest, unit) : "no data"}</strong>
      </figcaption>
      <svg width={width} height={HEIGHT} role="img" aria-label={`${title}, currently ${fmt(latest, unit)}`}
        onPointerMove={onMove} onPointerLeave={() => setHover(null)}>
        {gridlines.map((g) => (
          <g key={g}>
            <line x1={PAD.left} x2={width - PAD.right} y1={y(g)} y2={y(g)} className="grid" />
            <text x={PAD.left - 5} y={y(g) + 3} textAnchor="end" className="tick">{g >= 100 ? Math.round(g) : g.toFixed(g < 10 ? 1 : 0)}</text>
          </g>
        ))}
        {reference && (
          <g>
            <line x1={PAD.left} x2={width - PAD.right} y1={y(reference.value)} y2={y(reference.value)} className="reference" />
            <text x={width - PAD.right} y={y(reference.value) - 3} textAnchor="end" className="tick">{reference.label}</text>
          </g>
        )}
        <path d={path} className="series" />
        {n > 0 && <text x={PAD.left} y={HEIGHT - 4} className="tick">{clock(ticks[0]).replace("Day ", "D")}</text>}
        {n > 0 && <text x={width - PAD.right} y={HEIGHT - 4} textAnchor="end" className="tick">{clock(ticks[n - 1]).replace("Day ", "D")}</text>}
        {hover !== null && (
          <g>
            <line x1={x(hover)} x2={x(hover)} y1={PAD.top} y2={PAD.top + innerH} className="crosshair" />
            <circle cx={x(hover)} cy={y(values[hover])} r={4} className="dot" />
          </g>
        )}
      </svg>
      {hover !== null && (
        <div className="tooltip" style={{ left: Math.min(x(hover) + 8, width - 130), top: 24 }}>
          <strong>{fmt(values[hover], unit)}</strong>
          <span>{clock(ticks[hover])}</span>
        </div>
      )}
    </figure>
  );
}
