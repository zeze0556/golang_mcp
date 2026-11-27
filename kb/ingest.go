package kb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// ingestStateFileName 去重状态落盘文件名（位于 write_dir）。
// 记录每个 doc 上次成功入库的内容 hash，进程重启后据此跳过未变更文档，
// 避免全量重调 embedding（冷启动最大的时间开销）。
const ingestStateFileName = "kb_ingest_state.json"

// ingestState 是去重状态的持久化格式。
// EmbeddedVersion 绑定当前 embedding 模型：换模型后旧 hash 全部失效，强制重建。
type ingestState struct {
	Version          int               `json:"version"`
	EmbeddingVersion string            `json:"embedding_version"`
	Hashes           map[string]string `json:"hashes"` // docID -> sha256(content)
}

const ingestStateVersion = 1

// Ingestor 写路径：监听 watch_dir，把文档分块→embedding→写入索引。
// 写路径不进 MCP 工具集（防提示注入投毒知识库），由服务进程后台驱动。
type Ingestor struct {
	store     VectorStore
	embed     *Embedder
	cfg       ingestConfig
	logger    *slog.Logger
	statePath string
	mu        sync.Mutex // 串行化 RunOnce：保护 seenHash 与 state 落盘，避免轮询重入/退出竞争

	seenHash map[string]string // docID -> 上次入库的内容 hash（内存镜像；持久化在 statePath）
}

type ingestConfig struct {
	watchDir     string
	writeDir     string
	chunkSize    int
	chunkOverlap int
}

// NewIngestor 构造写路径处理器。
// 同步部分只读一个很小的 state 文件（KB 级），不做任何文档扫描或 embedding 调用，
// 因此不会拖慢服务启动；真正的扫描由 StartPolling 放到后台。
func NewIngestor(store VectorStore, embed *Embedder, watchDir, writeDir string, chunkSize, overlap int, logger *slog.Logger) *Ingestor {
	if chunkSize <= 0 {
		chunkSize = 800
	}
	if overlap < 0 {
		overlap = 0
	}
	if overlap >= chunkSize {
		overlap = chunkSize / 4
	}
	ing := &Ingestor{
		store: store, embed: embed,
		cfg:       ingestConfig{watchDir: watchDir, writeDir: writeDir, chunkSize: chunkSize, chunkOverlap: overlap},
		logger:    logger,
		statePath: filepath.Join(writeDir, ingestStateFileName),
		seenHash:  make(map[string]string),
	}
	ing.loadState()
	return ing
}

// loadState 从 write_dir 载入去重状态。失败一律降级为空缓存（最坏是全量重建，不会漏检）。
// 失效条件（任一触发即丢弃全部缓存）：
//   - 文件缺失/损坏/版本不符
//   - embedding 版本变化（换模型后向量不可复用）
//   - 索引中存在 state 未记录的 doc，或 state 记录的 doc 在索引中不存在
//
// 最后一条是关键的一致性校验：若只信 state 不查索引，删掉索引文件后重启会
// 因为 hash 命中而跳过所有文档，导致索引永久为空。宁可多算一次 embedding。
func (ing *Ingestor) loadState() {
	raw, err := os.ReadFile(ing.statePath)
	if err != nil {
		if !os.IsNotExist(err) {
			ing.logger.Warn("kb.ingest.state_unreadable", slog.String("error", err.Error()), slog.String("hint", "按空缓存重建"))
		}
		return
	}
	var st ingestState
	if err := json.Unmarshal(raw, &st); err != nil {
		ing.logger.Warn("kb.ingest.state_corrupt", slog.String("error", err.Error()), slog.String("hint", "按空缓存重建"))
		return
	}
	if st.Version != ingestStateVersion {
		ing.logger.Info("kb.ingest.state_version_mismatch",
			slog.Int("state", st.Version), slog.Int("current", ingestStateVersion))
		return
	}
	if want := ing.embed.Version(); st.EmbeddingVersion != want {
		ing.logger.Info("kb.ingest.state_embedding_changed",
			slog.String("state", st.EmbeddingVersion), slog.String("current", want), slog.String("hint", "全量重建索引"))
		return
	}
	indexed := make(map[string]bool)
	for _, id := range ing.store.DocIDs() {
		indexed[id] = true
	}
	if len(indexed) != len(st.Hashes) {
		ing.logger.Info("kb.ingest.state_doc_mismatch",
			slog.Int("indexed", len(indexed)), slog.Int("cached", len(st.Hashes)), slog.String("hint", "全量重建索引"))
		return
	}
	for id := range st.Hashes {
		if !indexed[id] {
			ing.logger.Info("kb.ingest.state_doc_mismatch",
				slog.String("doc_id", id), slog.String("hint", "state 与索引不一致，全量重建"))
			return
		}
	}
	for id, h := range st.Hashes {
		ing.seenHash[id] = h
	}
	ing.logger.Info("kb.ingest.state_loaded", slog.Int("docs", len(ing.seenHash)), slog.String("state", ing.statePath))
}

