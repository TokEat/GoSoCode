# 客户端直连模式（client/server 架构）

状态：已实现。

## 目标

把系统拆为 **server（控制面）** 与 **client（数据面，可选）** 两端，两端绑定同一个 Web UI：

- **server 模式**（原有形态）：浏览器 ⇄ 服务器 ⇄ 百度，文件字节经过服务器中转。
- **client 模式**：本机运行 `gsc-client`，浏览器访问客户端本地入口。文件上传/下载由**本机直连百度**（原生进程可设置 `User-Agent: pan.baidu.com`、无 CORS 限制、可多连接），字节不经过平台服务器；并且加大传输窗口（上传并发分片、下载并发连接）。
- 账号 token、文件元数据、节点调度、会话与流量记账**始终在服务端**；客户端启动时必须先连通服务端，否则拒绝启动。

Web UI 通过 `GET /api/client/info` 自动识别绑定对象：服务端答 `{"mode":"server"}`，客户端拦截同一路径答 `{"mode":"client", window, dl_conns}`，UI 顶栏显示「客户端直连」徽标，传输引擎自动采用更大的窗口（plan 响应中的 `window` 已被客户端改写）。

## 运行

```bash
# 服务端（不变）
go run ./cmd/server                        # :8080；GSC_WEBUI 指向前端 dist 可直接托管页面

# 客户端
go run ./cmd/client -server http://<服务端>:8080 [-listen :8090] [-window 8] [-dl-conns 4] [-webui <dist目录>]
# 然后浏览器访问 http://localhost:8090（-webui 留空时页面由服务端代理提供）
```

前端开发时把 vite 代理指向客户端即可体验 client 模式：`VITE_API_TARGET=http://localhost:8090 npm run dev`。

## 数据流

```
上传（client 模式）
浏览器 ──plan/init/slice(localhost)──▶ gsc-client ──superfile2 分片──▶ 百度 d.pcs
                                        │  init/进度/finish（控制面，小请求）
                                        ▼
                                      server ──precreate/create/元数据/记账──▶ 百度 pan API

下载（client 模式）
浏览器 ◀──顺序字节流(localhost)── gsc-client ◀══N 条并发 Range 连接══ 百度 CDN
                                        │ resolve dlink / 流量上报
                                        ▼
                                      server
```

- **上传**：客户端拦截 `/api/files/stream/init|slice`，经 `/api/agent/upload/init` 拿到 `uploadid + 节点 token + remote_path`，分片本机校验 MD5 后直传 superfile2（幂等，窗口默认 8），每片成功后 `POST /api/agent/upload/progress` 同步服务端位图（finish/断点续传仍以服务端位图为准）；`finish/status/cancel` 原样代理给服务端（create/记账在服务端执行）。分片 200 = 已到云端 + 已记录，浏览器端语义与 server 模式完全一致，前端传输引擎零改动。
- **下载**：客户端拦截 `/api/files/:id/download`，经 `/api/agent/download/resolve` 拿 `dlink + token`，按 8MB 分段、默认 4 条连接并发拉取、按序组装回浏览器，支持 Range 断点续传，dlink 过期自动重解析一次；每 32MB `POST /api/agent/download/report` 上报流量（实时图表 + 累计记账）。

## 服务端新增接口（`/api/agent/*`，JWT 认证）

| 接口 | 作用 |
|---|---|
| `POST /agent/upload/init` | 同 stream init，额外返回 `access_token`、`remote_path`；不占服务端流式并发额度 |
| `GET /agent/upload/session` | 客户端重启后重建本地会话状态 |
| `POST /agent/upload/progress` | 直传分片后同步服务端位图 + 实时上行流量 |
| `GET /agent/download/resolve` | 下发 `dlink + access_token + UA` |
| `POST /agent/download/report` | 直连下载流量记账（meter + 用户/节点累计） |

另有公开探测端点 `GET /api/health`、`GET /api/client/info`。

## 安全边界（重要）

agent 接口会把节点的 `access_token` 交给客户端——**等同于该百度账号的完全控制权**（百度 OpenAPI 无受限令牌、无 revoke，泄露的 token 最长 30 天有效）。因此：

- 客户端模式只适合**自部署 / 受信成员**场景，不适合完全开放注册的平台；
- 可用 `GSC_AGENT_ENABLED=0` 整体关闭 agent 接口（客户端会退化为无法上传下载）；
- 依旧只使用官方 OpenAPI（superfile2 / dlink），不触碰 BDUSS 等封号高危路径。

## 配置

| 环境变量 / 参数 | 默认 | 说明 |
|---|---|---|
| `GSC_AGENT_ENABLED` | `1` | 服务端 agent 接口开关 |
| `GSC_AGENT_WINDOW` | `8` | agent init 响应中的建议上传窗口 |
| `GSC_WEBUI` | `webui` | 服务端静态 Web UI 目录（存在才生效，SPA 回退 index.html） |
| client `-window` | `8` | 上传并发分片窗口（覆盖 plan 的 window） |
| client `-dl-conns` | `4` | 下载并发连接数（上限 8） |
| client `-webui` | 空 | 本地静态页面目录；留空则代理服务端页面 |

## 已知取舍

- 直传 superfile2 不经过服务端的 per-account QPS 限流；靠客户端窗口上限约束并发（数据面请求对 QPS 的敏感度远低于控制面 JSON 接口，控制面仍全部限流）。
- 流量记账依赖客户端上报（服务端按 need_seqs/文件大小可校验上传侧；下载侧为信任模型的一部分）。
