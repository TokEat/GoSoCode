# GoSoCode 开发文档

本文档面向开发者和自部署运维，说明如何在本地运行、配置、构建和部署 GoSoCode。使用者请先看仓库根目录的 [README](../README.md) 和 [使用手册](user-manual.md)。

## 技术栈

- **后端**：Go + Gin + GORM + SQLite（Redis 可选，多实例时用于分布式锁和限流）
- **前端**：React + TypeScript + Vite
- **客户端**：同仓库的 Go CLI。数据面绕过服务端，本机直连百度网盘

界面文案和面向用户的错误信息使用中文。

## 本地开发

### 启动服务端

```bash
cd backend
go run ./cmd/server
# 监听 :8080，首次启动会自动创建并迁移 gosocode.db
```

打开 http://localhost:8080 。若数据库是新建的，服务端会创建一个 `admin` 账号：密码取自环境变量 `GSC_ADMIN_PASSWORD`，若未设置则随机生成，并在启动日志中打印一次。**首次登录后请立即修改密码**（「个人资料」或「设置」→「修改密码」）。不要把生产环境的真实密码写进仓库或文档。

### 启动前端（可选）

服务端可以托管已构建的前端。本地改 UI 时：

```bash
cd frontend
npm install
npm run dev          # Vite :5173，/api 和 WebSocket 代理到 :8080
npm run build        # tsc -b && vite build，产物在 dist/
```

调试参数：

```bash
VITE_API_TARGET=http://localhost:8090 npm run dev   # 前端连到客户端模式
GSC_WEBUI=./frontend/dist ./server                  # 用本地 dist 替换内嵌 UI
```

### 添加百度账号

1. 在百度网盘开放平台创建应用
2. 把 AppKey / SecretKey 填到管理后台「系统设置」，或使用环境变量（后台填写优先）
3. 在「网盘账号」中添加账号，走 OAuth 授权（支持免回调、粘贴授权码）
4. 同一个 AppKey 可以绑定多个百度账号

### 本机客户端

```bash
go run ./cmd/client -server http://localhost:8080
# 默认监听 127.0.0.1:8190，浏览器访问 http://localhost:8190
```

服务端与客户端需配置同一份 `GSC_AGENT_CONTROL_KEY`（64 位十六进制）。更完整的说明见 [client-mode.md](client-mode.md)。

## 常用命令

后端（在 `backend/` 下）：

```bash
go run ./cmd/server                      # 服务端 :8080
go run ./cmd/client -server <URL>        # 客户端，默认 :8190
go test ./internal/crypto/ ./internal/manifest/ ./internal/api/ ./internal/clientd/
go build ./cmd/server && go build ./... && go vet ./...
```

前端（在 `frontend/` 下）：

```bash
npm install
npm run dev
npm test                                 # vitest
npm run build
```

## 配置（环境变量）

| 变量 | 默认值 | 说明 |
|---|---|---|
| `GSC_ADDR` | `:8080` | HTTP 监听地址（生产环境应由 HTTPS 反向代理对外提供服务） |
| `GSC_DB` | `gosocode.db` | SQLite 文件路径 |
| `GSC_ENV` | _(空)_ | `production`/`prod` 按生产环境处理；`development`/`dev`/`test` 强制为开发。未设置时，`GIN_MODE=release` 也视为生产 |
| `GSC_JWT_SECRET` | 开发占位值（不安全） | JWT 签名密钥。开发环境未设置会大声告警；生产环境（见 `GSC_ENV` / `GIN_MODE`）拒绝以占位值或空值启动 |
| `GSC_ADMIN_PASSWORD` | _(空)_ | 仅在空库首次启动时使用的初始管理员密码；未设置则随机生成并打印到日志 |
| `GSC_REDIS` | _(空)_ | Redis 地址；空 = 内存协调（单实例） |
| `GSC_APP_NAME` | `gosocode` | 百度 OpenAPI 应用名，决定沙盒目录 `/apps/<AppName>/` |
| `GSC_BAIDU_APPKEY` / `GSC_BAIDU_SECRET` | _(空)_ | OAuth 凭证（推荐在管理后台填写） |
| `GSC_REDIRECT_URI` | `http://localhost:8080/api/oauth/callback` | OAuth 回调地址，需与百度应用配置一致 |
| `GSC_STREAM_ENABLED` | `1` | 浏览器流式上传开关 |
| `GSC_STREAM_MAX_CONCURRENT` | `8` | 同时进行的流式上传会话数 |
| `GSC_STREAM_WINDOW` | `3` | 流式上传飞行分片数。内存紧张时不要盲目加大：SVIP 分片 32MB，窗口与并发会话会叠加占用内存 |
| `GSC_AGENT_ENABLED` | `1` | 是否允许 CLI 客户端直连（会下发 access_token） |
| `GSC_AGENT_CONTROL_KEY` | _(空)_ | 64 位十六进制密钥；启用 CLI 直连时必须与每个客户端的同名变量一致，用于 AES-256-GCM 加密控制面报文 |
| `GSC_AGENT_WINDOW` | `8` | 客户端飞行分片数 |
| `GSC_WEBUI` | `webui` | 内嵌 UI 的目录覆盖 |
| `GSC_CLIENT_DIR` | _(空)_ | CLI 客户端二进制目录，启动后可在网页下载 |
| `GSC_NOTES_DIR` | `gsc-notes` | 备注图片等本地存储目录 |

