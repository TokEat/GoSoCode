// Typed client for the GoSoCode backend.

export interface FileItem {
  id: number; path: string; size: number; status: string; created_at: string; modified: string; is_dir: boolean; owner: string;
  node_id?: number; remote_path?: string; download_url?: string; mount?: boolean;
  protected?: boolean; protected_own?: boolean; protected_by?: string;
  encrypted?: boolean;
}
export interface ProtectInfo { protected: boolean; owner?: string; is_dir?: boolean; password?: string; }
export interface Storage { total: number; used: number; free: number; }
export interface TransferLogItem { ID: number; Kind: string; Name: string; Path: string; Size: number; At: string; }
export interface JobItem { ID: string; Type: string; Status: string; Progress: number; Error: string; CreatedAt: string; }
export interface NodeItem {
	  id: number; nickname: string; mount_name: string; mount_segment: string; mount_path: string; remark: string; status: string;
  quota_total: number; quota_used: number; quota_free: number; current_load: number;
  bytes_up: number; bytes_down: number;
  vip_type: number; slice_size: number;
}
export interface NodeDirectory { name: string; path: string; }
export interface NodeDirectoryList { current: string; parent: string; directories: NodeDirectory[]; }
export interface UserItem {
  id: number; username: string; role: string; status: string;
  bytes_up: number; bytes_down: number; created_at: string;
  sso?: boolean; // provisioned via unified OIDC login (no local password)
}
export interface ProtectedItem { path: string; is_dir: boolean; created_at: string; }
export interface Me {
  id: number; username: string; role: string; status: string;
  sso?: boolean;
  bytes_up: number; bytes_down: number; file_count: number; created_at: string;
  protected?: ProtectedItem[];
}
export interface LogItem { ID: number; At: string; Username: string; Action: string; Detail: string; IP: string; }
export interface LogsPage { logs: LogItem[]; total: number; page: number; page_size: number; }
export interface BaiduConfig {
  app_key: string; sign_key: string; redirect_uri: string;
  has_secret: boolean; configured: boolean;
}
export interface SystemConfig {
  encrypt_storage: boolean; uuid_storage: boolean; log_max_entries: number;
  share_import_enabled: boolean; share_links_enabled: boolean;
  oidc_enabled: boolean; oidc_issuer: string; oidc_client_id: string;
  oidc_redirect_url: string; oidc_has_secret: boolean;
  oidc_client_secret?: string; // write-only: sent on save, never echoed back
}

export interface ShareLinkResult {
  token: string;
  url: string;
  expires_at: string | null;
  is_dir: boolean;
}

export interface ShareInfo {
  name: string;
  size: number;
  is_dir: boolean;
  expires_at: string | null;
  protected: boolean;
  owner?: string;
}
// fs_id/share_id/uk arrive as strings: they can exceed Number.MAX_SAFE_INTEGER
export interface ShareFile { fs_id: string; name: string; size: number; is_dir: boolean; }
export interface ShareResolveResult {
  surl: string; sekey: string; share_id: string; uk: string; files: ShareFile[];
}
export type UserInput = { username: string; password?: string; role: string; status: string };
export interface TrafficRow { id: number; label: string; bytes_up: number; bytes_down: number; }
export interface TrafficPoint { t: number; up: number; down: number; }
export interface Traffic {
  live: { total_up: number; total_down: number; up_bps: number; down_bps: number };
  nodes: TrafficRow[];
  users: TrafficRow[];
  series: TrafficPoint[]; // live throughput (avg B/s per bucket), oldest first
  interval: number;       // bucket width of `series` in seconds
  hourly: TrafficPoint[]; // per-hour totals for the last 24h (from TransferLog)
}

const TOKEN = "gsc_token";
const ROLE = "gsc_role";

export const auth = {
  get token() { return localStorage.getItem(TOKEN) || ""; },
  get role() { return localStorage.getItem(ROLE) || "user"; },
  set(token: string, role: string) { localStorage.setItem(TOKEN, token); localStorage.setItem(ROLE, role); },
  clear() { localStorage.removeItem(TOKEN); localStorage.removeItem(ROLE); },
};

