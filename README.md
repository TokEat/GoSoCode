# GoSoCode

> 百度网盘账号池 · 共享文件系统 · 多端传输

GoSoCode 把多个百度网盘账号聚合成"存储节点"，对平台用户呈现为一个统一的逻辑文件系统。
支持浏览器流式上传、客户端直连加速、轻量运维。

- **后端**: Go + Gin + GORM + SQLite (Redis 可选)
- **前端**: React + TypeScript + Vite
- **客户端**: 同仓库的 Go CLI，数据面绕过服务端直接连百度

---

## 功能特性

- 多百度账号聚合为一个共享文件系统
- 元数据由本地 SQLite 维护，列表/搜索不依赖百度接口
- 三条传输路径 (任选):
  - **浏览器流式**: 分片秒传 + 云端同步实时回显
  - **先存后转**: 临时落盘，异步推送到云
  - **客户端直连**: 数据不经服务端，绕过浏览器限速
- 自动账号选择 (容量感知 + 单飞令牌刷新 + 配额刷新)
- 分布式锁 (路径并发安全) + 每账号 QPS 限流
- 实时进度 (WebSocket) + 流量计量
- 跨平台发布 (Linux/Windows/macOS × amd64/arm64)

---

## 快速开始

### 1. 启动服务端

```bash
cd backend
go run ./cmd/server
# 监听 :8080，首次启动自动创建并迁移 gosocode.db
```

打开 http://localhost:8080 ，使用种子管理员登录:

```
用户名: admin
密码:   admin@qwe123
```

**首次登录后请立即修改密码** (`设置` → `修改密码`)。

### 2. 添加百度账号 (管理员)

进入 `管理` → `账号池`，点击 `添加账号`：

1. 在百度网盘开放平台创建一个应用
2. 把 AppKey / SecretKey 粘贴到平台 `管理员 → 设置`
3. 在账号列表点击 `添加账号`，跳转百度 OAuth 授权
4. 同一个 AppKey 可以绑定任意多个百度账号

### 3. 下载客户端 (可选，提升上传/下载速度)

进入 `客户端设置` 页面，自动识别你的操作系统，点击 `下载` 即可。
客户端是一个本地守护进程，数据在本机和百度之间直接传输，不经过平台服务器。

```bash
# 解压后 (Windows 用 .exe)
./client -server http://localhost:8080
# 默认仅监听 127.0.0.1:8190，浏览器访问 http://localhost:8190 即可
```

客户端会接管全部 UI，浏览器看到的页面就是同一个，只是上传/下载的数据走本机。

---

## 命令清单

### 后端 (`backend/`)

```bash
go run ./cmd/server                      # 服务端 :8080
go run ./cmd/client -server <URL>        # 客户端 :8090

go build ./... && go vet ./...           # 编译 + 静态检查
```

### 前端 (`frontend/`)

```bash
npm install
npm run dev                              # vite :5173，代理 /api 到 :8080
npm run build                            # tsc -b && vite build，输出到 dist/
```

### 调试参数

```bash
VITE_API_TARGET=http://localhost:8090 npm run dev   # 客户端模式下开发前端
GSC_WEBUI=./frontend/dist ./server                  # 用本地 dist 替换内嵌 UI
```

---

## 配置 (环境变量)

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GSC_ADDR` | `:8080` | HTTP 监听地址（生产环境应由 HTTPS 反向代理对外提供服务） |
| `GSC_DB` | `gosocode.db` | SQLite 文件路径 |
| `GSC_JWT_SECRET` | `dev-insecure-change-me` | JWT 签名密钥 (生产必改) |
| `GSC_REDIS` | _(空)_ | Redis 地址；空 = 内存协调 (单实例) |
| `GSC_APP_NAME` | `gosocode` | 百度 OpenAPI 应用名，决定沙盒目录 `/apps/<AppName>/` |
| `GSC_BAIDU_APPKEY` / `GSC_BAIDU_SECRET` | _(空)_ | OAuth 凭证 (推荐改为在管理后台填) |
| `GSC_REDIRECT_URI` | `http://localhost:8080/api/oauth/callback` | OAuth 回调地址，需与百度应用配置一致 |
| `GSC_STREAM_ENABLED` | `1` | 浏览器流式上传开关 |
| `GSC_STREAM_MAX_CONCURRENT` | `8` | 同时进行的流式上传会话数 |
| `GSC_STREAM_WINDOW` | `3` | 流式上传飞行分片数 |
| `GSC_AGENT_ENABLED` | `1` | 是否允许 CLI 客户端直连 (会下发 access_token) |
| `GSC_AGENT_CONTROL_KEY` | _(空)_ | 64 位十六进制密钥；启用 CLI 直连时必须与每个客户端的同名变量一致，用于 AES-256-GCM 加密控制面报文 |
| `GSC_AGENT_WINDOW` | `8` | 客户端飞行分片数 (本机 leg 免费可加大) |
| `GSC_WEBUI` | `webui` | 内嵌 UI 的目录覆盖 |
| `GSC_CLIENT_DIR` | _(空)_ | CLI 客户端二进制目录，启动后可通过 Web 下载 |

