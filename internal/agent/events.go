package agent

import "sync"

// Event 是本地 SSE 事件（见 docs/agent-api.md「事件流」）。
//
// 事件类型：mount / unmount / revoked / server / session / update / web_update / heartbeat / download / upload。
type Event struct {
	// Type SSE 事件名。
	Type string
	// Data 事件负载（会被 JSON 序列化）。
	Data any
}

// eventSubscriberBuffer 是单个订阅者的事件缓冲槽位数。
//
// 取 256（而非更小的 32）：高速传输下进度事件可能每秒上千次推送，远超 UI 消费速度，
// 更大的缓冲显著降低"进度事件被中间丢弃"的概率。
const eventSubscriberBuffer = 256

// EventHub 是本地事件广播器。
//
// 语义与服务端 EventHub 一致：订阅者槽位满时**丢弃**事件而不是阻塞发布方 ——
// 事件流是「尽力而为」的通知通道，权威状态以 GET /agent/state 为准。
// 满时优先丢弃**最旧**的一条以便写入最新（进度类事件只关心最新值，见 Publish）。
type EventHub struct {
	mu     sync.Mutex
	subs   map[int]chan Event
	nextID int
}

// NewEventHub 构造本地事件广播器。
func NewEventHub() *EventHub {
	return &EventHub{subs: make(map[int]chan Event)}
}

// Subscribe 注册订阅者，返回订阅 ID 与事件通道。
func (h *EventHub) Subscribe() (int, <-chan Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	id := h.nextID
	ch := make(chan Event, eventSubscriberBuffer)
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

// Publish 广播事件。
//
// 绝不阻塞发布方：订阅者槽位满时优先丢弃「最旧的一条」再写入最新（进度类事件保留最新值
// 比保留完整历史更重要）；若并发消费导致仍满，则放弃本条。权威状态以 GET /agent/state 为准。
func (h *EventHub) Publish(ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, ch := range h.subs {
		select {
		case ch <- ev:
			continue
		default:
		}
		// 缓冲已满：丢掉最旧的一条，为最新事件腾位（仍不阻塞）。
		select {
		case <-ch:
		default:
		}
		select {
		case ch <- ev:
		default:
		}
	}
}
