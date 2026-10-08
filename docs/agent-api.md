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

**多服务端：一台机器可同时登录多台服务端**（即客户端 `servers[]` 里的每一台），代理为**每台**
各自维护一份独立状态：会话与令牌、证书身份、事件订阅、租约心跳、自动挂载、令牌续期、
断线重连。所有带服务端上下文的操作都以 `server_key` 定位到具体某一台，互不覆盖、互不串台。
`server_key` 的算法与前端 `ServerEntry.key` 一致：**实例 ID 优先，其次规范化地址**
（小写、去首尾空白与末尾斜杠）。

客户端启动时为 `servers[]` 里的每一项各推一次会话（顺序不限）即完成"全部自动登录"：
代理收到后各自建流并自动挂载该台的库，无需用户逐台点击。

```
POST   /agent/session
  ↓ {server_url, server_instance_id, server_name,
     alias?,             # 可选：客户端为**本机**给该台起的别名（目录模式的挂载点命名用）
     token, user_id, username, expires_at, cert_sha256,
     server_key?,        # 可选：指明这是哪一台（省略时按 instance_id / server_url 推导）
     primary?}           # 可选：把该台设为主服务端（见下）；首台注册时自动成为主服务端
  ↑ {ok:true, server_key}   # 回传该会话实际归属的服务端键（同一台的两种键在代理内会合并）

DELETE /agent/session              # 退出登录：不带参数 = 清**全部**会话（逐台停止心跳、解除订阅）
  ↑ {ok:true, cleared:[server_key...]}
DELETE /agent/session?server_key=… # 只登出**这一台**
  ↑ {ok:true, server_key}          # 键匹配不上任何已登记服务端 → 404 agent.server_unknown
# 退出登录只丢令牌，不卸载已有挂载，也不删该台的服务端条目（本机客户端证书身份仍在）。

POST   /agent/server/reconnect     # 界面"重试"按钮：清零自动重连失败计数并立即重连一次
  ↓ {server_key?}                  # 省略时针对主服务端
  ↑ AgentServerState               # 不可归属 → 404 agent.server_unknown
```

**主服务端（primary）**：多台之中只有一台是"主"，供**没有服务端上下文**的操作使用 ——
自更新与渲染层资源热更的**更新源**（`update` / `web_update` 只认一台，混用会让版本判定自相矛盾，
见 implementation.md 7.4.1）。首台注册时自动成为主服务端，之后由 `primary:true` 显式指定。
桌面客户端把**当前活动服务端**标为 primary（每次推送会话时随 `server_key` 一起声明：
活动那台 `primary:true`、其余为 false），因此**切换服务端会重新推送会话，更新源随之切到新台** ——
否则主服务端会一直停在"第一台注册的"，切换服务端后"检查更新"仍打旧服务器（真实反馈）。
`/agent/state` 的 `primary_key` 与每台的 `primary` 字段可查当前是哪一台。

**兼容**：以上 `server_key` / `primary` / `alias` 都是可选字段。只推一台会话的老客户端
行为完全不变（那台即主服务端，`DELETE /agent/session` 不带参数也只清它一台）。

`cert_sha256` 是服务端证书 DER 的 SHA-256（小写十六进制），由前端在「测试连接」时经
`POST /agent/server/test` 得到并固化（TOFU 首次信任）。服务端使用自建 CA，代理无法用公信
CA 验证，因此以该指纹固定服务端身份：**指纹不符即拒绝连接**。该字段可选，缺省时退回
Go 默认的证书链校验。渲染进程侧的同名信任由 Electron 主进程的 `certificate-error` 实现
（同样只放行指纹一致的自签证书）。

### 客户端证书会话自动续期

代理**常驻进程**负责续期（界面关掉也能续），渲染进程只是消费方：

- **前置条件**：已安装**该服务端**的客户端证书身份（`GET /agent/identity?server_url=…` 的
  `installed=true`）且当前有会话；
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

### 无会话时的自动重连与手动重试

