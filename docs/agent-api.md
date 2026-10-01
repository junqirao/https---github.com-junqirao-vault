# Vault-Agent 本地 API 契约（v1）

> 本文件是客户端 Go 代理与前端之间的**接口契约**，双方以此为准实现。
> 代理只监听 `127.0.0.1:<随机端口>`，所有请求必须携带 `X-Vault-Agent-Token: <一次性令牌>`。
> 令牌由 Electron 主进程在拉起代理时通过参数/环境变量传入，代理启动后写入本地配置供渲染进程读取。

## 为什么需要这个代理

iSCSI 挂载涉及：连接 iSCSI 目标、把磁盘上线、分配盘符或目录挂载点、维持租约心跳、
接收服务端的「踢下线」指令并主动断开。这些都需要本机管理员权限与 Windows 原生调用，
不能放在浏览器/渲染进程里做，因此由 Go 代理承担。

## 认证与会话

前端完成「服务端配置 → 初始化 / 登录」后，把会话推给代理；代理据此调用服务端 API。
代理**不参与登录流程**，只消费前端已有的会话。

```
POST   /agent/session
  ↓ {server_url, server_instance_id, server_name, token, user_id, username, expires_at,
     cert_sha256}
  ↑ {ok:true}

DELETE /agent/session          # 退出登录时调用，代理停止心跳并解除订阅
  ↑ {ok:true}
```

`cert_sha256` 是服务端证书 DER 的 SHA-256（小写十六进制），由前端在「测试连接」时经
`POST /agent/server/test` 得到并固化（TOFU 首次信任）。服务端使用自建 CA，代理无法用公信
CA 验证，因此以该指纹固定服务端身份：**指纹不符即拒绝连接**。该字段可选，缺省时退回
Go 默认的证书链校验。渲染进程侧的同名信任由 Electron 主进程的 `certificate-error` 实现
（同样只放行指纹一致的自签证书）。

### 客户端证书会话自动续期

代理**常驻进程**负责续期（界面关掉也能续），渲染进程只是消费方：

- **前置条件**：已安装客户端证书身份（`GET /agent/identity` 的 `installed=true`）且当前有会话；
- **检查频率**：每 60s 检查一次；**续期窗口** = 剩余时间 < `max(5min, 观测寿命 * 25%)`，
  寿命 = `expires_at - 收到该会话的时刻`；
- **最小间隔**：两次续期尝试之间至少 5 分钟（避免短 TTL 下反复续期）；
- **续期方式**：复用 mTLS 发 `POST {server}/v1/auth/cert-login`，成功后按与
  `POST /agent/session` 一致的效果写回本地会话（token / expires_at / user 全部刷新），
  并广播 `session` 事件（见「事件流」）；
- **失败策略**：绝不清除现有会话、绝不影响挂载。确定性错误（证书被吊销/过期/未知、
  `auth.forbidden` 等）→ 停止自动续期并标记"需要重新登录"（不静默无限重试）；
  网络/服务端暂时不可用 → 按最小间隔退避重试。
- **日志**：只记录 `server_url` / `username` / 新旧过期时间，**绝不记录 token 原文**。

## 状态查询

