package agent

import "time"

// 进度推送节流参数（download / upload / update / web_update 共用一套，避免各处参数漂移）。
//
// 规则是"有时间闸门"的：
//   - 最快每 progressMinInterval 推一次（硬下限，避免高速传输每秒上千次刷屏）；
//   - 距上次推送达到 progressMaxInterval 时必推（慢速传输也能有心跳式更新）；
//   - 处于两者之间时，只有累计新增字节达到 progressByteThreshold 才推。
const (
	progressMinInterval   = 200 * time.Millisecond
	progressMaxInterval   = 500 * time.Millisecond
	progressByteThreshold = 1 << 20
)

// progressThrottle 实现上述"有时间闸门"的进度推送节流。
//
// 调用方需自行加锁（本类型不做并发保护），因为各进度聚合器本就有自己的互斥量。
type progressThrottle struct {
	lastAt  time.Time
	lastVal int64
}

// newProgressThrottle 以给定初始累计值构造节流器（lastAt 取当前时刻）。
func newProgressThrottle(initial int64) progressThrottle {
	return progressThrottle{lastAt: time.Now(), lastVal: initial}
}

// allow 判断此刻是否应推送：force 时无条件推送；否则按"最小间隔 / 最大间隔 / 字节阈值"判定。
// 返回 true 时会同时刷新基线（lastAt / lastVal）。
func (t *progressThrottle) allow(force bool, val int64, now time.Time) bool {
	if !force {
		elapsed := now.Sub(t.lastAt)
		if elapsed < progressMinInterval {
			return false
		}
		if elapsed < progressMaxInterval && val-t.lastVal < progressByteThreshold {
			return false
		}
	}
	t.lastAt = now
	t.lastVal = val
	return true
}
