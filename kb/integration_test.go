package kb

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

// TestIntegrationRealEmbedding 用真实 embedding 服务跑通全链路。
// 默认跳过；设置 KB_IT=1 启用（依赖 192.168.2.236:8082 在线）。
func TestIntegrationRealEmbedding(t *testing.T) {
	if os.Getenv("KB_IT") != "1" {
		t.Skip("设置 KB_IT=1 以运行真实 embedding 集成测试")
	}
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tmp := t.TempDir()
	watch := filepath.Join(tmp, "watch")
	write := filepath.Join(tmp, "write")
	if err := os.MkdirAll(watch, 0o755); err != nil {
		t.Fatal(err)
	}
	// 造两个 scope 的文档
	if err := os.WriteFile(filepath.Join(watch, "billing.md"),
		[]byte("退款流程\n用户在订单页申请退款，审核通过后 3-5 个工作日到账。\n退款金额原路返回。"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(watch, "hr"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(watch, "hr", "leave.md"),
		[]byte("请假制度\n年假需提前 3 天申请，事假需主管批准。"), 0o644); err != nil {
		t.Fatal(err)
	}

	emb := NewEmbedder("http://192.168.2.236:8082/v1", "Qwen3-Embedding-0.6B", "", 1024, "Instruct: 检索知识库文档\nQuery: ", 60)
	store, err := NewLocalStore(filepath.Join(write, "kb_index.json"))
	if err != nil {
		t.Fatal(err)
	}
	store.SetEmbeddingVersion(emb.Version())
	ing := NewIngestor(store, emb, watch, write, 200, 30, logger)

	n, failed := ing.RunOnce(ctx)
	if failed > 0 || n != 2 {
		t.Fatalf("入库应成功 2 篇, 实际 n=%d failed=%d", n, failed)
	}
	if store.Count() == 0 {
		t.Fatal("入库后索引为空")
	}

	// 指纹探针（验证服务稳定）
	v1, err := emb.FingerprintProbe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := emb.FingerprintProbe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if Cosine(v1, v2) < 0.9999 {
		t.Errorf("指纹漂移: cos=%f", Cosine(v1, v2))
	}

	// 查询侧检索
	qVec, err := emb.EmbedQuery(ctx, "如何申请退款")
	if err != nil {
		t.Fatal(err)
	}
	hits, err := store.Search(qVec, SearchParams{TopK: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("检索无结果")
	}
	t.Logf("查询 '如何申请退款' top1: doc=%s scope=%s score=%.4f text=%q", hits[0].DocID, hits[0].Scope, hits[0].Score, hits[0].Text[:min(40, len(hits[0].Text))])
	// 退款相关文档应排在请假文档前（billing.md 在根目录故 scope=default，按 docID 判断）
	if hits[0].DocID != "billing.md" {
		t.Errorf("期望退款文档 billing.md 排首位, 实际 %s", hits[0].DocID)
	}

	// ACL: 只看 hr scope（leave.md 在 hr/ 子目录）
	hitsHR, _ := store.Search(qVec, SearchParams{TopK: 5, Scopes: []string{"hr"}})
	if len(hitsHR) != 1 || hitsHR[0].DocID != "hr/leave.md" {
		t.Errorf("ACL hr 过滤应只返回 leave.md, 实际 %v", hitsHR)
	}
	for _, h := range hitsHR {
		if h.Scope != "hr" {
			t.Errorf("ACL 过滤失效: 返回了 %s", h.Scope)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
