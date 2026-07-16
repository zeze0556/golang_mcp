#!/bin/bash
# SSH MCP Server 启动脚本 (Windows/Git Bash)
# 用法: ./run.sh            # 使用 config.yaml 中的 transport 模式
#       ./run.sh stdio      # 强制 stdio 模式
#       ./run.sh http       # 强制 http 模式 (需在 config.yaml 配置 http 节点)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

# 设置 Go 路径（如果在 PATH 中则跳过）
GO_BIN="go"
if ! command -v go &> /dev/null; then
    GO_BIN="/c/Program Files/Go/bin/go"
fi

MODE="$1"
if [ -n "$MODE" ]; then
    echo "以 $MODE 模式运行 (通过 MCP_TRANSPORT 环境变量覆盖配置)"
    MCP_TRANSPORT="$MODE" "$GO_BIN" run .
else
    "$GO_BIN" run .
fi