```
GET /agent/health
  ↑ {ok:true, agent_version, platform, admin:true|false,
     host:HostState,
     server:{connected:bool, url, name, last_error}}

GET /agent/state
  ↑ {
      server:{url, instance_id, name, connected, last_error},
      user:{id, username},
      mounts:[MountState...],
      auto_mount:bool,
      host:HostState,
      update:UpdateInfo,
      web_update:WebUpdateState,
      session:SessionView            # 可选：当前服务端会话（未推送会话时缺省）
    }

GET /agent/log?tail=<字节数>        # 日志模块：读当天日志文件末尾（默认 256KiB）
  ↑ {path:<当天日志文件路径>, text:<原始文本(JSON Lines)>, error?}

HostState = {
  iscsi_available:bool,              # 找得到 IscsiInitiator 模块/cmdlet
  iscsi_service:"running"|"stopped"|"unknown",
  iscsi_ready:bool,                  # 现在能否发起点 iSCSI 连接（模块可用且服务在运行）
  reason:"module_missing"|"service_stopped"|"probe_failed",   # 就绪时缺省
  error,                             # 探测失败时的稳定错误码
  checked_at                         # 毫秒时间戳；**0 表示代理尚未探测完**（界面此时不应提示）
}

SessionView = {
  server_url, token, expires_at, user_id, username
}
# session 只在本机 127.0.0.1 的本地接口上返回（与 /agent/identity 同信任级别），
# 渲染进程据此在代理**自动续期**后更新自己持有的令牌。

UpdateInfo = {
  channel, current_version,
  available_version,            # 可选：发现的新版本
  downloading:bool,
  received_bytes, total_bytes,  # 自更新产物下载进度（≥1MiB 或 ≥500ms 节流更新）
  reason,                       # 可选：最近一次"无可用更新"的稳定原因码
                                #   no_session / server_unreachable / no_manifest / not_newer /
                                #   no_artifact / bad_version / artifact_too_large
  server_reason                 # 可选：更新源（服务端）返回的具体原因，如 manifest_missing /
                                #   artifacts_dir_not_configured / channel_unavailable / signature_missing
}

MountState = {
  repo_id, repo_name,
  allocation_id, lease_id,
  target_iqn, portal, mount_mode:"letter"|"directory",
  mount_path,                 # letter 模式为 "E:"；directory 模式为 "C:\\Vault\\home\\game-x"
  disk_number,                # 可选，Windows 磁盘号
  state:"mounting"|"mounted"|"unmounting"|"error"|"revoked",
  # phase：state="mounting" 期间的**细粒度阶段**（取值见下），非挂载中时缺省。
  # 用途：挂载在服务端是同步重活（等差异盘 + PowerShell 全量下发 iSCSI 目标，实测数十秒），
  # 只给一个"挂载中"用户无从知道卡在哪一步（真实反馈：希望看到"正在创建虚拟磁盘 /
  # 正在创建 iSCSI 目标"）。阶段**只前进不回退**（迟到的服务端事件不会让界面倒着走）。
  phase,
  mounted_at, last_heartbeat_at, last_error,
  # ⚠️ last_error / last_error_detail 只在**真失败**时有值：state="error"（挂载失败，或
  #    心跳连续失败超过租约 TTL）与 state="revoked"（被服务端撤销）。
  #    state="mounted" 的挂载**永远不带错误**：心跳偶发失败只进日志与 server.last_error，
  #    绝不往挂载上贴（真实反馈："挂载成功时就不要展示最后错误了" —— 否则每个挂好的库
  #    旁边都挂着一个红色感叹号）。心跳恢复后代理会把这类 error 复位回 mounted
  #    （磁盘全程没被卸载），因此 error 不是单向门。
  # last_error_detail：最近一次失败的**原始报错文本**（如 PowerShell 自报的 .NET 异常）。
  # 与 last_error（稳定码，如 `connect:platform.ps_failed`）的区别：这个才是"为什么"。
  # ⚠️ 对"原始报错只进日志"的**刻意例外**，范围严格受限：只出现在挂载状态里（本机界面
  # 悬浮可读、可复制），**不进任何 API 错误响应**（ScriptError.Error() 不含它）。
  last_error_detail
}
```

**挂载阶段（`MountState.phase`）**——按真实顺序：

| phase | 含义 | 谁推进 |
|---|---|---|
| `requesting` | 已向服务端申请挂载 | 代理（调用服务端**之前**写入，界面立刻有反馈） |
| `allocating` | 服务端正在分配资源 | 服务端（SSE `event: mount`） |
| `preparing_disk` | 服务端正在准备虚拟磁盘（等差异盘/VHDX 就绪） | 服务端 |
| `configuring_target` | 服务端正在创建/下发 iSCSI 目标（PowerShell，最慢的一步） | 服务端 |
| `connecting` | 本机正在建立 iSCSI 会话（发起端服务/门户/CHAP） | 代理 |
| `online` | 本机正在让磁盘上线（去离线、去只读） | 代理 |
| `mount_point` | 本机正在挂载到盘符/目录 | 代理 |
| `post_script` | 本机正在执行后置脚本 | 代理 |

> 服务端侧三个阶段由服务端在自己的同步 mount 请求内发事件（`topic: mount`，
> `GET /v1/system/events`，`data` 内为 `{action:"progress", allocation_id, phase, at}`），
> 代理订阅后写进对应分配的状态并广播 `event: mount`。事件是**尽力而为**的：没有订阅者或
> 订阅缓冲满时会丢，此时界面停在 `requesting`/`connecting`，**不影响挂载本身**——
> 权威结果始终以 mount 响应与磁盘状态为准。

> 因此 `state="error"` 的记录只有两种含义：**本机还有残留待清理**，或**启动时自动挂载失败**
> （见 POST /agent/mount 的"失败不留记录"）。用户主动挂载失败**不会**产生记录。
>
> **界面展示规则**：错误只用**一个**红色感叹号标记（hover 展开、可复制），"本次请求错误"与
> "本机残留错误"合并成**多行**展示 —— 不再单列"最后错误"文本列，也不再与"错误"标签并列
> （真实反馈："只展示一次就行了，如果有多个错误信息加多一行去展示，不要展示多个，这样很乱"）。

