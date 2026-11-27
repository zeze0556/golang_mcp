package kb

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEmbedServer 返回一个 OpenAI 兼容的 /v1/embeddings 假服务，
// 用原子计数记录被调用次数，用来精确验证"缓存命中时没有重调 embedding"。
type fakeEmbedServer struct {
	srv   *httptest.Server
	calls atomic.Int32
	delay time.Duration
}

func newFakeEmbedServer(t *testing.T, delay time.Duration) *fakeEmbedServer {
	t.Helper()
	f := &fakeEmbedServer{delay: delay}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if f.delay > 0 {
			time.Sleep(f.delay)
		}
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		inputs, _ := req.Input.([]any)
		data := make([]map[string]any, 0, len(inputs))
		for i := range inputs {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, 0, 0}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEmbedServer) embedder(model string, dim int) *Embedder {
	return NewEmbedder(f.srv.URL+"/v1", model, "", dim, "", 5)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// writeDoc 在 watch 目录写一个文档，docID 为相对路径。
func writeDoc(t *testing.T, watch, docID, content string) {
	t.Helper()
	p := filepath.Join(watch, filepath.FromSlash(docID))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newTestStore(t *testing.T, path string) *LocalStore {
	t.Helper()
	s, err := NewLocalStore(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// 场景：进程重启后 state 命中，未变更文档不应再调 embedding。
func TestIngestStateCacheHitAcrossRestart(t *testing.T) {
	tmp := t.TempDir()
	watch := filepath.Join(tmp, "watch")
	write := filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)
	writeDoc(t, watch, "a.md", "内容甲，用于缓存测试")
	writeDoc(t, watch, "b.md", "内容乙，用于缓存测试")

	fake := newFakeEmbedServer(t, 0)
	emb := fake.embedder("m1", 3)
	indexPath := filepath.Join(write, "kb_index.json")

	// 第一次运行：每个文档 1 次 embedding 调用
	s1 := newTestStore(t, indexPath)
	s1.SetEmbeddingVersion(emb.Version())
	ing1 := NewIngestor(s1, emb, watch, write, 800, 0, testLogger())
	ing1.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("首轮应调用 embedding 2 次，实际 %d", got)
	}
	if _, err := os.Stat(filepath.Join(write, ingestStateFileName)); err != nil {
		t.Fatalf("state 文件未落盘: %v", err)
	}

	// 模拟进程重启：全新 store/embedder/ingestor，读同一 write 目录
	s2 := newTestStore(t, indexPath)
	ing2 := NewIngestor(s2, emb, watch, write, 800, 0, testLogger())
	if len(ing2.seenHash) != 2 {
		t.Fatalf("重启后应恢复 2 条 hash，实际 %d", len(ing2.seenHash))
	}
	ing2.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("内容未变不应重调 embedding，累计调用 %d（期望仍为 2）", got)
	}
}

// 场景：文档内容变了 → 只重算变更的那篇。
func TestIngestStateRecachesChangedDocOnly(t *testing.T) {
	tmp := t.TempDir()
	watch, write := filepath.Join(tmp, "watch"), filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)
	writeDoc(t, watch, "a.md", "原文甲")
	writeDoc(t, watch, "b.md", "原文乙")

	fake := newFakeEmbedServer(t, 0)
	emb := fake.embedder("m1", 3)
	indexPath := filepath.Join(write, "kb_index.json")
	s := newTestStore(t, indexPath)
	s.SetEmbeddingVersion(emb.Version())
	ing := NewIngestor(s, emb, watch, write, 800, 0, testLogger())
	ing.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 2 {
		t.Fatalf("首轮应 2 次，实际 %d", got)
	}

	// 只改 a.md
	writeDoc(t, watch, "a.md", "改后的甲内容")
	ing.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 3 {
		t.Errorf("只应重算 a.md 一次，累计调用 %d（期望 3）", got)
	}
}

// 场景：换 embedding 模型 → 缓存整体失效，全量重建。
func TestIngestStateInvalidatedOnModelChange(t *testing.T) {
	tmp := t.TempDir()
	watch, write := filepath.Join(tmp, "watch"), filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)
	writeDoc(t, watch, "a.md", "模型切换测试")

	fake := newFakeEmbedServer(t, 0)
	emb1 := fake.embedder("model-v1", 3)
	indexPath := filepath.Join(write, "kb_index.json")
	s := newTestStore(t, indexPath)
	s.SetEmbeddingVersion(emb1.Version())
	ing1 := NewIngestor(s, emb1, watch, write, 800, 0, testLogger())
	ing1.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 1 {
		t.Fatalf("首轮应 1 次，实际 %d", got)
	}

	// 换模型重启：state 里的 embedding_version 是 model-v1，当前是 model-v2
	emb2 := fake.embedder("model-v2", 3)
	s2 := newTestStore(t, indexPath)
	ing2 := NewIngestor(s2, emb2, watch, write, 800, 0, testLogger())
	if len(ing2.seenHash) != 0 {
		t.Fatalf("换模型后缓存应清空，实际还有 %d 条", len(ing2.seenHash))
	}
	ing2.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("换模型后应全量重建，累计调用 %d（期望 2）", got)
	}
}

