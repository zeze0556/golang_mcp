package kb

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	sshmcp "github.com/zeze0556/golang_mcp/sshmcp"
)

// Service 是知识库子系统的运行时装配体。由 main.go 依据配置构造。
type Service struct {
	embed         *Embedder
	store         VectorStore
	ingestor      *Ingestor
	resolver      *ScopeResolver
	logger        *slog.Logger
	topKCap       int
	maxCharBudget int
	stopIngest    func()
}

// NewService 从 sshmcp 配置构造 KB 服务。
// 副作用：创建索引目录、绑定 embedding 版本、启动写路径轮询。
// 调用方负责在进程退出时调用 Close。
func NewService(cfg sshmcp.KnowledgeBaseConfig, logger *slog.Logger) (*Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// embedding
	emb := NewEmbedder(
		cfg.Embedding.Endpoint, cfg.Embedding.Model, cfg.Embedding.APIKey,
		cfg.Embedding.Dim, cfg.Embedding.QueryPrefix, cfg.Embedding.TimeoutSec,
	)
	// store：按 backend 装配。两种后端实现同一 VectorStore 接口，上层无感。
	backend := strings.ToLower(strings.TrimSpace(cfg.Backend))
	var store VectorStore
	storeDesc := ""
	if backend == "pgvector" {
		pgCfg := cfg.PGVector
		autoSchema := pgCfg.AutoSchema == nil || *pgCfg.AutoSchema // 未设置默认建表
		pgStore, err := NewPGVectorStore(
			pgCfg.DSN, cfg.Embedding.Dim, pgCfg.Table, pgCfg.MaxConns, autoSchema,
		)
		if err != nil {
			return nil, err
		}
		store = pgStore
		storeDesc = fmt.Sprintf("pgvector://%s", pgCfg.Table)
		if pgCfg.Table == "" {
			storeDesc = "pgvector://" + defaultPGTable
		}
	} else {
		indexFile := cfg.IndexFile
		if indexFile == "" {
			indexFile = "kb_index.json"
		}
		indexPath := filepath.Join(cfg.WriteDir, indexFile)
		localStore, err := NewLocalStore(indexPath)
		if err != nil {
			return nil, err
		}
		store = localStore
		storeDesc = indexPath
	}

	// embedding 版本绑定。两种后端通用。
	// 版本不匹配 = 换过模型：旧向量不可复用。重写为新版本并让写路径全量重建：
	// Ingestor.loadState 检测到版本变化会丢弃 hash 缓存，后台首轮扫描即重建全部文档。
	// 重建完成前 tools.go 不会拒绝查询（版本已对齐），只是召回旧向量——这是可接受的过渡态。
	ver := emb.Version()
	if cur := store.EmbeddingVersion(); cur != ver {
		if cur != "" {
			logger.Warn("kb.embed_version_mismatch",
				"index", cur, "current", ver, "hint", "后台将按当前模型全量重建索引")
		}
		store.SetEmbeddingVersion(ver)
	}

	// ACL resolver：把 sshmcp 配置类型转成 kb 中性类型
	pairs := make([]TokenScope, 0, len(cfg.TokenScopes))
	for _, ts := range cfg.TokenScopes {
		pairs = append(pairs, TokenScope{Token: ts.Token, Scopes: ts.Scopes})
	}
	resolver := NewScopeResolver(pairs)

	topK := cfg.TopKCap
	if topK <= 0 {
		topK = 20
	}
	if topK > 20 {
		topK = 20 // 硬上限
	}
	budget := cfg.MaxCharBudget
	if budget <= 0 {
		budget = 12000
	}
	chunkSize, overlap := cfg.ChunkSize, cfg.ChunkOverlap

	svc := &Service{
		embed: emb, store: store, resolver: resolver, logger: logger,
		topKCap: topK, maxCharBudget: budget,
	}

	// 写路径
	ing := NewIngestor(store, emb, cfg.WatchDir, cfg.WriteDir, chunkSize, overlap, logger)
	svc.ingestor = ing
	interval := time.Duration(cfg.PollSeconds) * time.Second
	if cfg.PollSeconds < 0 {
		interval = 0
	}
	svc.stopIngest = ing.StartPolling(context.Background(), interval)

	logger.Info("kb.service.ready",
		"backend", backend, "store", storeDesc, "watch_dir", cfg.WatchDir,
		"embedding_version", ver, "chunks", store.Count())
	return svc, nil
}

// storeReady 报告索引是否可用。
func (s *Service) storeReady() bool { return s.store != nil }

// storeCloser 是可选接口：只有持有外部资源（pgx 连接池）的后端需要实现；local 后端不实现。
type storeCloser interface {
	Close() error
}

// Close 停止写路径轮询，并释放后端持有的外部资源（如 pgvector 连接池）。
func (s *Service) Close() {
	if s.stopIngest != nil {
		s.stopIngest()
	}
	if c, ok := s.store.(storeCloser); ok {
		if err := c.Close(); err != nil {
			s.logger.Warn("kb.store.close_failed", "error", err.Error())
		}
	}
}

// Ingestor 暴露写路径处理器（供管理面手动触发一次扫描，可选）。
func (s *Service) Ingestor() *Ingestor { return s.ingestor }

// Store 暴露存储（供管理面查看统计，可选）。
func (s *Service) Store() VectorStore { return s.store }