// SSO landing: after Keycloak login the server redirects to
// /#oidc_token=...&role=... (or /#oidc_error=...). Consume it before the app
// reads auth.token, then scrub the fragment so the token never sticks in the URL.
(() => {
  const h = window.location.hash;
  if (!h.includes("oidc_token=") && !h.includes("oidc_error=")) return;
  const p = new URLSearchParams(h.slice(1));
  const tok = p.get("oidc_token");
  if (tok) auth.set(tok, p.get("role") || "user");
  const err = p.get("oidc_error");
  if (err) sessionStorage.setItem("gsc_oidc_error", err);
  history.replaceState(null, "", window.location.pathname + window.location.search);
})();

async function req(path: string, opts: RequestInit = {}) {
  const headers = new Headers(opts.headers);
  if (auth.token) headers.set("Authorization", `Bearer ${auth.token}`);
  const res = await fetch(`/api${path}`, { ...opts, headers });
  if (!res.ok) {
    if (res.status === 401) {
      auth.clear();
      window.location.reload();
      return new Promise(() => {}) as Promise<Response>;
    }
    const body = await res.json().catch(() => ({}));
    const err = new Error(body.error || res.statusText) as Error & { status?: number; protected?: boolean; owner?: string };
    err.status = res.status;
    err.protected = !!body.protected;
    err.owner = body.owner;
    throw err;
  }
  return res;
}
// appends ?pwd= for operations on password-protected items
const pq = (pwd?: string) => (pwd ? `?pwd=${encodeURIComponent(pwd)}` : "");
const json = (p: string, method: string, body?: any) =>
  req(p, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined })
    .then((r) => r.json());

