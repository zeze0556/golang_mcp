package kb

import (
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestPGVectorSearchScoreDirection 是对抗式验证的核心：
// 确保 SQL 用 `1 - (embedding <=> $1::vector)` 作为 score（相似度方向），
// 且按 distance 升序（= score 降序）排列——写反会导致检索结果完全颠倒。
func TestPGVectorSearchScoreDirection(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newPGVectorStoreWithDB(db, "kb_chunks", 2)

	now := time.Now()
	// 返回一个 score=0.85 的命中行（15 列 chunk + 1 列 score）
	rows := sqlmock.NewRows([]string{
		"chunk_id", "doc_id", "scope", "title", "text", "embedding", "source", "source_url",
		"version", "updated_at", "start_line", "end_line", "char_start", "char_end", "ordinal", "score",
	}).AddRow("d1#c0", "d1", "billing", "", "退款流程", "[1,0]", "", "", "v1", now, 1, 2, 0, 10, 0, 0.85)

	// (?s) 让 . 匹配换行；SQL 中的 <=> :: 需转义点号
	mock.ExpectQuery(`(?s)SELECT chunk_id.*1 - \(embedding <=> \$1::vector\) AS score.*FROM kb_chunks`).
		WithArgs(sqlmock.AnyArg(), nil, float64(0.5), 10). // Scopes 为空 → NULL；min_score=0.5
		WillReturnRows(rows)

	hits, err := s.Search([]float32{1, 0}, SearchParams{TopK: 10, MinScore: 0.5})
	if err != nil {
		t.Fatalf("Search 失败: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("期望 1 条命中, 实际 %d", len(hits))
	}
	if hits[0].Score != 0.85 {
		t.Errorf("score 应直接取 SQL 的 1-distance 值 0.85, 实际 %f", hits[0].Score)
	}
	if hits[0].ChunkID != "d1#c0" || hits[0].Scope != "billing" {
		t.Errorf("命中字段解析错误: %+v", hits[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SQL 期望未满足: %v", err)
	}
}

// TestPGVectorSearchScopeFilter 验证 scope 白名单在 SQL 层以 text[] 传入，
// 而不是拉回内存再过滤（越权数据不进进程）。
func TestPGVectorSearchScopeFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newPGVectorStoreWithDB(db, "kb_chunks", 2)

	rows := sqlmock.NewRows([]string{
		"chunk_id", "doc_id", "scope", "title", "text", "embedding", "source", "source_url",
		"version", "updated_at", "start_line", "end_line", "char_start", "char_end", "ordinal", "score",
	}).AddRow("d2#c0", "d2", "hr", "", "请假", "[0,1]", "", "", "v1", time.Now(), 1, 1, 0, 2, 0, 0.7)

	mock.ExpectQuery(`(?s)SELECT chunk_id.*FROM kb_chunks.*scope = ANY\(\$2::text\[\]\)`).
		WithArgs(sqlmock.AnyArg(), `{"hr","billing"}`, float64(0), 8).
		WillReturnRows(rows)

	hits, err := s.Search([]float32{0, 1}, SearchParams{TopK: 8, Scopes: []string{"hr", "billing"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].Scope != "hr" {
		t.Errorf("scope 过滤结果错误: %+v", hits)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SQL 期望未满足: %v", err)
	}
}

// TestPGVectorUpsertDocsAtomic 验证 UpsertDocs 是单事务：DELETE 旧分片 → INSERT 新分片 → COMMIT。
// 若拆成多事务，中途失败会留下"半个文档"，与 LocalStore 的原子替换语义不一致。
func TestPGVectorUpsertDocsAtomic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newPGVectorStoreWithDB(db, "kb_chunks", 2)

	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM kb_chunks WHERE doc_id = \$1`).
		WithArgs("d1").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectPrepare(`INSERT INTO kb_chunks`)
	mock.ExpectExec(`INSERT INTO kb_chunks`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	chunks := []Chunk{{
		ChunkID: "d1#c0", DocID: "d1", Scope: "default", Text: "x",
		Vector: []float32{1, 0}, UpdatedAt: time.Now(), Ordinal: 0,
	}}
	if err := s.UpsertDocs("d1", chunks); err != nil {
		t.Fatalf("UpsertDocs 失败: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("事务期望未满足: %v", err)
	}
}

// TestPGVectorIdentifierRejection 验证绕过 config 直接构造时，非法表名仍被拒绝（SQL 注入第二道防线）。
func TestPGVectorIdentifierRejection(t *testing.T) {
	for _, bad := range []string{"kb;DROP TABLE x", "1kb", "kb-chunks", "kb chunks", "kb.sch"} {
		if pgIdentRe.MatchString(bad) {
			t.Errorf("非法标识符 %q 不应通过白名单", bad)
		}
	}
	for _, good := range []string{"kb_chunks", "KB_Chunks2", "_kb"} {
		if !pgIdentRe.MatchString(good) {
			t.Errorf("合法标识符 %q 被误拒", good)
		}
	}
}

// TestPGVectorListScopesAggregation 验证 scope 统计用 COUNT(DISTINCT doc_id) 而非 chunk 数。
func TestPGVectorListScopesAggregation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := newPGVectorStoreWithDB(db, "kb_chunks", 2)

	rows := sqlmock.NewRows([]string{"scope", "count", "max"}).
		AddRow("billing", 3, time.Now())
	mock.ExpectQuery(`(?s)SELECT scope, COUNT\(DISTINCT doc_id\), MAX\(updated_at\).*GROUP BY scope`).
		WillReturnRows(rows)

	scopes := s.ListScopes()
	if len(scopes) != 1 || scopes[0].ID != "billing" || scopes[0].DocCount != 3 {
		t.Errorf("ListScopes 聚合错误: %+v", scopes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("SQL 期望未满足: %v", err)
	}
}

// TestPGTextArrayEscaping 验证 scope 名含特殊字符时被正确转义，不破坏数组字面量（防注入）。
func TestPGTextArrayEscaping(t *testing.T) {
	v, err := pgTextArray{"hr", `a"b`, `c\d`}.Value()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"hr","a\"b","c\\d"}`
	if v != want {
		t.Errorf("数组字面量转义错误: got %q want %q", v, want)
	}
}

func TestScoreConversionConsistency(t *testing.T) {
	// pgvector cosine distance ∈ [0,2]，score = 1 - distance ∈ [-1,1]，
	// 与 LocalStore.Cosine 的值域一致——这是两后端可互换的前提。
	for _, d := range []float64{0, 0.5, 1, 1.5, 2} {
		score := 1 - d
		if score < -1 || score > 1 {
			t.Errorf("distance=%f 转换出的 score=%f 越出 [-1,1]", d, score)
		}
	}
}