> 排障要点：客户端报"没有更新"时，先看 `update.reason`；若为 `no_manifest`，再看
> `update.server_reason`（`manifest_missing` = 服务端解析出的 `artifacts_dir` 里没有
> `manifest.json`；`artifacts_dir_not_configured` = 未配置；`channel_unavailable` = 通道不符）。

## 挂载与卸载

```
POST /agent/mount
  ↓ {allocation_id, mount_mode?: "letter"|"directory", mount_path?: string}
  ↑ {mount:MountState}
  # 代理内部：POST {server}/v1/allocations/{id}/mount 取 MountSpec
  #          → New-IscsiTargetPortal（幂等）→ Connect-IscsiTarget（含 CHAP）
  #          → 等待会话连接 → 磁盘上线 → 挂载点分配（盘符或目录）
  #          → 执行 post_script（变量注入）→ 回写 /v1/leases/{id}/mounted
  # 幂等：同一 allocation 已挂载时直接返回既有状态
  #
  # **失败不留记录**（真实反馈："都错误了就不要有挂载记录了"）：
  #   - 用户主动挂载/重新挂载失败 → 清理这次尝试并**删除挂载记录**（不再留一条永久"错误"）；
  #     失败原因由**错误响应**当场带回（稳定码 + args 诊断 + 阶段提示），界面用红色感叹号
  #     展示一次即可。旧版本那条 error 记录会写进状态文件、跨重启存活、且每次启动 automount
  #     还会去"还原"它，于是永远消不掉 —— 这条路径已不存在。
  #   - 唯一例外：清理本身失败（会话/磁盘可能还挂在本机）→ 保留一条 error 记录作为人工
  #     "卸载"入口。残留绝不能变成"看不见"，那比一条刺眼的错误记录糟得多。
  #   - 启动时**自动挂载**（restore）失败则保留记录：本地记录代表"用户希望它保持挂载"的意图，
  #     抹掉它等于以后再也不自动挂回。

POST /agent/unmount
  ↓ {allocation_id}            # 没有 force：只有一种卸载语义
  ↑ {ok:true}
  # 代理内部：移除挂载点（必须先做，盘 Offline 后盘符就没了）
  #          → 磁盘 Offline（必须先于断开，否则报告 0xefff0040）
  #          → Disconnect-IscsiTarget（device_in_use 时补下线并重试）
  #          → Unregister-IscsiSession → POST /v1/leases/{id}/release
  # 只有一种失败：**挂载点还在**（盘符/目录仍属于该卷，Windows 认为卷被占用）→ 带
  # stage="mount_point" 返回 500，记录回滚成卸载前的状态（盘确实还挂着，界面如实显示）。
  # 用户关掉占用它的程序再点一次即可，不需要"强制卸载"这种第二入口。
  # 挂载点一旦移除（含报错但实际已不存在），后面几步都只是清残留：任一步失败只记日志、
  # 不中断，记录照样删除（服务端 release 会停用目标把残留会话踢掉）—— 因此不会留下
  # "盘符没了、状态还写着已挂载、还得再点一次强制卸载"的记录。

POST /agent/remount
  ↓ {allocation_id}
  ↑ {mount:MountState}

GET  /agent/mounts
  ↑ {items:[MountState...]}
```

## 磁盘内容下载（把母盘拷贝到本地）

把服务端的**母盘（parent）VHDX** 整盘拷贝到客户端本地目录。仅母盘可下载；
差异盘/独立盘会被服务端拒绝（`disk.not_parent`）。下载在代理后台异步执行，接口立即返回。

```
POST /agent/disks/download
  ↓ {disk_id, target_dir?, file_name?, server_url?}
  ↑ 202 {download:DownloadState}
  # target_dir 省略时用 config.default_download_dir；必须是绝对路径。
  # file_name 省略时由服务端 Content-Disposition 推导，推导失败回退 disk-<id>.vhdx；
  #           文件名必须是单个纯文件名（拒绝路径穿越、绝对路径与 \ / : 等分隔符）。
  # server_url 为预留字段，当前不生效：下载源始终取本地会话对应的服务端。
  # 同一 disk 同时只允许一个进行中的任务：重复请求返回既有任务（202），不重复下载。
  # 无会话时返回 409 agent.no_session（绝不静默失败）。

GET /agent/downloads
  ↑ {items:[DownloadState...]}                 # 按开始时间倒序

DELETE /agent/downloads/{disk_id}
  ↑ {ok:true}                                  # 取消进行中的任务；幂等（不存在也返回 ok）

DownloadState = {
  disk_id,
  file_name, target_path,
  state:"running"|"done"|"failed"|"canceled",
  received_bytes, total_bytes,
  error,                                       # 稳定错误码字符串（如 agent.download_failed / disk.not_parent / agent.server_unreachable）
  started_at, finished_at                      # 毫秒时间戳，finished_at 完成后才有
}
```

