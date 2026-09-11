# Kiro-Go

[![Go Version](https://img.shields.io/badge/Go-1.21+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

将 Kiro 账号转换为 OpenAI / Anthropic 兼容的 API 服务。

[English](README.md) | 中文 | [Tiếng Việt](README_VI.md)

如果这个项目帮到了你，欢迎点个 Star 支持一下。

## 功能特性

- Anthropic `/v1/messages`、OpenAI `/v1/chat/completions` 与 OpenAI `/v1/responses`
- 多账号池轮询负载均衡
- 自动 Token 刷新、SSE 流式输出、Web 管理面板
- 多种认证方式：AWS Builder ID、IAM Identity Center (企业 SSO)、Microsoft 企业 SSO、SSO Token、本地缓存、凭证 JSON、Kiro API Key
- 用量追踪、账号导入导出、中英越三语
- 支持设置出站代理（SOCKS5 / HTTP）

## 快速开始

### Docker Compose（推荐）

```bash
git clone https://github.com/Quorinex/Kiro-Go.git
cd Kiro-Go
mkdir -p data
docker-compose up -d
```

### Docker 运行

```bash
docker run -d \
  --name kiro-go \
  -p 8080:8080 \
  -e ADMIN_PASSWORD=your_secure_password \
  -v /path/to/data:/app/data \
  --restart unless-stopped \
  ghcr.io/quorinex/kiro-go:latest
```

### 源码编译

```bash
git clone https://github.com/Quorinex/Kiro-Go.git
cd Kiro-Go
go build -o kiro-go .
./kiro-go
```

### 部署到 Zeabur

仓库已包含 `Dockerfile`，可直接在 Zeabur 上构建运行。

**方式一：面板一键部署**

1. Fork 本仓库到你的 GitHub 账号。
2. 在 Zeabur 新建服务，选择 **Deploy from GitHub**，绑定刚才 fork 的仓库。
3. Zeabur 自动识别 `Dockerfile` 并完成构建。
4. 在 **Networking** 标签暴露端口 `8080` 并绑定域名。
5. 在 **Variables** 标签至少设置 `ADMIN_PASSWORD`（管理面板密码）。
6. 如需持久化账号 / 配置，挂载 Volume 到 `/app/data`。

**方式二：CLI 部署**

```bash
npm i -g zeabur
zeabur auth login
zeabur deploy
```

> 命令需在项目根目录执行。CLI 会生成 `.zeabur/context.json` 记录目标 project / service，包含个人 ID，请勿提交。

部署完成后访问 `https://<你的域名>/admin` 登录管理面板。

首次运行会在 `data/config.json` 自动生成配置，挂载 `/app/data` 以持久化。默认管理密码为 `changeme`，生产环境请务必通过 `ADMIN_PASSWORD` 环境变量或在管理面板中修改。

## 使用方法

访问 `http://localhost:8080/admin` 登录、添加账号，然后调用 API：

```bash
# Claude
curl http://localhost:8080/v1/messages \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{"model":"claude-sonnet-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好！"}]}'

# OpenAI
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer any" \
  -d '{"model":"gpt-4o","messages":[{"role":"user","content":"你好！"}]}'
```

### 添加 Kiro API Key 账号

管理面板「添加账号」可选择 **API Key**，填写 `ksk_...`（也支持 `ksk_...|region`）。

也可通过凭证导入接口添加：

```bash
curl -X POST http://localhost:8080/admin/api/auth/credentials \
  -H "Content-Type: application/json" \
  -H "Cookie: <admin-session>" \
  -d '{"kiroApiKey":"ksk_your_key|us-east-1","authMethod":"api_key","nickname":"cli-key"}'
```

API Key 账号会走 Kiro CLI runtime（`https://runtime.{region}.kiro.dev/`），请求头带 `tokentype: API_KEY`，无需 OAuth 刷新，也不使用 `profileArn`。

## 思考模式

在模型名后加后缀（默认 `-thinking`）即可启用，例如 `claude-sonnet-4.5-thinking`。Claude 兼容请求如果带有顶层 `thinking` 配置，例如 `{"type":"enabled","budget_tokens":2048}` 或 `{"type":"adaptive"}`，也会自动启用 thinking 模式。`enabled` 请求的 `budget_tokens` 会写入上游 `<max_thinking_length>` 提示；这是软预算，不保证精确的 token 上限。`adaptive` 和仅使用模型后缀时继续使用默认预算。输出格式可在管理面板「设置 - Thinking 模式」中配置。

OpenAI 兼容接口也支持标准参数，无需修改模型名：

`POST /v1/chat/completions`：

```json
{
  "model": "claude-sonnet-4.5",
  "reasoning_effort": "high",
  "messages": [{"role": "user", "content": "请解决这个问题"}]
}
```

`POST /v1/responses`：

```json
{
  "model": "claude-sonnet-4.5",
  "reasoning": {"effort": "high", "summary": "auto"},
  "input": "请解决这个问题"
}
```

支持的 effort 为 `none`、`minimal`、`low`、`medium`、`high`、`xhigh`、`max`。代理暂时将它们映射为 Kiro 的软思考预算：`none` 关闭，其他档位依次为 `256`、`1024`、`4096`、`16384`、`65536`、`200000`。该值会写入 `<max_thinking_length>`，是上游提示协议的软预算，不承诺精确的 token 或字符上限。

显式 OpenAI 参数的优先级高于 `-thinking` 后缀，因此 `reasoning_effort: "none"` 或 `reasoning.effort: "none"` 可以关闭带后缀模型的思考。没有传标准参数时，旧后缀行为保持不变。Responses 的 `reasoning` 对象省略 `effort` 时暂按 `medium` 处理；`summary` 支持 `auto`、`concise`、`detailed`（也兼容已弃用的 `generate_summary` 字段）。

## 出站代理

可在管理面板「设置 - 出站代理设置」中配置代理。支持 SOCKS5 和 HTTP 代理。

设置保存后即时生效，无需重启服务。

## 环境变量

| 变量 | 说明 | 默认值 |
|-----|------|-------|
| `CONFIG_PATH` | 配置文件路径 | `data/config.json` |
| `ADMIN_PASSWORD` | 管理面板密码（覆盖配置文件） | - |
| `KIRO_INTEGRATION_TOKEN` | 服务间凭据导入专用密钥（至少 32 字符）；未设置时联动接口关闭 | - |

### 与 kiro-login-web 联动

先为 Kiro-Go 生成独立的服务间密钥（不要复用管理密码）：

```bash
openssl rand -hex 32
```

将结果配置为 `KIRO_INTEGRATION_TOKEN` 并重启 Kiro-Go。联动使用两个限权接口：

- `GET /internal/v1/credentials/status`：测试连接并读取协议能力。
- `POST /internal/v1/credentials/import`：一次导入最多 100 条凭据。

请求使用 `Authorization: Bearer <KIRO_INTEGRATION_TOKEN>`。批量接口会自动识别并规范化
`idc`、`external_idp` 与 `api_key`，逐条返回 `imported`、`duplicate` 或 `failed`；重复的
refresh token / API Key 按幂等成功处理。接口明确拒绝登录密码和 MFA 密钥。

## 参与贡献

欢迎友好交流。遇到问题时，建议先让 Claude Code、Codex 等工具帮忙排查一下，大部分问题都能自己解决。如果能直接提个 PR 就更好了。

## 联系方式

Telegram：[@tutua16888](https://t.me/tutua16888)

## 友情链接

- [LINUX DO](https://linux.do)

## 免责声明

本项目仅供学习和研究目的使用，与 Amazon、AWS 或 Kiro 没有任何关联。用户需自行确保使用行为符合所有适用的服务条款和法律法规，使用风险自负。

## 许可证

[MIT](LICENSE)
