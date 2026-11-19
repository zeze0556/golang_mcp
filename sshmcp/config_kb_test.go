package sshmcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigKnowledgeBase(t *testing.T) {
	dir := t.TempDir()
	watch := filepath.Join(dir, "watch")
	write := filepath.Join(dir, "write")
	yamlContent := `transport: http
servers:
  dummy:
    host: 127.0.0.1
    port: 22
    user: root
    key_path: /tmp/key
knowledge_base:
  enabled: true
  watch_dir: "` + filepath.ToSlash(watch) + `"
  write_dir: "` + filepath.ToSlash(write) + `"
  embedding:
    endpoint: http://192.168.2.236:8082/v1
    model: Qwen3-Embedding-0.6B
    dim: 1024
  token_scopes:
    - token: tk1
      scopes: [billing, hr]
`
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if !cfg.KnowledgeBase.Enabled {
		t.Error("KB enabled 未解析")
	}
	if cfg.KnowledgeBase.Embedding.Dim != 1024 {
		t.Errorf("dim 解析错误: %d", cfg.KnowledgeBase.Embedding.Dim)
	}
	if len(cfg.KnowledgeBase.TokenScopes) != 1 || cfg.KnowledgeBase.TokenScopes[0].Scopes[0] != "billing" {
		t.Errorf("token_scopes 解析错误: %+v", cfg.KnowledgeBase.TokenScopes)
	}
}

func TestValidateKBRejectsOverlap(t *testing.T) {
	base := t.TempDir() // 当前平台的真实绝对路径（跨平台）
	cases := []struct {
		name      string
		watch     string
		write     string
		wantError string
	}{
		{"same dir", filepath.Join(base, "kb"), filepath.Join(base, "kb"), "同一目录"},
		{"nested write in watch", filepath.Join(base, "kb"), filepath.Join(base, "kb", "out"), "嵌套"},
		{"nested watch in write", filepath.Join(base, "kb", "in"), filepath.Join(base, "kb"), "嵌套"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kb := KnowledgeBaseConfig{
				WatchDir:  tc.watch,
				WriteDir:  tc.write,
				Embedding: KBEmbeddingConfig{Endpoint: "http://x/v1", Model: "m", Dim: 1024},
			}
			err := validateKnowledgeBase(kb)
			if err == nil {
				t.Fatalf("期望校验失败，实际通过（watch=%s write=%s）", tc.watch, tc.write)
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Errorf("错误信息应含 %q, 实际 %q", tc.wantError, err.Error())
			}
		})
	}
}

func TestValidateKBRequiresAbsolutePaths(t *testing.T) {
	base := t.TempDir()
	kb := KnowledgeBaseConfig{
		WatchDir:  filepath.Join(base, "watch"),
		WriteDir:  "relative/write", // 唯一的相对路径，应被拒绝
		Embedding: KBEmbeddingConfig{Endpoint: "http://x/v1", Model: "m", Dim: 1024},
	}
	if err := validateKnowledgeBase(kb); err == nil || !strings.Contains(err.Error(), "绝对路径") {
		t.Errorf("应拒绝相对路径, 实际 %v", err)
	}
}

// validKB 构造一份合法的 pgvector 后端配置基线，供下游用例按需改写。
func validKBPg(t *testing.T) KnowledgeBaseConfig {
	t.Helper()
	base := t.TempDir()
	return KnowledgeBaseConfig{
		Backend:   "pgvector",
		WatchDir:  filepath.Join(base, "watch"),
		WriteDir:  filepath.Join(base, "write"),
		Embedding: KBEmbeddingConfig{Endpoint: "http://x/v1", Model: "m", Dim: 1024},
		PGVector:  KBPGVectorConfig{DSN: "postgres://u:p@127.0.0.1:5432/kb"},
	}
}

func TestValidateKBPgVectorBackend(t *testing.T) {
	// 合法基线（table 留空走默认）应通过
	if err := validateKnowledgeBase(validKBPg(t)); err != nil {
		t.Fatalf("合法 pgvector 配置应通过: %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*KnowledgeBaseConfig)
		wantErr string
	}{
		{"empty dsn", func(c *KnowledgeBaseConfig) { c.PGVector.DSN = "" }, "dsn 不能为空"},
		{"bad backend", func(c *KnowledgeBaseConfig) { c.Backend = "mysql" }, "backend 只能是"},
		{"injection table", func(c *KnowledgeBaseConfig) { c.PGVector.Table = "kb;DROP TABLE x" }, "table 只能用"},
		{"digit-leading table", func(c *KnowledgeBaseConfig) { c.PGVector.Table = "1kb" }, "table 只能用"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validKBPg(t)
			tc.mutate(&c)
			err := validateKnowledgeBase(c)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("期望错误含 %q, 实际 %v", tc.wantErr, err)
			}
		})
	}

	// local（含空值默认）仍按原逻辑校验，不应因 pgvector 字段缺失而失败
	local := validKBPg(t)
	local.Backend = "local"
	local.PGVector = KBPGVectorConfig{} // 清空 pgvector 配置
	if err := validateKnowledgeBase(local); err != nil {
		t.Errorf("local 后端不应要求 pgvector.dsn: %v", err)
	}
}

func TestLoadConfigParsesPgVector(t *testing.T) {
	dir := t.TempDir()
	watch := filepath.Join(dir, "watch")
	write := filepath.Join(dir, "write")
	yamlContent := `transport: http
servers:
  dummy:
    host: 127.0.0.1
    port: 22
    user: root
    key_path: /tmp/key
knowledge_base:
  enabled: true
  backend: pgvector
  watch_dir: "` + filepath.ToSlash(watch) + `"
  write_dir: "` + filepath.ToSlash(write) + `"
  embedding:
    endpoint: http://x/v1
    model: m
    dim: 1024
  pgvector:
    dsn: "postgres://u:p@db:5432/kb?sslmode=disable"
    table: kb_docs
    max_conns: 8
    auto_schema: false
`
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(yamlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatalf("LoadConfig 失败: %v", err)
	}
	if cfg.KnowledgeBase.Backend != "pgvector" {
		t.Errorf("backend 解析错误: %q", cfg.KnowledgeBase.Backend)
	}
	pg := cfg.KnowledgeBase.PGVector
	if pg.DSN == "" || pg.Table != "kb_docs" || pg.MaxConns != 8 {
		t.Errorf("pgvector 配置解析错误: %+v", pg)
	}
	if pg.AutoSchema == nil || *pg.AutoSchema {
		t.Errorf("auto_schema:false 应解析为非 nil 的 false 指针, 实际 %v", pg.AutoSchema)
	}
}