服务端契约：`GET /v1/disks/{id}/content`（需登录 + 该磁盘所属存储库的读权限）。

- 只允许母盘；非母盘返回 409 `disk.not_parent`（args.kind 为实际类型）。
- 磁盘记录不存在或 `vhdx_path` 文件缺失返回 404。
- 响应 `Content-Type: application/octet-stream`、
  `Content-Disposition: attachment; filename="<磁盘文件名>"`、`Accept-Ranges: bytes`、
  `Cache-Control: no-store`，支持标准 `Range`/`206`/`416`/`If-Range`（由 `http.ServeContent` 处理）。

代理实现要点：

- 目标文件先写 `<target_dir>/<file_name>.part`，完整（字节数 == total_bytes）后同目录 `os.Rename`
  为正式文件；进程重启或失败不会留下半截正式文件。
- **并行分段下载**：当服务端声明 `Accept-Ranges: bytes`、`total_bytes` 已知且 ≥ 64MiB、
  且 `download_connections > 1` 时，把 `[0, total)` 均分为 N 段（N = `download_connections`，
  1..8，默认 4）并发下载：边界固定为 `start_i = total*i/N`、`end_i = total*(i+1)/N - 1`。
  `.part` 先按 `total` 预分配（稀疏文件），每段用绝对偏移 `WriteAt` 写自己的区间；
  每段独立发 `Range: bytes=<start+done>-<end>`，互不影响。否则回退**单流**下载。
  拷贝缓冲固定 **1 MiB**（Go 默认 32 KiB 在高带宽链路上 syscall/CPU 开销明显）。
- **自动重试**：单个下载任务最多尝试 5 次，退避 1s/2s/4s/8s（可被取消打断），
  每次重试都从当前断点继续。可重试：5xx、网络中断、`io.ErrUnexpectedEOF`、连接重置等；
  不重试：404（磁盘/文件不存在）、403（权限）、409（非母盘）、416（Range 不满足）、空间不足等确定性错误。
- **断点元数据**：`<target_dir>/<file_name>.part.meta`（JSON）记录远端校验信息与各段已完成偏移，
  使取消 / 异常退出 / 进程重启后每个分段都能从各自的 `done` 继续，而非整段重下。字段：

  ```json
  {
    "version": 1,
    "disk_id": "<母盘 id>",
    "total_bytes": 21474836480,
    "last_modified": "Mon, 02 Jan 2006 15:04:05 GMT",  // If-Range 首选校验值
    "etag": "\"<可选>\"",                              // Last-Modified 缺失时的备选
    "connections": 4,                                  // 分段数（仅分段模式）
    "segments": [ {"start":0,"end":5368709119,"done":123456} ],  // 分段模式；done 为段内已写入字节数
    "single_done": 0                                   // 单流模式已落盘字节数（续传以 .part 实际大小为准）
  }
  ```

  `.part.meta` 采用「先写 `<meta>.tmp` 再原子替换」的方式更新（Windows 上用
  `MOVEFILE_REPLACE_EXISTING`），因此任何时刻磁盘上都有一份完整可解析的元数据；
  元数据中记录的偏移**永不超前于 `.part` 中已落盘的字节数**（`done` 仅在对应字节写完后累加），
  故进程被杀只会让进度"落后"（下次重下少量数据），不会产生静默损坏。
- **陈旧前缀保护（If-Range）**：续传时携带 `If-Range: <记录的 Last-Modified / ETag>`。
  若服务端返回 **200（而非 206）**，或返回 206 但 `Content-Range` 的总长与记录不符，
  说明远端母盘已被替换 → **丢弃 `.part` 与 `.part.meta`，从头重新下载**；返回 206 则正常续传。
  远端既无 `Last-Modified` 也无 `ETag` 时不启用该保护。
- **降级兼容**：`.part.meta` 缺失但 `.part` 存在（旧版本残留）时，视为「校验信息不可用」，
  降级为单流续传（不启用分段、不发 `If-Range`）；读取到响应头后再补全元数据，
  后续续传即可获得保护。元数据损坏则整体重置，避免把新内容拼到空洞后面。
