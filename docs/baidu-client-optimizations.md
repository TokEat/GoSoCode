# 百度网盘操作优化分析（参考 OpenList / BaiduPCS-Go）

> 状态:分析文档(待逐项实施)
> 参考对象:[OpenList](https://github.com/OpenListTeam/OpenList) `drivers/baidu_netdisk`(官方 OpenAPI 路线,与本项目同源)、[BaiduPCS-Go](https://github.com/qjfoidnh/BaiduPCS-Go)(BDUSS 私有接口路线,仅借鉴策略)
> 关联代码:`internal/baidu/client.go`、`internal/api/stream.go`、`internal/job/manager.go`、`internal/account/pool.go`、`frontend/src/transfer.tsx`

---

## 0. 结论速览

| # | 优化项 | 参考来源 | 收益 | 复杂度 | 优先级 |
|---|---|---|---|---|---|
| 1 | errno 111/-6 → 强制刷新 token 并重试 | OpenList | 消除一类"随机失败" | 低 | **高** |
| 2 | precreate 补传 content-md5 + slice-md5 | OpenList | 秒传真正可命中 | 低 | **高** |
| 3 | 同一 uploadid 并发推送分片 | OpenList(1-32 线程实证可行) | 服务器→云端吞吐 ×N | 中 | **高** |
| 4 | uploadid 失效识别与自动重建 | OpenList | 长传/断点续传可靠性 | 中 | 中高 |
| 5 | 分片数 ≤2048 校验与自适应 | OpenList / 百度限制 | 超大文件正确性 | 低 | 中 |
| 6 | locateupload 上传域名优选 | OpenList / BaiduPCS-Go | 跨地域上传提速 | 中 | 中 |
| 7 | dlink 按 fsid 缓存(TTL) | 自研(dlink 有效期 ~8h) | 少 2 次 API/下载,断点更快 | 低 | 中 |
| 8 | 按账号等级自适应并发(风控) | BaiduPCS-Go 经验值 | 降低限速/封控概率 | 低 | 中 |
| 9 | 重试退避加上限 | OpenList | 细节健壮性 | 低 | 低 |
| ✗ | crack/BDUSS 下载、多连接分段下载 | 两者 | — | — | **不采纳** |

---

## 1. errno 111 / -6:token 失效的运行时处理(高优先)

**现状**:`pool.Token` 只按 `ExpiresAt` 提前 72h 刷新。若 token 在有效期内被百度侧提前作废(改密、风控、重复授权),所有 API 将持续返回 `errno=-6`(身份校验失败)或 `111`(token 过期),本项目会将其当作普通错误重试 3~4 次后失败,且**永远不会自愈**。

**OpenList 做法**:`request()` 里统一检查响应 errno,命中 `111`/`-6` 时立即调 `refreshToken()` 并重试本次请求。

**改法**:
- `baidu.Client` 的 `baseErr.err()` 保留 errno 信息(已有);在 `postForm`/`UploadChunk` 等出口返回可判别的错误类型(如 `IsAuthError(err)`)。
- `pool` 增加 `ForceRefresh(ctx, accID)`(绕过 72h 检查,走 singleflight)。
- 调用侧(stream.go、manager.go、files.go)在 `withRetry`/`streamRetry` 中捕获 auth 错误 → `ForceRefresh` → 用新 token 重试一次;二次失败则将节点标记异常并记日志。

**收益**:消除一整类"节点突然全部失败、要等 72h 窗口才恢复"的问题。

---

## 2. 秒传参数补全:content-md5 + slice-md5(高优先)

**现状**:`Precreate` 只传 `block_list`。百度官方秒传判定需要 **`content-md5`(整文件 MD5)+ `slice-md5`(文件前 256KB 的 MD5)**,只有 block_list 时 `return_type=2` 基本不会命中——目前的"秒传短路"实际上很难触发。

**OpenList 做法**:precreate 同时携带 `content-md5`、`slice-md5`、`block_list`;命中 `returnType==2` 时直接建档跳过传输。
**BaiduPCS-Go 注记**:官方 API 的秒传近年被百度收紧("秒传已不被支持"指其转存链路),但 precreate 的 MD5 匹配路径仍是官方文档行为,值得传全参数争取命中。

**改法**:
- 前端 `computeBlockList` 已逐片读文件:顺带用 spark-md5 **增量**计算整文件 MD5(每片 append 一次,零额外 IO),再单独取前 256KB 算 slice-md5;`init` 请求体增加 `content_md5`、`slice_md5` 两个字段。
- 后端 `Precreate` 增加对应 form 字段(`content-md5`、`slice-md5`);`handleStreamInit` 透传。
- 先存后转路径:`SliceFileMD5s` 扫描时同样增量累计整文件 MD5(几乎零成本)。

**收益**:重复文件(多用户传同一资源是共享盘高频场景)零流量入库;失败重传的文件二次上传也可能秒传。

---

## 3. 同一 uploadid 并发推送分片(高优先)

**现状**:设计文档 §9 当时存疑"百度是否接受同 uploadid 并发 partseq",故 `stream.go` 用 per-session mutex 串行推送、`manager.go streamChunks` 也是顺序循环。

**OpenList 实证**:其上传用 `errgroup` 对同一 uploadid 并发 superfile2,线程数可配 **1~32**——证明该并发是百度支持的正常用法。

**改法**:
- `handleStreamSlice`:把 per-session mutex 的临界区缩小到**位图读写**,superfile2 推送移出锁外;窗口内的 W 个分片即可真并发直达云端。服务器内存峰值不变(仍是 W×slice_size),但服务器→云端吞吐从 1 片变 W 片并行。
- `streamChunks`(先存后转):改为有界并发(errgroup + 信号量,按 §8 的等级并发数),分片读取仍顺序、推送并发。
- 并发度与 `coord.Limiter`(QPS)联动,见 §8。

**收益**:这是当前流式上传"1 片时延"之外最大的吞吐瓶颈;SVIP 大分片 + 并发后,服务器→云端一段几乎不再是短板。

---

## 4. uploadid 失效识别与自动重建(中高)

**现状**:uploadid 有效期约 1 天;失效后 superfile2/create 报错,本项目盲重试后整单失败,用户只能重新上传。

**OpenList 做法**:扫描错误响应文本,命中 `uploadid` + `invalid`/`expired`/`not found` 即判定为 `ErrUploadIDExpired`,**跳过常规重试**,直接重新 precreate 拿新 uploadid 后继续(已传分片信息通过进度缓存恢复)。

**改法**:
- `baidu` 包增加 `IsUploadIDExpired(err)`(按响应文本/errno 判定)。
- `handleStreamSlice`/`handleStreamFinish` 捕获后:用 session 里已有的 `block_list` 重新 `precreate`(百度对同一 block_list 返回的 `block_list` 缺失索引即需重传的片),更新 session 的 `upload_id` 与位图,响应 409 让前端按新 `need_seqs` 补传,而不是报废整个会话。

**收益**:超大文件、跨天断点续传不再"临门一脚全部重来"。

---

## 5. 分片数上限 2048 校验(中,正确性)

**现状**:百度限制单文件最多 **2048 个分片**,本项目未校验。普通账号 4MB×2048=8GB、SVIP 32MB×2048=64GB,超过后 precreate/create 会失败,且错误信息难懂。

**OpenList 做法**:`getSliceSize()` 按文件大小与 vip 上限联合决定分片大小;低带宽模式按 1MB 步进增大分片,始终满足 ≤2048 片。

**改法**:`SliceSizeForVip` 扩展为 `SliceSizeFor(vip int, size int64)`:基准取等级默认值,若 `size/slice > 2048` 则按 1MB 步进增大直到满足;若达到等级上限仍超 2048,plan/init 直接返回明确错误("文件超出该存储节点单文件上限 X GB")。`plan` 响应的 slice_size 随之变化,前端逻辑无需改动(本来就以服务端返回为准)。

---

## 6. locateupload 上传域名优选(中)

**现状**:superfile2 域名硬编码 `d.pcs.baidu.com`。

**参考做法**:OpenList 通过 `locateupload` 接口动态获取当前网络环境的最优上传服务器列表(优先主服务器,失败回退配置域名);BaiduPCS-Go 同理提供 `pcs_addr`/`fix_pcs_addr` 且 v3.9.8+ 支持动态获取。

**改法**:`baidu.Client` 增加 `LocateUpload(ctx, token, path, uploadid)`,按账号缓存结果(如 1h TTL);`UploadChunk` 优先用优选域名,失败自动回退默认域名。可加配置 `GSC_UPLOAD_DOMAIN` 允许手动固定。

**收益**:服务器与百度默认接入点跨地域(如海外 VPS)时提升明显;同地域收益有限,故列中优先级。

---

## 7. dlink 缓存(中)

**现状**:每次下载(含每次 Range 断点重连)都要 2 次 API(limiter 两跳 + filemetas)重新解析 dlink,而 dlink 本身有效期约 8h。

**改法**:按 `fs_id` 内存缓存 dlink(TTL 建议 1h,留足余量),命中直接开流;上游 4xx 时已有"重解析一次"逻辑,天然兜底缓存失效。多实例部署下各实例独立缓存即可,无一致性要求。

**收益**:多线程/多次 Range 下载的首字节时间明显缩短,并降低账号 QPS 消耗(QPS 是百度风控的敏感指标)。

---

## 8. 按账号等级自适应并发——风控友好(中)

**BaiduPCS-Go 的经验值**(下载侧):普通账号 `max_parallel=1` 避免触发限速,SVIP 才建议 10-20;被限速后降到 10KB/s 数天。这说明**并发度应与账号等级挂钩**,而不是全局一个值。

**改法**(与 §3 配套):
- 上传分片并发数按等级:普通 1~2、VIP 2~3、SVIP 4~6(可配 `GSC_SLICE_PARALLEL_[TIER]`),并始终经过 `coord.Limiter`。
- 下载保持单连接直通(见"不采纳"),但可按等级微调 `PerAccQPS`。

**收益**:在提速(§3)与账号安全之间给出确定的边界,避免把普通账号打进限速黑屋。

---

## 9. 重试退避加上限(低)

OpenList 的上传重试有 `UPLOAD_RETRY_MAX_WAIT_TIME` 封顶;本项目 `withRetry`/`streamRetry` 为 `400ms × 2^i` 无上限(当前次数少影响小)。若后续把重试次数调大,应加 8s 左右的封顶与抖动。

---

## 不采纳项(明确记录,避免反复讨论)

| 做法 | 来源 | 不采纳原因 |
|---|---|---|
| crack / crack_video 下载(伪装 UA 走网页接口) | OpenList 可选模式 | 非官方接口,违反 ToS,有封号/封 AppID 风险(参见 BaiduPCS-Go 的 403/AppID 封杀史) |
| BDUSS 免开放平台接入 | BaiduPCS-Go | 同上;本项目坚持官方 OpenAPI 路线 |
| 下载多连接分段并发(locate 模式 + max_parallel) | BaiduPCS-Go | 与本项目"云端↔服务端↔客户端同步直通 + 背压"的设计冲突,需服务端缓冲乱序段;且普通账号多连接极易触发限速。客户端侧已有 Range 断点,收益不值得复杂度 |
| 秒传链接(rapid link)导入/导出 | BaiduPCS-Go | 官方 API 已封堵该转存路径,且属灰色功能 |

---

## 建议落地顺序

1. **§1 token 自愈 + §2 秒传参数**:改动小、纯收益,先行。
2. **§5 分片数校验**:正确性问题,随 §2 一起动 `Precreate` 顺手做。
3. **§3 并发推送 + §8 等级并发**:核心吞吐优化,需要压测验证(观察 errno 限速反馈)。
4. **§4 uploadid 重建 + §7 dlink 缓存**:可靠性/体验补强。
5. **§6 locateupload**:部署环境跨地域时再做。

---

## 参考

- OpenList `drivers/baidu_netdisk/driver.go`、`util.go`:errno 111/-6 刷新重试、content-md5/slice-md5 秒传、errgroup 并发 superfile2(1-32 线程)、uploadid 过期文本判定、locateupload 域名优选、vip 分片上限与 2048 片限制、低带宽 1MB 步进。
- BaiduPCS-Go README/config:`max_parallel`(普通账号=1 防限速)、`pcs_addr`/`fix_pcs_addr` 服务器优选、`--retry`、限速与恢复经验。