代理重启后客户端**不会**重新推会话（客户端只在自身启动/登录时推一次），此时没有会话可用。
为此代理内建一条自动重连循环：**无会话 + 未连接 + 已装该服务端的身份**时用该服务端的客户端证书
（`POST {server}/v1/auth/cert-login`）换一个新会话回来，效果与 `POST /agent/session` 一致。

- **首次等待**：启动后 20s 才试第一次（给客户端推会话留出时间，避免抢先后白多一次证书登录）；
- **复检间隔**：30s；
- **放弃条件**：**连续失败 2 次**即停手，并把 `server.phase` 置为 `disconnected`
  （`fail_count` 记在上限上）。无休止重试只会刷满日志，用户也永远分不清"还在连"和"连不上"；
- **手动重试**：界面在"未连接"旁给"重试"按钮，调下面的接口。重试会**清零失败计数**并立即
  试一次，成功前 `phase` 回到 `connecting`（界面显示转圈的"连接中"，不显示"未连接"）。

```
POST /agent/server/reconnect      # 界面"重试"按钮：清零失败计数 + 立即重连一次
  ↑ AgentServerState              # 成功时的最新服务端状态（连接状态另经 server 事件广播）
```

### 客户端证书身份（一台机器可装多个服务端）

服务端校验 `POST {server}/v1/auth/cert-login` 的证书必须由**本实例**签发，因此代理把身份
**按服务端**保存：同时连多个服务端时每个服务端各有一份证书/私钥，**互不覆盖**。
定位规则：**服务端实例 ID 优先，其次规范化地址**（小写、去首尾空白与末尾斜杠）。

登录、自动续期、断线重连都**先按目标服务端定位身份**，绝不拿别的服务端的证书去试探
（盲试会消耗服务端防爆破计数，见 implementation.md 9.1/R22）。

```
GET /agent/identity                  # 不带参数：返回"活动"身份（最近安装的那份）
GET /agent/identity?server_url=<url>&instance_id=<id>
  ↑ AgentIdentity                    # 指定服务端：该服务端没装证书时 installed=false
GET /agent/identities                # 本机保存的全部身份（设置页列"其他服务端的证书"用）
  ↑ {active?:<server_key>, identities:[AgentIdentity...]}
POST /agent/identity/install         # 用当前会话向服务端换取证书并落盘（按服务端归属）
  ↓ {server_url, token, user_id, username, cert_sha256}
  ↑ AgentIdentity
DELETE /agent/identity?server_url=<url>&instance_id=<id>   # 只删该服务端的证书
DELETE /agent/identity?all=true                            # 删全部
DELETE /agent/identity                                     # 都不带：删"活动"身份
  ↑ {ok:true}
POST /agent/identity/login           # 用本地证书免密登录
  ↓ {server_url?, instance_id?}      # 都省略时用"活动"身份
  ↑ {token, expires_at, user:{id, username}, ...}

AgentIdentity = {
  installed:bool,
  server_key,                        # 身份归属的服务端标识（实例 ID 优先，其次规范化地址）
  server_url, server_instance_id, server_cert_sha256,
  user_id, username, serial, fingerprint_sha256, spki_sha256,
  not_before, not_after, installed_at
}
# 永不含私钥与证书原文；落盘文件（identity.json，v2 结构）写后收紧 ACL（仅 SYSTEM 与 Administrators）。
# 兼容：老版本的"整份文件就是一个身份"（v1）会被自动读入并在下次写入时升级为 v2；
# 本机只有一份身份时，连接路径允许按它兜底（老客户端只能装一份，地址/实例 ID 可能变过）。
```

## 状态查询