- **空间预检**：开始写盘前检查目标目录所在卷可用空间。已知 `total_bytes` 时要求
  `可用 >= total + max(1GiB, total*5%)`；未知时至少要求 1GiB。不足则直接失败并返回
  `agent.insufficient_local_space`（args.need_bytes / args.free_bytes），不写任何数据。
  写盘期命中磁盘写满（ENOSPC）同样映射为该错误码；其它写错误保留 `.part` 供续传。
- 完整性只按「字节数 == total_bytes」判断（服务端不提供摘要，不做全量 SHA256）。
- 取消/失败**保留 `.part` 与 `.part.meta`**，下次发起同一磁盘下载时自动断点续传；
  仅长度校验不通过（确定性损坏）或 Range 不满足（416）时删除断点。
- 每个场景都会通过 SSE `download` 事件推送一次 `DownloadState`；进度为各段已收字节之和，
  统一按「写入 ≥1MiB 或距上次 ≥500ms」节流推送。

页面刷新后恢复展示：直接调用 `GET /agent/downloads` 即可（任务为代理内存态，进程重启后清空，
但 `.part` 仍在磁盘上，重新发起下载会续传）。

## 本地目录扫描与上传建库（把客户端本地目录上传成存储库）

把客户端本地某个目录（可能是几十 GB / 几十万文件）分块上传到服务端并**建库**。全程在代理后台
异步执行，接口立即返回；进度经 SSE `upload` 事件推送。

```
POST /agent/fs/scan
  ↓ {path}
  ↑ {path, file_count, total_bytes, warnings:[string]}
  # path 必须是非空绝对路径，且存在、是目录、可读（否则 system.invalid_param）。
  # 只递归统计常规文件；遇符号链接/不可读项记入 warnings 并跳过（不因此失败）。
  # 空目录（无常规文件）→ 409 agent.scan_empty（服务端不接受空 manifest）。
  # 文件数 > 200000 或总字节 > 2TiB → 413 agent.scan_too_large（args 含上限与实测值）。
  # 不返回完整文件清单（可能几十万条；清单只在代理内部使用）。

POST /agent/uploads/start
  ↓ {local_dir, repo_name, repo_mode?, storage_id?, quota_bytes?, source_mode?}
  ↑ 202 {upload:UploadState}
  # local_dir 必须是非空绝对路径且存在、是目录。
  # repo_mode："shared"（默认）| "exclusive"。
  # storage_id 省略时由服务端按可用空间自动选根；给定时校验该存储存在且启用。
  # quota_bytes 省略/0 表示不限。
  # source_mode："copy"（默认）| "move"；move 仅在建库成功后删除本地源目录。
  # 无会话 → 409 agent.no_session（绝不静默失败）。
  # 同一 local_dir 同时只允许一个进行中的上传：重复请求返回既有任务（202），不重复上传。

GET /agent/uploads
  ↑ {items:[UploadState...]}                    # 按 started_at 倒序

DELETE /agent/uploads/{upload_id}
  ↑ {ok:true}                                    # 取消进行中的上传；幂等（不存在也返回 ok）
  # 取消会停止上传并**尽力**调用服务端 DELETE /v1/uploads/{id} 放弃会话（清理服务端暂存），
  # 同时删除本地续传记录。

UploadState = {
  upload_id,                                     # 服务端上传会话 ID（scanning 阶段可能为空）
  repo_name, local_dir,
  state:"scanning"|"creating"|"uploading"|"completing"|"done"|"failed"|"canceled",
  total_files, total_bytes, uploaded_bytes,
  chunk_total, chunk_done,
  job_id,                                        # 建库任务 ID（completing 起）
  repo_id,                                       # 建库成功后按名称反查得到（拿不到时省略）
  error,                                         # 稳定错误码字符串（如 agent.upload_failed / agent.scan_empty）
  started_at, finished_at                        # 毫秒时间戳
}
```

服务端契约：`POST /v1/uploads`、`PUT /v1/uploads/{id}/chunks/{index}`、`GET /v1/uploads/{id}`、
`POST /v1/uploads/{id}/complete`、`GET /v1/jobs/{id}`、`GET /v1/repos`（需登录，Bearer 会话令牌）。
所有调用都走当前会话对应的服务端，并沿用连接指纹固定。

代理实现要点：

- **顺序**：扫描（生成 manifest，按相对路径升序排序，该顺序即字节流顺序）
  → `POST /v1/uploads` 建会话 → 按 `missing_chunks` 并发 4 路 PUT → `POST /complete`
  → 轮询 `GET /v1/jobs/{id}` 直到终态 → 成功则按名称反查 `repo_id`。
