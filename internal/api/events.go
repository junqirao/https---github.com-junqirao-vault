package api

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"
)

// Event 是 SSE 推送的事件。
type Event struct {
	// ID 是进程内单调递增的事件序号，供客户端通过 Last-Event-ID 断线重连（尽力而为）。
	ID   int64  `json:"id,omitempty"`
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
	At   int64  `json:"at"`
}

// EventHub 是进程内的事件广播器（SSE 用）。
//
// 语义：订阅者槽位满时丢弃事件而不是阻塞发布方 —— 事件流是"尽力而为"的通知通道，
// 权威状态始终以 DB / 显式查询接口为准（见 5.7：租约表是在线状态的权威来源）。
//
// ⚠️ 本广播器是**进程内**的，单实例部署下够用；不保留历史事件，
// 因此 Last-Event-ID 仅做透传记录，不提供回放（见 handleEvents 的说明）。
type EventHub struct {
	mu     sync.Mutex
	subs   map[int]chan Event
	nextID int
	seq    int64
}

// NewEventHub 构造事件广播器。
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[int]chan Event)}
}

// Subscribe 注册订阅者，返回订阅 ID 与事件通道。
func (h *EventHub) Subscribe() (int, <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan Event, 32)
	h.subs[id] = ch
	return id, ch
}

// Unsubscribe 注销订阅者并关闭其通道。
func (h *EventHub) Unsubscribe(id int) {
	h.mu.Lock()
	ch, ok := h.subs[id]
	delete(h.subs, id)
	h.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Publish 广播事件。事件 ID 由广播器单调分配。
func (h *EventHub) Publish(ev Event) {
	if ev.At == 0 {
		ev.At = time.Now().UnixMilli()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	ev.ID = h.seq
	for _, ch := range h.subs {
		select {
		case ch <- ev:
		default:
			// 订阅者处理不过来：丢弃本条，避免拖慢发布方。
		}
	}
}

// sseEncoder 把事件写成 SSE 帧。
type sseEncoder struct{ w io.Writer }

// newSSEEncoder 构造 SSE 编码器。
func newSSEEncoder(w io.Writer) *sseEncoder { return &sseEncoder{w: w} }

// write 输出一帧 `id:` + `event:` + `data:`。
func (e *sseEncoder) write(ev Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if ev.ID > 0 {
		if _, err := fmt.Fprintf(e.w, "id: %d\n", ev.ID); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", ev.Type, data)
	return err
}
