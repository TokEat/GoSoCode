import { useEffect, useState } from "react";
import { api, Me } from "../lib/api";
import { Stat, Card } from "../ui";
import { humanSize } from "../lib/format";
import { ClientDownloadCard } from "./ClientSettings";

export default function Overview({ isAdmin, clientMode = false, goto }: { isAdmin: boolean; clientMode?: boolean; goto: (p: any) => void }) {
  const [me, setMe] = useState<Me | null>(null);

  useEffect(() => { api.me().then(setMe).catch(() => {}); }, []);

  return (
    <div className="grid" style={{ gap: 20 }}>
      <div className="grid cols-4">
        <Stat label="我上传的文件" value={me?.file_count ?? "—"} />
        <Stat label="累计上传" value={humanSize(me?.bytes_up ?? 0)} />
        <Stat label="累计下载" value={humanSize(me?.bytes_down ?? 0)} />
        <Stat label="账号角色" value={me?.role === "admin" ? "管理员" : "普通用户"} sub={me?.username} />
      </div>

      <Card title="快速开始">
        <ol style={{ margin: 0, paddingLeft: 18, lineHeight: 2, color: "#475569" }}>
          {isAdmin && <li>前往 <b>网盘账号</b> 接入一个或多个百度网盘，扩展共享空间</li>}
          <li>在 <b>文件管理</b> 上传文件，所有成员共享同一个文件空间</li>
          <li>在 <b>传输管理</b> 查看实时进度、速度和预计剩余时间</li>
          {isAdmin && <li>在 <b>流量检测</b> 与 <b>系统日志</b> 掌握平台运行情况</li>}
        </ol>
        <div className="row" style={{ marginTop: 16 }}>
          <button className="btn" onClick={() => goto("files")}>去上传文件</button>
          {isAdmin && <button className="btn ghost" onClick={() => goto("nodes")}>管理存储节点</button>}
        </div>
      </Card>

      {!clientMode && <ClientDownloadCard />}
    </div>
  );
}