## 架构要点

- **文件列表**是从百度网盘实时拉取当前目录；本地 SQLite 保存上传映射、UUID 文件名、加密标记、私密规则和备注等元数据，不是列表的唯一来源。
- **全局搜索**会先查本地元数据，再对每个活跃节点做递归网盘搜索，合并去重后返回。
- **上传有三条路径**：浏览器流式（默认，分片校验后可秒传，切片同步转发到百度）、先存后转（临时落盘再异步推送）、客户端直连（数据不经服务端）。
- **传输**：浏览器上传前会并行计算分片 MD5 并显示校验进度；遇到短暂网络错误会自动恢复（默认最多 3 次）。401/403/404 和用户取消不会自动重试。
- **清单**（各节点根目录的 `.gsc-manifest.json`）经 `SyncNow` 写回。禁止用空清单覆盖网盘上已有的非空清单，以免丢失映射和密钥。
- **下载保护**：文件已标记加密但缺少密钥时，下载会在发起网盘请求前中止；未标记加密但内容看起来像密文时，同样立即返回错误，避免用户长时间下载无法打开的数据。
- 删除在平台侧是软删除；远端删除是尽力而为。不要绕过平台直接在百度网盘里改挂载目录，否则对账后可能出现名称或路径漂移。

更细的设计见：

- [streaming-upload-sync.md](streaming-upload-sync.md) — 浏览器流式上传
- [client-mode.md](client-mode.md) — 客户端模式与 Agent 接口
- [baidu-client-optimizations.md](baidu-client-optimizations.md) — 百度 API 调优

## 项目结构

```
backend/
  cmd/server/              # 服务端入口
  cmd/client/              # CLI 客户端入口
  internal/
    api/                   # Gin handlers 与路由
    account/               # 账号池 / 选盘 / token 刷新
    baidu/                 # 百度 OpenAPI 客户端
    coord/                 # 分布式锁 + 限流（内存/Redis）
    crypto/                # 加密与密文探测
    manifest/              # 元数据清单及空写保护
    job/                   # worker pool + janitor
    models/                # GORM 模型
    realtime/              # WebSocket hub
    metrics/               # 流量计量
    store/                 # SQLite + 自动迁移
    webui/                 # go:embed 前端产物
    config/                # 环境变量配置
    clientd/               # 客户端数据面

frontend/
  src/
    pages/                 # 各页面（Files、Transfers、Login 等）
    lib/                   # API 客户端、路由、校验与备注渲染
    transfer.tsx           # 传输引擎
    TransferDrawer.tsx     # 底部传输栏

docs/
  user-manual.md           # 使用手册
  development.md           # 本文档
  screenshots/             # README 与手册中的界面截图
  streaming-upload-sync.md
  client-mode.md
  baidu-client-optimizations.md
```

## 部署

最简方式是直接运行服务端二进制，监听 `:8080`，SQLite 默认在 `./gosocode.db`。前面加 nginx 等反向代理并启用 HTTPS 即可。

生产环境建议：

- 设置 `GSC_JWT_SECRET` 为强随机值；生产环境若仍使用占位值会拒绝启动
- 首次启动后立刻修改初始管理员密码，不要继续使用日志里打印的生成密码
- 反向代理 + HTTPS
- 定时备份 `gosocode.db`（元数据丢失后很难从网盘完整重建）
- 同时备份备注目录（`GSC_NOTES_DIR`）和密钥目录（如 `gsc-keys`）
- 多实例时必须配置 `GSC_REDIS`
- 将客户端 release 解压到 `GSC_CLIENT_DIR`，概览页会提供下载
- 更新程序时只替换二进制和前端资源，不要删除数据库、密钥和备注目录，也不要手动改网盘上的清单文件

## 客户端二进制分发

项目带有 GitHub Actions 发布流水线（`.github/workflows/release.yml`）。推送 `v*` 标签会自动：

1. 交叉编译 Linux / Windows / macOS × amd64 / arm64 的服务端和客户端
2. 输出各平台服务端程序、按平台打包的客户端，以及合并的 `gosocode-client-binaries.tar.gz`
3. 创建对应版本的 GitHub Release 并附上产物

推送默认分支会滚动更新预发布的 `latest` Release，产物相同。

把 `gosocode-client-binaries.tar.gz` 解压到 `GSC_CLIENT_DIR` 指向的目录（或服务端启动目录下的 `client-bin/`），网页「概览」会按浏览器自动推荐当前平台的客户端。

## 安全须知

- **不要在公网使用默认 JWT secret**。设置 `GSC_ENV=production`（或 `GIN_MODE=release`）后，未配置强随机 `GSC_JWT_SECRET` 会拒绝启动。
- 访问密码只通过 `X-Access-Password` 请求头传递，不要放进 URL。
- `GSC_AGENT_ENABLED=1` 时，平台会向 CLI 客户端下发百度 `access_token`。必须在服务端和客户端设置同一份 `GSC_AGENT_CONTROL_KEY`。这相当于把该百度账号的控制权交给运行客户端的人，只应在可信部署中开启。
- OAuth AppKey / SecretKey 可通过环境变量或管理后台保存到数据库；后台填写的优先级更高。
- 百度账号授权走 OAuth 2.0，**不要把百度账号密码交给 GoSoCode**。
- 不要把生产环境的登录密码、密钥或备份写进 Git 仓库或公开文档。

## 许可证

MIT
