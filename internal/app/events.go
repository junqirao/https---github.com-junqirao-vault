package app

import (
	"strings"
	"time"
)

// EventSink 是事件发布的最小接口。
//
// 为什么不是直接依赖 api.EventHub：`internal/api` 依赖 `internal/app`，
// app 若反向 import api 会形成循环依赖。因此这里只声明一个极小的接口，
// 由 `main.go` 用适配器把 `api.EventHub` 接进 `Deps.Events`。
//
// ⚠️ 语义说明：事件通道是**进程内**的（与 api.EventHub 一致），
// 仅在单实例部署下足够；多实例部署时事件不会跨进程广播，
// 权威状态始终以数据库与显式查询接口为准（见 docs/implementation.md 5.7）。
type EventSink interface {
	// Publish 发布一条事件。topic 形如 job / lease / repo；payload 可序列化为 JSON。
	Publish(topic string, payload any)
}

// emit 尽力而为地发布一条事件；未装配事件通道时静默忽略。
func (d Deps) emit(topic string, payload any) {
	if d.Events != nil {
		d.Events.Publish(topic, payload)
	}
}

// MountEventTopic 是挂载**阶段事件**的 topic（即 SSE 的 event: 名）。
//
// 为什么用独立 topic 而不复用 lease：语义不同 —— lease 事件表示"租约状态变化"
// （挂载成功后一次），mount 事件表示"挂载过程中的阶段推进"。代理按 topic 分流
// （见 internal/agent/sse.go 的 handleServerEvent），复用会让两件事混在一起。
const MountEventTopic = "mount"

// 挂载阶段取值（服务端侧部分）。
//
// 必须与 internal/agent 的 MountPhase* 以及 docs/agent-api.md「挂载阶段」保持一致 ——
// 这三个字符串是跨进程的线上契约。
const (
	// MountPhaseAllocating 服务端正在分配资源。
	MountPhaseAllocating = "allocating"
	// MountPhasePreparingDisk 服务端正在准备虚拟磁盘（等差异盘/VHDX 就绪）。
	MountPhasePreparingDisk = "preparing_disk"
	// MountPhaseConfiguringTarget 服务端正在创建/下发 iSCSI 目标。
	MountPhaseConfiguringTarget = "configuring_target"
)

// emitMountPhase 尽力而为地发布一条挂载阶段事件（topic=mount，event: mount）。
//
// 为什么需要：挂载在服务端是**同步重活** —— 等差异盘就绪（最多 15s）再用 PowerShell
// 全量下发 iSCSI 目标（实测约 15s），全部发生在客户端拿到 HTTP 响应之前。没有这条通道，
// 界面在这几十秒里只有一个转圈的按钮，用户无从知道卡在哪一步
// （真实反馈：希望看到"正在创建虚拟磁盘 / 正在创建 iSCSI 目标"）。
//
// 语义：**尽力而为**的通知，不保证送达（无订阅者、缓冲满都会丢）。权威结果始终以
// mount 接口的响应与磁盘状态为准；阶段事件只用于界面反馈。
func (d Deps) emitMountPhase(allocationID, phase string) {
	allocationID = strings.TrimSpace(allocationID)
	phase = strings.TrimSpace(phase)
	if allocationID == "" || phase == "" {
		return
	}
	d.emit(MountEventTopic, map[string]any{
		"action":        "progress",
		"allocation_id": allocationID,
		"phase":         phase,
		"at":            time.Now().UnixMilli(),
	})
}
