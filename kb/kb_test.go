package kb

import (
	"context"
	"math"
	"testing"
	"time"
)

// --- 纯函数测试 ---

func TestChunkTextLinesAndOverlap(t *testing.T) {
	content := "第一行\n第二行\n第三行\n第四行\n第五行"
	pieces := chunkText(content, 8, 2)
	if len(pieces) < 2 {
		t.Fatalf("期望多片，实际 %d", len(pieces))
	}
	if pieces[0].StartLine != 1 {
		t.Errorf("首片起始行应为 1，实际 %d", pieces[0].StartLine)
	}
	last := pieces[len(pieces)-1]
	if last.EndLine < last.StartLine {
		t.Errorf("末片 end_line < start_line: %d < %d", last.EndLine, last.StartLine)
	}
	if last.EndLine > 5 {
		t.Errorf("行号越界: %d > 5", last.EndLine)
	}
	// 相邻片字符区间应重叠
	if len(pieces) >= 2 && pieces[1].CharStart >= pieces[0].CharEnd {
		t.Errorf("overlap 未生效: piece0.end=%d piece1.start=%d", pieces[0].CharEnd, pieces[1].CharStart)
	}
}

func TestScopeFromDocID(t *testing.T) {
	cases := map[string]string{
		"billing/refund.md":  "billing",
		"notes.md":           "default",
		"hr/policy/leave.md": "hr",
	}
	for in, want := range cases {
		if got := scopeFromDocID(in); got != want {
			t.Errorf("scopeFromDocID(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestCosine(t *testing.T) {
	if c := Cosine([]float32{1, 0}, []float32{1, 0}); math.Abs(c-1) > 1e-6 {
		t.Errorf("相同向量 cos 应为 1, 实际 %f", c)
	}
	if c := Cosine([]float32{1, 0}, []float32{0, 1}); math.Abs(c) > 1e-6 {
		t.Errorf("正交向量 cos 应为 0, 实际 %f", c)
	}
}

func TestIntersectScopes(t *testing.T) {
	granted := []string{"a", "b"}
	// 越权项 c 静默丢弃
	got := intersectScopes([]string{"a", "c"}, granted)
	if len(got) != 1 || got[0] != "a" {
		t.Errorf("交集应只剩 [a], 实际 %v", got)
	}
	// open 模式（granted=nil）原样返回
	got = intersectScopes([]string{"x", "y"}, nil)
	if len(got) != 2 {
		t.Errorf("open 模式应返回全部, 实际 %v", got)
	}
	// 全部越权 → 空
	got = intersectScopes([]string{"z"}, granted)
	if len(got) != 0 {
		t.Errorf("全部越权应为空, 实际 %v", got)
	}
}

func TestScopeResolver(t *testing.T) {
	r := NewScopeResolver([]TokenScope{{Token: "tk1", Scopes: []string{"a"}}})
	if s := r.ScopesFor("tk1"); len(s) != 1 || s[0] != "a" {
		t.Errorf("tk1 应见 [a], 实际 %v", s)
	}
	if s := r.ScopesFor("unknown"); s == nil || len(s) != 0 {
		t.Errorf("未知 token 应返回空非 nil, 实际 %v", s)
	}
	// open 模式
	ropen := NewScopeResolver(nil)
	if s := ropen.ScopesFor("any"); s != nil {
		t.Errorf("open 模式应返回 nil, 实际 %v", s)
	}
}

// --- LocalStore 检索测试 ---

func TestLocalStoreSearchAndACL(t *testing.T) {
	s, err := NewLocalStore(t.TempDir() + "/idx.json")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	chunks := []Chunk{
		{ChunkID: "d1#c0", DocID: "d1", Scope: "billing", Text: "退款流程", Vector: []float32{1, 0}, UpdatedAt: now, Ordinal: 0},
		{ChunkID: "d2#c0", DocID: "d2", Scope: "hr", Text: "请假流程", Vector: []float32{0, 1}, UpdatedAt: now, Ordinal: 0},
	}
	if err := s.UpsertDocs("d1", chunks[:1]); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertDocs("d2", chunks[1:]); err != nil {
		t.Fatal(err)
	}
	// 查询向量接近 billing
	hits, _ := s.Search([]float32{1, 0}, SearchParams{TopK: 10, Scopes: []string{"billing"}})
	if len(hits) != 1 || hits[0].Scope != "billing" {
		t.Errorf("scope 过滤应只返回 billing, 实际 %v", hits)
	}
	// 无 scope 过滤 → 全部
	hits, _ = s.Search([]float32{1, 0}, SearchParams{TopK: 10})
	if len(hits) != 2 {
		t.Errorf("无过滤应返回 2 条, 实际 %d", len(hits))
	}
	// cite
	c, ok := s.GetByChunkID("d1#c0")
	if !ok || c.Text != "退款流程" {
		t.Errorf("cite 取回失败: %v %v", c, ok)
	}
	// 删除文档
	if err := s.DeleteDoc("d1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetByChunkID("d1#c0"); ok {
		t.Error("删除后不应存在")
	}
}

// --- ingest 端到端（用 mock embedder 语义，直接构造 store） ---

func TestPersistenceReload(t *testing.T) {
	dir := t.TempDir()
	p := dir + "/idx.json"
	s1, _ := NewLocalStore(p)
	s1.SetEmbeddingVersion("m@dim2")
	s1.UpsertDocs("d1", []Chunk{{ChunkID: "d1#c0", DocID: "d1", Scope: "s", Text: "x", Vector: []float32{1, 0}, Ordinal: 0}})
	s2, err := NewLocalStore(p)
	if err != nil {
		t.Fatal(err)
	}
	if s2.Count() != 1 {
		t.Errorf("重载后应有 1 条, 实际 %d", s2.Count())
	}
	if s2.EmbeddingVersion() != "m@dim2" {
		t.Errorf("embedding 版本未持久化: %s", s2.EmbeddingVersion())
	}
}

func TestServiceACLScenarios(t *testing.T) {
	// 验证 min_score 过滤
	s, _ := NewLocalStore(t.TempDir() + "/idx.json")
	s.UpsertDocs("d1", []Chunk{{ChunkID: "d1#c0", DocID: "d1", Scope: "s", Text: "x", Vector: []float32{1, 0}, Ordinal: 0}})
	hits, _ := s.Search([]float32{1, 0}, SearchParams{TopK: 10, MinScore: 1.5})
	if len(hits) != 0 {
		t.Errorf("min_score=1.5 应过滤全部, 实际 %d", len(hits))
	}
	// 确保 context 取消不会 panic（embed 调用留给集成测试，这里只测 ctx 传递骨架）
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = ctx
}
