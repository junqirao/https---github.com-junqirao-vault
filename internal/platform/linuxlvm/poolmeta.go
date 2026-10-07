package linuxlvm

import (
	"math"
	"strconv"
	"strings"
)

// 本文件刻意**不带 //go:build linux**：里面全是容量算术，不发任何 LVM 命令，
// 因此可以在非 Linux 开发机上直接跑单测。
// "自动填好的最大容量能不能真建成功"全靠这点算术，不该只有真机才能验证
// （真机反馈：两块 8G 盘自动填了 14 GB，提交却报可用 11.6 GB——就是这里算错了）。

// poolMaxBytesWith 按指定元数据大小算"这些空间够建多大的池"（<=0 表示建不出来）。
//
// 抽成独立函数，是为了让"卷组还不存在时的估算"（EstimatePoolMaxBytes）与真正建池前的
// 校验走同一段代码：前端自动填进去的容量必须恰好是后端会放行的上限，
// 否则就会变成"自动填好 14 GB、提交却报可用 11.6 GB"这种自相矛盾（真机反馈）。
//
// metaSize 留空表示元数据随池容量自适应，此时"预留取决于池容量、池容量又取决于预留"，
// 是个不动点；reserve(c) 单调不减，从"整块空间都给数据"出发迭代几次即收敛。
func poolMaxBytesWith(freeBytes int64, metaSize string) int64 {
	if freeBytes <= 0 {
		return 0
	}
	if parseLVSize(metaSize) > 0 {
		// 显式指定了元数据大小：预留与池容量无关，一步算出。
		usable := freeBytes - poolMetaReserveBytes(metaSize, freeBytes)
		if usable <= 0 {
			return 0
		}
		return usable
	}
	usable := freeBytes
	for i := 0; i < 4; i++ {
		next := freeBytes - poolMetaReserveBytes("", usable)
		if next <= 0 {
			return 0
		}
		if next == usable {
			break
		}
		usable = next
	}
	return usable
}

// 池元数据大小（自适应口径）的区间与比例。
const (
	// minPoolMetaBytes 元数据下限。按 LVM 的经验比例（元数据 ≈ 数据的 1/1000），
	// 64 MiB 足以承载约 64 GiB 的数据映射，对单机小盘绰绰有余。
	minPoolMetaBytes = int64(64) << 20
	// maxPoolMetaBytes 元数据上限 4 GiB：够 4 TB 级别的池用（也正是老配置的固定默认值）。
	maxPoolMetaBytes = int64(4) << 30
	// poolMetaDivisor 元数据 ≈ 池数据容量 / 500（0.2%），比 LVM 自身的 1/1000 略宽。
	poolMetaDivisor = 500
	// poolAlignSlackBytes 给 extent 对齐留的余量：VG 的 PE 默认 4 MiB，
	// tdata / tmeta / pmspare 三个 LV 各自向上取整；元数据小的时候，
	// 这点开销会超过 10% 的比例余量，所以单列一个固定值垫住。
	poolAlignSlackBytes = int64(32) << 20
)

// poolMetaSizeFor 按池数据容量选元数据大小（字节）。
//
// 为什么不让它固定成一个值：元数据要从**同一个卷组**另划，而且 pmspare 还要再占一份，
// 固定 4G 在小盘上就是灾难——真机上两块 8G 盘（16G 卷组）想建 14G 的池，
// 光元数据就要 4G(tmeta) + 4G(pmspare)，LVM 直接 insufficient free space（真机反馈）；
// 反过来，给 4 TB 的池配 64 MB 元数据又确实不够。按容量比例来则两头都对：
// 16G 盘 → 64M（可建约 15.8G 的池），4T 盘 → 4G（与老默认值一致）。
func poolMetaSizeFor(dataBytes int64) int64 {
	if dataBytes <= 0 {
		return minPoolMetaBytes
	}
	meta := dataBytes / poolMetaDivisor
	if meta < minPoolMetaBytes {
		meta = minPoolMetaBytes
	}
	if meta > maxPoolMetaBytes {
		meta = maxPoolMetaBytes
	}
	return meta
}