- **分块**：从本地文件按**全局偏移量**读取 `[index*8MiB, min(...))` 区间拼成一个 8MiB 缓冲
  （可跨多个文件，用 `sync.Pool` 复用），计算 SHA256（小写 hex）后 PUT；请求头 `X-Chunk-SHA256` 必填，
  除最后一块外必须恰好 8MiB。
- **重试**：单块最多尝试 5 次，退避 1s/2s/4s/8s（可被取消打断）。可重试：429、5xx、网络错误、读超时；
  不可重试：4xx 确定性错误与上下文取消。
- **续传**：把 `{upload_id, local_dir, repo_name, total_bytes, total_files, started_at, 选项}` 持久化到
  `<DataDir>/uploads.json`（临时文件 + `MoveFileEx` 原子替换）。再次 `start` 时若存在**同一 local_dir +
  repo_name** 的未完成会话，且服务端 `GET /v1/uploads/{id}` 仍报未完成、且总大小/文件数与本地一致，
  则复用该 `upload_id` 并从 `missing_chunks` 继续（不重传已完成的块）。会话已终态或本地内容已变化时，
  丢弃记录并新建会话；服务端不可达时明确失败（不静默新建）。
- **move 语义**：**仅**在服务端建库任务成功完成后删除本地源目录（删除前记 INFO 日志）；失败/取消**绝不删**。
- **取消语义**：进程退出等上下文取消 → 停止上传、保留服务端会话与已传分块（可续传），推送 `canceled`；
  显式 `DELETE /agent/uploads/{id}` → 除上述外，尽力调用服务端 DELETE 放弃会话并删除续传记录。
  例外：完成建库任务**已提交**（状态 `completing`）后取消，**不再**调用服务端 DELETE——
  此时删除暂存目录会破坏正在运行的建库任务；服务端任务会自行跑完，客户端仅停止跟踪。
- **去重**：同一 `local_dir` 同时只允许一个进行中的上传。
- 日志只记录路径/字节数/块序号，**绝不记录文件内容**。

## 本地配置

```
GET   /agent/config
  ↑ {auto_mount:bool, default_mount_mode, default_mount_dir, default_download_dir,
     download_connections, language, start_at_login:bool, update_channel, server_alias}
PATCH /agent/config
  ↓ 上述字段的任意子集
  ↑ {config:{...}}
```

`default_download_dir` 是「母盘拷贝到本地」的默认目标目录，默认 `C:\Vault\Downloads`。
取值必须是非空绝对路径；非法值返回 `system.invalid_param`（args.field=default_download_dir）。
该目录不要求预先存在，下载时会按需创建。

`download_connections` 是「母盘拷贝到本地」的并行分段数，取值 1..8，默认 4。
取值超出范围时 PATCH 返回 `system.invalid_param`（args.field=download_connections）；
从配置文件读取到越界值时按「<=0 取默认 4、>8 取 8」归一化。

> 客户端资源热更（client_web）**没有独立的本地配置项**：它使用与自更新相同的更新通道
> （`update_channel`）与更新源（当前会话的服务端）；激活状态以磁盘上的
> `%ProgramData%\Vault\webapp\current.json` 为准，查询状态用 `GET /agent/update/web`。
>
> **静默热更（默认行为）**：会话建立后（客户端每次启动都会推送会话）代理自动检查并激活一次，
> 无需用户操作；激活成功后客户端主进程会**立即切换到新资源**（`vault:apply-web-layer`，
> 不重启客户端）。只有**比本机应用版本更新**的资源才会被激活 —— 客户端主进程只在"热更层比
> 内置层新"时才加载热更层，代理侧用同一口径提前拦截，避免出现"设置页说已更新、界面却纹丝不动"
> 的分裂状态（该情形以 `not_newer` 记入 `web_update.error`，不再是 `activated`）。

## 事件流（SSE）

```
GET /agent/events
  ↑ text/event-stream，事件类型：
     event: mount       data: MountState
     event: unmount     data: {allocation_id}
     event: revoked     data: {allocation_id, reason}   # 服务端踢下线，代理已自行卸载
     event: server      data: {connected, last_error}   # 连接状态**真正变化**时才发（值未变不发）
    event: host        data: HostState                 # 本机就绪状态（iSCSI 发起端）变化时才发：
                                                       # 启动探测完成、管理员启动 MSiSCSI 后自动转就绪
     event: session     data: SessionView               # 客户端证书会话自动续期成功（新令牌）
     event: update      data: {available_version, downloading, received_bytes, total_bytes}
                                                         # 自更新下载进度
     event: web_update  data: WebUpdateState             # 渲染层资源热更状态/进度
     event: heartbeat   data: {at}                       # 可用于界面上展示"在线"
     event: download    data: DownloadState              # 磁盘下载进度/开始/完成/失败/取消
     event: upload      data: UploadState                # 本地目录上传建库进度/开始/完成/失败/取消
```

