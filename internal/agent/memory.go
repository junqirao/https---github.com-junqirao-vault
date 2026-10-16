package agent

import (
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
)

// memLimitHeadroom 是软内存上限在 L1 预算之外留的余量。
//
// 它要盖住 L1 之外那部分常驻堆：Go 运行时自身、若干 sync.Pool（读缓冲、上传缓冲，
// 每个 P 各留一份）、L2 的 bounce buffer，以及一轮 GC 需要的周转空间。实测
// （L1=512MiB、256 分片）扣掉缓存的常驻堆约 150~350MiB，512MiB 有三倍以上余量，
// 不会把 GC 压到抖动。L1 是所有库共享的**总**预算（见 splitCacheBudget），所以
// 这个上限不会随库数增长而失真。
const memLimitHeadroom = 512 << 20

// applyMemoryLimit 给 Go 运行时设软内存上限（GOMEMLIMIT），口径是「L1 总预算 + 余量」。
//
// 为什么需要：进程的存活堆实测是 L1 账本的 1.3~1.8 倍（块数据之外还有 entry 头、
// map 桶、队列槽位、以及各 sync.Pool 的高水位），而默认 GOGC=100 会把 GC 的堆目标
// 定在**存活堆的两倍**上。于是"缓存上限设 512MiB"最终对应约 2GB 的 RSS，并且因为
// 目标是跟着存活堆浮动的，看起来会一直涨。SetMemoryLimit 是**软**上限：只在逼近时
// 才加紧 GC，平时不改变任何行为，所以它能让 RSS 封顶而不牺牲吞吐。
//
// GOMEMLIMIT 环境变量优先：运维显式设过就完全不动，保持原有能力。
func applyMemoryLimit(cfg Config, log *slog.Logger) {
	if v, ok := os.LookupEnv("GOMEMLIMIT"); ok {
		log.Info("agent: 已由 GOMEMLIMIT 指定内存上限，跳过自动设置", "GOMEMLIMIT", v)
		return
	}
	limit := cfg.CacheL1Bytes + memLimitHeadroom
	if limit < memLimitHeadroom {
		limit = memLimitHeadroom
	}
	if debug.SetMemoryLimit(limit) == limit {
		return // 值没变（例如 PATCH 里改的是别的字段），不必重复记日志
	}
	log.Info("agent: 设置 Go 软内存上限",
		"limit_bytes", limit, "l1_bytes", cfg.CacheL1Bytes, "headroom_bytes", int64(memLimitHeadroom))
}

// MemStats 是 GET /agent/memstats 的响应。
//
// 字段名与 runtime.MemStats 对齐，另外附上缓存自己的账本，便于一眼对账：
// l1_used_bytes_total 是块数据的账本，l1_resident_bytes_total 是这份索引实际占的堆，
// 两者的差就是账外部分（entry 头、map 桶、队列槽位、池的高水位）。
type MemStats struct {
	// HeapAlloc 是当前存活堆字节数（GC 后仍然可达的对象）。
	HeapAlloc uint64 `json:"heap_alloc"`
	// HeapInuse 是已交给堆用的 span 字节数，HeapSys 是向 OS 要到的总量。
	HeapInuse uint64 `json:"heap_inuse"`
	HeapSys   uint64 `json:"heap_sys"`
	// HeapIdle/HeapReleased 是闲置与已归还 OS 的部分。HeapSys 减 HeapAlloc 是
	// 清扫滞后与闲置碎片的上界，RSS 降不下去通常卡在这里。
	HeapIdle     uint64 `json:"heap_idle"`
	HeapReleased uint64 `json:"heap_released"`
	// NextGC 是下一次 GC 的目标堆大小。RSS"看起来一直在涨"的直接原因就是它：
	// 默认 GOGC=100 时它等于存活堆的两倍，而存活堆随缓存占用变化。
	NextGC uint64 `json:"next_gc"`
	// GCCPUFraction 是进程启动以来 GC 占用 CPU 的比例：判断"毛刺是不是 GC 的锅"看它。
	GCCPUFraction float64 `json:"gc_cpu_fraction"`
	NumGC         uint32  `json:"num_gc"`
	NumGoroutine  int     `json:"num_goroutine"`
	GOMAXPROCS    int     `json:"gomaxprocs"`
	// GOMEMLimitBytes 是当前生效的软内存上限（见 applyMemoryLimit）。
	GOMEMLimitBytes int64 `json:"gomemlimit_bytes"`
	// GOGCEnv 是 GOGC 环境变量的原始值，空串表示用默认的 100（运行时没有只读的
	// 读取接口，SetGCPercent(-1) 会真的关掉 GC，所以这里只回显环境变量）。
	GOGCEnv string `json:"gogc_env"`
	// L1UsedBytesTotal/L1ResidentBytesTotal 见类型注释。
	L1UsedBytesTotal     int64 `json:"l1_used_bytes_total"`
	L1ResidentBytesTotal int64 `json:"l1_resident_bytes_total"`
}

// handleMemStats 返回进程的内存与 GC 运行时读数。
//
// 存在的理由：界面上"缓存用量"是块数据的账本，没法解释"设了 512MiB 却占 2GB 而且
// 看着在涨"。这里把账本和运行时的真实数字放在同一个响应里，两者的差就是账外部分，
// NextGC 减 HeapAlloc 就是"还能涨多少"。
//
// ReadMemStats 会短暂停顿所有 goroutine，所以这是一个诊断端点，不要轮询。
func (a *Agent) handleMemStats(w http.ResponseWriter, _ *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	out := MemStats{
		HeapAlloc:       ms.HeapAlloc,
		HeapInuse:       ms.HeapInuse,
		HeapSys:         ms.HeapSys,
		HeapIdle:        ms.HeapIdle,
		HeapReleased:    ms.HeapReleased,
		NextGC:          ms.NextGC,
		GCCPUFraction:   ms.GCCPUFraction,
		NumGC:           ms.NumGC,
		NumGoroutine:    runtime.NumGoroutine(),
		GOMAXPROCS:      runtime.GOMAXPROCS(0),
		GOMEMLimitBytes: debug.SetMemoryLimit(-1), // 负值只读取，不修改
		GOGCEnv:         os.Getenv("GOGC"),
	}
	if a.cache != nil {
		for _, t := range a.cache.Status().Targets {
			out.L1UsedBytesTotal += t.L1UsedBytes
			out.L1ResidentBytesTotal += t.L1ResidentBytes
		}
	}
	a.writeJSON(w, http.StatusOK, out)
}
