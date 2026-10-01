package app

import "testing"

// fakeEventSink 记录被发布出去的事件（topic + payload）。
type fakeEventSink struct {
	topics   []string
	payloads []any
}

func (f *fakeEventSink) Publish(topic string, payload any) {
	f.topics = append(f.topics, topic)
	f.payloads = append(f.payloads, payload)
}

// TestEmitMountPhasePublishesMountEvent 锁定挂载阶段事件的线上契约：
// **topic=mount + action=progress + allocation_id + phase**。
//
// 代理按 topic 分流（internal/agent/sse.go 的 handleServerEvent），按 allocation_id
// 归属到本机正在挂载的分配，再按 phase 推进界面文案。任何一个字段改名，界面就永远停在
// "挂载中"——而且不会有任何报错，只能靠这条测试拦住。
func TestEmitMountPhasePublishesMountEvent(t *testing.T) {
	sink := &fakeEventSink{}
	Deps{Events: sink}.emitMountPhase("alloc-1", MountPhaseConfiguringTarget)

	if len(sink.topics) != 1 {
		t.Fatalf("发布条数 = %d，期望 1", len(sink.topics))
	}
	if sink.topics[0] != MountEventTopic {
		t.Fatalf("topic = %q，期望 %q", sink.topics[0], MountEventTopic)
	}
	payload, ok := sink.payloads[0].(map[string]any)
	if !ok {
		t.Fatalf("payload 类型 = %T，期望 map[string]any", sink.payloads[0])
	}
	if payload["action"] != "progress" {
		t.Fatalf("action = %v，期望 progress", payload["action"])
	}
	if payload["allocation_id"] != "alloc-1" {
		t.Fatalf("allocation_id = %v，期望 alloc-1", payload["allocation_id"])
	}
	if payload["phase"] != MountPhaseConfiguringTarget {
		t.Fatalf("phase = %v，期望 %q", payload["phase"], MountPhaseConfiguringTarget)
	}
	if payload["at"] == nil {
		t.Fatal("应带 at（毫秒时间戳），否则阶段事件无法排序排查")
	}
}

// TestEmitMountPhaseIgnoresIncomplete 参数不全时不发布。
//
// 代理收到没有 allocation_id 或 phase 的事件无法归属，只会被静默丢弃；
// 与其发出去，不如在这里就不发（也避免日志里出现无意义的事件）。
func TestEmitMountPhaseIgnoresIncomplete(t *testing.T) {
	cases := []struct {
		name         string
		allocationID string
		phase        string
	}{
		{"缺 allocation_id", "", MountPhaseAllocating},
		{"缺 phase", "alloc-1", ""},
		{"都是空白", "  ", "  "},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink := &fakeEventSink{}
			Deps{Events: sink}.emitMountPhase(c.allocationID, c.phase)
			if len(sink.topics) != 0 {
				t.Fatalf("不应发布事件，实际 %v", sink.topics)
			}
		})
	}
}

// TestEmitMountPhaseWithoutSink 未装配事件通道时必须静默忽略（挂载不能因此 panic）。
func TestEmitMountPhaseWithoutSink(t *testing.T) {
	Deps{}.emitMountPhase("alloc-1", MountPhaseAllocating)
}
