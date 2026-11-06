import type { CommandResult, CommitRecord, Frame, LogLine, Plan, Run, Series, GameEvent, Trace } from "./types";

export const GATEWAY = process.env.NEXT_PUBLIC_GATEWAY_URL ?? "http://localhost:8080";

// Distinct from a failed fetch: the gateway answered, so its message is worth showing.
export class ApiError extends Error {
  constructor(message: string, readonly status: number) {
    super(message);
  }
}

async function call<T>(path: string, body?: unknown): Promise<T> {
  const res = await fetch(`${GATEWAY}/api/v1${path}`, {
    method: body === undefined ? "GET" : "POST",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw new ApiError((await res.text()).trim() || `${res.status} from the gateway`, res.status);
  return res.json() as Promise<T>;
}

async function adminCall(passcode: string, method: string, path: string, body?: unknown): Promise<void> {
  const res = await fetch(`${GATEWAY}/api/v1/admin${path}`, {
    method,
    headers: { Authorization: `Bearer ${passcode}`, ...(body === undefined ? {} : { "Content-Type": "application/json" }) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!res.ok) throw new ApiError((await res.text()).trim() || `${res.status} from the gateway`, res.status);
}

export const api = {
  createGame: (player: string, seed: string) => call<Frame>("/games", { player, seed }),
  frame: (id: string) => call<Frame>(`/games/${id}`),
  leaderboard: () => call<{ runs: Run[] }>("/leaderboard"),
  commits: (id: string) => call<{ commits: CommitRecord[] }>(`/games/${id}/commits`),
  plan: (id: string, yaml: string) => call<Plan>(`/games/${id}/plan`, { yaml }),
  push: (id: string, yaml: string, message: string) => call<CommandResult>(`/games/${id}/commits`, { yaml, message }),
  ship: (id: string, kind: string) => call<CommandResult>(`/games/${id}/ship`, { kind }),
  speed: (id: string, speed: number) => call<CommandResult>(`/games/${id}/speed`, { speed }),
  hire: (id: string, delta: number) => call<CommandResult>(`/games/${id}/hire`, { delta }),
  payDebt: (id: string, hours: number) => call<CommandResult>(`/games/${id}/debt`, { hours }),
  diagnose: (id: string, incident: string, cause: string) =>
    call<CommandResult>(`/games/${id}/incidents/${incident}/diagnosis`, { cause }),
  postmortem: (id: string, incident: string) => call<CommandResult>(`/games/${id}/incidents/${incident}/postmortem`, {}),
  metrics: (id: string) => call<{ series: Series[]; tier: string }>(`/games/${id}/metrics?window=192`),
  logs: (id: string, q: string) => call<{ lines: LogLine[]; tier: string }>(`/games/${id}/logs?q=${encodeURIComponent(q)}`),
  traces: (id: string) => call<{ traces: Trace[]; tier: string }>(`/games/${id}/traces`),
  timeline: (id: string) => call<{ events: GameEvent[] }>(`/games/${id}/timeline`),
  adminUnlock: (passcode: string) => adminCall(passcode, "POST", "/session"),
  adminEditRun: (passcode: string, id: string, player: string, score: number) =>
    adminCall(passcode, "PATCH", `/runs/${id}`, { player, score }),
  adminDeleteRun: (passcode: string, id: string) => adminCall(passcode, "DELETE", `/runs/${id}`),
};