```
GET /agent/health
  ↑ {ok:true, agent_version, platform, admin:true|false,
     host:HostState,
     servers:[AgentServerState...],   # 全部已登记服务端（多服务端：逐台一份，主服务端在前）
     primary_key,                     # 主服务端的 server_key
     server:AgentServerState}         # 主服务端那一台（单服务端时代的兼容字段；未登记时为空值）
# health 里的每台**不含** user / session：健康检查没必要顺带吐出会话令牌。

GET /agent/state
  ↑ {
      servers:[AgentServerState...],   # 逐台完整状态（含该台的登录用户与会话）
      primary_key,
      # 以下三个是单服务端时代的**兼容字段**，一律取主服务端那一台：
      server:{url, instance_id, name, connected, phase, fail_count, last_error,
              server_key, primary, alias},
      user:{id, username},
      session:SessionView,             # 可选：主服务端会话（未推送会话时缺省）
      mounts:[MountState...],
      auto_mount:bool,
      host:HostState,
      update:UpdateInfo,
      web_update:WebUpdateState
    }

AgentServerState = {
  server_key,                        # 该台的本地键（实例 ID 优先，其次规范化地址）
  primary:bool,                      # 是否主服务端（更新源等无上下文操作的默认目标）
  url, instance_id, name,
  alias,                             # 本机给该台起的别名（挂载目录命名用），可空
  # phase / fail_count：连接阶段与**连续**失败次数。界面据此区分"正在连"与"连不上" ——
  #   connecting   ：正在连接/自动重连（界面显示转圈的"连接中"，**不显示**"未连接"）；
  #   connected    ：已连接；
  #   disconnected ：自动重连次数已用尽（界面显示"未连接" + 手动重试按钮）。
  # fail_count 达上限后代理不再自动重连，等界面调 POST /agent/server/reconnect。
  connected, phase, fail_count, last_error,
  user:{id, username},               # 仅 /agent/state 返回
  session:SessionView                # 仅 /agent/state 返回；未推送会话时缺省
}
```

**界面必须逐台渲染**：`server_key` 是状态、挂载、事件三者的归属键。一条 `server` 事件只更新
**它那一台**（事件负载也带 `server_key`），否则两台服务端的状态会互相覆盖。

