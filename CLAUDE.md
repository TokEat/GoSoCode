# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

GoSoCode pools multiple Baidu Netdisk (百度网盘) accounts into "storage nodes" and exposes them as one shared logical filesystem for platform users. Go backend + React frontend. UI text and user-facing errors are in Chinese.

## Commands

```bash
# Backend (from backend/)
go run ./cmd/server        # runs on :8080, creates/migrates gosocode.db (SQLite)
go run ./cmd/client -server http://<server>:8080   # local CLI client on :8090 (data plane direct to Baidu)
go build ./... && go vet ./...

# Frontend (from frontend/)
npm run dev                # vite on :5173, proxies /api (+WebSocket) to :8080
                           # VITE_API_TARGET=http://localhost:8090 npm run dev -> client mode
npm run build              # tsc -b && vite build; point GSC_WEBUI (or client -webui) at dist/
```

There are no tests. Config is env vars only (see `backend/internal/config/config.go`): `GSC_ADDR`, `GSC_DB`, `GSC_JWT_SECRET`, `GSC_REDIS` (empty = in-memory coordination, single instance), `GSC_STREAM_ENABLED/_MAX_CONCURRENT/_WINDOW`, etc. The first user in an empty database is a seeded `admin` account; the password comes from `GSC_ADMIN_PASSWORD` or is generated and printed once in the server log. Baidu OAuth app credentials live in the DB (admin UI), not env.

## Architecture

Backend packages (`backend/internal/`):

- **models** — GORM models; auto-migrated in `store.Open`. `File` is the *authoritative* metadata for the shared filesystem (listing never queries Baidu). `File.LogicalPath` is the user-facing path; `RemotePath` is the real path in the Baidu app sandbox (`/apps/<AppName>/shared/...`, traversal-safe via `Server.toRemote`).
- **account** (`Pool`) — storage-node selection + token lifecycle. `PickForSize` does capacity-aware placement **and takes a load slot** that must be paired with `Release` (worker defer, stream finish/cancel, janitor). `Token` refreshes OAuth tokens via singleflight. `RefreshQuota` also refreshes `VipType`.
- **coord** — `Locker` (distributed mutex, e.g. `"path:"+remotePath` around Baidu `create`) and `Limiter` (per-account QPS, key `"acct:"+id`). Memory or Redis backed. **Every Baidu API call must `Limiter.Wait` first.**
- **baidu** — Baidu OpenAPI client. Upload is always precreate → superfile2 (per-slice, idempotent by partseq) → create. Slice size is per-account-tier: `SliceSizeForVip` (normal 4MB / VIP 16MB / SVIP 32MB). Download: filemetas → dlink (temporary, needs `User-Agent: pan.baidu.com`). Share (转存) API quirks (`share.go`): `verify`/`transfer` require `Referer: https://pan.baidu.com/` or Baidu answers errno=2; `verify`'s randsk arrives percent-encoded (decode before reuse); `list` returns numeric fields (`fs_id`/`size`/`isdir`/`shareid`) as quoted strings — `ShareItem` has a tolerant `UnmarshalJSON`.
- **job** (`Manager`) — worker pool for the *store-and-forward* upload path; also runs the janitor that reaps stale `UploadSession`s and `StreamSession`s (releasing their load slots).
- **api** — gin handlers; `server.go` wires all routes. JWT auth middleware puts `uid`/`role` in context. Admin-only routes under `/api/admin`.
- **realtime** (WS hub, per-user job events) and **metrics** (live traffic meter).

### Two upload paths (both must be kept working)

1. **Streaming/pipelined** (`api/stream.go`, default): browser hashes slices locally (spark-md5), `init` runs precreate (rapid-upload/秒传 short-circuit), each `PUT /files/stream/slice` is synchronously forwarded to Baidu — a 200 means that slice is cloud-synced. Resume state is `StreamSession.DoneBitmap`. Per-session pushes are serialized via `Server.streamStates`; concurrency capped by `GSC_STREAM_MAX_CONCURRENT`.
2. **Store-and-forward** (`api/uploads.go` + `job/manager.go`, fallback): chunks accumulate in a temp file, then an async job pushes to Baidu. Used when streaming is disabled/full, plan fails, or file is empty.

Design doc: `docs/streaming-upload-sync.md`.

