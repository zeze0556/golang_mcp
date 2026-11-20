// Package kb 实现知识库子系统：写路径（目录监听→分块→embedding→索引）与读工具（kb.search/fetch/cite/list_scopes）。
//
// 设计遵循 kb-mcp-contract-draft.md v0.2：检索能力收敛成服务端纯函数 search(query, acl) → chunks[]，
// acl 由服务端从 token 推导、绝不进入工具 schema。retrieved text 一律视为不可信数据。
package kb

import "time"

// Chunk 索引中的一个文本分片（向量与元数据同体）。
// StartLine/EndLine 是写路径分块时记录的 1-based 行号，供 kb.cite 溯源；缺则为 0。
type Chunk struct {
	ChunkID   string    `json:"chunk_id"`
	DocID     string    `json:"doc_id"`
	Scope     string    `json:"scope"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text"`
	Vector    []float32 `json:"vector"`
	Source    string    `json:"source,omitempty"`     // 源文件相对路径（仅存相对名，不暴露本机绝对路径）
	SourceURL string    `json:"source_url,omitempty"` // 可选的对外引用链接
	Version   string    `json:"version"`              // 文档版本/内容 hash
	UpdatedAt time.Time `json:"updated_at"`
	StartLine int       `json:"start_line,omitempty"`
	EndLine   int       `json:"end_line,omitempty"`
	CharStart int       `json:"char_start,omitempty"` // 在原文中的字符偏移，fetch 相邻片用
	CharEnd   int       `json:"char_end,omitempty"`
	Ordinal   int       `json:"ordinal"` // 同文档内的分片序号，fetch 相邻片用
}

// Hit 是检索结果的一条（不含向量，避免回传膨胀）。
type Hit struct {
	ChunkID   string    `json:"chunk_id"`
	DocID     string    `json:"doc_id"`
	Scope     string    `json:"scope"`
	Title     string    `json:"title,omitempty"`
	Text      string    `json:"text"`
	Score     float64   `json:"score"`
	SourceURL string    `json:"source_url,omitempty"`
	Version   string    `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
	StartLine int       `json:"start_line,omitempty"`
	EndLine   int       `json:"end_line,omitempty"`
}

// ToHit 去掉向量，转成对外结果。
func (c *Chunk) ToHit(score float64) Hit {
	return Hit{
		ChunkID: c.ChunkID, DocID: c.DocID, Scope: c.Scope, Title: c.Title,
		Text: c.Text, Score: score, SourceURL: c.SourceURL, Version: c.Version,
		UpdatedAt: c.UpdatedAt, StartLine: c.StartLine, EndLine: c.EndLine,
	}
}

// SearchParams 服务端检索入参（acl 由调用方从 token 推导后传入，客户端不可控）。
type SearchParams struct {
	Query    string
	TopK     int
	Scopes   []string // 允许访问的 scope 集合；空表示不过滤（仅在 token_scopes 未配置时）
	MinScore float64
}

// VectorStore 向量存储抽象。上层检索逻辑只依赖此接口，切换后端只换实现。
// 注意：不同后端的数据不互通，切换 backend 必须重建索引（写路径重新消费 watch_dir）。
type VectorStore interface {
	// UpsertDocs 用同一 doc 的新分片整体替换旧分片（按 doc_id）。
	UpsertDocs(docID string, chunks []Chunk) error
	// Search 在给定 scope 集合内做向量召回，返回按 score 降序的命中。
	Search(qVector []float32, p SearchParams) ([]Hit, error)
	// GetByChunkID 按 chunk_id 精确取一个分片。
	GetByChunkID(chunkID string) (*Chunk, bool)
	// GetNeighbors 取同一 doc 内 ordinal 相邻的若干分片（用于 kb.fetch window）。
	GetNeighbors(chunkID string, window int) ([]Chunk, error)
	// ListScopes 返回各 scope 的文档数与最近更新时间。
	ListScopes() []ScopeInfo
	// Count 返回分片总数。
	Count() int
	// DocIDs 返回全部 doc_id（写路径用于判断哪些文档已从 watch_dir 消失）。
	DocIDs() []string
	// DeleteDoc 删除一个文档的全部分片。
	DeleteDoc(docID string) error
	// EmbeddingVersion 返回本索引绑定的 embedding 模型标识（只读回显）。
	EmbeddingVersion() string
	// SetEmbeddingVersion 绑定 embedding 模型标识（建库时写入；不匹配则拒绝查询）。
	SetEmbeddingVersion(v string)
}

// ScopeInfo kb.list_scopes 的单条结果。
type ScopeInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	DocCount  int       `json:"doc_count"`
	UpdatedAt time.Time `json:"updated_at"`
}
