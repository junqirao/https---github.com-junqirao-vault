package cache

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
)

// L1 内存快照：把内存缓存落一份盘，让下一次启动能装回去。
//
// 为什么需要：L1 是纯内存缓存，进程退出、目标被移除或用户重挂之后它是冷的，
// 最热的那一层每次都得重新预热。L2 文件本身能跨进程恢复，但它是"温"数据，
// 而且用户可以关掉 L2（L2Bytes=0）。把 L1 自己落一份盘，关掉 L2 也能把内存
// 缓存带回来。
//
// 一致性：只有正常收尾时才写快照，写完才把"干净"标记留在文件里；加载时先把
// 干净标记抹掉再读数据。于是进程若在下一次正常收尾前被强杀（快照是脏的），
// 下次启动整体丢弃 —— 绝不会把崩溃前已被写过的扇区当成仍然有效。
//
// 布局（小端）：
//
//	0..7    magic "vaultL1c"
//	8..11   version
//	12..15  clean（1 = 上一次是正常收尾写下的）
//	16..19  blockSize
//	20..23  sectorSize
//	24..27  shards
//	28..31  reserved
//	32..95  binding（后端命名空间，64 字节十六进制）
//	96..103 条目数
//
// 之后是条目，每条：
//
//	blk(8) valid(4) freq(4) queue(4) sectors(4)
//	接着 sectors 个有效扇区的数据，按扇区号递增排列
//
// 只写有效扇区，所以一个刚被冷读填了几百字节的块不会按整块占用磁盘。
const (
	snapshotMagic      = "vaultL1c"
	snapshotVersion    = 1
	snapshotHeaderSize = 104
	snapshotBindingLen = 64
	snapshotEntrySize  = 24
	// snapshotCleanOffset is where the clean flag lives inside the header.
	snapshotCleanOffset = 12
	// snapshotCountOffset is where the entry count lives inside the header.
	snapshotCountOffset = 96
)

// ErrSnapshotStale reports a snapshot file that exists but must not be trusted:
// it was written by a run that did not finish cleanly, or it belongs to another
// disk (or another geometry). Callers should throw the file away, not treat it
// as a failure.
var ErrSnapshotStale = errors.New("cache: L1 snapshot is stale")

// SaveSnapshot writes the L1 contents to path so the next process can load them
// back. binding identifies the backing LUN; a snapshot written for another disk
// is refused on load. It returns the blocks written and the file size.
//
// The snapshot is written to path.tmp and renamed into place, so a failure
// half way through cannot destroy the previous usable file. Callers must have
// stopped all initiator traffic first (target.close waits for its sessions):
// the fill workers are drained here, but a write that invalidated an L1 block
// while it is being copied would end up in the file.
func (c *Cache) SaveSnapshot(path, binding string) (blocks int, bytes int64, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, 0, nil
	}
	// 在飞的填充先落定，否则会把半块数据当成有效写出去。
	c.Wait()
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err = os.MkdirAll(dir, 0o755); err != nil {
			return 0, 0, err
		}
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, 0, err
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()

	bw := bufio.NewWriterSize(f, 1<<20)
	head := make([]byte, snapshotHeaderSize)
	copy(head[0:8], snapshotMagic)
	binary.LittleEndian.PutUint32(head[8:12], snapshotVersion)
	binary.LittleEndian.PutUint32(head[snapshotCleanOffset:snapshotCleanOffset+4], 1)
	binary.LittleEndian.PutUint32(head[16:20], uint32(c.blockSize))
	binary.LittleEndian.PutUint32(head[20:24], uint32(c.sectorSize))
	binary.LittleEndian.PutUint32(head[24:28], uint32(len(c.shards)))
	copy(head[32:32+snapshotBindingLen], snapshotBinding(binding))
	if _, err = bw.Write(head); err != nil {
		return 0, 0, err
	}

	rec := make([]byte, snapshotEntrySize)
	for _, sh := range c.shards {
		// 逐分片取快照：这里持锁做 I/O 是可接受的，因为保存只发生在收尾路径上，
		// 此时会话已经全部退出，锁没有争用；换来的是不必复制整个分片到内存。
		sh.mu.Lock()
		for _, e := range sh.blocks {
			mask := e.valid.Load()
			// 一个有效位都没有的块（写失效刚清过、填充还没开始）留着也没用。
			if mask == 0 || len(e.data) < c.blockSize {
				continue
			}
			nsec := bits.OnesCount32(mask)
			binary.LittleEndian.PutUint64(rec[0:8], e.blk)
			binary.LittleEndian.PutUint32(rec[8:12], mask)
			binary.LittleEndian.PutUint32(rec[12:16], e.freq.Load())
			binary.LittleEndian.PutUint32(rec[16:20], uint32(e.q))
			binary.LittleEndian.PutUint32(rec[20:24], uint32(nsec))
			if _, err = bw.Write(rec); err != nil {
				sh.mu.Unlock()
				return 0, 0, err
			}
			for i := 0; i < c.sectorsPer; i++ {
				if mask&(1<<uint(i)) == 0 {
					continue
				}
				off := i * c.sectorSize
				if _, err = bw.Write(e.data[off : off+c.sectorSize]); err != nil {
					sh.mu.Unlock()
					return 0, 0, err
				}
			}
			blocks++
		}
		sh.mu.Unlock()
	}

	if err = bw.Flush(); err != nil {
		return 0, 0, err
	}
	var cnt [8]byte
	binary.LittleEndian.PutUint64(cnt[:], uint64(blocks))
	if _, err = f.WriteAt(cnt[:], snapshotCountOffset); err != nil {
		return 0, 0, err
	}
	if err = f.Sync(); err != nil {
		return 0, 0, err
	}
	st, statErr := f.Stat()
	if err = f.Close(); err != nil {
		return 0, 0, err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return 0, 0, err
	}
	if statErr == nil {
		bytes = st.Size()
	}
	return blocks, bytes, nil
}

