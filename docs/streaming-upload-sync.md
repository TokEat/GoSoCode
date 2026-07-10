# 边上传边云端同步(流式管道上传)设计方案

> 状态:已实现(2026-07)。后端 `internal/api/stream.go`,前端 `transfer.tsx` 的 `driveStreamUpload`;保留先存后转作为降级路径(`GSC_STREAM_ENABLED` 开关)。
> 目标读者:GoSoCode 后端/前端开发
> 关联代码:`internal/api/uploads.go`、`internal/job/manager.go`、`internal/baidu/client.go`、`frontend/src/transfer.tsx`

---

## 1. 背景与目标

### 1.1 现状:先存后转(store-and-forward)
当前上传是两段**串行**:

```
浏览器 ──(分片, 4MB)──▶ 服务器临时文件(落全量磁盘) ──(finish)──▶ 后台 Job 读临时文件 ──▶ 百度(precreate→superfile2→create)
        第一段完成后                                        才开始第二段
```

代价:
- **总耗时 ≈ (浏览器→服务器) + (服务器→百度)**,两段串行相加。
- **磁盘峰值 = 整个文件大小**(临时文件)。
- 用户看到"上传 100%"后,还要等"云端合并中"(第二段)才真正完成。

### 1.2 目标:边传边同步(pipelined / streaming)
浏览器还在上传第 N+1 片时,服务器已经把第 N 片推给百度,两段**重叠**:

```
浏览器 ─片0─▶ 服务器 ─片0─▶ 百度
        ─片1─▶ 服务器 ─片1─▶ 百度   (片1的浏览器上传 与 片0的百度上传 并行)
        ─片2─▶ 服务器 ─片2─▶ 百度
                     ...          finish ─▶ 百度 create
```

收益:
- **总耗时 ≈ max(浏览器→服务器, 服务器→百度) + 1 片延迟**,而非相加。尤其当"用户上行带宽 < 服务器出口带宽"(常见)时,服务器→百度这段被**完全隐藏**在浏览器上传时间里。
- **磁盘峰值 ≈ 几个分片**(不再落全量),甚至可做到零磁盘(纯内存滑动窗口)。
- 用户"上传完"时,云端也几乎同步完成,取消"云端合并中"的额外等待。

### 1.3 非目标
- 不改变"共享文件系统 + 容量感知放置"的语义。
- 不追求单片内的字节级流式(见 §9 高级优化),先做**分片级管道**即可。

---

## 2. 关键约束:百度 API 的顺序要求

百度网盘上传是固定三步,决定了方案骨架:

1. **precreate**:必须提前提供 **`block_list`(每个分片的 MD5,基于整份文件计算)**。
   - 用于:①返回还需上传的分片索引;②**秒传检测**(命中则 `return_type=2`,直接完成)。
   - **含义:在上传任何分片之前,必须先知道所有分片的 MD5** → 必须先对整份文件做一次 MD5 扫描。
2. **superfile2**:按 `partseq` 逐片上传,需要 precreate 返回的 `uploadid`。**幂等**(同一 partseq 重传即覆盖)。
3. **create**:合并分片、落地文件,需再次带 `block_list`。

> 结论:`block_list` 必须在开始上传前就绪 → 采用**客户端先做一遍本地 MD5 预扫描(不走网络)**,把 `block_list` 交给服务器 precreate,然后再流式上传。这个预扫描是纯 CPU + 本地读盘,不产生网络流量,代价可接受;并且它顺带解锁了**秒传**。

---

## 3. 总体架构

```
┌─────────────┐   ①plan(size)         ┌──────────────┐
│   浏览器     │ ─────────────────────▶ │   服务器      │  选节点(按等级定分片大小)
│ transfer.ts │ ◀───────────────────── │  api/stream  │  返回 slice_size
│             │   slice_size, node                     │
│  ②本地MD5预扫描(Web Crypto, 无网络)                    │
│             │   ③init(path,size,block_list)          │
│             │ ─────────────────────▶ │              │─precreate─▶ 百度
│             │ ◀───────────────────── │              │◀── uploadid / 缺失分片 / 秒传命中
│             │   ④slice(seq, bytes) 逐片              │
│             │ ═════════════════════▶ │  滑动窗口     │═superfile2═▶ 百度(片N)
│             │       (片N+1 上传中)     │  (并行)       │      (片N 同步中)
│             │   ⑤finish                              │
│             │ ─────────────────────▶ │              │─create────▶ 百度
└─────────────┘                        └──────────────┘  写元数据/流量/历史
```

- **浏览器→服务器**与**服务器→百度**在分片粒度上重叠(滑动窗口)。
- 秒传命中时,③直接返回"完成",跳过④⑤。

---

## 4. 交互流程(时序)