export const api = {
  // auth + profile
  login: (username: string, password: string) => json("/login", "POST", { username, password }),
  oidcStatus: (): Promise<{ enabled: boolean }> => json("/auth/oidc/status", "GET"),
  me: (): Promise<Me> => json("/me", "GET"),
  changePassword: (oldp: string, newp: string) => json("/me/password", "PUT", { old_password: oldp, new_password: newp }),

  // files
  storage: (): Promise<Storage> => json("/storage", "GET"),
  listFiles: (path = "/", pwd?: string): Promise<FileItem[]> =>
    json(`/files?path=${encodeURIComponent(path)}${pwd ? `&pwd=${encodeURIComponent(pwd)}` : ""}`, "GET").then((r) => r.files),
  createFolder: (path: string, pwd?: string) => json(`/files/folder${pq(pwd)}`, "POST", { path }),
  move: (src: string, dst: string, pwd?: string) => json(`/files/move${pq(pwd)}`, "POST", { src, dst }),
  deletePath: (path: string, pwd?: string) => json(`/files/delete-path${pq(pwd)}`, "POST", { path }),
  deleteFolder: (path: string, pwd?: string) => json(`/files/delete-folder${pq(pwd)}`, "POST", { path }),
  deleteFile: (id: number, pwd?: string) => json(`/files/${id}${pq(pwd)}`, "DELETE"),
  renameFile: (id: number, path: string, pwd?: string) => json(`/files/${id}/rename${pq(pwd)}`, "POST", { path }),

  // password protection (私密)
  getProtect: (path: string): Promise<ProtectInfo> => json(`/files/protect?path=${encodeURIComponent(path)}`, "GET"),
  setProtect: (path: string, isDir: boolean, password: string, pwd?: string) =>
    json(`/files/protect${pq(pwd)}`, "POST", { path, is_dir: isDir, password }),
  unprotect: (path: string, pwd?: string) => json(`/files/unprotect${pq(pwd)}`, "POST", { path }),
  checkAccess: (id?: number, path?: string, pwd?: string): Promise<{ ok: boolean }> => {
    const params = new URLSearchParams();
    if (id !== undefined) params.set("id", String(id));
    if (path) params.set("path", path);
    if (pwd) params.set("pwd", pwd);
    return json(`/files/access-check?${params.toString()}`, "GET");
  },

  // share-link import (转存)
  shareResolve: (link: string, pwd?: string): Promise<ShareResolveResult> =>
    json("/share/resolve", "POST", { link, pwd }),
  shareImport: (p: {
    surl: string; sekey: string; share_id: string; uk: string;
    fs_ids: string[]; total_size: number; target_dir: string; encrypt: boolean;
    rand_name: boolean;
  }, pwd?: string): Promise<{ job_id: string }> =>
    json(`/share/import${pwd ? `?pwd=${encodeURIComponent(pwd)}` : ""}`, "POST", p),

  // system share links
  createShare: (p: { id?: number; path?: string; expires_in: string }): Promise<ShareLinkResult> =>
    json("/shares", "POST", p),
  getShare: (token: string): Promise<ShareInfo> => json(`/shares/${encodeURIComponent(token)}`, "GET"),

  // transfers
  listJobs: (): Promise<JobItem[]> => json("/jobs", "GET").then((r) => r.jobs),
  cancelJob: (id: string) => json(`/jobs/${id}/cancel`, "POST"),
  transferHistory: (page = 1, pageSize = 20): Promise<{ logs: TransferLogItem[]; total: number }> =>
    json(`/transfers/log?page=${page}&page_size=${pageSize}`, "GET")
      .then((r) => ({ logs: r.logs || [], total: r.total || 0 })),
  clearTransferHistory: () => json("/transfers/log", "DELETE"),

  // admin: users
  listUsers: (): Promise<UserItem[]> => json("/admin/users", "GET").then((r) => r.users),
  createUser: (u: UserInput) => json("/admin/users", "POST", u),
  updateUser: (id: number, u: UserInput) => json(`/admin/users/${id}`, "PUT", u),
  setUserStatus: (id: number, status: string) => json(`/admin/users/${id}/status`, "POST", { status }),
  setUserRole: (id: number, role: string) => json(`/admin/users/${id}/role`, "POST", { role }),
  deleteUser: (id: number) => json(`/admin/users/${id}`, "DELETE"),

  // admin: nodes
  listNodes: (): Promise<NodeItem[]> => json("/admin/nodes", "GET").then((r) => r.nodes),
  getBaiduConfig: (): Promise<BaiduConfig> => json("/admin/baidu-config", "GET"),
  saveBaiduConfig: (b: { app_key: string; secret_key: string; sign_key: string; redirect_uri: string }) =>
    json("/admin/baidu-config", "PUT", b),
  oauthUrl: (): Promise<{ url: string; oob: boolean }> => json("/admin/oauth/url", "GET"),
	  oauthExchange: (code: string, nickname: string) => json("/admin/oauth/exchange", "POST", { code, nickname }),
	  updateNode: (id: number, node: { mount_name: string; mount_path: string; remark: string }) => json(`/admin/nodes/${id}`, "PUT", node),
	  listNodeDirectories: (id: number, path: string): Promise<NodeDirectoryList> =>
	    json(`/admin/nodes/${id}/directories?path=${encodeURIComponent(path)}`, "GET"),
	  setNodeStatus: (id: number, status: string) => json(`/admin/nodes/${id}/status`, "POST", { status }),
  refreshNode: (id: number) => json(`/admin/nodes/${id}/refresh`, "POST"),
  syncNodeManifest: (id: number): Promise<{ imported: number; updated: number; skipped: number }> =>
    json(`/admin/nodes/${id}/sync-manifest`, "POST"),
  deleteNode: (id: number) => json(`/admin/nodes/${id}`, "DELETE"),

  // admin: traffic + logs
  traffic: (interval = 30): Promise<Traffic> => json(`/admin/traffic?interval=${interval}`, "GET"),
  logs: (page = 1, pageSize = 20, action = ""): Promise<LogsPage> =>
    json(`/admin/logs?page=${page}&page_size=${pageSize}${action ? `&action=${encodeURIComponent(action)}` : ""}`, "GET")
      .then((r) => ({ ...r, logs: r.logs || [] })),

  // admin: system settings
  getSystemConfig: (): Promise<SystemConfig> => json("/admin/system-config", "GET"),
  saveSystemConfig: (cfg: Partial<SystemConfig>): Promise<SystemConfig> => json("/admin/system-config", "PUT", cfg),
};