// LoadSnapshot restores a snapshot written by SaveSnapshot into L1 and reports
// how many blocks were loaded. A missing file yields (0, nil); a file that
// cannot be trusted yields ErrSnapshotStale so the caller can delete it.
func (c *Cache) LoadSnapshot(path, binding string) (int, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, nil
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	head := make([]byte, snapshotHeaderSize)
	if _, err := io.ReadFull(f, head); err != nil {
		return 0, fmt.Errorf("%w: header: %v", ErrSnapshotStale, err)
	}
	switch {
	case string(head[0:8]) != snapshotMagic:
		return 0, ErrSnapshotStale
	case binary.LittleEndian.Uint32(head[8:12]) != snapshotVersion:
		return 0, fmt.Errorf("%w: version %d", ErrSnapshotStale, binary.LittleEndian.Uint32(head[8:12]))
	case binary.LittleEndian.Uint32(head[16:20]) != uint32(c.blockSize),
		binary.LittleEndian.Uint32(head[20:24]) != uint32(c.sectorSize),
		binary.LittleEndian.Uint32(head[24:28]) != uint32(len(c.shards)):
		return 0, fmt.Errorf("%w: cache geometry changed", ErrSnapshotStale)
	case string(head[32:32+snapshotBindingLen]) != string(snapshotBinding(binding)):
		return 0, fmt.Errorf("%w: belongs to another LUN", ErrSnapshotStale)
	case binary.LittleEndian.Uint32(head[snapshotCleanOffset:snapshotCleanOffset+4]) != 1:
		return 0, fmt.Errorf("%w: the previous run did not stop cleanly", ErrSnapshotStale)
	}
	// 先抹掉干净标记再读数据：读完之后若进程被强杀，文件是脏的，下次启动整体
	// 丢弃，而不是把崩溃前已经写过的扇区当成仍然有效。
	if _, err := f.WriteAt([]byte{0, 0, 0, 0}, snapshotCleanOffset); err != nil {
		return 0, ErrSnapshotStale
	}
	if err := f.Sync(); err != nil {
		return 0, ErrSnapshotStale
	}

	count := int64(binary.LittleEndian.Uint64(head[snapshotCountOffset : snapshotCountOffset+8]))
	if count < 0 {
		return 0, ErrSnapshotStale
	}
	br := bufio.NewReaderSize(f, 1<<20)
	rec := make([]byte, snapshotEntrySize)
	loaded := 0
	for i := int64(0); i < count; i++ {
		if _, err := io.ReadFull(br, rec); err != nil {
			return loaded, fmt.Errorf("%w: entry %d: %v", ErrSnapshotStale, i, err)
		}
		blk := binary.LittleEndian.Uint64(rec[0:8])
		mask := binary.LittleEndian.Uint32(rec[8:12])
		freq := binary.LittleEndian.Uint32(rec[12:16])
		q := uint8(binary.LittleEndian.Uint32(rec[16:20]))
		nsec := int(binary.LittleEndian.Uint32(rec[20:24]))
		// 快照比当前 L1 预算大（用户调小了缓存）：装到满为止，余下的丢掉。
		if loaded >= c.totalBlocks {
			continue
		}
		data := make([]byte, c.blockSize)
		got := 0
		for s := 0; s < c.sectorsPer && got < nsec; s++ {
			if mask&(1<<uint(s)) == 0 {
				continue
			}
			off := s * c.sectorSize
			if off+c.sectorSize > len(data) {
				return loaded, fmt.Errorf("%w: entry %d has a bad sector mask", ErrSnapshotStale, i)
			}
			if _, err := io.ReadFull(br, data[off:off+c.sectorSize]); err != nil {
				return loaded, fmt.Errorf("%w: entry %d: %v", ErrSnapshotStale, i, err)
			}
			got++
		}
		if got != nsec {
			return loaded, fmt.Errorf("%w: entry %d claims %d sectors", ErrSnapshotStale, i, nsec)
		}
		if c.installSnapshotBlock(blk, mask, freq, q, data) {
			loaded++
		}
	}
	return loaded, nil
}

// installSnapshotBlock puts a restored block back into its shard, in the queue
// it sat in when it was saved: where a block sits in S3-FIFO is the whole value
// of a warm start, so a restored block must not be forced back to the new queue.
func (c *Cache) installSnapshotBlock(blk uint64, mask, freq uint32, q uint8, data []byte) bool {
	sh := c.shard(blk)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if _, ok := sh.blocks[blk]; ok {
		return false
	}
	e := &entry{blk: blk, data: data, q: q}
	e.valid.Store(mask)
	e.freq.Store(freq)
	sh.blocks[blk] = e
	switch q {
	case queueMid:
		sh.midQ = append(sh.midQ, e)
	case queueOld:
		sh.oldQ = append(sh.oldQ, e)
	default:
		e.q = queueNew
		sh.newQ = append(sh.newQ, e)
	}
	// 队列配额（尤其是 new 队列）必须立刻收回来，否则恢复一个满载的 L1 会让
	// 分片短暂超出配额，下一次填充就会破坏淘汰顺序。
	sh.maintainLocked()
	return true
}

// snapshotBinding renders binding into the header's fixed width field.
func snapshotBinding(binding string) []byte {
	b := make([]byte, snapshotBindingLen)
	copy(b, strings.TrimSpace(binding))
	return b
}