---

## 部署

### 单机 (最简)

```bash
./server
# 监听 :8080，SQLite 在 ./gosocode.db
```

放在 nginx/systemd 后面即可，参考 `docs/` 下的部署说明。

### 生产清单

- [ ] 设置 `GSC_JWT_SECRET` 为强随机值
- [ ] 反向代理 (nginx) + HTTPS
- [ ] 定时备份 `gosocode.db` (元数据是权威，丢了没法重建)
- [ ] 配置 `GSC_REDIS` 启用分布式协调 (多实例时必填)
- [ ] 把 release tarball 解压到 `GSC_CLIENT_DIR`，Web UI 自动提供下载

---

## 客户端二进制分发

项目自带 GitHub Actions 发布流水线 (`.github/workflows/release.yml`)。推送 tag 自动:

1. 交叉编译 6 个平台二进制
2. 同时输出按平台打包的 tar.gz / zip 和一份 `gosocode-client-binaries-<tag>.tar.gz`
3. 附件附加到 GitHub Release

运营侧只要把那份 `client-binaries` tarball 解压到 `GSC_CLIENT_DIR` 指向的目录，
Web 端的 `客户端设置` 页面就会自动出现下载按钮。

下载流程见 [docs/client-mode.md](docs/client-mode.md)。

---

## 项目结构

```
backend/
  cmd/
    server/                # 服务端入口
    client/                # CLI 客户端入口
  internal/
    api/                   # gin handlers + 路由
    account/               # 账号池 / 选盘 / token 刷新
    baidu/                 # 百度 OpenAPI 客户端
    coord/                 # 分布式锁 + 限流 (内存/Redis)
    job/                   # worker pool + janitor
    models/                # GORM 模型
    realtime/              # WebSocket hub
    metrics/               # 流量计量
    store/                 # SQLite + 自动迁移
    webui/                 //go:embed 前端产物
    config/                # 环境变量配置

frontend/
  src/
    pages/                 # 各页面 (Files, Transfers, Login, ...)
    lib/                   # API 客户端
    transfer.tsx           # 传输引擎 (流式上传 + 范围下载)
    LocalFilePicker.tsx    # 客户端模式下的本地文件选择

docs/
  streaming-upload-sync.md # 流式上传设计
  client-mode.md           # 客户端模式
  baidu-client-optimizations.md
```

---

## 安全须知

- **不要在公网裸跑默认 JWT secret**。第一件事就是改 `GSC_JWT_SECRET`。
- `GSC_AGENT_ENABLED=1` 时，平台会向 CLI 客户端下发百度 access_token。必须设置同一份 `GSC_AGENT_CONTROL_KEY` 到服务端与客户端，控制请求中的 JWT、参数和 agent 响应中的令牌均使用 AES-256-GCM 加密；仍建议在生产环境部署 HTTPS。
  这等于把整个百度账号的控制权交给运行客户端的人 — 仅在可信部署中开启。
- OAuth AppKey / SecretKey 可以通过环境变量传入，**也可以在管理后台填并保存到 DB**。
  管理后台填写的优先级更高，便于不重启滚动更新。
- 百度账号授权是 OAuth 2.0 标准流程，**不要把百度账号密码交给 GoSoCode**。

---

## 文档

- [docs/streaming-upload-sync.md](docs/streaming-upload-sync.md) — 浏览器流式上传原理
- [docs/client-mode.md](docs/client-mode.md) — 客户端模式与 Agent 接口
- [docs/baidu-client-optimizations.md](docs/baidu-client-optimizations.md) — 百度 API 调优笔记

---

## 许可证

MIT