// 场景：索引文件被删但 state 还在 → 一致性校验生效，全量重建（防永久漏检）。
func TestIngestStateInvalidatedOnIndexMismatch(t *testing.T) {
	tmp := t.TempDir()
	watch, write := filepath.Join(tmp, "watch"), filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)
	writeDoc(t, watch, "a.md", "索引丢失测试")

	fake := newFakeEmbedServer(t, 0)
	emb := fake.embedder("m1", 3)
	indexPath := filepath.Join(write, "kb_index.json")
	s1 := newTestStore(t, indexPath)
	s1.SetEmbeddingVersion(emb.Version())
	ing1 := NewIngestor(s1, emb, watch, write, 800, 0, testLogger())
	ing1.RunOnce(context.Background())

	// 用户删了索引文件，state 文件保留
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}
	s2 := newTestStore(t, indexPath) // 全新空索引
	ing2 := NewIngestor(s2, emb, watch, write, 800, 0, testLogger())
	if len(ing2.seenHash) != 0 {
		t.Fatalf("索引与 state 不一致时缓存应清空，实际 %d 条", len(ing2.seenHash))
	}
	ing2.RunOnce(context.Background())
	if got := fake.calls.Load(); got != 2 {
		t.Errorf("索引丢失应触发重建，累计调用 %d（期望 2）", got)
	}
}

// 场景：StartPolling 不阻塞调用方，即便 embedding 很慢。
func TestStartPollingNonBlocking(t *testing.T) {
	tmp := t.TempDir()
	watch, write := filepath.Join(tmp, "watch"), filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)
	writeDoc(t, watch, "slow.md", "慢服务测试")

	fake := newFakeEmbedServer(t, 400*time.Millisecond)
	emb := fake.embedder("m1", 3)
	s := newTestStore(t, filepath.Join(write, "kb_index.json"))
	s.SetEmbeddingVersion(emb.Version())
	ing := NewIngestor(s, emb, watch, write, 800, 0, testLogger())

	start := time.Now()
	stop := ing.StartPolling(context.Background(), 0)
	elapsed := time.Since(start)
	if elapsed > 150*time.Millisecond {
		t.Errorf("StartPolling 阻塞了 %v，应立刻返回（扫描在后台）", elapsed)
	}

	// 等后台首轮完成
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if s.Count() > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Count() == 0 {
		t.Error("后台首轮扫描应完成入库")
	}
	stop()
}

// 场景：stop 幂等，重复调用不 panic。
func TestStopIdempotent(t *testing.T) {
	tmp := t.TempDir()
	watch, write := filepath.Join(tmp, "watch"), filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)

	fake := newFakeEmbedServer(t, 0)
	emb := fake.embedder("m1", 3)
	s := newTestStore(t, filepath.Join(write, "kb_index.json"))
	ing := NewIngestor(s, emb, watch, write, 800, 0, testLogger())
	stop := ing.StartPolling(context.Background(), 0)
	stop()
	stop()
	stop()
}

// 场景：取消能中断在途扫描（stop 后不再处理后续文档）。
func TestStopCancelsInFlightScan(t *testing.T) {
	tmp := t.TempDir()
	watch, write := filepath.Join(tmp, "watch"), filepath.Join(tmp, "write")
	_ = os.MkdirAll(watch, 0o755)
	_ = os.MkdirAll(write, 0o755)
	// 多篇文档，首篇慢、后续应被取消
	for i := 0; i < 5; i++ {
		writeDoc(t, watch, fmt.Sprintf("d%d.md", i), "取消测试文档")
	}

	fake := newFakeEmbedServer(t, 200*time.Millisecond)
	emb := fake.embedder("m1", 3)
	s := newTestStore(t, filepath.Join(write, "kb_index.json"))
	s.SetEmbeddingVersion(emb.Version())
	ing := NewIngestor(s, emb, watch, write, 800, 0, testLogger())
	stop := ing.StartPolling(context.Background(), 0)
	// 首篇进入 embedding 后取消
	time.Sleep(80 * time.Millisecond)
	stop()
	time.Sleep(300 * time.Millisecond)
	if got := fake.calls.Load(); got >= 5 {
		t.Errorf("取消后不应继续处理全部文档，实际调用 %d 次", got)
	}
}