// saveState 把去重状态原子写回磁盘。调用方必须持有 ing.mu。
// 原子写避免崩溃时留下半截 JSON——半截 JSON 会被 loadState 当作损坏而全量重建（安全侧）。
func (ing *Ingestor) saveState() {
	st := ingestState{
		Version:          ingestStateVersion,
		EmbeddingVersion: ing.embed.Version(),
		Hashes:           ing.seenHash,
	}
	raw, err := json.Marshal(st)
	if err != nil {
		ing.logger.Error("kb.ingest.state_marshal_failed", slog.String("error", err.Error()))
		return
	}
	tmp := ing.statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		ing.logger.Error("kb.ingest.state_write_failed", slog.String("error", err.Error()))
		return
	}
	if err := os.Rename(tmp, ing.statePath); err != nil {
		ing.logger.Error("kb.ingest.state_commit_failed", slog.String("error", err.Error()))
	}
}

// RunOnce 扫描 watch_dir 一次：新增/变更的文档入库，已删除的文档从索引移除。
// 返回 (处理数, 失败数)。串行执行：轮询 ticker 与手动触发不会重入。
func (ing *Ingestor) RunOnce(ctx context.Context) (int, int) {
	ing.mu.Lock()
	defer ing.mu.Unlock()
	files := make(map[string]string) // docID -> 绝对路径
	err := filepath.WalkDir(ing.cfg.watchDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !isTextFile(d.Name()) {
			return nil
		}
		rel, e := filepath.Rel(ing.cfg.watchDir, path)
		if e != nil {
			return nil
		}
		files[filepath.ToSlash(rel)] = path
		return nil
	})
	if err != nil {
		ing.logger.Error("kb.ingest.scan_failed", slog.String("error", err.Error()))
		return 0, 0
	}

	// 移除已从 watch_dir 消失的文档
	changed := false
	for _, docID := range ing.store.DocIDs() {
		if _, ok := files[docID]; !ok {
			if e := ing.store.DeleteDoc(docID); e != nil {
				ing.logger.Error("kb.ingest.delete_failed", slog.String("doc_id", docID), slog.String("error", e.Error()))
			} else {
				ing.logger.Info("kb.ingest.deleted", slog.String("doc_id", docID))
				delete(ing.seenHash, docID)
				changed = true
			}
		}
	}

	processed, failed := 0, 0
	// 按 docID 排序，保证处理顺序稳定
	ids := make([]string, 0, len(files))
	for id := range files {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, docID := range ids {
		if err := ctx.Err(); err != nil {
			break // stop()/超时取消：立刻停手，已入库的文档保持完整
		}
		didChange, err := ing.ingestOne(ctx, docID, files[docID])
		if err != nil {
			if ctx.Err() != nil {
				break // 正常 stop()/取消导致的中断：不算文档失败，也不写失败清单
			}
			failed++
			ing.writeFailure(docID, err)
			ing.logger.Error("kb.ingest.doc_failed", slog.String("doc_id", docID), slog.String("error", err.Error()))
			continue
		}
		processed++
		if didChange {
			changed = true
		}
	}
	// 只有真正有变更才落盘；全量命中缓存的空轮询不产生磁盘写。
	if changed {
		ing.saveState()
	}
	return processed, failed
}

