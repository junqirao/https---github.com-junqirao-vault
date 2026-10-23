package agent

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEnsureMyAllocationUsesAllocate 钉住"挂载前必须让服务端确认分配"这一口径。
//
// 分配记录本身看不出差异盘的 parent_version：先 ListAllocations 再拿第一条，会把
// 母盘维护前的旧差异盘一路带到 RequestMount，最后报 disk.parent_version_mismatch，
// 而用户在界面上没有删除旧差异盘的入口。Allocate 才能触发服务端的"作废旧盘 + 重建"。
func TestEnsureMyAllocationUsesAllocate(t *testing.T) {
	var listCalls, allocCalls int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/repos/r1/allocations", func(w http.ResponseWriter, _ *http.Request) {
		listCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{{"id": "stale-alloc", "repo_id": "r1", "user_id": "u1", "state": "allocated"}},
		})
	})
	mux.HandleFunc("POST /v1/repos/r1/allocations", func(w http.ResponseWriter, _ *http.Request) {
		allocCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "fresh-alloc", "repo_id": "r1", "user_id": "u1", "state": "allocated",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	client, err := newServerClient(srv.URL, "test-token", "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}

	a := &Agent{}
	id, err := a.ensureMyAllocation(context.Background(), client, "r1", "u1")
	if err != nil {
		t.Fatalf("ensureMyAllocation: %v", err)
	}
	if id != "fresh-alloc" {
		t.Fatalf("应使用 Allocate 返回的分配，实际 %q", id)
	}
	if allocCalls != 1 {
		t.Fatalf("Allocate 调用次数 = %d，期望 1", allocCalls)
	}
	if listCalls != 0 {
		t.Fatalf("不应再走 ListAllocations（会拿到旧分配），实际调用 %d 次", listCalls)
	}
}
