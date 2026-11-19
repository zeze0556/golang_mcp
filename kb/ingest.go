package kb

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Ingestor 写路径：监听 watch_dir，把文档分块→embedding→写入索引。
// 写路径不进 MCP 工具集（防提示注入投毒知识库），由服务进程后台驱动。
type Ingestor struct {
	store    VectorStore
	embed    *Embedder
	cfg      ingestConfig
	logger   *slog.Logger
	seenHash map[string]string // docID -> 上次入库的内容 hash（去重，避免重复调用 embedding）
}

type ingestConfig struct {
	watchDir     string
	writeDir     string
	chunkSize    int
	chunkOverlap int
}

// NewIngestor 构造写路径处理器。
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
	return &Ingestor{
		store: store, embed: embed,
		cfg:    ingestConfig{watchDir: watchDir, writeDir: writeDir, chunkSize: chunkSize, chunkOverlap: overlap},
		logger: logger, seenHash: make(map[string]string),
	}
}

// RunOnce 扫描 watch_dir 一次：新增/变更的文档入库，已删除的文档从索引移除。
// 返回 (处理数, 失败数)。
func (ing *Ingestor) RunOnce(ctx context.Context) (int, int) {
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
	for _, docID := range ing.store.DocIDs() {
		if _, ok := files[docID]; !ok {
			if e := ing.store.DeleteDoc(docID); e != nil {
				ing.logger.Error("kb.ingest.delete_failed", slog.String("doc_id", docID), slog.String("error", e.Error()))
			} else {
				ing.logger.Info("kb.ingest.deleted", slog.String("doc_id", docID))
				delete(ing.seenHash, docID)
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
		if err := ing.ingestOne(ctx, docID, files[docID]); err != nil {
			failed++
			ing.writeFailure(docID, err)
			ing.logger.Error("kb.ingest.doc_failed", slog.String("doc_id", docID), slog.String("error", err.Error()))
			continue
		}
		processed++
	}
	return processed, failed
}

// ingestOne 处理单个文档：读内容→hash 去重→分块→embedding→upsert。
func (ing *Ingestor) ingestOne(ctx context.Context, docID, absPath string) error {
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("读取文档失败: %w", err)
	}
	content := strings.TrimSpace(string(data))
	if content == "" {
		return fmt.Errorf("文档为空")
	}

	sum := sha256.Sum256([]byte(content))
	hash := hex.EncodeToString(sum[:])
	if ing.seenHash[docID] == hash {
		return nil // 内容未变，跳过（失败隔离后重试靠删除 seenHash 或进程重启重建）
	}

	pieces := chunkText(content, ing.cfg.chunkSize, ing.cfg.chunkOverlap)
	if len(pieces) == 0 {
		return fmt.Errorf("分块结果为空")
	}
	texts := make([]string, len(pieces))
	for i, p := range pieces {
		texts[i] = p.Text
	}
	vectors, err := ing.embed.EmbedDocuments(ctx, texts)
	if err != nil {
		return fmt.Errorf("embedding 失败: %w", err)
	}
	if len(vectors) != len(pieces) {
		return fmt.Errorf("embedding 数量不符: %d vs %d", len(vectors), len(pieces))
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
		return fmt.Errorf("写入索引失败: %w", err)
	}
	ing.seenHash[docID] = hash
	ing.logger.Info("kb.ingest.doc_ok", slog.String("doc_id", docID), slog.Int("chunks", len(chunks)))
	return nil
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

// StartPolling 启动后台轮询（间隔 <=0 时只扫描一次）。返回 stop 函数。
func (ing *Ingestor) StartPolling(ctx context.Context, interval time.Duration) func() {
	ing.RunOnce(ctx) // 启动即扫一次
	if interval <= 0 {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				ing.RunOnce(ctx)
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() { close(stop) }
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
