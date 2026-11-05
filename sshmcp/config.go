// Package sshmcp 是一个可复用的 SSH MCP 服务核心库。
//
// 它把“多服务器 SSH 命令执行 / 文件读写（SFTP）/ 本地文件受控读取”封装成一组 MCP 工具，
// 并提供配置加载、HTTP（Streamable）传输、CORS、静态令牌鉴权、审计日志等构建块，
// 供独立二进制（本仓库的 cmd）或宿主程序（如 mynas）直接组合使用，避免重复维护两套实现。
package sshmcp

import (
	"fmt"
	"os"

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
	Host            string   `yaml:"host,omitempty"`             // 监听地址，默认 0.0.0.0
	Port            int      `yaml:"port,omitempty"`             // 监听端口，默认 8080
	Path            string   `yaml:"path,omitempty"`             // MCP 端点路径，默认 /mcp
	CORSOrigins     []string `yaml:"cors_origins,omitempty"`     // 允许跨域的源（网页前端）
	CORSCredentials bool     `yaml:"cors_credentials,omitempty"` // 是否允许携带凭证
	Auth            AuthConfig `yaml:"auth,omitempty"`           // 访问令牌鉴权
}

// Config YAML 配置文件结构
type Config struct {
	Transport       string               `yaml:"transport,omitempty"` // stdio 或 http，默认 stdio
	HTTP            HTTPConfig           `yaml:"http,omitempty"`      // HTTP 模式配置
	Servers         map[string]SSHConfig `yaml:"servers"`
	ToolValidation  ToolValidation       `yaml:"tool_validation,omitempty"` // 工具参数校验配置
	Command         CommandConfig        `yaml:"command,omitempty"`          // 命令执行超时/输出上限
	LocalRead       LocalReadConfig      `yaml:"local_read,omitempty"`       // 本地文件读取白名单
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
	return cfg, nil
}
