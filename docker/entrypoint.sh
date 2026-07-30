#!/bin/sh
# Multi-SSH MCP Server 容器入口脚本
# 职责：配置缺失时尽早失败并给出清晰提示；随后前台启动服务
set -e

cd /app

# 友好提示：配置文件未挂载时尽早失败，而不是让 Go 程序打印难懂的 fatal
if [ ! -f /app/config.yaml ] && [ ! -f /app/config.yml ]; then
  echo "[entrypoint] 错误：未找到 /app/config.yaml" >&2
  echo "[entrypoint] 请通过 volume 把宿主机 config.yaml 挂载到 /app/config.yaml" >&2
  echo "[entrypoint] 容器化部署请使用 key_content（直接贴私钥内容），不要用 key_path 的 Windows 路径" >&2
  exit 1
fi

# MCP_TRANSPORT 环境变量若设置，会覆盖 config.yaml 的 transport 字段
# 容器内通常用 http；如需 stdio 可设 MCP_TRANSPORT=stdio
# exec 替换为进程，确保信号（SIGTERM）能正确传给 Go 程序以优雅退出
exec ./ssh-mcp-server