// ingestOne 处理单个文档：读内容→hash 去重→分块→embedding→upsert。
// 返回 didChange 表示是否真正重新入库（false = hash 命中，本次没调 embedding）。
func (ing *Ingestor) ingestOne(ctx context.Context, docID, absPath string) (bool, error) {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return false, fmt.Errorf("读取文档失败: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return false, fmt.Errorf("文档为空")
	}

	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if ing.seenHash[docID] == hash {
		return false, nil // 内容未变，命中缓存直接跳过（不调用 embedding）
	}

	pieces := chunkText(content, ing.cfg.chunkSize, ing.cfg.chunkOverlap)
	if len(pieces) == 0 {
		return false, fmt.Errorf("分块结果为空")
	}
	texts := make([]string, len(pieces))
	for i, p := range pieces {
		texts[i] = p.Text
	}
	vectors, err := ing.embed.EmbedDocuments(ctx, texts)
	if err != nil {
		return false, fmt.Errorf("embedding 失败: %w", err)
	}
	if len(vectors) != len(pieces) {
		return false, fmt.Errorf("embedding 数量不符: %d vs %d", len(vectors), len(pieces))
	}

	now := time.Now()
	chunks := make([]Chunk, len(pieces))
	for i, p := range pieces {
		title := docID
		if idx := strings.LastIndex(docID, "/"); idx >= 0 {
			title = docID[idx+1:]
		}
		chunks[i] = Chunk{
			ChunkID:   fmt.Sprintf("%s#c%d", docID, i),
			DocID:     docID,
			Scope:     scopeFromDocID(docID),
			Title:     title,
			Text:      p.Text,
			Vector:    vectors[i],
			Source:    docID,
			Version:   hash[:12],
			UpdatedAt: now,
			StartLine: p.StartLine, EndLine: p.EndLine,
			CharStart: p.CharStart, CharEnd: p.CharEnd,
			Ordinal: i,
		}
	}
	if err := ing.store.UpsertDocs(docID, chunks); err != nil {
		return false, fmt.Errorf("写入索引失败: %w", err)
	}
	ing.seenHash[docID] = hash
	ing.logger.Info("kb.ingest.doc_ok", slog.String("doc_id", docID), slog.Int("chunks", len(chunks)))
	return true, nil
}

// writeFailure 把失败文档写到 write_dir 的失败清单（失败隔离，不阻塞其它文档）。
func (ing *Ingestor) writeFailure(docID string, err error) {
	line := fmt.Sprintf("%s\t%s\t%s\n", time.Now().Format(time.RFC3339), docID, err.Error())
	f, e := os.OpenFile(filepath.Join(ing.cfg.writeDir, "ingest_failures.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if e != nil {
		return
	}
	defer f.Close()
	_, _ = f.WriteString(line)
}

// StartPolling 启动后台轮询：首轮扫描与后续 ticker 全在 goroutine 内执行，
// 调用方立即返回，不被文档数量/embedding 延迟阻塞。
// interval <= 0 表示只做一次后台扫描，不重复。
// 返回的 stop 函数幂等（可多次调用），并取消正在进行的扫描/embedding 请求。
func (ing *Ingestor) StartPolling(ctx context.Context, interval time.Duration) func() {
	scanCtx, cancel := context.WithCancel(ctx)
	var once sync.Once
	stop := func() { once.Do(cancel) }

	go func() {
		ing.RunOnce(scanCtx) // 后台首轮扫描：不阻塞 NewService / 服务启动
		if interval <= 0 {
			return
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ing.RunOnce(scanCtx)
			case <-scanCtx.Done():
				return
			}
		}
	}()
	return stop
}

// textPiece 分块结果及其在原文中的位置信息。
type textPiece struct {
	Text      string
	StartLine int
	EndLine   int
	CharStart int
	CharEnd   int
}

// chunkText 按字符（rune）分块并记录 1-based 行号。overlap 在字符级重叠。
func chunkText(content string, size, overlap int) []textPiece {
	runes := []rune(content)
	total := len(runes)
	if total == 0 {
		return nil
	}
	// 预计算每个 rune 所在行号（1-based）
	lineAt := make([]int, total)
	line := 1
	for i, r := range runes {
		lineAt[i] = line
		if r == '\n' {
			line++
		}
	}
	step := size - overlap
	if step <= 0 {
		step = size
	}
	var out []textPiece
	for start := 0; start < total; start += step {
		end := start + size
		if end > total {
			end = total
		}
		seg := runes[start:end]
		p := textPiece{
			Text:      strings.TrimSpace(string(seg)),
			StartLine: lineAt[start],
			EndLine:   lineAt[end-1],
			CharStart: start,
			CharEnd:   end,
		}
		if p.Text != "" && utf8.ValidString(p.Text) {
			out = append(out, p)
		}
		if end == total {
			break
		}
	}
	return out
}

// scopeFromDocID 从文档相对路径的第一级目录推导 scope（集合级 ACL 的最小单位）。
// 例 "billing/refund.md" -> "billing"；"notes.md" -> "default"。
func scopeFromDocID(docID string) string {
	parts := strings.Split(filepath.ToSlash(docID), "/")
	if len(parts) > 1 && parts[0] != "" {
		return parts[0]
	}
	return "default"
}

// isTextFile 只吃常见文本扩展名，跳过二进制/图片等。
func isTextFile(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".md", ".txt", ".text", ".rst", ".csv", ".json", ".yaml", ".yml", ".log", ".html", ".htm", ".xml", ".go", ".py", ".js", ".ts", ".java", ".c", ".cpp", ".h", ".sh", ".sql", ".ini", ".toml", ".conf", ".properties", "":
		return true
	default:
		return false
	}
}
