package kb

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// TestMCPToolRegistration 验证工具在 MCP 框架层正确注册，且 schema 不含 token/acl。
func TestMCPToolRegistration(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, err := NewLocalStore(t.TempDir() + "/idx.json")
	if err != nil {
		t.Fatal(err)
	}
	svc := &Service{
		embed:         NewEmbedder("http://127.0.0.1:1/v1", "m", "", 4, "", 1),
		store:         store,
		resolver:      NewScopeResolver(nil), // open 模式
		logger:        logger,
		topKCap:       20,
		maxCharBudget: 12000,
	}
	s := server.NewMCPServer("test", "1.0")
	RegisterTools(s, svc)

	// 通过 ListTools 检查注册结果
	tools := s.ListTools()
	if len(tools) != 4 {
		t.Fatalf("应注册 4 个工具, 实际 %d", len(tools))
	}
	names := map[string]bool{}
	for _, tl := range tools {
		tool := tl.Tool
		names[tool.Name] = true
		schemaBytes, _ := json.Marshal(tool.InputSchema)
		schema := string(schemaBytes)
		if strings.Contains(schema, "token") || strings.Contains(schema, "acl") {
			t.Errorf("工具 %s 的 schema 含敏感字段: %s", tool.Name, schema)
		}
		if tool.Description == "" {
			t.Errorf("工具 %s 缺少 description", tool.Name)
		}
		// 只有返回文档内容的工具需要声明 untrusted；list_scopes 只返回元数据
		if tool.Name == "kb.search" || tool.Name == "kb.fetch" || tool.Name == "kb.cite" {
			if !strings.Contains(strings.ToLower(tool.Description), "untrusted") {
				t.Errorf("工具 %s 的 description 未声明 untrusted", tool.Name)
			}
		}
	}
	for _, want := range []string{"kb.search", "kb.fetch", "kb.cite", "kb.list_scopes"} {
		if !names[want] {
			t.Errorf("缺少工具 %s", want)
		}
	}
}

// TestCiteViaHandler 直接调 handler 验证双返回结构（content + structuredContent）。
func TestCiteViaHandler(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store, _ := NewLocalStore(t.TempDir() + "/idx.json")
	store.UpsertDocs("d1", []Chunk{{
		ChunkID: "d1#c0", DocID: "d1", Scope: "s", Text: "原文",
		Vector: []float32{1, 0}, StartLine: 3, EndLine: 5, Ordinal: 0,
	}})
	svc := &Service{
		embed: NewEmbedder("http://127.0.0.1:1/v1", "m", "", 2, "", 1),
		store: store, resolver: NewScopeResolver(nil), logger: logger,
		topKCap: 20, maxCharBudget: 12000,
	}
	h := handleCite(svc)
	req := mcp.CallToolRequest{}
	req.Params.Name = "kb.cite"
	req.Params.Arguments = map[string]any{"chunk_id": "d1#c0"}
	result, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || len(result.Content) == 0 {
		t.Fatal("应返回 content")
	}
	if result.StructuredContent == nil {
		t.Fatal("应返回 structuredContent")
	}
	sc := result.StructuredContent.(citeResponse)
	if sc.StartLine != 3 || sc.EndLine != 5 || !strings.Contains(sc.Verbatim, "L3-L5") {
		t.Errorf("溯源行号错误: %+v", sc)
	}
}
