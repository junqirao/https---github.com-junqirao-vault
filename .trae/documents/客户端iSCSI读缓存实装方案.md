# 客户端 iSCSI 读缓存实装方案

## Context

`internal/iscsicache` 已实现并跑通真实 target 的读缓存代理（L1 内存 + L2 文件、writearound、扫描旁路、命中率已在 QD 1/4/8 下验证：热读 25.9k → 103.8k IOPS，100% L1 命中、热读阶段 0 次回后端）。但它目前是独立模块 + 独立可执行 `cmd/vault-iscsi-cache`，只支持**单个**后端目标。

现在要把它实装进 Windows 客户端（Vault-Agent），让存储库挂载走 `服务端下发 → 本地代理 → 本地发起端连接本地代理`。

已确认的四个决策：

| 决策 | 结论 |
| --- | --- |
| 代理形态 | **内嵌进 Vault-Agent 进程**：一个客户端一个代理（一个门户监听），直接复用 Agent 的配置、日志、管理员权限与生命周期 |
| L1/L2 空间语义 | **客户端总量**，在已启用缓存的库之间均分 |
| 代理不可用/连不上后端 | **自动回退直连服务端目标**，挂载照常成功，卡片提示"缓存未生效" |
| 界面范围 | **仅桌面客户端**（`frontend/packages/ui` + `frontend/apps/client`），Web 控制台不改 |

## 目标链路

```
存储库(服务端) ──下发 MountSpec──▶ Agent
                                    │ 若该库开启缓存
                                    ▼
                          [in-process 代理门户 127.0.0.1:3261]
                          frontend 按 TargetName 路由到对应 LUN
                                    │
                          backend.Initiator ──iSCSI──▶ 服务端 target
                                    ▲
   Windows MS 发起端 ──iSCSI──────┘（连接本地门户 + 本地 IQN，无 CHAP）
```

## 改动清单

### A. `internal/iscsicache/frontend` — 单门户承载多目标

现状：`Target` 持有一个 `Handler` 与一个 `Config.TargetIQN`，登录时校验 `TargetName == cfg.TargetIQN`。

- [target.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/frontend/target.go)：`Config.TargetIQN` 变为可选；`Target` 增加 `handlers map[string]Handler`（配 `sync.RWMutex`）与 `Register(iqn, h)` / `Unregister(iqn)`；`Close` 仍只关整台门户。保留 `New(cfg, handler)` 单目标构造，既有测试与 `cmd/vault-iscsi-cache` 不受影响。
- [session.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/frontend/session.go)：
  - 会话增加 `h Handler` 与 `iqn string`；`applySessionKeys`（约 L233-248）在 Normal 会话里用 `TargetName` 查注册表，未注册返回 `class=0x02 detail=0x03`（target not found）。
  - `operationalResponse`（约 L252-278）回会话自己的 `TargetName`。
  - `handleText`（L615-632）的 `SendTargets=all` 列出**全部**已注册 IQN；指定名字则只回该名字（未注册回空）。
  - 命令执行 / INQUIRY 改走 `s.h`（现在是 `s.t.h`）。

### B. `internal/iscsicache/cache` — 上报用量

`Snapshot` 增加 `L1UsedBytes` / `L1LimitBytes`；新增 `Cache.Usage() (used, limit int64)`（各 shard `len(blocks)` 求和 × blockSize，逐 shard 加锁）。L2 已有 `l2.File.Slots()` / `UsedSlots()`，无需改。

### C. `internal/iscsicache` — 多目标门户（保持模块自足，不引用 Agent 代码）

新增 `internal/iscsicache/portal.go`：

```go
type PortalConfig struct { ListenAddr string; ...; Logger *slog.Logger }
type TargetConfig struct {
    Key       string          // 调用方的稳定标识（Agent 传 allocation ID）
    TargetIQN string
    Backend   BackendConfig
    Cache     CacheConfig
    Vendor, Product, Revision string
}
func NewPortal(cfg PortalConfig) (*Portal, error)
func (p *Portal) Addr() net.Addr
func (p *Portal) Add(ctx context.Context, tc TargetConfig) error  // 幂等：同 Key 已存在直接返回
func (p *Portal) Remove(key string) error                        // 幂等
func (p *Portal) Stats(key string) (Stats, bool)
func (p *Portal) Close() error
```