# 日志模块：日志按天切分，一次回一天的文件尾部。
# day=YYYY-MM-DD 指定日期（省略或非法一律按当天）；tail 为读取字节数（默认 256KiB，上限 8MiB）。
GET /agent/log?day=<日期>&tail=<字节数>
  ↑ {day:<本次返回的日期>, days:[<可查询日期(升序)>],
     path:<该日期日志文件路径>, text:<原始文本(JSON Lines)>, error?}

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
  # server_key / server_url / server_name：**这条挂载归属哪台服务端**。
  #   心跳、挂载点回写（/v1/leases/{id}/mounted）、租约释放都发给它 ——
  #   多服务端下绝不能把 A 的租约回写到 B。
  #   老状态文件里没有该字段的记录，加载时归给"当时唯一/主服务端"。
  server_key, server_url, server_name,
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
  # session_active：**实测**的本机 iSCSI 会话状态 —— 本机发起端当前是否还有到 target_iqn 的
  #   活动连接（Get-IscsiSession 里 IsConnected 为真的会话）。
  #   为什么要有它：state 只是**代理的记录/意图**，盘是否真的挂着由会话决定。会话被系统或
  #   用户断开（MSiSCSI 服务重启、长时间断网、手动在发起程序里断开）之后，记录仍是 mounted，
  #   界面于是显示"已挂载 + 卸载"，而盘根本不在 —— 用户既用不了盘、点卸载还会因为"挂载点
  #   不存在"失败（真实诉求："挂载和卸载的按钮应以实际为准，看对应的 iSCSI 连接是否在活动中的"）。
  #   界面判定**一律看它**：实测无会话时显示"已断开"、主按钮换成"挂载"。
  #   代理对 mounted / error 记录（后者语义是"本机还有残留待清理"，也可能还挂着）每 ≈20s 实测
  #   一次，结论变化时推 mount 事件；mounting/unmounting/revoked 不实测。
  #   ⚠️ 只有实测为真才是 true；探测失败**不猜**（保持原值，绝不把好盘显示成断线）。
  # session_checked_at：最近一次**得出会话结论**的时刻（毫秒）；结论不变时不刷新，不写盘。
  #   0/缺省 = **尚未核对**（代理刚启动、记录刚从状态文件加载）。此时界面按 state 展示，
  #   **不得**把 session_active 的缺省值当成"断线"（旧代理没有这两个字段，同样按 state 展示）。
  session_active, session_checked_at,
  # cache_enabled / cache_active / cache_error：本库的**本地 iSCSI 读缓存**状态。
  #   cache_enabled：本库是否启用了缓存（来自 repo_mounts，见「本地配置」）—— 用户的**意图**；
  #   cache_active ：本次挂载实际是否走了本地缓存代理（挂载链路 = 本机 → 本地缓存代理 →
  #                  服务端目标）—— 实际**生效**结果。此时 target_iqn/portal 记录的是**本地门户**
  #                  公示的 IQN 与地址（127.0.0.1:3261），不是服务端目标；
  #   cache_error  ：启用了却没生效时的原因（如门户端口被占用）。代理不可用时挂载会**自动回退**
  #                  直连服务端目标（cache_active=false 且 cache_error 说明原因）—— 绝不因缓存
  #                  导致挂载失败。
  # 界面判据：cache_enabled=true 且 cache_active=false 时显示"缓存未生效"+原因（tooltip）。
  # 三者在挂载时定下（与配额一致），改 repo_mounts 里的 cache_enabled **需重新挂载**才生效。
  cache_enabled, cache_active, cache_error,
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
  ↓ {allocation_id, mount_mode?: "letter"|"directory", mount_path?: string,
     server_key?, server_url?}     # 归属服务端：省略时按"本机既有记录 → 主服务端"兜底；
                                   # 给了却匹配不上任何已登记服务端 → 404 agent.server_unknown
  ↑ {mount:MountState}
  # 代理内部：POST {server}/v1/allocations/{id}/mount 取 MountSpec
  #          → New-IscsiTargetPortal（幂等）→ Connect-IscsiTarget（含 CHAP）
  #          → 等待会话连接 → 磁盘上线 → 挂载点分配（盘符或目录）
  #          → 执行 post_script（变量注入）→ 回写 /v1/leases/{id}/mounted
  # 幂等：同一 allocation 已挂载**且实测会话还在**（session_active）时直接返回既有状态。
  #   判据必须带上实测会话，只看记录会撒谎：会话断掉后记录仍是 mounted，直接返回等于告诉
  #   用户"已挂载"，而盘根本不在。实测无会话（或尚未核对/探测失败）时照常走完整流程去真重连，
  #   幂等步骤会复用门户/会话，服务端也复用租约 —— 用户不必先"卸载"再"挂载"。
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
  #   唯一例外：卸载前的状态**本身就是 unmounting**（上次进程死在卸载中途留下的脏记录，
  #   见 GET /agent/mounts 的状态说明）→ 落成 error 并写入失败原因。把"卸载中"回滚成
  #   "卸载中"等于什么都没发生：界面永远显示"卸载中"、点几次都一样（真实反馈："重启之后
  #   客户端一直提示在卸载中，这种名存实亡的我要能在客户端自己闭环"）。
  # 用户关掉占用它的程序再点一次即可，不需要"强制卸载"这种第二入口。
  # 判"挂载点还在不在"有三条兜底，任一条成立即视为已移除：
  #   ① **盘符模式的挂载点自己不存在** —— 最硬的一条：不需要磁盘号、也不需要发起端。进程
  #      重启后磁盘号无从得知（运行时信息只在内存里），发起端没跑时连会话都问不出来，此时
  #      它是唯一出路；缺了它，一条"盘符早没了"的记录会永远卡在卸载里。只认 os.ErrNotExist：
  #      盘符在、卷没就绪（ERROR_NOT_READY 等）不算 —— 那正是"让用户重试"的正常场景，判成
  #      已移除就会谎报卸载完成；
  #   ② 磁盘号已知且查不到挂载点；
  #   ③ 本机实测已无活动会话（盘随会话消失，挂载点不可能还在）——专治"会话被断后记录仍是
  #      已挂载"：进程重启后磁盘号无从得知，缺了它就会既挂不上又卸不掉。
  #      这三条都在 mountPointGone（internal/agent/mount.go），结论都拿不到时按"移除失败"
  #      处理：宁可让用户重试，也不谎报卸载完成。
  # 挂载点一旦移除（含报错但实际已不存在），后面几步都只是清残留：任一步失败只记日志、
  # 不中断，记录照样删除（服务端 release 会停用目标把残留会话踢掉）—— 因此不会留下
  # "盘符没了、状态还写着已挂载、还得再点一次强制卸载"的记录。

