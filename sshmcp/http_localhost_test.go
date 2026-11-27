package sshmcp

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/server"
)

// postWithHost 向 url 发一个最小 POST，并把 Host 头强制设为 host。
// httptest.NewServer 监听 127.0.0.1（loopback），正好复现「loopback 连接 + 非 loopback Host」的触发条件。
func postWithHost(t *testing.T, url, host string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// 默认：mcp-go 的 localhost 防护开启，非 loopback Host 应被 403 拒绝。
// 这是库的安全默认值，改动不得破坏。
func TestLocalhostProtectionDefaultRejectsNonlocalHost(t *testing.T) {
	cfg := HTTPConfig{Path: "/mcp"}
	h := BuildHTTPHandler(server.NewMCPServer("probe", "1.0"), cfg)
	srv := httptest.NewServer(h)
	defer srv.Close()

	if got := postWithHost(t, srv.URL+"/mcp", "192.168.199.199"); got != http.StatusForbidden {
		t.Fatalf("默认应拒绝非 loopback Host，期望 403，实际 %d", got)
	}
	// loopback Host 仍应放行（走到协议层，返回非 403 即可）。
	if got := postWithHost(t, srv.URL+"/mcp", "localhost"); got == http.StatusForbidden {
		t.Fatalf("loopback Host 不应被拒绝，实际 %d", got)
	}
}

// 开关打开：Host 校验被关闭，非 loopback Host 不再返回 403。
func TestAllowNonlocalHostDisablesProtection(t *testing.T) {
	cfg := HTTPConfig{Path: "/mcp", AllowNonlocalHost: true}
	h := BuildHTTPHandler(server.NewMCPServer("probe", "1.0"), cfg)
	srv := httptest.NewServer(h)
	defer srv.Close()

	if got := postWithHost(t, srv.URL+"/mcp", "192.168.199.199"); got == http.StatusForbidden {
		t.Fatalf("allow_nonlocal_host=true 时不应再被 Host 校验拒绝，实际 %d", got)
	}
}

// 开关打开 + 鉴权启用：鉴权中间件仍在 Host 校验外层，缺失 token 应先被 401 拦下。
// 保证关闭防护不会顺带绕过鉴权。
func TestAllowNonlocalHostDoesNotBypassAuth(t *testing.T) {
	cfg := HTTPConfig{
		Path:              "/mcp",
		AllowNonlocalHost: true,
		Auth:              AuthConfig{Enabled: true, Tokens: []string{"secret-token"}},
	}
	h := BuildHTTPHandler(server.NewMCPServer("probe", "1.0"), cfg)
	srv := httptest.NewServer(h)
	defer srv.Close()

	if got := postWithHost(t, srv.URL+"/mcp", "192.168.199.199"); got != http.StatusUnauthorized {
		t.Fatalf("缺失 token 应仍被鉴权拒绝 401，实际 %d", got)
	}
}