把 `Service.build`（[iscsicache.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/iscsicache.go#L240-L324) 的 backend→L2→cache→proxy 组装）抽成共用 helper，`Service`（单目标，供 `cmd/vault-iscsi-cache`）与 `Portal`（多目标，供 Agent）都调用它，避免两份组装逻辑。

### D. `internal/agent` — 挂载链路与代理管理器

- **新增 `internal/agent/cacheproxy.go`（`cacheManager`）**
  - 构造于 `Agent.New`（不做 I/O）；`Start(ctx)` 保留给首次 `Add` 时懒建门户；`EnsureTarget` / `ReleaseTarget(allocationID)` / `Close` / `Stats`，全部幂等。
  - 本地 IQN：`iqn.2024-01.local.vault:vcache-<allocationID 规范化>`。**必须**校验其短名与服务端 IQN 短名（`vault-<repo>-<alloc>`）不互相包含 —— `initiator_is_connected.ps1` / `wait_connected` / `disconnect` 是**遍历会话按短名包含**匹配的，包含关系会让两端串台。
  - 门户地址 `127.0.0.1:3261`（常量，避开标准 3260）。
  - 配额：`每目标额度 = 客户端总量 / 已启用缓存的库数`，加入时再按"剩余额度"截断；不足最小值（`blockSize × shards`）则**跳过缓存**并回退直连 + 告警。已运行目标的额度在其创建时确定，运行中不重算；移除后释放的额度在下一个目标创建时生效（写入文档）。
  - L2：`%ProgramData%\Vault\iscsi-cache\l2-<allocationID>.bin`，`L2Bytes=0` 时不启用 L2。
  - 显式覆盖 `Vendor/Product/Revision`（如 `VAULT` / `Vault Cache Disk`），避免代理盘与直连盘在 Windows 侧被视为同一设备标识。
  - `EnsureTarget` 前先断开该本地 IQN 的残留会话（重启后旧会话可能仍在"重连中"，`initiator_connect.ps1` 的 already_connected 判据会误判）。
- [config.go](file:///c:/Users/89412/Projects/golang/vault/internal/agent/config.go)：`Config` 加 `CacheL1Bytes`（默认 512 MiB）、`CacheL2Bytes`（默认 4 GiB，0=关闭）；`RepoMountPref`（L33）加 `CacheEnabled bool`（默认 false，用户手动开）；`ConfigPatch` 加 `cache_l1_bytes` / `cache_l2_bytes`；`normalizeConfig` 补默认与范围校验。
- [mount.go](file:///c:/Users/89412/Projects/golang/vault/internal/agent/mount.go#L271-L302)：Connect 之前按 `repoPref(repoID).CacheEnabled` 决定生效门户与生效 IQN；失败则告警 + 回退直连。`EnsurePortal / Connect / WaitConnected / Disconnect / Unregister` 全部改用生效值。切换前保证"单会话"：缓存路径先断开直连 IQN，回退路径先断开本地 IQN（否则会出现两块同尺寸 iSCSI 盘，`volume_find_iscsi_disk.ps1` 取最小编号，可能静默挂到直连盘）。`failConnect` 诊断按生效门户输出。
- [state.go](file:///c:/Users/89412/Projects/golang/vault/internal/agent/state.go#L200-L249)：`MountState.TargetIQN` 记**生效** IQN；新增 `CacheEnabled` / `CacheActive` / `CacheError`。`mountRuntime` 继续持有服务端 CHAP（专供代理后端 `backend.Dial`），本地会话用 `none`，密钥不落盘。
- 卸载：`unmountLocked` 在 Disconnect/Unregister 之后、回写 release 之前调 `ReleaseTarget(allocationID)`。
- [agent.go](file:///c:/Users/89412/Projects/golang/vault/internal/agent/agent.go#L76-L149)：`Agent` 增加 `cache`；`Start` 用 `baseCtx` 启动；**`Stop` 必须 `cache.Close()`** —— 信号退出与自更新退出都不走 `UnmountAll`（`cmd/vault-agent/main.go` 只有 `POST /agent/shutdown` 会卸载），否则 L2 不 flush、后端不 logout。
- [httpapi.go](file:///c:/Users/89412/Projects/golang/vault/internal/agent/httpapi.go#L24-L85)：新增 `GET /agent/cache`（门户地址 + 每目标 `key/repo_id/allocation_id` + L1/L2 用量与上限 + reads/L1 命中/L2 命中/后端读/命中率 + 汇总）。`/agent/state` 的 mounts 已随新增字段带出缓存状态。
- `cmd/vault-iscsi-cache` 保持不变（可选改成基于 `Portal`，非本次必需）。

### E. 桌面客户端界面

- [RepoMountSettings.tsx](file:///c:/Users/89412/Projects/golang/vault/frontend/packages/ui/src/features/repo/RepoMountSettings.tsx)：新增"启用缓存"开关（默认关），保存扩展 `cache_enabled`；提示"需重新挂载生效"。
- [RepoCards.tsx](file:///c:/Users/89412/Projects/golang/vault/frontend/packages/ui/src/features/repo/RepoCards.tsx)：开启缓存的库显示 L1/L2 已用/上限与命中率（数据来自 `GET /agent/cache`）；`cache_active=false` 显示"缓存未生效"。
- [ClientSettings.tsx](file:///c:/Users/89412/Projects/golang/vault/frontend/packages/ui/src/features/settings/ClientSettings.tsx)：新增 L1/L2 上限输入（MiB/GiB），PATCH `/agent/config`。
- [agentClient.ts](file:///c:/Users/89412/Projects/golang/vault/frontend/packages/ui/src/api/agentClient.ts)：新增 `getCache()`，扩展 `setRepoMountPref`。
- i18n 文案补齐。Electron 主进程白名单已放行 `GET /agent/*`，无需改。

### F. 文档

- `docs/agent-api.md`：新接口 `GET /agent/cache`、新配置项、`repo_mounts` 的 `cache_enabled`。
- `docs/iscsi-cache/iscsi_cache_proxy_windows_mvp.md`：补多目标门户与客户端集成；该文档现在"单会话 / 单命令"的前提已与代码不符（队列深度已放开），一并更正。

## 关键规则

1. **命名**：本地 IQN 短名与服务端 IQN 短名不得互为子串（会话匹配靠短名包含）。
2. **单会话**：任一存储库在任一时刻只存在一条 iSCSI 会话（要么直连、要么走代理）。
3. **配额**：客户端总量均分，创建时确定、运行中不重算、不足则放弃缓存。
4. **回退**：代理不可用时直连可用，且状态里明确可见"缓存未生效"。
5. **幂等**：`EnsureTarget` / `ReleaseTarget` / `Close` 必须可重复调用（重启恢复、失败清理、退出路径都会碰）。

## 验证方式

1. 静态与单测：`gofmt -l internal/iscsicache internal/agent`、`go vet ./...`、`go test ./internal/iscsicache/... ./internal/agent/...`。
   - 新增：frontend 多 IQN 注册 / 未注册拒绝 / `SendTargets=all` 列出全部；`cache.Usage`；`cacheManager` 配额分配与幂等 Release。
2. 端到端 loopback：一个门户 + 两个 in-memory 后端、两个 IQN，各自读写并确认各自 L1 命中（扩展 [loopback_test.go](file:///c:/Users/89412/Projects/golang/vault/internal/iscsicache/frontend/loopback_test.go)）。
3. 真机（Windows + 真实 target `172.18.28.200`）：开启缓存 → 挂载 → `diskmgmt.msc` 看到盘并能初始化/格式化为 NTFS → 读写正常 → 存储库卡片显示用量与命中率 → 卸载后代理 target 释放。
4. 回退：故意占用 3261 端口或断开后端 → 挂载仍成功且卡片提示"缓存未生效"。
5. 退出：接收停止信号退出再启动，确认无 L2 未 flush / 后端未 logout 的残留，且能正常重挂。
6. 前端：`pnpm -r build` 通过，并在客户端界面走一遍开关与展示。
7. 注意：本机无 C 编译器，`go test -race` 不可用，并发正确性靠单测 + 真机联调。

## 风险

- MS 发起端对**环回门户 + 自定义端口**的真机登录行为需实测确认（相关 PS 脚本多处标注"需实测"），若不被接受则改回 3260。
- 代理与直连盘同尺寸同时在位时，磁盘定位脚本只按尺寸取最小编号，故"单会话"保证是硬前提。
- 配额不重算意味着：先开 1 个库再开 3 个库时，第一个库的额度不会自动缩到 1/4，直到它重新挂载。