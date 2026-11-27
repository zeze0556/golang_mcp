package kb

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// LocalStore 零依赖本地向量存储：全部 chunk 持久化到一个 JSON 文件，
// 启动时载入内存，检索时算余弦。适合私有小库（几万 chunk 顶天）。
// 崩溃一致性：每次 Upsert 先写临时文件再原子 rename；检索只读内存快照。
type LocalStore struct {
	mu     sync.RWMutex
	path   string           // 索引文件路径
	chunks map[string]Chunk // chunk_id -> chunk
	embVer string
}

type localIndex struct {
	EmbeddingVersion string  `json:"embedding_version"`
	Chunks           []Chunk `json:"chunks"`
}

// NewLocalStore 载入索引文件（不存在则创建空索引）。
func NewLocalStore(indexPath string) (*LocalStore, error) {
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o755); err != nil {
		return nil, fmt.Errorf("创建索引目录失败: %w", err)
	}
	s := &LocalStore{path: indexPath, chunks: make(map[string]Chunk)}
	if _, err := os.Stat(indexPath); err == nil {
		if err := s.load(); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *LocalStore) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return fmt.Errorf("读取索引失败: %w", err)
	}
	var idx localIndex
	if err := json.Unmarshal(raw, &idx); err != nil {
		return fmt.Errorf("解析索引失败（embedding/格式变更需重建）: %w", err)
	}
	for _, c := range idx.Chunks {
		s.chunks[c.ChunkID] = c
	}
	s.embVer = idx.EmbeddingVersion
	return nil
}

// persist 把内存快照原子写回磁盘（持写锁调用）。
func (s *LocalStore) persist() error {
	idx := localIndex{EmbeddingVersion: s.embVer, Chunks: make([]Chunk, 0, len(s.chunks))}
	for _, c := range s.chunks {
		idx.Chunks = append(idx.Chunks, c)
	}
	raw, err := json.Marshal(idx)
	if err != nil {
		return fmt.Errorf("序列化索引失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("写临时索引失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		return fmt.Errorf("提交索引失败: %w", err)
	}
	return nil
}

// UpsertDocs 用新分片整体替换同一 doc_id 的旧分片（保证幂等、无残留）。
func (s *LocalStore) UpsertDocs(docID string, chunks []Chunk) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.chunks {
		if c.DocID == docID {
			delete(s.chunks, id)
		}
	}
	for _, c := range chunks {
		s.chunks[c.ChunkID] = c
	}
	return s.persist()
}

// DeleteDoc 删除一个文档的全部分片。
func (s *LocalStore) DeleteDoc(docID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, c := range s.chunks {
		if c.DocID == docID {
			delete(s.chunks, id)
		}
	}
	return s.persist()
}

// Search 在 scope 集合内做全量余弦召回。
func (s *LocalStore) Search(qVector []float32, p SearchParams) ([]Hit, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	scopeSet := make(map[string]bool, len(p.Scopes))
	for _, sc := range p.Scopes {
		scopeSet[sc] = true
	}
	hits := make([]Hit, 0, len(s.chunks))
	for _, c := range s.chunks {
		if len(p.Scopes) > 0 && !scopeSet[c.Scope] {
			continue // 召回阶段即过滤越权 scope（不先召回再裁）
		}
		score := Cosine(qVector, c.Vector)
		if score < p.MinScore {
			continue
		}
		hits = append(hits, c.ToHit(score))
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	return hits, nil
}

// GetByChunkID 精确取分片。
func (s *LocalStore) GetByChunkID(chunkID string) (*Chunk, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.chunks[chunkID]
	if !ok {
		return nil, false
	}
	cp := c
	return &cp, true
}

// GetNeighbors 取同 doc 内 ordinal 相邻的 window 个分片（含自身）。
func (s *LocalStore) GetNeighbors(chunkID string, window int) ([]Chunk, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cur, ok := s.chunks[chunkID]
	if !ok {
		return nil, fmt.Errorf("chunk_id 不存在")
	}
	if window < 0 {
		window = 0
	}
	if window > 3 {
		window = 3 // 上限 3
	}
	// 收齐同文档全部分片并按 ordinal 排序
	peers := make([]Chunk, 0)
	for _, c := range s.chunks {
		if c.DocID == cur.DocID {
			peers = append(peers, c)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].Ordinal < peers[j].Ordinal })
	lo, hi := cur.Ordinal-window, cur.Ordinal+window
	out := make([]Chunk, 0, window*2+1)
	for _, c := range peers {
		if c.Ordinal >= lo && c.Ordinal <= hi {
			out = append(out, c)
		}
	}
	return out, nil
}

// ListScopes 汇总各 scope 的文档数与最近更新时间。
func (s *LocalStore) ListScopes() []ScopeInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type agg struct {
		docs int
		upd  time.Time
	}
	m := make(map[string]*agg)
	for _, c := range s.chunks {
		a := m[c.Scope]
		if a == nil {
			a = &agg{}
			m[c.Scope] = a
		}
		a.docs++
		if c.UpdatedAt.After(a.upd) {
			a.upd = c.UpdatedAt
		}
	}
	out := make([]ScopeInfo, 0, len(m))
	for sc, a := range m {
		out = append(out, ScopeInfo{ID: sc, Name: sc, DocCount: a.docs, UpdatedAt: a.upd})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (s *LocalStore) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.chunks)
}

// DocIDs 返回全部 doc_id（去重）。
func (s *LocalStore) DocIDs() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	set := make(map[string]bool)
	for _, c := range s.chunks {
		set[c.DocID] = true
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (s *LocalStore) EmbeddingVersion() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embVer
}

func (s *LocalStore) SetEmbeddingVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embVer = v
	// 必须落盘：换 embedding 模型后服务会重写版本，若只改内存，
	// 进程重启时版本回退到旧值，tools.go 的版本校验会永久拒绝查询。
	if err := s.persist(); err != nil {
		// SetEmbeddingVersion 无返回值，错误通过下次查询的版本校验兜底暴露。
		// 用 stderr 直出避免引入 logger 依赖（此包其余持久错误已走日志，但此处签名是既有接口）。
		fmt.Fprintf(os.Stderr, "kb.store: 持久化 embedding 版本失败: %v\n", err)
	}
}