// poolMetaReserveBytes 估算建池时必须留给元数据的空间。
//
// 元数据不止一份：`lvcreate --type thin-pool` 会同时建出 <pool>_tmeta（池元数据）与
// VG 级的 <vg>_pmspare（备用元数据，大小与 metadata 相同，用于元数据损坏时就地恢复），
// 两者都从**同一个卷组**另划——真机上 VG 里能看到 lvol0_pmspare，
// 空间不够时 LVM 还会反问"要不要把这个卷删掉"（见 InitializePool 里的报错记录）。
// 只扣一份的话，用户照着"可用容量"填就会撞上 LVM 的 insufficient free space。
//
// metaSize 留空表示按容量自适应（poolMetaSizeFor），与建池时的取舍一致；
// dataBytes 仅在自适应时用于推元数据大小。
func poolMetaReserveBytes(metaSize string, dataBytes int64) int64 {
	meta := parseLVSize(metaSize)
	if meta <= 0 {
		meta = poolMetaSizeFor(dataBytes)
	}
	two := meta * 2
	return two + two/10 + poolAlignSlackBytes
}

// lvmSectorBytes LVM 的最小分配单位：512 字节扇区。
const lvmSectorBytes = int64(512)

// alignSectorDown 把容量向下对齐到 512 字节扇区边界。
//
// 为什么必须有这一步：容量在界面上以 GB（两位小数）呈现，换算回字节几乎不可能是 512 的倍数
// ——1 GiB 的 0.01 是 10737418.24 字节，乘出来的尾数天然与扇区无关。而 lvcreate / lvextend
// 的 -L / -V 只要不是 512 的整数倍就**直接拒绝**，真机反馈：
//
//	Size is not a multiple of 512. Try using 16825533952 or 16825534464.
//	Invalid argument for --size: 16825534382B
//
// 向下取整最多少用 511 字节，对容量没有实际影响；向上取整则可能顶破池容量或卷组剩余空间
// （而那个上限正是后端刚刚校验过的），因此一律向下。
// 对齐只放在发命令这一处：接口收到的 size_bytes、库里记的数仍按用户填的原样保存，
// 免得"记一个数、建出来另一个数"在多处各自取整后越差越远。
func alignSectorDown(n int64) int64 {
	if n <= 0 {
		return n
	}
	return n - n%lvmSectorBytes
}

// poolFreeBytesWith 由池数据容量与 data_percent 算出池**还能写进去**多少字节。
//
// 这是"这块 thin 卷还能不能用"的正确上限：往 thin 卷里写数据消耗的是**池**的空间，
// 而池一旦建好就把卷组空间整块划走了，卷组剩余（vg_free）只剩 PE 对齐与 pmspare
// 留下的几 MB 零头。拿 vg_free 当上限会得出"16G 的存储卷只剩 48M 可用"——
// 于是新建存储库被判 storage.low_free_space，选根也会跳过这个还空着大半的卷（真机反馈）。
//
// data_percent 读歪（NaN / 越界）时按 0% 处理：宁可乐观退回"只看文件系统"的老口径，
// 也不要因为一个坏百分比把整块盘判成"一点也写不进去"。
func poolFreeBytesWith(sizeBytes int64, dataPercent float64) int64 {
	if sizeBytes <= 0 {
		return 0
	}
	pct := dataPercent
	if math.IsNaN(pct) || pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return int64(float64(sizeBytes) * (100 - pct) / 100)
}

// parseLVSize 把 LVM 的容量写法（4G / 512m / 8T）换算成字节；解析不了返回 0。
//
// 只服务于"建池前估算预留"，所以刻意只认单字母后缀：解析失败就当没填（走自适应），
// 绝不会因此误拒一个合法请求。
func parseLVSize(s string) int64 {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return 0
	}
	var mult int64
	switch s[len(s)-1] {
	case 'k', 'K':
		mult = 1 << 10
	case 'm', 'M':
		mult = 1 << 20
	case 'g', 'G':
		mult = 1 << 30
	case 't', 'T':
		mult = 1 << 40
	case 'p', 'P':
		mult = 1 << 50
	default:
		return 0
	}
	num, err := strconv.ParseFloat(strings.TrimSpace(s[:len(s)-1]), 64)
	if err != nil || num <= 0 {
		return 0
	}
	return int64(num * float64(mult))
}
