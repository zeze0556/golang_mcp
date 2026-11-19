package kb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql 的 pgx 驱动注册名 "pgx"
	pgvector "github.com/pgvector/pgvector-go"
)

const (
	defaultPGTable    = "kb_chunks"
	defaultPGMaxConns = 4
	pgQueryTimeout    = 30 * time.Second

	// pgChunkColumns 是 Chunk 全字段的固定列序，scanChunkFrom 依赖此顺序，改动须同步。
	pgChunkColumns = `chunk_id, doc_id, scope, title, text, embedding, source, source_url, version, updated_at, start_line, end_line, char_start, char_end, ordinal`
)

// pgIdentRe 是标识符白名单的第二道防线：配置层已校验一次，这里防止绕过 config 直接构造 store。
// 表名要拼进 DDL，而占位符不能用于标识符——白名单是唯一安全的做法。
var pgIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// PGVectorStore 用 PostgreSQL + pgvector 扩展作为向量存储。
// 与 LocalStore 实现同一 VectorStore 接口，上层工具/检索逻辑完全不感知后端差异。
//
// score 语义对齐：pgvector 的 `<=>` 是余弦距离 [0,2]，LocalStore.Cosine 是相似度 [-1,1]，
// 故查询一律用 `1 - (embedding <=> $1::vector)` 作为 score——归一化向量下等价于余弦相似度。
// HNSW 索引也须用 vector_cosine_ops 与该距离算子配套。
type PGVectorStore struct {
	db        *sql.DB
	table     string
	metaTable string
	dim       int
	timeout   time.Duration
}

// NewPGVectorStore 连接 PG、按需幂等建表，返回可用 store。
// dsn 支持 ${ENV_VAR}；dim 直接取 embedding.dim（单一真相源）；table 为空时用 kb_chunks。
// autoSchema=false 时跳过建表（要求库/扩展/表已由运维准备好）。
func NewPGVectorStore(dsn string, dim int, table string, maxConns int, autoSchema bool) (*PGVectorStore, error) {
	dsn = strings.TrimSpace(dsn)
	if strings.HasPrefix(dsn, "${") && strings.HasSuffix(dsn, "}") {
		env := dsn[2 : len(dsn)-1]
		dsn = os.Getenv(env)
		if dsn == "" {
			return nil, fmt.Errorf("环境变量 %s 未设置或为空", env)
		}
	}
	if dsn == "" {
		return nil, fmt.Errorf("pgvector dsn 不能为空")
	}
	if dim <= 0 {
		return nil, fmt.Errorf("pgvector 向量维度必须 > 0")
	}
	if table == "" {
		table = defaultPGTable
	}
	if !pgIdentRe.MatchString(table) {
		return nil, fmt.Errorf("pgvector 表名非法（只允许字母/数字/下划线且不以数字开头）: %q", table)
	}
	if maxConns <= 0 {
		maxConns = defaultPGMaxConns
	}

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开 pgvector 连接失败: %w", err)
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), pgQueryTimeout)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连接 pgvector 失败: %w", err)
	}

	s := newPGVectorStoreWithDB(db, table, dim)
	if autoSchema {
		if err := s.initSchema(ctx); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	return s, nil
}

// newPGVectorStoreWithDB 从已建立的 *sql.DB 构造 store。生产路径与 sqlmock 测试共用，
// 使 SQL 语义无需真实 PG 即可验证（避免为测试污染公开的构造函数签名）。
func newPGVectorStoreWithDB(db *sql.DB, table string, dim int) *PGVectorStore {
	return &PGVectorStore{
		db: db, table: table, metaTable: table + "_meta",
		dim: dim, timeout: pgQueryTimeout,
	}
}

