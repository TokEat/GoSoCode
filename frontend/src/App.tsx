import { ReactNode, useEffect, useState } from "react";
import { auth, clientInfo, ClientInfo, openProgressSocket } from "./lib/api";
import { ToastProvider, DialogProvider } from "./ui";
import { TransferProvider, useTransfers } from "./transfer";
import Login from "./pages/Login";
import Overview from "./pages/Overview";
import Files from "./pages/Files";
import Transfers from "./pages/Transfers";
import Profile from "./pages/Profile";
import Users from "./pages/Users";
import Nodes from "./pages/Nodes";
import Traffic from "./pages/Traffic";
import Logs from "./pages/Logs";
import ClientSettings from "./pages/ClientSettings";

export interface JobEvent {
  job_id: string; status: string; progress: number; logical_path: string; error?: string;
}

type Page =
  | "overview" | "files" | "transfers" | "profile"
  | "users" | "nodes" | "traffic" | "logs" | "client";

const USER_NAV: { key: Page; label: string; ico: string }[] = [
  { key: "overview", label: "概览", ico: "▦" },
  { key: "files", label: "文件管理", ico: "🗂" },
  { key: "transfers", label: "传输管理", ico: "⇅" },
  { key: "profile", label: "个人资料", ico: "👤" },
];
const ADMIN_NAV: { key: Page; label: string; ico: string }[] = [
  { key: "users", label: "用户管理", ico: "👥" },
  { key: "nodes", label: "网盘账号", ico: "🖴" },
  { key: "traffic", label: "流量检测", ico: "📈" },
  { key: "logs", label: "系统日志", ico: "📜" },
];
const CLIENT_NAV: { key: Page; label: string; ico: string }[] = [
  { key: "client", label: "客户端设置", ico: "⌁" },
];

export default function App() {
  const [loggedIn, setLoggedIn] = useState(!!auth.token);
  const [runtime, setRuntime] = useState<ClientInfo | null>(null);

  useEffect(() => { clientInfo().then(setRuntime); }, []);

  if (!runtime) return <div className="app-loading"><span>G</span><p>正在启动 GoSoCode…</p></div>;

  const providers = (content: ReactNode) => (
    <ToastProvider>
      <DialogProvider>{content}</DialogProvider>
    </ToastProvider>
  );

  if (runtime.mode === "client" && !runtime.configured) {
    return providers(<ClientSettings info={runtime} firstRun onSaved={(next) => setRuntime(next)} />);
  }

  return (
    <ToastProvider>
      <DialogProvider>
        <TransferProvider mode={runtime.mode}>
          {loggedIn
            ? <Shell
                runtime={runtime}
                onRuntimeChange={setRuntime}
                onLogout={() => { auth.clear(); setLoggedIn(false); }}
              />
            : <Login onDone={() => setLoggedIn(true)} />}
        </TransferProvider>
      </DialogProvider>
    </ToastProvider>
  );
}

function Shell({ runtime, onRuntimeChange, onLogout }: {
  runtime: ClientInfo;
  onRuntimeChange: (info: ClientInfo) => void;
  onLogout: () => void;
}) {
  const [page, setPage] = useState<Page>("overview");
  const [jobs, setJobs] = useState<Record<string, JobEvent>>({});
  const clientMode = runtime.mode === "client";
  const isAdmin = auth.role === "admin";
  const { resolveJob } = useTransfers();

  // Single WebSocket for the whole session; feeds live transfer progress and
  // flips a finalizing upload to done/error once the server->Baidu leg finishes.
  useEffect(() => {
    // Client-mode progress is owned entirely by the local daemon and polled via
    // /api/client/transfers. Do not open the server WebSocket in this mode.
    if (clientMode) return;
    const ws = openProgressSocket((e: JobEvent) => {
      if (!e.job_id) return;
      setJobs((p) => ({ ...p, [e.job_id]: e }));
      if (e.status === "done" || e.status === "failed") resolveJob(e.job_id, e.status, e.error);
    });
    return () => ws.close();
  }, [clientMode, resolveJob]);

  const active = [...USER_NAV, ...ADMIN_NAV, ...CLIENT_NAV].find((n) => n.key === page);

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          <span className="logo">G</span>
          <div>
            <div className="brand-name">GoSoCode</div>
            <small>云盘聚合 · 共享文件空间</small>
          </div>
        </div>
        <nav className="nav">
          <div className="nav-group">工作台</div>
          {USER_NAV.map((n) => (
            <div key={n.key} className={`nav-item ${page === n.key ? "active" : ""}`} onClick={() => setPage(n.key)}>
              <span className="ico">{n.ico}</span>{n.label}
            </div>
          ))}
          {isAdmin && <>
            <div className="nav-group">管理后台</div>
            {ADMIN_NAV.map((n) => (
              <div key={n.key} className={`nav-item ${page === n.key ? "active" : ""}`} onClick={() => setPage(n.key)}>
                <span className="ico">{n.ico}</span>{n.label}
              </div>
            ))}
          </>}
          {clientMode && <>
            <div className="nav-group">本地客户端</div>
            {CLIENT_NAV.map((n) => (
              <div key={n.key} className={`nav-item ${page === n.key ? "active" : ""}`} onClick={() => setPage(n.key)}>
                <span className="ico">{n.ico}</span>{n.label}
              </div>
            ))}
          </>}
        </nav>
        <div className="sidebar-foot">
          <div className="nav-item" onClick={onLogout}><span className="ico">⎋</span>退出登录</div>
        </div>
      </aside>

      <main className="main">
        <header className="topbar">
          <h1>{active?.label}</h1>
          <div className="row">
            {clientMode && <span className="badge green" title="浏览器只查看进度，文件由本机客户端直连云端">本机数据通道</span>}
            <span className={`badge ${isAdmin ? "blue" : "gray"}`}>{isAdmin ? "管理员" : "用户"}</span>
          </div>
        </header>
        <section className="content">
          {page === "overview" && <Overview isAdmin={isAdmin} goto={setPage} />}
          {page === "files" && <Files jobs={jobs} />}
          {page === "transfers" && <Transfers />}
          {page === "profile" && <Profile />}
          {page === "users" && isAdmin && <Users />}
          {page === "nodes" && isAdmin && <Nodes />}
          {page === "traffic" && isAdmin && <Traffic />}
          {page === "logs" && isAdmin && <Logs />}
          {page === "client" && clientMode && (
            <ClientSettings
              info={runtime}
              onSaved={(next, serverChanged) => {
                onRuntimeChange(next);
                if (serverChanged) { auth.clear(); window.location.reload(); }
              }}
            />
          )}
        </section>
      </main>
    </div>
  );
}