POST /agent/remount
  ↓ {allocation_id}
  ↑ {mount:MountState}
  # 归属跟随既有挂载记录里的 server_key（重挂不会换台）。

GET  /agent/mounts
  ↑ {items:[MountState...]}
```

## 本地读缓存（iSCSI 缓存代理）

客户端本地读加速：**一个客户端只起一个代理进程**（内嵌在 Vault-Agent 内），代理在
`127.0.0.1:3261` 上开一个 iSCSI 门户，**同时服务多个**启用了缓存的库 —— 按 TargetName
（每个库一个本地 IQN）路由到各自的后端目标。启用缓存的库挂载链路变为
**本机 → 本地缓存代理 → 服务端目标**；未启用的库仍是本机 → 服务端目标直连。

```
GET /agent/cache
  ↑ CacheStatus
  # 门户没开（没有任何库启用缓存）时 running=false、targets 为空；存储库页面据此展示
  # "缓存用量/命中情况"。

CacheStatus = {
  running:bool,                 # 门户是否已打开（**懒启动**：首个库启用缓存时创建）
  addr,                         # 门户监听地址（running 时有值），如 "127.0.0.1:3261"
  l1_limit_bytes, l2_limit_bytes,   # 客户端总量预算（L2 为 0 表示关闭 L2）
  l1_quota_bytes, l2_quota_bytes,   # 当前**均分**给每个启用缓存的库的配额（总量 / 库数）
  enabled_repos:int,            # 已启用缓存的库数（决定配额分母）
  targets:[CacheTargetStatus...],   # 按库（分配）汇总的用量与命中情况
  error                         # 可选：最近一次打开门户失败的原因（如端口被占用）
}

CacheTargetStatus = {
  allocation_id,                # 与挂载记录、存储库卡片一一对应
  target_iqn,                   # 本地门户为该库公示的 IQN（挂载链路上实际连接的）
  l1_used_bytes, l1_limit_bytes,   # 内存缓存占用与配额
  l2_used_bytes, l2_limit_bytes,   # L2 文件占用与配额（未启用 L2 时均为 0）
  reads,                        # 读命令数
  request_hits,                 # **整条命令**完全由缓存满足的次数
  l1_hits, partial_hits, l2_hits, backend_reads,   # 分段细粒度计数
  writes,
  hit_rate                      # request_hits / reads（0..1）；reads 为 0 时为 0
}
```

要点：

- **设面只按客户端**：L1/L2 总量在「客户端设置」里配（`PATCH /agent/config` 的
  `cache_l1_bytes` / `cache_l2_bytes`，见「本地配置」），**是否启用按库配**
  （`POST /agent/repo-mounts/{repo_id}` 的 `cache_enabled`）。总量由所有已启用缓存的库均分。
- **懒启动**：没有任何库启用缓存时不开门户；第一个库启用并挂载时才创建监听。
- **回退直连**：门户建不起来（如 3261 被占用）时，库照常挂载，只是回退直连服务端目标，
  原因记在该库挂载状态的 `cache_error` 里。
- **卸载即回收**：卸载时先断会话再注销该库在门户上的目标并回收其 L2 文件（顺序不能反，
  否则仍在飞的命令会失败）。

## 磁盘内容下载（把母盘拷贝到本地）

把服务端的**母盘（parent）VHDX** 整盘拷贝到客户端本地目录。仅母盘可下载；
差异盘/独立盘会被服务端拒绝（`disk.not_parent`）。下载在代理后台异步执行，接口立即返回。

```
POST /agent/disks/download
  ↓ {disk_id, target_dir?, file_name?, server_key?, server_url?}
  ↑ 202 {download:DownloadState}
  # target_dir 省略时用 config.default_download_dir；必须是绝对路径。
  # file_name 省略时由服务端 Content-Disposition 推导，推导失败回退 disk-<id>.vhdx；
  #           文件名必须是单个纯文件名（拒绝路径穿越、绝对路径与 \ / : 等分隔符）。
  # server_key / server_url 指明从**哪台**服务端下载；都省略时按主服务端兜底（老客户端）。
  # 磁盘 ID 只对"它所属的那台服务端"有意义：给了标识却匹配不上 → 404 agent.server_unknown，
  # 绝不改从别的台下载。
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
  ↓ {local_dir, repo_name, repo_mode?, storage_id?, quota_bytes?, source_mode?,
     server_key?, server_url?}
  ↑ 202 {upload:UploadState}
  # local_dir 必须是非空绝对路径且存在、是目录。
  # repo_mode："shared"（默认）| "exclusive"。
  # storage_id 省略时由服务端按可用空间自动选根；给定时校验该存储存在且启用。
  # quota_bytes 省略/0 表示不限。
  # source_mode："copy"（默认）| "move"；move 仅在建库成功后删除本地源目录。
  # server_key / server_url 指明把库建到**哪台**服务端；都省略时按主服务端兜底（老客户端）。
  # 存储 ID 只对"它所属的那台服务端"有意义：给了标识却匹配不上 → 404 agent.server_unknown。
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
所有调用都走**本次上传所属的那台服务端**（见请求体的 `server_key` / `server_url`），
并沿用该台的连接指纹固定。

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
     download_connections, language, start_at_login:bool, update_channel, server_alias,
     cache_l1_bytes:int, cache_l2_bytes:int, cache_l2_dir:string,
     repo_mounts:{<repo_id>:{mount_mode, mount_dir, auto_mount:bool, cache_enabled:bool}}}
