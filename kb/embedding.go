package kb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"
)

// embedRequest / embedResponse 是 OpenAI 兼容 embeddings 端点的最小契约。
type embedRequest struct {
	Model string `json:"model"`
	Input any    `json:"input"` // string 或 []string
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Embedder 封装远程 embedding 服务。模型身份冻结在配置里（model + dim），
// 通过 fingerprint 探针检测服务端静默换模型（别名路由漂移）。
type Embedder struct {
	endpoint    string // 到 /v1
	model       string
	apiKey      string
	dim         int
	queryPrefix string
	timeout     time.Duration
	client      *http.Client
}

// NewEmbedder 从配置构造。apiKey 支持 ${ENV_VAR}（由调用方 resolve 后传入或在此解析）。
func NewEmbedder(endpoint, model, apiKey string, dim int, queryPrefix string, timeoutSec int) *Embedder {
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	if apiKey != "" && strings.HasPrefix(apiKey, "${") && strings.HasSuffix(apiKey, "}") {
		apiKey = os.Getenv(apiKey[2 : len(apiKey)-1])
	}
	return &Embedder{
		endpoint:    strings.TrimRight(endpoint, "/"),
		model:       model,
		apiKey:      apiKey,
		dim:         dim,
		queryPrefix: queryPrefix,
		timeout:     time.Duration(timeoutSec) * time.Second,
		// 禁用 keep-alive 复用：部分 OpenAI 兼容网关（如 Ollama 前置）在复用连接上
		// 会错误返回 404 "File Not Found"（实测第二次连续请求必现）。短连接规避。
		client: &http.Client{
			Timeout: time.Duration(timeoutSec) * time.Second,
			Transport: &http.Transport{
				DisableKeepAlives: true,
			},
		},
	}
}

// Version 返回冻结的模型标识，写入索引并在查询时回显（embedding 冻结铁律）。
func (e *Embedder) Version() string {
	return fmt.Sprintf("%s@dim%d", e.model, e.dim)
}

// EmbedDocuments 编码文档侧文本（不加查询前缀）。批量请求，返回顺序与输入一致。
func (e *Embedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float32, error) {
	return e.embed(ctx, texts, false)
}

// EmbedQuery 编码查询侧文本（加指令前缀——指令感知模型只在查询侧加）。
func (e *Embedder) EmbedQuery(ctx context.Context, q string) ([]float32, error) {
	text := q
	if e.queryPrefix != "" {
		text = e.queryPrefix + q
	}
	out, err := e.embed(ctx, []string{text}, false) // 前缀已加，不要再重复
	if err != nil || len(out) == 0 {
		return nil, err
	}
	return out[0], nil
}

// embed 调 /v1/embeddings。addPrefix=false 时 input 原样发送（前缀由调用方决定）。
func (e *Embedder) embed(ctx context.Context, texts []string, _ bool) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: e.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("序列化 embedding 请求失败: %w", err)
	}
	url := e.endpoint + "/embeddings"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("构造 embedding 请求失败: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	if e.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
	}

	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("调用 embedding 服务失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 embedding 响应失败: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("embedding 服务返回 HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}
	var er embedResponse
	if err := json.Unmarshal(raw, &er); err != nil {
		return nil, fmt.Errorf("解析 embedding 响应失败: %w", err)
	}
	if er.Error != nil {
		return nil, fmt.Errorf("embedding 服务错误: %s", er.Error.Message)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embedding 返回数量不符: 期望 %d, 实际 %d", len(texts), len(er.Data))
	}
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(texts) {
			return nil, fmt.Errorf("embedding 返回非法 index: %d", d.Index)
		}
		if e.dim > 0 && len(d.Embedding) != e.dim {
			return nil, fmt.Errorf("embedding 维度不符: 期望 %d, 实际 %d（换模型需重建索引）", e.dim, len(d.Embedding))
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// FingerprintProbe 对固定探针文本取向量，用于与基线比对。
// 服务端已 L2 归一化，返回向量可直接算余弦。
func (e *Embedder) FingerprintProbe(ctx context.Context) ([]float32, error) {
	v, err := e.embed(ctx, []string{"kb-fingerprint-probe-v1:知识库指纹探针"}, false)
	if err != nil || len(v) == 0 {
		return nil, err
	}
	return v[0], nil
}

// Cosine 计算两个向量的余弦相似度。服务端已归一化时等价于点积。
func Cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