export function downloadShareUrl(token: string, pwd?: string): string {
  return `/api/shares/${encodeURIComponent(token)}/download${pwd ? `?pwd=${encodeURIComponent(pwd)}` : ""}`;
}

export function downloadZipUrl(path: string, pwd?: string): string {
  return `/api/files/zip?path=${encodeURIComponent(path)}${pwd ? `&pwd=${encodeURIComponent(pwd)}` : ""}`;
}

export interface ClientInfo {
  mode: "client" | "server";
  configured?: boolean;
  window?: number;
  dl_conns?: number;
  server?: string;
  download_dir?: string;
}

export interface LocalEntry { name: string; path: string; dir: boolean; size: number; }
export interface LocalDirectory { path: string; parent: string; entries: LocalEntry[]; drives?: LocalEntry[]; }
export interface ClientTask {
  id: string;
  kind: "upload" | "download";
  name: string;
  local_path: string;
  remote_path: string;
  size: number;
  done: number;
  status: "waiting" | "hashing" | "running" | "done" | "error" | "canceled";
  // Human-readable current phase from the daemon, e.g. 云端合并处理中 / 网络波动重试
  detail?: string;
  error?: string;
  speed: number;
  created_at: string;
}

// Mode probe: the local CLI client intercepts this path and answers
// mode=client; the server answers mode=server. Lets the UI auto-detect
// whether transfers go through the local client (direct to Baidu) or the server.
export async function clientInfo(): Promise<ClientInfo> {
  try {
    const res = await fetch("/api/client/info");
    if (!res.ok) return { mode: "server" };
    const data = await res.json();
    return data.mode === "client" ? data : { mode: "server" };
  } catch {
    return { mode: "server" };
  }
}

export const clientApi = {
  saveConfig: (config: { server?: string; download_dir?: string }): Promise<ClientInfo> =>
    json("/client/config", "PUT", config),
  localFiles: (path = ""): Promise<LocalDirectory> =>
    json(`/client/fs${path ? `?path=${encodeURIComponent(path)}` : ""}`, "GET"),
  tasks: (): Promise<ClientTask[]> => json("/client/transfers", "GET").then((r) => r.tasks),
  upload: (localPath: string, remoteDir: string): Promise<{ id: string }> =>
    json("/client/transfers/upload", "POST", { local_path: localPath, remote_dir: remoteDir }),
  download: (file: { path: string; name: string; pwd?: string }): Promise<{ id: string }> =>
    json("/client/transfers/download", "POST", { path: file.path, name: file.name, pwd: file.pwd }),
  cancel: (id: string) => json(`/client/transfers/${encodeURIComponent(id)}/cancel`, "POST"),
  remove: (id: string) => json(`/client/transfers/${encodeURIComponent(id)}`, "DELETE"),
};

// CLI client binary downloads served by the backend from $GSC_CLIENT_DIR.
export interface ClientDownloadInfo {
  enabled: boolean;
  platforms: { os: string; arch: string; size: number; missing?: boolean }[];
  // Server-side platform detection from the request User-Agent (arch defaults
  // to amd64). Absent for mobile/unknown platforms.
  detected?: { os: string; arch: string };
}
export async function clientDownloads(): Promise<ClientDownloadInfo> {
  try {
    const res = await fetch("/api/client/download/info");
    if (!res.ok) return { enabled: false, platforms: [] };
    return res.json();
  } catch {
    return { enabled: false, platforms: [] };
  }
}
export function clientDownloadUrl(os: string, arch: string): string {
  return `/api/client/download/${encodeURIComponent(os)}/${encodeURIComponent(arch)}`;
}

export function openProgressSocket(onEvent: (e: any) => void): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/api/ws?token=${auth.token}`);
  ws.onmessage = (m) => { try { onEvent(JSON.parse(m.data)); } catch { /* ignore */ } };
  return ws;
}