PATCH /agent/config
  ↓ 上述字段的任意子集（不含 repo_mounts / default_mount_mode / default_mount_dir）
  ↑ {config:{...}}
POST  /agent/repo-mounts/{repo_id}
  ↓ {mount_mode, mount_dir, auto_mount:bool, cache_enabled:bool}
  ↑ {config:{...}}
```

`repo_mounts` 是**每个存储库各自独立**的挂载偏好（形态 / 目录 / 启动后自动挂载），
没有条目的库跟随 `default_mount_mode`、`default_mount_dir` 与 `auto_mount` 的兜底默认值；
界面上的默认值是"填进去等用户确认"，不是隐式继承。

- 写入必须用 `POST /agent/repo-mounts/{repo_id}` 单库更新：`PATCH /agent/config` 不接受
  `repo_mounts`（整表替换会让"两个窗口各改一个库"变成后写覆盖前写）。
- `default_mount_mode` / `default_mount_dir` 是**只读**的兜底默认值（`letter` / `C:\Vault`）：
  界面上没有这两项，`PATCH /agent/config` 也不接受 —— 挂载形态与目录一律按库配置
  （`repo_mounts`），放一份"全局默认"只会让用户以为改一处就能管所有库。
- 用 POST 而非 PUT：界面不直连代理，请求由 Electron 主进程代发，主进程**按方法放行**
  （只接受 GET/POST/PATCH/DELETE），PUT 会被挡在代理之外并向上报成 `network.error`。
- `mount_mode` 为空串表示跟随 `default_mount_mode`；非法值返回
  `system.invalid_param`（args.field=mount_mode），空 `repo_id` 返回 args.field=repo_id。
- `cache_enabled` 是"是否为该库启用本地 iSCSI 读缓存代理"（默认 **false**，必须由用户在
  「存储库 → 挂载设置」手动开启），与 `mount_mode` / `mount_dir` / `auto_mount` 同属**整条
  偏好**：`POST /agent/repo-mounts/{repo_id}` 是整条替换，只发其中几项会把没发的项重置为
  默认值。改动**需重新挂载才生效**（挂载链路在挂载时确定），界面据此提示用户。
- `server_alias` 是**全局**别名，只在多服务端改造前/单服务端场景下作为兜底生效。
  多服务端下别名**按台**下发（`POST /agent/session` 的 `alias`，落在 `/agent/state` 里该台的
  `alias` 字段）：全局那一个名字已不足以标识"是哪一台"，继续沿用会让两台服务端的不同库
  挤进同一个命名空间（同名目录）。
- `mount_dir` 是目录模式下的**父目录**（绝对路径，或相对 `default_mount_dir` 的相对路径）：
  真正的挂载点是它下面一层 `<服务端别名>_<存储库名称>` 子目录，界面上把算好的最终路径
  如实显示给用户。`<服务端别名>` 的取用顺序：**该台下发的 `alias`** → 全局 `server_alias`
  （**仅当本机至多登记了一台服务端时**）→ 服务端名称。服务端下发的 `mount_path`：
  **绝对路径**（管理员的显式指定）原样使用；**相对路径**（服务端自动拼的
  `<服务端名称>\<库名>`）也归一到同一命名，同一个库不会因为"从哪挂"得到两种目录名。
  父目录末段已经就是那一层时不再追加（重挂/恢复会把上次的最终挂载点当请求传回来，
  否则每重挂一次就多套一层目录）。
- 语义与全局 `auto_mount` 的区别：全局开关只管"恢复本机上次留下的挂载记录"；
  某库 `auto_mount=true` 时即使本机没有记录（甚至还没有分配）也会在**该台服务端确认连上**后
  自动挂载（没有可用分配时由代理调用 `POST /v1/repos/{id}/allocations` 建一个），
  `auto_mount=false` 时不恢复该库的记录。
- **自动挂载必须等服务端连上，且按台各自进行**：拿到会话（`POST /agent/session`）只表示令牌
  已交给代理，不代表服务端可达。因此代理会**逐台**等该台 `Connected=true`（SystemInfo 成功 /
  事件流建立 / 心跳成功三者之一）后**才开始**挂载该台的库，每台最多等 60s；等不到就跳过该台
  （该台下一次会话建立或连接确认会重来），避免"没连上就挂"留下一串失败记录
  （启动时的自动挂载失败按设计会保留记录）。
  等待期间**该台**的会话被换掉（重新登录 / 退出登录）同样跳过；另一台推来会话**不算**
  更换，不影响本台的等待与挂载。
- **用户手动卸载压过自动挂载（本次运行内）**：`POST /agent/unmount` 成功后，该库进代理内存里
  的"本次运行手动卸载过"名单，之后不再被自动挂载 —— 否则卸载删掉记录后，下一次会话连上
  （客户端推会话 / 证书免密登录 / 令牌定时续期都会触发）会把它当成"从没挂过的自动挂载库"
  立刻挂回来。名单只活在代理进程内存里，重启即清空（客户端退出会结束代理），
  即"本次运行不再自动挂载，直到下次启动"；它只挡自动挂载，用户手动点"挂载"照常可用。

`default_download_dir` 是「母盘拷贝到本地」的默认目标目录，默认 `C:\Vault\Downloads`。
取值必须是非空绝对路径；非法值返回 `system.invalid_param`（args.field=default_download_dir）。
该目录不要求预先存在，下载时会按需创建。

`download_connections` 是「母盘拷贝到本地」的并行分段数，取值 1..8，默认 4。
取值超出范围时 PATCH 返回 `system.invalid_param`（args.field=download_connections）；
从配置文件读取到越界值时按「<=0 取默认 4、>8 取 8」归一化。

`cache_l1_bytes` / `cache_l2_bytes` 是本机 iSCSI 读缓存代理的 **L1（内存）/ L2（本地磁盘
文件）总量预算**（字节），前者默认 512MiB、后者默认 4GiB。语义是**客户端总量**：设面只按
客户端设置，一个客户端只起一个代理，多个已启用缓存的库**均分**这份总量（见
`GET /agent/cache` 的 `l1_quota_bytes` / `l2_quota_bytes`）。配额在**挂载建立时定下，运行中
不重算**，因此：

- **取值范围**：L1 必须落在 `[64MiB, 64GiB]`；L2 允许 **0（= 关闭 L2，只做内存缓存）**，
  非 0 时必须落在 `[256MiB, 1TiB]`。越界时 PATCH 返回 `system.invalid_param`
  （args.field=`cache_l1_bytes` / `cache_l2_bytes`），界面在提交前就按同一口径拦下。
- **0 与"没配"必须区分**：L2 的 0 是合法取值（关闭），配置里显式写过 0 之后不再被归一回
  默认值（见 `cacheL2Set`）；只有从未配过 L2 时才取默认 4GiB。
- 改这项**不影响已有挂载**的既有配额（新配额在下一次挂载时生效）。

`cache_l2_dir` 是 L2 缓存文件的**存放目录**（客户端设置里可改，配合上下限一起调节磁盘占用）。
空串表示默认位置 `<DataDir>\iscsi-cache`；非空必须是**绝对路径**，否则 PATCH 返回
`system.invalid_param`（args.field=`cache_l2_dir`）。目录不存在时按需创建（`os.MkdirAll`）。
改动在**下一次挂载**时生效：L2 文件路径在创建缓存目标时定下，已在挂载中的库仍用旧目录
（换目录不会搬移既有缓存文件，旧目录里的 `.bin` 需用户自行清理）。

> 客户端资源热更（client_web）**没有独立的本地配置项**：它使用与自更新相同的更新通道
> （`update_channel`）与更新源（**主服务端**，见「认证与会话」的 primary）；激活状态以磁盘上的
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
                       # 挂载/卸载/阶段推进时推；**会话实测结论变化时也推**（见 MountState
                       # 的 session_active）—— 会话断开/恢复时 state 仍是 mounted，界面靠这条
                       # 事件把"卸载"按钮翻回"挂载"、标签翻成"已断开"，不必用户手动刷新。
                       # 因此界面不得把"收到 state=mounted"当成"刚挂上"：只有**亲眼看到它从
                       # 非 mounted 变成 mounted** 才提示"已挂载"（会话实测不会产生这种变化）。
     event: unmount     data: {allocation_id}
     event: revoked     data: {allocation_id, reason, server_key}   # 服务端踢下线，代理已自行卸载
     event: server      data: {server_key, url, name, connected, phase, fail_count, last_error}
                      # **按台发**：connected / phase / fail_count / last_error 任一变化时发（值未变不发）。
                      # server_key 指这条状态属于哪一台，界面必须据此只更新那一台。
                      # phase 必须一起发：只推 connected 的话，"没连上、但一直在重连"的代理在界面上
                      # 永远停在"未连接"，用户分不清"还在连"和"连不上"。
    event: host        data: HostState                 # 本机就绪状态（iSCSI 发起端）变化时才发：
                                                       # 启动探测完成、管理员启动 MSiSCSI 后自动转就绪
     event: session     data: SessionView + {server_key} # 某台客户端证书会话自动续期成功（新令牌）
     event: update      data: {available_version, downloading, received_bytes, total_bytes}
                                                         # 自更新下载进度（更新源 = 主服务端）
     event: web_update  data: WebUpdateState             # 渲染层资源热更状态/进度
     event: heartbeat   data: {at, allocation_id, server_key}  # 可用于界面上展示"在线"
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
- **启动自愈**：应用升级后，版本不高于应用版本的热更层永远不会再被加载。代理启动时后台执行
  一次 `reconcileWebLayer`：摘掉失效的 `current.json`、删除失效版本目录、清理 `.tmp-*` 与
  `.part` 残留（判据与激活拦截同一套：应用版本不可解析时不判定、不动手）。
  用户**无需手工删除** `%ProgramData%\Vault\webapp`。
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
| `agent.no_session` | 尚未推送服务端会话（409；按台判断：该台没有会话时也算） |
| `agent.server_unknown` | 请求里指定的服务端（`server_key` / `server_url`）在本机匹配不上任何已登记服务端（404）。**多服务端下的防串台护栏**：绝不"猜"一台替你执行，尤其是磁盘/存储 ID 这类只对某台有意义的标识 |
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
