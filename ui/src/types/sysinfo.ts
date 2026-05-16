// Mirrors internal/app/dock/sysinfo.go SystemHostInfo. Served by the
// public GET /api/system/info endpoint — no auth, no Workspace header
// needed. Deploy tooling can hit this on a fresh box.

export type SystemHostIP = {
  interface: string;
  address: string;
  family: "ipv4" | "ipv6";
};

export type SystemHostDisk = {
  mount: string;
  filesystem?: string;
  total_bytes: number;
  used_bytes: number;
  free_bytes: number;
  used_pct: number;
};

export type SystemHostMemory = {
  total_bytes: number;
  available_bytes?: number;
};

export type SystemHostCPU = {
  count: number;
  model?: string;
};

export type SystemHostVersion = {
  git_sha?: string;
  build_time?: string;
  go: string;
  module?: string;
};

export type SystemHostInfo = {
  hostname: string;
  os: string;
  arch: string;
  uname?: string;
  version: SystemHostVersion;
  uptime_seconds: number;
  ip_addresses: SystemHostIP[];
  disks: SystemHostDisk[];
  memory: SystemHostMemory;
  cpu: SystemHostCPU;
  sampled_at: string;
};
