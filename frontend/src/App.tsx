import { ReactNode, useEffect, useRef, useState } from "react";
import { auth, api, clientInfo, ClientInfo, openProgressSocket } from "./lib/api";
import { ToastProvider, DialogProvider } from "./ui";
import { TransferProvider, useTransfers } from "./transfer";
import Login from "./pages/Login";
import Overview from "./pages/Overview";
import Files from "./pages/Files";
import ShareImport from "./pages/ShareImport";
import Transfers from "./pages/Transfers";
import Profile from "./pages/Profile";
import Users from "./pages/Users";
import Nodes from "./pages/Nodes";
import Traffic from "./pages/Traffic";
import Logs from "./pages/Logs";
import Settings from "./pages/Settings";
import ClientSettings from "./pages/ClientSettings";
import TransferDrawer from "./TransferDrawer";
import SharePage from "./pages/SharePage";

export interface JobEvent {
  job_id: string; status: string; progress: number; logical_path: string; error?: string;
}

type Page =
  | "overview" | "files" | "share" | "transfers" | "profile"
  | "users" | "nodes" | "traffic" | "logs" | "settings" | "client";

const PAGES: Page[] = ["overview", "files", "share", "transfers", "profile", "users", "nodes", "traffic", "logs", "settings", "client"];

// Hash-based routing (#/files, #/transfers, …) so a refresh or a shared link
// lands on the same page instead of resetting to the overview.
function pageFromHash(): Page {
  const h = window.location.hash.replace(/^#\/?/, "").split("?")[0];
  return PAGES.includes(h as Page) ? (h as Page) : "overview";
}

const USER_NAV: { key: Page; label: string; ico: string }[] = [
  { key: "overview", label: "概览", ico: "▦" },
  { key: "files", label: "文件管理", ico: "🗂" },
  { key: "share", label: "分享转存", ico: "🔗" },
  { key: "transfers", label: "传输管理", ico: "⇅" },
  { key: "profile", label: "个人资料", ico: "👤" },
];
const ADMIN_NAV: { key: Page; label: string; ico: string }[] = [
  { key: "users", label: "用户管理", ico: "👥" },
  { key: "nodes", label: "网盘账号", ico: "🖴" },
  { key: "traffic", label: "流量检测", ico: "📈" },
  { key: "logs", label: "系统日志", ico: "📜" },
  { key: "settings", label: "系统设置", ico: "⚙" },
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

  // Public share landing page uses pathname routing, not the hash router.
  if (window.location.pathname.startsWith("/s/")) {
    return providers(<SharePage />);
  }

  return (
    <ToastProvider>
      <DialogProvider>
        {/* keyed on login state: switching accounts remounts the provider so
            in-memory transfers reset and persistence reloads under the new
            user's namespace (see storeKey in transfer.tsx) */}
        <TransferProvider key={loggedIn ? "in" : "out"} mode={runtime.mode}>
          {loggedIn
            ? <Shell
                runtime={runtime}
                onRuntimeChange={setRuntime}
                onLogout={() => { auth.clear(); setLoggedIn(false); }}
              />
            : <Login onDone={() => setLoggedIn(true)} runtime={runtime} />}
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
  const clientMode = runtime.mode === "client";
  const isAdmin = auth.role === "admin";
  const guard = (p: Page): Page =>
    (!isAdmin && ADMIN_NAV.some((n) => n.key === p)) || (p === "client" && !clientMode) ? "overview" : p;
  const [page, setPageState] = useState<Page>(() => guard(pageFromHash()));
  const setPage = (p: Page) => { window.location.hash = "/" + p; };
  useEffect(() => {
    const onHash = () => setPageState(guard(pageFromHash()));
    window.addEventListener("hashchange", onHash);
    return () => window.removeEventListener("hashchange", onHash);
  }, []);
  const [collapsed, setCollapsed] = useState(false);
  const [jobs, setJobs] = useState<Record<string, JobEvent>>({});
  const { resolveJob } = useTransfers();

  const resolvedJobs = useRef(new Set<string>());

  // Single WebSocket for the whole session; feeds live transfer progress and
  // flips a finalizing upload to done/error once the server->Baidu leg finishes.
  // We also poll /api/jobs as a fallback so a missed WS event (e.g. reload,
  // reconnect) does not leave legacy uploads stuck at "云端处理中".
  useEffect(() => {
    // Client-mode progress is owned entirely by the local daemon and polled via
    // /api/client/transfers. Do not open the server WebSocket in this mode.
    if (clientMode) return;
    const ws = openProgressSocket((e: JobEvent) => {
      if (!e.job_id) return;
      setJobs((p) => ({ ...p, [e.job_id]: e }));
      if (e.status === "done" || e.status === "failed") {
        resolvedJobs.current.add(e.job_id);
        resolveJob(e.job_id, e.status, e.error);
      } else {
        // running: feeds progress of server-side jobs (e.g. 分享转存)
        resolveJob(e.job_id, e.status, e.error, e.progress);
      }
    });

    const pollJobs = async () => {
      try {
        const jobs = await api.listJobs();
        for (const j of jobs) {
          if (j.Status !== "done" && j.Status !== "failed") continue;
          if (resolvedJobs.current.has(j.ID)) continue;
          resolvedJobs.current.add(j.ID);
          setJobs((p) => ({ ...p, [j.ID]: { job_id: j.ID, status: j.Status, progress: j.Progress, logical_path: "", error: j.Error } }));
          resolveJob(j.ID, j.Status === "done" ? "done" : "failed", j.Error);
        }
      } catch { /* ignore */ }
    };
    const timer = setInterval(pollJobs, 3000);
    pollJobs();

    return () => { ws.close(); clearInterval(timer); };
  }, [clientMode, resolveJob]);

  const active = [...USER_NAV, ...ADMIN_NAV].find((n) => n.key === page);
  const activeLabel = page === "client" ? "客户端设置" : active?.label;

  return (
    <div className="app">
      <aside className={`sidebar ${collapsed ? "collapsed" : ""}`}>
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
            <div key={n.key} className={`nav-item ${page === n.key ? "active" : ""}`} title={n.label} onClick={() => setPage(n.key)}>
              <span className="ico">{n.ico}</span><span className="label">{n.label}</span>
            </div>
          ))}
          {isAdmin && <>
            <div className="nav-group">管理后台</div>
            {ADMIN_NAV.map((n) => (
              <div key={n.key} className={`nav-item ${page === n.key ? "active" : ""}`} title={n.label} onClick={() => setPage(n.key)}>
                <span className="ico">{n.ico}</span><span className="label">{n.label}</span>
              </div>
            ))}
          </>}
          {clientMode && <>
            <div className="nav-group">本地客户端</div>
            <div className={`nav-item ${page === "client" ? "active" : ""}`} title="客户端设置" onClick={() => setPage("client")}>
              <span className="ico">⌁</span><span className="label">客户端设置</span>
            </div>
          </>}
        </nav>
        <div className="sidebar-foot">
          <div className="nav-item" title={collapsed ? "展开边栏" : "收起边栏"} onClick={() => setCollapsed(!collapsed)}>
            <span className="ico">{collapsed ? "»" : "«"}</span><span className="label">收起边栏</span>
          </div>
          <div className="nav-item" title="退出登录" onClick={onLogout}><span className="ico">⎋</span><span className="label">退出登录</span></div>
        </div>
      </aside>

      <main className="main">
        <header className="topbar">
          <h1>{activeLabel}</h1>
          <div className="row">
            {clientMode && <span className="badge green" title="浏览器只查看进度，文件由本机客户端直连云端">本机数据通道</span>}
            <span className={`badge ${isAdmin ? "blue" : "gray"}`}>{isAdmin ? "管理员" : "用户"}</span>
          </div>
        </header>
        <section className="content">
          {page === "overview" && <Overview isAdmin={isAdmin} clientMode={clientMode} goto={setPage} />}
          {/* Files stays mounted so the browsed directory (and entered
              passwords) survive switching to other pages and back. */}
          <div style={{ display: page === "files" ? undefined : "none" }}>
            <Files jobs={jobs} />
          </div>
          {page === "share" && <ShareImport />}
          {page === "transfers" && <Transfers />}
          {page === "profile" && <Profile />}
          {page === "users" && isAdmin && <Users />}
          {page === "nodes" && isAdmin && <Nodes />}
          {page === "traffic" && isAdmin && <Traffic />}
          {page === "logs" && isAdmin && <Logs />}
          {page === "settings" && isAdmin && <Settings />}
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
        <TransferDrawer hidden={page !== "files"} onViewAll={() => setPage("transfers")} />
      </main>
    </div>
  );
}
