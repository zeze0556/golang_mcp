// Package sshmcp 是一个可复用的 SSH MCP 服务核心库。
//
// 它把“多服务器 SSH 命令执行 / 文件读写（SFTP）/ 本地文件受控读取”封装成一组 MCP 工具，
// 并提供配置加载、HTTP（Streamable）传输、CORS、静态令牌鉴权、审计日志等构建块，
// 供独立二进制（本仓库的 cmd）或宿主程序（如 mynas）直接组合使用，避免重复维护两套实现。
package sshmcp

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// SSHConfig 单个服务器的连接配置
type SSHConfig struct {
	Host        string `yaml:"host"`
	Port        int    `yaml:"port"`
	User        string `yaml:"user"`
	KeyPath     string `yaml:"key_path,omitempty"`
	KeyContent  string `yaml:"key_content,omitempty"`
	Description string `yaml:"description,omitempty"` // 描述信息，方便 AI 理解用途
	// InsecureIgnoreHostKey 为 true 时关闭 SSH 主机密钥校验（等价于 ssh.InsecureIgnoreHostKey，存在中间人风险）。
	// 默认 false：走 known_hosts 校验，校验失败（未知/变更主机）将拒绝连接。
	InsecureIgnoreHostKey bool `yaml:"insecure_ignore_host_key,omitempty"`
}

// ToolValidation MCP 工具参数校验配置
type ToolValidation struct {
	AllowAdditionalProperties bool `yaml:"allow_additional_properties,omitempty"` // 是否允许客户端传递未声明的额外参数，默认 false（严格模式）
}

// CommandConfig 命令执行相关的安全/健壮性配置
type CommandConfig struct {
	// TimeoutSeconds 单条命令的最大执行时间（秒）。默认 120；设为 0 表示不限时（不推荐）。
	TimeoutSeconds int `yaml:"timeout_seconds,omitempty"`
	// MaxOutputMB 单条命令 stdout+stderr 的采集上限（MB）。默认 4；超出部分截断，防止超大输出撑爆响应。
	MaxOutputMB int `yaml:"max_output_mb,omitempty"`
}

// LocalReadConfig 本地文件读取（read_local_file 工具）的访问控制
// 出于安全考虑，该工具默认关闭；开启后仅允许读取 allowed_dirs 指定目录下的文件，
// 避免被（即便持有 token 的）调用方读取 SSH 私钥、config.yaml 等敏感本地文件。
type LocalReadConfig struct {
	Enabled     bool     `yaml:"enabled,omitempty"`
	AllowedDirs []string `yaml:"allowed_dirs,omitempty"` // 允许读取的本地目录白名单（绝对路径）
}

// AuthConfig 访问令牌鉴权配置（防止 MCP 端点被未授权访问）
type AuthConfig struct {
	Enabled bool     `yaml:"enabled,omitempty"` // 是否启用鉴权，默认 false
	Tokens  []string `yaml:"tokens,omitempty"`  // 合法的 access_token 列表（支持 ${ENV_VAR} 从环境变量读取）
}

// HTTPConfig HTTP 传输配置
type HTTPConfig struct {
	Host            string     `yaml:"host,omitempty"`             // 监听地址，默认 0.0.0.0
	Port            int        `yaml:"port,omitempty"`             // 监听端口，默认 8080
	Path            string     `yaml:"path,omitempty"`             // MCP 端点路径，默认 /mcp
	CORSOrigins     []string   `yaml:"cors_origins,omitempty"`     // 允许跨域的源（网页前端）
	CORSCredentials bool       `yaml:"cors_credentials,omitempty"` // 是否允许携带凭证
	Auth            AuthConfig `yaml:"auth,omitempty"`             // 访问令牌鉴权
}

