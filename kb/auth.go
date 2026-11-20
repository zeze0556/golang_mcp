package kb

import (
	"context"
	"net/http"
	"strings"
)

// tokenContextKey 用自定义类型做 context key，避免与框架 key 冲突。
type tokenContextKey struct{}

// WithToken 把从 HTTP 请求解析出的 access token 注入 context。
// 由 sshmcp 的 WithHTTPContextFunc 调用；tool handler 通过 TokenFromContext 读取。
// 第一性原理：acl 不进工具 schema，只从服务端 token 推导——这是服务端 ACL 的地基。
func WithToken(ctx context.Context, r *http.Request) context.Context {
	token := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
	if token == "" {
		token = strings.TrimSpace(r.Header.Get("X-Access-Token"))
	}
	return context.WithValue(ctx, tokenContextKey{}, token)
}

// TokenFromContext 从 context 读取当前请求的 token（stdio 模式可能为空）。
func TokenFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(tokenContextKey{}).(string); ok {
		return v
	}
	return ""
}

// ScopeResolver 把 token 解析成可见 scope 集合（集合级 ACL）。
// 规则：
//   - 未配置 token_scopes（映射为空）→ 返回 nil，表示"不过滤、可见全部"（仅建议内网，见配置为 open 模式）
//   - 配置了映射 → 只返回该 token 列出的 scope；未知 token 返回空非 nil 切片（可见空集，静默拒绝）
type ScopeResolver struct {
	mapping map[string][]string // token -> scopes
}

// NewScopeResolver 从配置构造（KBTokenScopeConfig 定义于 sshmcp，运行时由 Service 转换后传入）。
func NewScopeResolver(pairs []TokenScope) *ScopeResolver {
	m := make(map[string][]string)
	for _, p := range pairs {
		if p.Token == "" {
			continue
		}
		m[p.Token] = append([]string(nil), p.Scopes...)
	}
	return &ScopeResolver{mapping: m}
}

// TokenScope 是 ACL 映射的中性结构（解耦 kb 包对 sshmcp 配置类型的依赖）。
type TokenScope struct {
	Token  string
	Scopes []string
}

// ScopesFor 返回 token 可见的 scope 集合。
// 返回值语义：nil=不过滤（open 模式）；空切片=无任何可见 scope（拒绝）；非空=白名单。
func (r *ScopeResolver) ScopesFor(token string) []string {
	if r == nil || len(r.mapping) == 0 {
		return nil // open 模式
	}
	scopes, ok := r.mapping[token]
	if !ok {
		return []string{} // 已知映射存在但此 token 未列出 → 空集
	}
	return scopes
}
