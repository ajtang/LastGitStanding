// Hand-written mirror of the protojson the gateway sends, for the parts the UI
// reads. TODO: generate this from proto/ the day it drifts for the second time.

export interface Pod { id: string; version: string; phase: string; ageTicks: number }
export interface RegionState {
  name: string; nodeSize: string; nodesReady: number; nodesProvisioning: number;
  towers: number; pods: Pod[]; provisioningTicks: number;
}
export interface Topology {
  regions: RegionState[];
  database: {
    tier: string; readReplicas: number; storageGb: number; indexes: string[];
    primaryRegion: string; standbyRegion: string; failoverTicks: number;
  } | null;
  cache: { nodes: number; replicated: boolean } | null;
  ingress: { certManager: boolean; certExpiresAtTick: number } | null;
  observability: { metrics: string; logs: string; traces: string; alerting: string } | null;
  revision: string;
}
export interface ComponentHealth { utilization: number; latencyMs: number; errorRate: number }
export interface RegionHealth {
  name: string; up: boolean; offeredRps: number; capacityRps: number; utilization: number;
  errorRate: number; backlog: number; towerUtilization: number; podsReady: number; podsPending: number;
}
export interface Health {
  offeredRps: number; servedRps: number; errorRate: number; latencyP95Ms: number;
  regions: RegionHealth[]; ingress: ComponentHealth | null; app: ComponentHealth | null;
  cache: ComponentHealth | null; database: ComponentHealth | null; cacheHitRatio: number;
  storageUtilization: number; diurnal: number;
}
export interface Ledger {
  budget: number; revenuePerDay: number; costPerDay: number; users: number; engineerHours: number;
  engineers: number; techDebt: number; sloTarget: number; rollingAvailability: number;
  errorBudgetRemaining: number; stage: string; nextStageUsers: number; featureCostHours: number;
  score: number; gameOverReason: string; won: boolean;
}
export interface GameEvent { tick: number; source: string; kind: string; severity: string; message: string; ref: string }
export interface Incident {
  id: string; title: string; status: string; openedTick: number; detectedTick: number; resolvedTick: number;
  kind: string; runbook: string; postmortemDone: boolean; wrongGuesses: number; detectedBy: string;
}
export interface PipelineRun {
  sha: string; message: string; stage: string; status: string; stageTicksLeft: number; errors: string[]; startedTick: number;
}
export interface GitOps {
  headSha: string; pipelines: PipelineRun[]; versions: string[];
  flux: { nextSyncIn: number; appliedSha: string; targetSha: string; status: string; detail: string } | null;
}
export interface Alert { name: string; severity: string; summary: string; sinceTick: number }
export interface Frame {
  gameId: string; tick: number; speed: number; health: Health | null; ledger: Ledger | null;
  topology: Topology | null; incidents: Incident[]; events: GameEvent[]; gitops: GitOps | null; alerts: Alert[];
}
export interface CommitRecord { sha: string; parent: string; message: string; yaml: string; tick: number; status: string }
export interface Series { name: string; unit: string; resolutionTicks: number; ticks: number[]; values: number[] }
export interface LogLine { tick: number; level: string; service: string; message: string }
export interface Span { service: string; operation: string; startMs: number; durationMs: number; error: boolean; note: string }
export interface Trace { id: string; tick: number; totalMs: number; error: boolean; spans: Span[] }
export interface Run { gameId: string; player: string; ticks: number; score: number; peakUsers: number; outcome: string; won: boolean }
export interface CommandResult { ok: boolean; message: string; ref: string }
export interface Plan {
  valid: boolean; errors: string[];
  quote: { costPerDay: number; items: { name: string; costPerDay: number }[] } | null;
}