// KBEmbeddingConfig 知识库 embedding 服务配置（OpenAI 兼容 /v1/embeddings）。
// 服务端冻结模型：model/dim 一旦变化，既有向量索引失效，需全量重建。
type KBEmbeddingConfig struct {
	Endpoint    string `yaml:"endpoint"`                  // 例 http://192.168.2.236:8082/v1（到 /v1，不含 /embeddings）
	Model       string `yaml:"model"`                     // 服务通告的模型 id，例 Qwen3-Embedding-0.6B
	APIKey      string `yaml:"api_key,omitempty"`         // 上游鉴权 key，支持 ${ENV_VAR}；无鉴权可留空
	Dim         int    `yaml:"dim"`                       // 向量维度，例 1024；写入索引 schema
	QueryPrefix string `yaml:"query_prefix,omitempty"`    // 查询侧指令前缀（文档侧不加）。例 "Instruct: 检索知识库文档\nQuery: "
	TimeoutSec  int    `yaml:"timeout_seconds,omitempty"` // 单次请求超时，默认 60
}

// KBPGVectorConfig pgvector 后端配置。
// 向量维度刻意不单独配置：直接复用 embedding.dim，避免两处维度不一致导致写入被 PG 拒绝（单一真相源）。
type KBPGVectorConfig struct {
	DSN        string `yaml:"dsn,omitempty"`         // libpq 连接串，支持 ${ENV_VAR}；例 postgres://user:pass@host:5432/kb?sslmode=disable
	Table      string `yaml:"table,omitempty"`       // 表名，默认 kb_chunks；仅允许 [A-Za-z_][A-Za-z0-9_]*（要拼进 DDL）
	MaxConns   int    `yaml:"max_conns,omitempty"`   // 连接池上限，默认 4
	AutoSchema *bool  `yaml:"auto_schema,omitempty"` // 启动时自动建扩展/表/索引；未设置视为 true，显式 false 则要求库已就绪
}

// KBTokenScopeConfig token → 可见 scope 集合（集合级 ACL）。
// 一个 token 可访问多个 scope；未列出的 token 视为无任何 scope（看不到 KB 内容）。
type KBTokenScopeConfig struct {
	Token  string   `yaml:"token"`  // 明文 token 或 ${ENV_VAR}；与 http.auth.tokens 中的值对应
	Scopes []string `yaml:"scopes"` // 该 token 可见的知识域（集合）列表
}

// KnowledgeBaseConfig 知识库子系统配置。
// enabled=false 时完全不挂载：不注册 KB 工具、不监听目录，MCP 其余能力不受影响。
type KnowledgeBaseConfig struct {
	Enabled       bool                 `yaml:"enabled,omitempty"`
	Backend       string               `yaml:"backend,omitempty"`         // 向量存储后端：local（默认）或 pgvector
	WatchDir      string               `yaml:"watch_dir,omitempty"`       // inbox：待入库文档目录（绝对路径）
	WriteDir      string               `yaml:"write_dir,omitempty"`       // outbox：失败清单/去重状态落盘（绝对路径；local 后端还存索引文件）
	IndexFile     string               `yaml:"index_file,omitempty"`      // 索引文件名（相对 write_dir），仅 local 后端使用，默认 kb_index.json
	ChunkSize     int                  `yaml:"chunk_size,omitempty"`      // 每片目标字符数，默认 800
	ChunkOverlap  int                  `yaml:"chunk_overlap,omitempty"`   // 相邻片重叠字符数，默认 100
	TopKCap       int                  `yaml:"top_k_cap,omitempty"`       // top_k 硬上限，默认 20；最大不超过 20
	MaxCharBudget int                  `yaml:"max_char_budget,omitempty"` // 单次返回字符预算，默认 12000
	PollSeconds   int                  `yaml:"poll_seconds,omitempty"`    // 后台目录轮询间隔，默认 30；0 表示仅启动时后台扫描一次
	Embedding     KBEmbeddingConfig    `yaml:"embedding,omitempty"`
	PGVector      KBPGVectorConfig     `yaml:"pgvector,omitempty"`     // backend=pgvector 时生效
	TokenScopes   []KBTokenScopeConfig `yaml:"token_scopes,omitempty"` // 集合级 ACL；为空则 KB 工具对所有持有合法 token 者开放全部 scope（仅建议内网）
}