// initSchema 幂等建扩展/表/索引。HNSW 索引失败仅降级为精确扫描（不阻断服务）；
// 主表/扩展失败则致命——否则后续所有查询都不可用，不如启动即失败。
func (s *PGVectorStore) initSchema(ctx context.Context) error {
	stmts := []string{
		`CREATE EXTENSION IF NOT EXISTS vector`,
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
			chunk_id   TEXT PRIMARY KEY,
			doc_id     TEXT NOT NULL,
			scope      TEXT NOT NULL,
			title      TEXT NOT NULL DEFAULT '',
			text       TEXT NOT NULL,
			embedding  vector(%d) NOT NULL,
			source     TEXT NOT NULL DEFAULT '',
			source_url TEXT NOT NULL DEFAULT '',
			version    TEXT NOT NULL DEFAULT '',
			updated_at TIMESTAMPTZ NOT NULL,
			start_line INTEGER NOT NULL DEFAULT 0,
			end_line   INTEGER NOT NULL DEFAULT 0,
			char_start INTEGER NOT NULL DEFAULT 0,
			char_end   INTEGER NOT NULL DEFAULT 0,
			ordinal    INTEGER NOT NULL DEFAULT 0
		)`, s.table, s.dim),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_doc_idx ON %s (doc_id)`, s.table, s.table),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_scope_idx ON %s (scope)`, s.table, s.table),
		fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (key TEXT PRIMARY KEY, value TEXT NOT NULL)`, s.metaTable),
	}
	for _, q := range stmts {
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("初始化 pgvector schema 失败（表=%s）: %w", s.table, err)
		}
	}
	// HNSW 仅支持 dim<=2000；索引创建失败（版本/权限）不影响正确性，退化为全表精确扫描。
	if s.dim <= 2000 {
		q := fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %s_emb_hnsw ON %s USING hnsw (embedding vector_cosine_ops)`, s.table, s.table)
		if _, err := s.db.ExecContext(ctx, q); err != nil {
			// 刻意不返回错误：这是性能优化，不是功能前提。
			_ = err
		}
	}
	return nil
}

// UpsertDocs 在同一事务里整体替换 doc_id 的全部分片，保证与 LocalStore 一致的"原子替换"语义。
func (s *PGVectorStore) UpsertDocs(docID string, chunks []Chunk) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // 提交后 Rollback 是无害的 no-op

	if _, err := tx.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE doc_id = $1`, s.table), docID); err != nil {
		return fmt.Errorf("清理旧分片失败: %w", err)
	}
	if len(chunks) == 0 {
		return tx.Commit()
	}

	// 第 6 个参数是 embedding：pgvector.Vector 经 driver.Valuer 输出数组字面量字符串，
	// 显式 ::vector cast 保证 pgx stdlib 下 text→vector 一定成立（不依赖驱动的类型推断）。
	insert := fmt.Sprintf(`INSERT INTO %s (%s) VALUES
		($1,$2,$3,$4,$5,$6::vector,$7,$8,$9,$10,$11,$12,$13,$14,$15)`, s.table, pgChunkColumns)
	stmt, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, c := range chunks {
		_, err := stmt.ExecContext(ctx,
			c.ChunkID, c.DocID, c.Scope, c.Title, c.Text,
			pgvector.NewVector(c.Vector),
			c.Source, c.SourceURL, c.Version, c.UpdatedAt,
			c.StartLine, c.EndLine, c.CharStart, c.CharEnd, c.Ordinal,
		)
		if err != nil {
			return fmt.Errorf("写入分片 %s 失败: %w", c.ChunkID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务失败: %w", err)
	}
	return nil
}

// DeleteDoc 删除一个文档的全部分片。
func (s *PGVectorStore) DeleteDoc(docID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	_, err := s.db.ExecContext(ctx, fmt.Sprintf(`DELETE FROM %s WHERE doc_id = $1`, s.table), docID)
	return err
}

// Search 在 SQL 层完成 scope 过滤、min_score 门槛与向量排序，避免把越权数据拉回内存再裁。
// Scopes 为空（open 模式）时传 NULL，不过滤；非空时用 scope = ANY($2)。
func (s *PGVectorStore) Search(qVector []float32, p SearchParams) ([]Hit, error) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	var scopeArg any // nil → $2::text[] IS NULL → 不过滤
	if len(p.Scopes) > 0 {
		scopeArg = pgTextArray(p.Scopes)
	}
	limit := p.TopK
	if limit <= 0 {
		limit = 1000
	}
	q := fmt.Sprintf(`SELECT %s, 1 - (embedding <=> $1::vector) AS score
		FROM %s
		WHERE ($2::text[] IS NULL OR scope = ANY($2::text[]))
		  AND 1 - (embedding <=> $1::vector) >= $3
		ORDER BY embedding <=> $1::vector ASC
		LIMIT $4`, pgChunkColumns, s.table)

	rows, err := s.db.QueryContext(ctx, q, pgvector.NewVector(qVector), scopeArg, p.MinScore, limit)
	if err != nil {
		return nil, fmt.Errorf("pgvector 检索失败: %w", err)
	}
	defer rows.Close()

	hits := make([]Hit, 0, limit)
	for rows.Next() {
		var score float64
		c, err := scanChunkFrom(rows, &score)
		if err != nil {
			return nil, fmt.Errorf("读取检索结果失败: %w", err)
		}
		hits = append(hits, c.ToHit(score))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历检索结果失败: %w", err)
	}
	return hits, nil
}

// GetByChunkID 按主键精确取一个分片。
func (s *PGVectorStore) GetByChunkID(chunkID string) (*Chunk, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	q := fmt.Sprintf(`SELECT %s FROM %s WHERE chunk_id = $1`, pgChunkColumns, s.table)
	c, err := scanChunkFrom(s.db.QueryRowContext(ctx, q, chunkID), nil)
	if err != nil {
		return nil, false
	}
	return &c, true
}