进度类事件（`update` / `web_update` / `download` / `upload`）统一按"有时间闸门"节流：
**最快每 200ms 推一次，且距上次推送 ≥500ms 必推**（慢速传输也有心跳式更新），
位于两者之间时需累计新增 ≥1MiB 才推。每个订阅者缓冲 256 条，满时丢弃**最旧**的一条
（保留最新进度比保留全部历史更重要），发布方绝不阻塞。

## 服务端连通性测试（供"修改服务端地址"用）

```
POST /agent/server/test
  ↓ {url}
  ↑ {ok:true, server_name, api_version, server_version, client_compat:{enabled,min,max}}
  # 代理代发匿名 GET {url}/v1/system/info，规避渲染进程的跨域与证书问题
```

## 自更新（Electron 本体 / Go 代理）

```
POST /agent/update/check   ↑ {available:bool, version, notes, size_bytes, source:"<服务端名>",
                              reason?, server_code?, server_reason?, server_args?}
POST /agent/update/apply   ↑ {started:true}
  # 有活跃挂载时返回 409 + {code:"agent.busy_mounts", args:{count}}，由前端提示"请先卸载"
```

`check` 为"无可用更新"时返回软失败（HTTP 200，`available:false`），并通过以下字段说明原因：

- `reason`：代理侧的稳定原因码（`no_manifest` / `server_unreachable` / `not_newer` / `no_artifact` …）。
- `server_code` / `server_reason` / `server_args`：**原样保留更新源（服务端）的结构化错误**，
  例如 `server_code="update.no_manifest"`、`server_args={"reason":"manifest_missing"}`。
  这类字段只在服务端返回结构化错误时出现，用于定位"到底为什么没有更新"。

## 客户端资源热更（client_web）

不替换 Electron 本体，只把**渲染层静态资源**（Vite 产出的 `dist/`：`index.html` + `assets/`）
热更到本机，实现界面快速迭代。资源包由发布方以 `kind=client_web` 写入更新清单，走与自更新
相同的验签 / 摘要链路。

```
POST /agent/update/web/apply
  ↑ 202 {web_update: WebUpdateState}
  # 后台异步执行"下载 → 校验 → 解压 → 激活"；立即返回当前状态，进度经 web_update 事件推送。
  # 无会话 / 清单不可用 / 无 client_web 产物 / 版本不更高 时，状态置 failed 且 error 给出原因码。

GET /agent/update/web
  ↑ {web_update: WebUpdateState}      # 页面刷新后据此恢复展示

WebUpdateState = {
  state:"idle"|"downloading"|"verifying"|"extracting"|"activated"|"failed",
  available_version,             # 本次尝试热更到的目标版本（解析清单后填充）
  active_version,                # 当前已激活版本（current.json 指向的版本）
  received_bytes, total_bytes,   # 下载进度（≥1MiB 或 ≥500ms 节流推送）
  error,                         # 失败时的稳定错误码（如 agent.web_update_failed / update.not_available / not_newer）
  updated_at                     # 最后一次状态变更的毫秒时间戳
}
```

**落盘布局（与 Electron 主进程的冻结契约）**

```
<代理数据目录>/webapp/                 # 代理数据目录 = %ProgramData%\Vault
<webapp>/<version>/                   # 版本目录：渲染层产物（index.html、assets/**）
<webapp>/current.json                 # 激活指针
```

`current.json`：

```json
{ "version": "0.1.21", "dir": "0.1.21", "activated_at": 1730000000000 }
```

Electron 主进程应读取 `%ProgramData%\Vault\webapp\current.json`，以其中的 `version`
（或 `dir`）拼出 `%ProgramData%\Vault\webapp\<version>\index.html` 加载渲染层；
指针不存在时回退到随 Electron 本体打包的默认资源。

**校验与激活（代理侧实现要点）**

- 校验：清单签名已在 `check` 阶段校验（`VerifyAndParse` 先验签后解析）→ 产物 sha256
  **边下边算**并与清单比对（不符 → 失败且**不激活**）→ 版本单调性：**仅允许更新到比当前
  `active_version` 更"新"的版本**（不允许降级；当前无 active 时允许任意版本）。
- 解压：只接受 zip 内的**相对路径**，拒绝 `..` / 绝对路径 / 驱动器号（防 zip slip），
  且限制解压后总大小（清单声明 size 的 1.5 倍 + 1MiB，防 zip bomb）。
