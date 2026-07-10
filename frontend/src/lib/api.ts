// Typed client for the GoSoCode backend.

export interface FileItem {
  id: number; path: string; size: number; status: string; created_at: string; modified: string; is_dir: boolean; owner: string;
  node_id?: number; remote_path?: string; download_url?: string; mount?: boolean;
}
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
}
export interface Me {
  id: number; username: string; role: string; status: string;
  bytes_up: number; bytes_down: number; file_count: number; created_at: string;
}
export interface LogItem { ID: number; At: string; Username: string; Action: string; Detail: string; IP: string; }
export interface BaiduConfig {
  app_key: string; sign_key: string; redirect_uri: string;
  has_secret: boolean; configured: boolean;
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

async function req(path: string, opts: RequestInit = {}) {
  const headers = new Headers(opts.headers);
  if (auth.token) headers.set("Authorization", `Bearer ${auth.token}`);
  const res = await fetch(`/api${path}`, { ...opts, headers });
  if (!res.ok) {
    const body = await res.json().catch(() => ({}));
    throw new Error(body.error || res.statusText);
  }
  return res;
}
const json = (p: string, method: string, body?: any) =>
  req(p, { method, headers: body ? { "Content-Type": "application/json" } : {}, body: body ? JSON.stringify(body) : undefined })
    .then((r) => r.json());

export const api = {
  // auth + profile
  login: (username: string, password: string) => json("/login", "POST", { username, password }),
  register: (username: string, password: string) => json("/register", "POST", { username, password }),
  me: (): Promise<Me> => json("/me", "GET"),
  changePassword: (oldp: string, newp: string) => json("/me/password", "PUT", { old_password: oldp, new_password: newp }),

  // files
  storage: (): Promise<Storage> => json("/storage", "GET"),
  listFiles: (path = "/"): Promise<FileItem[]> => json(`/files?path=${encodeURIComponent(path)}`, "GET").then((r) => r.files),
  createFolder: (path: string) => json("/files/folder", "POST", { path }),
  move: (src: string, dst: string) => json("/files/move", "POST", { src, dst }),
  deletePath: (path: string) => json("/files/delete-path", "POST", { path }),
  deleteFolder: (path: string) => json("/files/delete-folder", "POST", { path }),
  deleteFile: (id: number) => json(`/files/${id}`, "DELETE"),
  renameFile: (id: number, path: string) => json(`/files/${id}/rename`, "POST", { path }),

  // transfers
  listJobs: (): Promise<JobItem[]> => json("/jobs", "GET").then((r) => r.jobs),
  cancelJob: (id: string) => json(`/jobs/${id}/cancel`, "POST"),
  transferHistory: (): Promise<TransferLogItem[]> => json("/transfers/log", "GET").then((r) => r.logs),

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
  deleteNode: (id: number) => json(`/admin/nodes/${id}`, "DELETE"),

  // admin: traffic + logs
  traffic: (interval = 30): Promise<Traffic> => json(`/admin/traffic?interval=${interval}`, "GET"),
  logs: (): Promise<LogItem[]> => json("/admin/logs", "GET").then((r) => r.logs),
};

export function openProgressSocket(onEvent: (e: any) => void): WebSocket {
  const proto = location.protocol === "https:" ? "wss" : "ws";
  const ws = new WebSocket(`${proto}://${location.host}/api/ws?token=${auth.token}`);
  ws.onmessage = (m) => { try { onEvent(JSON.parse(m.data)); } catch { /* ignore */ } };
  return ws;
}