// GetNeighbors 取同 doc 内 ordinal 相邻的 window 个分片（含自身），上限 3 与 LocalStore 一致。
func (s *PGVectorStore) GetNeighbors(chunkID string, window int) ([]Chunk, error) {
	if window < 0 {
		window = 0
	}
	if window > 3 {
		window = 3
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()

	var docID string
	var ordinal int
	q := fmt.Sprintf(`SELECT doc_id, ordinal FROM %s WHERE chunk_id = $1`, s.table)
	if err := s.db.QueryRowContext(ctx, q, chunkID).Scan(&docID, &ordinal); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("chunk_id 不存在")
		}
		return nil, err
	}

	q = fmt.Sprintf(`SELECT %s FROM %s WHERE doc_id = $1 AND ordinal BETWEEN $2 AND $3 ORDER BY ordinal ASC`,
		pgChunkColumns, s.table)
	rows, err := s.db.QueryContext(ctx, q, docID, ordinal-window, ordinal+window)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Chunk, 0, 2*window+1)
	for rows.Next() {
		c, err := scanChunkFrom(rows, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListScopes 汇总各 scope 的文档数与最近更新时间。
func (s *PGVectorStore) ListScopes() []ScopeInfo {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	q := fmt.Sprintf(`SELECT scope, COUNT(DISTINCT doc_id), MAX(updated_at) FROM %s GROUP BY scope ORDER BY scope ASC`, s.table)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]ScopeInfo, 0)
	for rows.Next() {
		var sc ScopeInfo
		if err := rows.Scan(&sc.ID, &sc.DocCount, &sc.UpdatedAt); err != nil {
			return nil
		}
		sc.Name = sc.ID
		out = append(out, sc)
	}
	return out
}

func (s *PGVectorStore) Count() int {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	var n int
	q := fmt.Sprintf(`SELECT COUNT(*) FROM %s`, s.table)
	if err := s.db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		return 0
	}
	return n
}

// DocIDs 返回全部 doc_id（写路径用于判断哪些文档已从 watch_dir 消失）。
func (s *PGVectorStore) DocIDs() []string {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	q := fmt.Sprintf(`SELECT DISTINCT doc_id FROM %s ORDER BY doc_id ASC`, s.table)
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil
		}
		out = append(out, id)
	}
	return out
}

// EmbeddingVersion 从 meta 表读取绑定的模型标识；meta 缺失/查询失败视为未绑定（触发写入）。
func (s *PGVectorStore) EmbeddingVersion() string {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	var v string
	q := fmt.Sprintf(`SELECT value FROM %s WHERE key = 'embedding_version'`, s.metaTable)
	if err := s.db.QueryRowContext(ctx, q).Scan(&v); err != nil {
		return ""
	}
	return v
}

func (s *PGVectorStore) SetEmbeddingVersion(v string) {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	q := fmt.Sprintf(`INSERT INTO %s (key, value) VALUES ('embedding_version', $1)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, s.metaTable)
	_, _ = s.db.ExecContext(ctx, q, v) // 绑定失败在查询时由版本校验兜底
}

// Close 释放连接池。由 Service.Close 通过可选接口调用。
func (s *PGVectorStore) Close() error { return s.db.Close() }

// pgTextArray 把 []string 编码成 PostgreSQL 数组字面量（如 {"hr","billing"}），
// 实现标准 driver.Valuer。这样 scope 参数走 database/sql 标准路径，不依赖 pgx 特有的
// []string→text[] 转换——既对真实 pgx 有效，也能被 sqlmock 等标准驱动测试。
// SQL 侧用 $2::text[] 显式 cast 完成解析。
type pgTextArray []string

func (a pgTextArray) Value() (driver.Value, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, s := range a {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s))
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String(), nil
}

// rowScanner 让 scanChunkFrom 同时接受 *sql.Row 与 *sql.Rows。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanChunkFrom 按 pgChunkColumns 的列序扫描；score 非 nil 时额外扫描末尾的 score 列。
func scanChunkFrom(r rowScanner, score *float64) (Chunk, error) {
	var c Chunk
	var vec pgvector.Vector
	dests := []any{
		&c.ChunkID, &c.DocID, &c.Scope, &c.Title, &c.Text, &vec,
		&c.Source, &c.SourceURL, &c.Version, &c.UpdatedAt,
		&c.StartLine, &c.EndLine, &c.CharStart, &c.CharEnd, &c.Ordinal,
	}
	if score != nil {
		dests = append(dests, score)
	}
	if err := r.Scan(dests...); err != nil {
		return Chunk{}, err
	}
	c.Vector = vec.Slice()
	return c, nil
}