- 原子激活：先解压到 `<webapp>/<version>.tmp-<random>/`，确认 `index.html` 存在后再
  `os.Rename` 成 `<webapp>/<version>/`，最后以"临时文件 + `MoveFileEx(REPLACE_EXISTING|
  WRITE_THROUGH)`"原子替换 `current.json`（**不用 `os.Rename`**：Windows 上它不覆盖已存在目标）。
- 失败/取消：清理 `.tmp-*` 目录；已存在的旧 `<version>/` 与 `current.json` 保持可用，
  **绝不留下半激活状态**。
- 保留最近 2 个版本目录，更旧的删除。
- 幂等：同一版本重复 apply 直接返回 `activated`，不重复下载。

## 退出

```
POST /agent/shutdown       ↑ {ok:true}   # 托盘"退出"使用；代理会先卸载全部挂载再退出
```

## 错误约定

与主服务端一致：

```json
{"error":{"code":"agent.xxx","args":{}}}
```

> **原始报错文本的唯一例外**：错误响应里永远只有稳定码与稳定参数（如
> `platform.ps_failed` + `args.reason/step`），PowerShell 的原始报错**不进** `code`、
> `args` 或 `message`。唯一的例外是挂载状态里的 `last_error_detail`（见上文 MountState）：
> 它由代理**主动**从 `winps.ScriptError.Message` 取出（该字段不参与 `Error()` 文本），
> 仅用于本机诊断展示 —— 否则用户只看到一个 `agent.mount_failed`，等于没有线索。

常用错误码：

| code | 含义 |
| --- | --- |
| `agent.token_invalid` | 本地令牌缺失或不匹配（401） |
| `agent.no_session` | 尚未推送服务端会话（409） |
| `agent.admin_required` | 代理未以管理员权限运行（403） |
| `agent.mount_failed` | 挂载失败（500，args.stage 指明阶段）。**connect / wait_connected 阶段**还会带诊断参数：`args.portal`（门户地址:端口）、`args.target_iqn`、`args.auth_mode`、`args.tcp`（门户 TCP 可达性探测结论：reachable/timeout/refused/unreachable），以及 `args.detail`（**原始报错**，如 Connect-IscsiTarget 的 .NET 异常 —— 失败记录会被抹掉，这是界面唯一能看到的真因）。**绝不含 CHAP 密钥** |
| `agent.unmount_failed` | 卸载失败（500，args.stage 指明阶段；统一卸载后只剩 `mount_point`：该盘仍被程序占用，界面会附"关掉占用它的程序再重试"的提示） |
| `agent.busy_mounts` | 有活跃挂载，操作被拒（409） |
| `agent.not_mounted` | 指定分配当前未挂载（404） |
| `agent.server_unreachable` | 无法连接服务端（502） |
| `agent.server_timeout` | 服务端未在超时内响应（504，args.timeout_seconds）。**与上一行的区别**：挂载申请等服务端同步重活超时用此码，避免把"服务端还在干活"误报成"连不上"（挂载申请超时为 2 分钟） |
| `agent.download_failed` | 母盘下载失败（500，args.stage 指明阶段，如 download_body / verify_size / resume_range） |
| `agent.web_update_failed` | 渲染层资源热更失败（500，args.stage 指明阶段，如 extract / activate / activate_pointer） |
| `update.not_available` | 当前没有可用更新（409；渲染层热更无 `client_web` 产物时也会以该码写入 `web_update.error`） |
| `update.digest_mismatch` | 更新产物 sha256/大小与清单不一致（422，不激活） |
| `agent.insufficient_local_space` | 目标卷可用空间不足，未开始/继续写盘（507，args.need_bytes / args.free_bytes） |
| `agent.scan_failed` | 本地目录扫描失败（500，args.stage 指明阶段，如 stat / walk） |
| `agent.scan_empty` | 本地目录为空（没有可上传的常规文件）（409） |
| `agent.scan_too_large` | 本地目录超出扫描上限（413，args.file_count / args.total_bytes / args.max_files / args.max_bytes） |
| `agent.upload_failed` | 本地目录上传建库失败（500，args.stage 指明阶段，如 read_local / attempts_exhausted / job） |
| `system.invalid_param` | 参数非法（400，args.field 指明字段，如 target_dir / file_name / default_download_dir / download_connections / path / local_dir / repo_name / repo_mode / source_mode / quota_bytes） |
| `disk.not_found` | 磁盘不存在（404，来自服务端，会原样透传到 `DownloadState.error`） |
| `disk.not_parent` | 该磁盘不是母盘，不支持内容下载（409，来自服务端） |
