// Must match the kinds in services/incident/catalog.go, or a cause can never be named.
export const CAUSES: { value: string; label: string }[] = [
  { value: "traffic_spike", label: "Traffic spike" },
  { value: "capacity_exhausted", label: "Ran out of capacity" },
  { value: "memory_leak", label: "Memory leak in a release" },
  { value: "bad_deployment", label: "Bad deployment" },
  { value: "database_hotspot", label: "Database query hotspot" },
  { value: "redis_failure", label: "Redis failure" },
  { value: "certificate_expiry", label: "Certificate expiry" },
  { value: "network_partition", label: "Network partition" },
  { value: "region_failure", label: "Region failure" },
  { value: "storage_full", label: "Disk full" },
  { value: "vulnerability", label: "Vulnerability" },
];

export const causeLabel = (kind: string) => CAUSES.find((c) => c.value === kind)?.label ?? kind;