### 4.1 plan(确定分片大小 / 预选节点)
- 目的:分片大小取决于**存储节点账号等级**(普通 4MB / VIP 16MB / SVIP 32MB),而客户端算 MD5 必须先知道分片大小 → 先向服务器要。
- `GET /files/stream/plan?size=<字节>`
- 服务器:按容量+等级选一个候选节点,返回 `{ slice_size, node_id, node_tier }`。
- 说明:此处只是**预选**,真正占用在 init 时确定(避免 plan 与 init 之间节点变动的竞态,init 会重新校验容量,必要时换节点并把新的 slice_size 回传要求客户端按新粒度处理——见 §8 边界)。

### 4.2 客户端本地 MD5 预扫描
- 用 `File.slice(off, off+slice_size)` + `crypto.subtle.digest("MD5"...)` 逐片算 MD5。
  - 注意:Web Crypto **不支持 MD5**。需用 WASM/JS 版 MD5(如 `spark-md5`)。这是本方案的一个前端依赖点。
- 产出 `block_list = [md5_0, md5_1, ...]`。纯本地,无网络。

### 4.3 init(precreate)
- `POST /files/stream/init` `{ path, size, slice_size, block_list, node_id }`
- 服务器:对节点 token 调百度 `precreate`。
- 返回 `{ session_id, upload_id, need_seqs: [缺失分片索引], rapid: bool }`。
  - `rapid=true`(秒传命中)→ 客户端直接跳到"完成",无需上传任何分片。
  - `need_seqs`:通常是全部;断点续传时是缺失子集。

### 4.4 slice(逐片流式上传)
- `PUT /files/stream/slice?session=<id>&seq=<n>`,body 为该片原始字节。
- 服务器:
  - 校验 session、seq;
  - 立即用 node token 调百度 `superfile2`(partseq=n)上传该片;
  - 成功后标记该 seq 已同步,返回 `{ seq, done_count, total }`。
- **管道**:客户端可**并发发若干片**(见 §5 窗口),服务器侧对同一节点用现有限速器 `coord.Limiter` 控制百度侧 QPS,并用有界并发把"接收片"和"推送片"重叠。

### 4.5 finish(create)
- `POST /files/stream/finish` `{ session_id }`
- 服务器:校验所有 `need_seqs` 已同步 → 调百度 `create` → 写 `File` 元数据(fs_id/size/ready)→ 流量记账 → 写 `TransferLog` → 删除 session。
- 返回 `{ file_id }`。此时**云端已就绪**,无"云端合并中"等待。

---

## 5. 管道与背压(核心机制)

### 5.1 滑动窗口
- 定义**在途窗口** `W`(如 2~4 片):客户端最多有 W 片"已发出但服务器尚未确认同步完成"。
- 客户端:维护一个信号量,发一片占一个额度,收到该片 200 释放一个额度;额度满则等待 → 天然背压。
- 服务器:每片处理 = 接收 body(浏览器→服务器) + superfile2(服务器→百度)。用有界 worker 让"接收片 N+1"与"上传片 N 到百度"并行。

### 5.2 背压来源
- **浏览器→服务器**:HTTP 请求本身;窗口满时客户端暂停发片。
- **服务器→百度**:`coord.Limiter`(每账号 QPS)+ 窗口大小共同限制,避免打爆百度限流。

### 5.3 内存预算(关键权衡)
- 每片在"服务器接收完 → 推给百度完成"期间驻留内存。
- **单个上传峰值内存 ≈ W × slice_size**(如 4 × 32MB = 128MB)。
- **必须设全局上限**:`最大并发流式上传数 × W × slice_size` 不得超过服务器内存预算。
  - 例:限制"同时进行的流式上传"为 N,配置项 `GSC_STREAM_MAX_CONCURRENT`、`GSC_STREAM_WINDOW`。
  - 超限时:降级为"先存后转"(§10),或排队等待。

---

## 6. 断点续传与流式共存

- session 持久化:`upload_id` + `slice_size` + `block_list` + 每个 seq 的"是否已同步百度"位图。
- 断网/刷新后:
  - 客户端(同会话内)重新发缺失片即可;superfile2 幂等,重发安全。
  - 跨刷新:客户端需重算/缓存 block_list(或调 `GET /files/stream/status` 取 `need_seqs`),用户重选同一文件后只补缺失片。
- `upload_id` 有有效期(约 1 天),过期需重新 precreate;由 §7 janitor 兜底清理过期 session。

---

## 7. 数据模型与清理

新增(或改造现有 `UploadSession`)`StreamSession`:

| 字段 | 说明 |
|---|---|
| `id` | 会话 ID |
| `user_id` | 归属用户 |
| `baidu_account_id` | 选定的存储节点 |
| `logical_path` / `remote_path` | 逻辑路径 / 沙箱路径 |
| `size` / `slice_size` | 文件大小 / 分片大小 |
| `upload_id` | 百度 precreate 返回 |
| `block_list` | 分片 MD5(JSON) |
| `done_bitmap` | 每片是否已同步(bitset/JSON) |
| `created_at` / `updated_at` | 用于 janitor 过期回收 |