### Client mode (CLI, data plane bypasses the server)

`cmd/client` + `internal/clientd`: a local daemon that reverse-proxies the whole API/UI to the server but intercepts the transfer data plane — upload slices go straight from the local machine to Baidu (superfile2) and downloads are fetched from Baidu with parallel ranged connections (both with larger windows than server mode). The server keeps the control plane via `/api/agent/*` (`api/agent.go`): `upload/init` returns `uploadid + node access_token + remote_path` (agent sessions skip the streamStates cap), `upload/progress` keeps the server's `StreamSession` bitmap authoritative, `download/resolve` hands out `dlink + token`, `download/report` feeds accounting. Stream `finish/status/cancel` are plain proxied (create + accounting stay server-side). The web UI auto-detects via `GET /api/client/info` (server answers `mode:server`; the client intercepts the same path with `mode:client`) and the client rewrites the plan `window` larger — the browser transfer engine is unchanged. Handing out `access_token` = full account control: trusted deployments only; gate with `GSC_AGENT_ENABLED=0`. See `docs/client-mode.md`.

Downloads (`api/files.go handleDownload`) proxy bytes cloud→server→client with a small buffer + Flush per write (no server buffering, backpressure end to end), forward `Range`, and re-resolve an expired dlink once.

### Share-link import (转存)

`api/share.go` + `job/share.go`: `/api/share/resolve` parses a pasted share link/text (`baidu.ParseShareURL`), verifies the extraction code and lists the share root (fs_id/share_id/uk shipped as strings — they can exceed JS `Number.MAX_SAFE_INTEGER`). `/api/share/import` submits an async job: Baidu-side copy into a per-job staging dir (`<mount>/.gsc-imports/<jobID>`), then everything is **moved out** to the target directory's real remote dir (collision → job-suffixed name) and the staging dir is deleted — `.gsc-imports` is transient plumbing, never long-term storage. Each successful job also sweeps stale staging dirs no live `File` row references. Files are registered as `File` rows at their final remote paths. Options: `encrypt` (download → AES-256 → re-upload in place) and `rand_name` (the *remote* file is renamed in place to a bare UUID like UUID storage; `LogicalPath` keeps the original name, `RemoteUUID` is set so the mapping lands in the cloud manifest). Importing into a 私密 directory goes through `accessDenied` — the frontend passes the access password as the `pwd` query param and prompts on 403. Progress is pushed over the jobs WS feed. Gated by admin toggle `ShareImportEnabled`.

### Frontend

`frontend/src/transfer.tsx` is the transfer engine (React context): `driveStreamUpload` (plan → md5 pre-scan → windowed XHR slices with dual progress: browser-upload speed from `xhr.upload.onprogress`, cloud-sync speed from confirmed slices) with automatic fallback to the legacy chunked path; ranged, resumable downloads. `pages/Transfers.tsx` renders dual progress bars. API types in `lib/api.ts`.

Routing is hash-based (`App.tsx`, `#/files`, `#/share?dir=…`). `pages/ShareImport.tsx` is the standalone 转存 page (two-column: link resolve + file picker on the left; node select, directory browser, rand-name/encrypt options on the right); the Files page's 分享转存 button navigates there with the current directory pre-filled. `TransferDrawer.tsx` (bottom dock) renders only on the Files page and only unfinished/failed tasks — done/canceled rows are dropped (full history is the Transfers page). The frontend build must be synced into `backend/internal/webui/static/` (the embedded UI served by server/client binaries).

### Invariants worth knowing

- Accounting on success: `pool.AddUsed`, `meter.AddUp/AddDown`, `bytes_up/bytes_down` on User and BaiduAccount, `TransferLog` row. Both upload paths do all of these — keep them in sync.
- Deletes are soft (`File.Status = "deleted"`); remote Baidu deletes are best-effort, metadata is authoritative.
- Folder/move/rename operations are metadata-only rewrites of `LogicalPath`.
- The cloud manifest (`manifest.Build`) only records entries that carry mapping information: UUID-named, encrypted, or remote-path ≠ logical-path rows. Plain passthrough files and `/__system` rows are excluded — reconcile re-derives them from the netdisk. Reconcile itself never imports `.gsc-imports` content or the manifest file.