// Config YAML 配置文件结构
type Config struct {
	Transport      string               `yaml:"transport,omitempty"` // stdio 或 http，默认 stdio
	HTTP           HTTPConfig           `yaml:"http,omitempty"`      // HTTP 模式配置
	Servers        map[string]SSHConfig `yaml:"servers"`
	ToolValidation ToolValidation       `yaml:"tool_validation,omitempty"` // 工具参数校验配置
	Command        CommandConfig        `yaml:"command,omitempty"`         // 命令执行超时/输出上限
	LocalRead      LocalReadConfig      `yaml:"local_read,omitempty"`      // 本地文件读取白名单
	KnowledgeBase  KnowledgeBaseConfig  `yaml:"knowledge_base,omitempty"`  // 知识库子系统（v1）
	// KnownHostsPath 全局 known_hosts 文件路径；为空时回退到 ~/.ssh/known_hosts。
	// 仅当所有服务器都未设置 insecure_ignore_host_key 时生效。
	KnownHostsPath string `yaml:"known_hosts_path,omitempty"`
}

// LoadConfig 读取并校验 YAML 配置文件。
// 校验规则：servers 必须非空；HTTP.Path 缺省时回退到 "/mcp"（宿主可按需覆盖）。
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("解析配置文件失败: %w", err)
	}
	if len(cfg.Servers) == 0 {
		return Config{}, fmt.Errorf("配置文件中没有找到任何服务器定义 (servers 节点为空)")
	}
	if cfg.HTTP.Path == "" {
		cfg.HTTP.Path = "/mcp"
	}
	if cfg.KnowledgeBase.Enabled {
		if err := validateKnowledgeBase(cfg.KnowledgeBase); err != nil {
			return Config{}, fmt.Errorf("knowledge_base 配置非法: %w", err)
		}
	}
	return cfg, nil
}

// validateKnowledgeBase 校验知识库配置的硬约束。
// 第一性原理：watch/write 必须分离且不嵌套，否则服务会把自己的产物当新文档重复入库（自激循环）。
// 该约束与后端无关（写路径的文件去重/失败清单仍落 write_dir），故两种 backend 都校验。
func validateKnowledgeBase(kb KnowledgeBaseConfig) error {
	backend := strings.ToLower(strings.TrimSpace(kb.Backend))
	switch backend {
	case "", "local", "pgvector":
	default:
		return fmt.Errorf("backend 只能是 local 或 pgvector，实际为 %q", kb.Backend)
	}
	if kb.WatchDir == "" {
		return fmt.Errorf("watch_dir 不能为空")
	}
	if kb.WriteDir == "" {
		return fmt.Errorf("write_dir 不能为空")
	}
	if !filepath.IsAbs(kb.WatchDir) || !filepath.IsAbs(kb.WriteDir) {
		return fmt.Errorf("watch_dir 与 write_dir 必须是绝对路径")
	}
	watch := filepath.Clean(kb.WatchDir)
	write := filepath.Clean(kb.WriteDir)
	if watch == write {
		return fmt.Errorf("watch_dir 与 write_dir 不能是同一目录")
	}
	if isSubPath(watch, write) || isSubPath(write, watch) {
		return fmt.Errorf("watch_dir 与 write_dir 不能相互嵌套（watch=%s, write=%s）", watch, write)
	}
	if kb.Embedding.Endpoint == "" {
		return fmt.Errorf("embedding.endpoint 不能为空")
	}
	if kb.Embedding.Model == "" {
		return fmt.Errorf("embedding.model 不能为空")
	}
	if kb.Embedding.Dim <= 0 {
		return fmt.Errorf("embedding.dim 必须 > 0")
	}
	if backend == "pgvector" {
		if kb.PGVector.DSN == "" {
			return fmt.Errorf("backend=pgvector 时 pgvector.dsn 不能为空")
		}
		// 表名要拼进 DDL，参数化占位符无法用于标识符——必须在配置层卡死白名单。
		if t := kb.PGVector.Table; t != "" && !sqlIdentRe.MatchString(t) {
			return fmt.Errorf("pgvector.table 只能用字母/数字/下划线，且不能以数字开头，实际为 %q", t)
		}
	}
	return nil
}

// sqlIdentRe PostgreSQL 未加引号标识符的白名单（表名由配置提供，要拼进 DDL）。
var sqlIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// isSubPath 判断 child 是否位于 parent 之下（含相等由调用方单独处理）。
func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "."
}