- **无全量临时文件**(区别于现方案),因此"清 tmp"主要是清 session 记录 + 少量在途缓冲。
- 复用现有 `uploadJanitor`:回收超时(如 >24h)未完成的 `StreamSession`。

---

## 8. 失败处理与重试

- **单片重试**:
  - 浏览器→服务器:沿用 `transfer.tsx` 现有分片重试(指数退避 + 幂等 seq)。
  - 服务器→百度:沿用 `job/manager.go` 的 `withRetry` 包 superfile2。
- **某片持续失败**:该片的 `PUT /slice` 返回错误 → 客户端重试;超上限则整单失败,清理 session。
- **precreate/create 失败**:同现有 `withRetry`,并打印 `[stream]` 日志。
- **原因展示**:复用现有 `note`(重试中)/`error`(终因)与后端日志规范(`[stream] ...`)。

### 边界情况
- **plan 与 init 节点不一致**:init 以服务器最终选定节点为准;若 slice_size 变化,返回错误码要求客户端按新 slice_size 重新 MD5(少见,仅当预选节点失效)。为降低概率,plan 可"软预留"节点。
- **空文件 / 极小文件**:直接走 §10 的先存后转,避免管道开销。
- **秒传命中**:init 即完成,零分片上传。

---

## 9. 高级优化(可选,非首期)

- **单片内字节级流式**:用 `io.Pipe` 把浏览器分片 body 边收边写入 superfile2 的 multipart body,内存降到 sub-slice。风险:百度 superfile2 可能要求 `Content-Length`(multipart 分块传输兼容性需实测),复杂度高。**首期不做**。
- **多片并发到百度**:同一 uploadid 下并发多个 partseq(受账号 QPS 限制);需实测百度是否接受同 uploadid 并发。
- **节点等级自适应分片**:与"账号等级"改造联动(4/16/32MB),SVIP 节点更大分片 → 更少请求 → 更快(见 `docs` 中账号等级说明/后续任务)。

---

## 10. 与"先存后转"的关系(共存 / 降级)

保留现有先存后转作为**降级路径**,以下情况自动回退:
- 前端无法算 MD5(禁用 JS/不支持)、文件过小、并发流式已达上限、内存吃紧。
- 通过特性开关 `GSC_STREAM_ENABLED` 控制灰度。

### 对比表

| 维度 | 先存后转(现状) | 边传边同步(本方案) |
|---|---|---|
| 总耗时 | 两段串行相加 | ≈ max(两段) + 1 片 |
| 磁盘峰值 | 整份文件 | 几个分片 / 零 |
| 内存峰值 | 低(流式写盘) | W×slice_size(需限并发) |
| 用户体验 | 传完还要"云端合并中" | 传完即云端就绪 |
| 秒传 | finish 时才可能命中 | **init 即命中,零上传** |
| 断点续传 | 支持 | 支持(幂等分片) |
| 前端依赖 | 无 | 需 JS/WASM MD5 |
| 实现复杂度 | 低 | 中(管道+背压+内存治理) |

---

## 11. API 草图

```
GET  /files/stream/plan?size=            -> { slice_size, node_id, node_tier }
POST /files/stream/init                  -> { session_id, upload_id, need_seqs, rapid }
     body: { path, size, slice_size, block_list, node_id }
PUT  /files/stream/slice?session=&seq=   -> { seq, done_count, total }   (body: 分片原始字节)
GET  /files/stream/status?session=       -> { need_seqs, done_count, total }  (断点续传)
POST /files/stream/finish                -> { file_id }
POST /files/stream/cancel                -> { ok }
```

---

## 12. 分阶段落地计划(实现时参考)

1. **阶段一:后端骨架**
   - `StreamSession` 模型 + 迁移;`plan/init/slice/finish/cancel/status` 接口。
   - `init` 调 precreate、`slice` 调 superfile2、`finish` 调 create;先**串行**(窗口=1),验证正确性与幂等。
2. **阶段二:前端流式**
   - 引入 MD5(spark-md5)本地预扫描;`transfer.tsx` 增加 `startStreamUpload`,分片 PUT + 秒传短路。
   - 复用现有速度/百分比/暂停恢复/取消 UI。
3. **阶段三:管道与背压**
   - 客户端在途窗口 W;服务器有界并发;内存上限与降级开关。
4. **阶段四:治理与观测**
   - `[stream]` 日志、janitor 回收、并发/内存指标接入"流量检测",压测调参 W。

---

## 13. 风险与结论

- **主要风险**:内存治理(必须限并发)、前端 MD5 依赖、单片内非流式导致的"1 片延迟"、百度并发/限流实测。
- **结论**:方案可行且收益明确(隐藏第二段延迟 + 去全量落盘 + 免费解锁秒传)。建议**分片级管道 + 客户端 MD5 预扫描**为首期形态,单片内字节流式作为后续优化;并始终保留"先存后转"作为降级兜底。
