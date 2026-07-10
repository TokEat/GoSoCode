# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

GoSoCode pools multiple Baidu Netdisk (百度网盘) accounts into "storage nodes" and exposes them as one shared logical filesystem for platform users. Go backend + React frontend. UI text and user-facing errors are in Chinese.

## Commands

```bash
# Backend (from backend/)
go run ./cmd/server        # runs on :8080, creates/migrates gosocode.db (SQLite)
go build ./... && go vet ./...

# Frontend (from frontend/)
npm run dev                # vite on :5173, proxies /api (+WebSocket) to :8080
npm run build              # tsc -b && vite build
```

There are no tests. Config is env vars only (see `backend/internal/config/config.go`): `GSC_ADDR`, `GSC_DB`, `GSC_JWT_SECRET`, `GSC_REDIS` (empty = in-memory coordination, single instance), `GSC_STREAM_ENABLED/_MAX_CONCURRENT/_WINDOW`, etc. First registered user (or seeded `admin`/`admin@qwe123`) is admin. Baidu OAuth app credentials live in the DB (admin UI), not env.

## Architecture

Backend packages (`backend/internal/`):

- **models** — GORM models; auto-migrated in `store.Open`. `File` is the *authoritative* metadata for the shared filesystem (listing never queries Baidu). `File.LogicalPath` is the user-facing path; `RemotePath` is the real path in the Baidu app sandbox (`/apps/<AppName>/shared/...`, traversal-safe via `Server.toRemote`).
- **account** (`Pool`) — storage-node selection + token lifecycle. `PickForSize` does capacity-aware placement **and takes a load slot** that must be paired with `Release` (worker defer, stream finish/cancel, janitor). `Token` refreshes OAuth tokens via singleflight. `RefreshQuota` also refreshes `VipType`.
- **coord** — `Locker` (distributed mutex, e.g. `"path:"+remotePath` around Baidu `create`) and `Limiter` (per-account QPS, key `"acct:"+id`). Memory or Redis backed. **Every Baidu API call must `Limiter.Wait` first.**
- **baidu** — Baidu OpenAPI client. Upload is always precreate → superfile2 (per-slice, idempotent by partseq) → create. Slice size is per-account-tier: `SliceSizeForVip` (normal 4MB / VIP 16MB / SVIP 32MB). Download: filemetas → dlink (temporary, needs `User-Agent: pan.baidu.com`).
- **job** (`Manager`) — worker pool for the *store-and-forward* upload path; also runs the janitor that reaps stale `UploadSession`s and `StreamSession`s (releasing their load slots).
- **api** — gin handlers; `server.go` wires all routes. JWT auth middleware puts `uid`/`role` in context. Admin-only routes under `/api/admin`.
- **realtime** (WS hub, per-user job events) and **metrics** (live traffic meter).

### Two upload paths (both must be kept working)

1. **Streaming/pipelined** (`api/stream.go`, default): browser hashes slices locally (spark-md5), `init` runs precreate (rapid-upload/秒传 short-circuit), each `PUT /files/stream/slice` is synchronously forwarded to Baidu — a 200 means that slice is cloud-synced. Resume state is `StreamSession.DoneBitmap`. Per-session pushes are serialized via `Server.streamStates`; concurrency capped by `GSC_STREAM_MAX_CONCURRENT`.
2. **Store-and-forward** (`api/uploads.go` + `job/manager.go`, fallback): chunks accumulate in a temp file, then an async job pushes to Baidu. Used when streaming is disabled/full, plan fails, or file is empty.

Design doc: `docs/streaming-upload-sync.md`.

Downloads (`api/files.go handleDownload`) proxy bytes cloud→server→client with a small buffer + Flush per write (no server buffering, backpressure end to end), forward `Range`, and re-resolve an expired dlink once.

### Frontend

`frontend/src/transfer.tsx` is the transfer engine (React context): `driveStreamUpload` (plan → md5 pre-scan → windowed XHR slices with dual progress: browser-upload speed from `xhr.upload.onprogress`, cloud-sync speed from confirmed slices) with automatic fallback to the legacy chunked path; ranged, resumable downloads. `pages/Transfers.tsx` renders dual progress bars. API types in `lib/api.ts`.

### Invariants worth knowing

- Accounting on success: `pool.AddUsed`, `meter.AddUp/AddDown`, `bytes_up/bytes_down` on User and BaiduAccount, `TransferLog` row. Both upload paths do all of these — keep them in sync.
- Deletes are soft (`File.Status = "deleted"`); remote Baidu deletes are best-effort, metadata is authoritative.
- Folder/move/rename operations are metadata-only rewrites of `LogicalPath`.
