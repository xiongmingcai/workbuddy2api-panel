<p align="center">
  <img src="https://raw.githubusercontent.com/DGZSbot/ai-icon/refs/heads/main/WorkBuddy.png" alt="WorkBuddy2API" width="120">
</p>

<h1 align="center">WorkBuddy2API Panel</h1>

<p align="center">
  <b>把 CodeBuddy 账号变成 Claude Code / OpenAI 兼容 API 的自托管网关</b><br>
  Web 面板 · OAuth 登录 · 账号池轮转 · 定时签到 / 保活 · Anthropic Messages & OpenAI Chat 双协议
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22.5-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-Anthropic%20%2F%20OpenAI-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Docker_Compose-2496ED?style=flat-square">
</p>

---

## 这是什么

把你的 CodeBuddy（`copilot.tencent.com` / 国际版 `workbuddy.ai`）账号包装成一个本地 API 服务，自带 Web 管理面板。**直接给 Claude Code 用**——配两行环境变量就能把 Claude Code 的后端切到你的账号池。

**前提**：已安装 [Claude Code](https://docs.claude.com/en/docs/claude-code) 和 Docker Compose。

## 快速开始（5 步接入 Claude Code）

### 1. 启动网关

```bash
git clone https://github.com/linguo2625469/workbuddy2api-panel.git
cd workbuddy2api-panel
cp config.example.json config.json      # 首次会自动生成随机 api_key，记下它
docker compose up -d --build             # 首次约 1-2 分钟
```

健康检查：

```bash
curl -s http://localhost:7863/healthz
# {"healthy":0,"total":0,...}  ← healthy=0 是因为还没加账号，下一步
```

### 2. 添加 CodeBuddy 账号

打开 **`http://localhost:7863/panel/`**，点右上角「**添加账号**」：浏览器完成授权登录 → 凭证自动落盘 → **热加载进池，无需重启**，顺带完成首次签到。

> 也可命令行：`./login.sh`（需 bash + python3，Linux/macOS）。

### 3. 查看可用模型

```bash
curl -s http://localhost:7863/v1/models -H "Authorization: Bearer <你的-api_key>" \
  | grep -o '"id":"[^"]*"' | head
```

输出形如 `"cn:deepseek-v4-flash"`、`"cn:glm-5.2"`、`"global:gpt-5.6-sol"`。**挑一个能用的**（部分模型受账号类型权限限制，见下方「常见问题」）。

### 4. 配置 Claude Code

编辑 `~/.claude/settings.json`，把 `env` 改成（模型名用上一步挑的，末尾 `[1m]` 是 Claude Code 的上下文标注，发请求时会自动剥掉）：

```json
{
  "env": {
    "ANTHROPIC_BASE_URL": "http://localhost:7863",
    "ANTHROPIC_AUTH_TOKEN": "<你的-api_key>",
    "ANTHROPIC_MODEL": "cn:deepseek-v4-flash[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL": "cn:deepseek-v4-flash[1m]",
    "ANTHROPIC_DEFAULT_OPUS_MODEL": "cn:deepseek-v4-flash[1m]",
    "ANTHROPIC_DEFAULT_HAIKU_MODEL": "cn:deepseek-v4-flash[1m]",
    "ANTHROPIC_DEFAULT_SONNET_MODEL_NAME": "cn:deepseek-v4-flash[1m]",
    "API_TIMEOUT_MS": "3000000",
    "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    "CLAUDE_CODE_MAX_OUTPUT_TOKENS": "131072"
  }
}
```

> 多供应商切换：把这份存成 `~/.claude/settings.json_workbuddy2api-panel`，切换时 `cat ~/.claude/settings.json_workbuddy2api-panel > ~/.claude/settings.json`。

### 5. 验证

**新开**终端会话（Claude Code 启动时读配置）：

```bash
claude -p "用一句话回答：1+1等于几？"
# 1+1等于2。
```

能正常对话、能触发工具（Read/Write/Bash 等）即接入成功。

## 模型名怎么写

| 写法 | 行为 |
|---|---|
| `cn:deepseek-v4-flash[1m]` | 用国内版账号，走 `deepseek-v4-flash`。`[1m]` 是 UI 标注，Claude Code 发请求时自动剥离 |
| `global:gpt-5.6-sol[1m]` | 用国际版账号 |
| `claude-sonnet-4` | 别名，网关映射到默认模型 `cn:glm-5.2`（可用 `WB2A_ANTHROPIC_MODEL` 环境变量覆盖） |

**建议直接用 `cn:<模型名>` 形式**——网关原样透传给上游，最少转换、最稳。

## 协议端点

| 端点 | 协议 | 用途 |
|---|---|---|
| `POST /v1/messages` | Anthropic Messages | **Claude Code 用这个**（支持流式 + 工具调用） |
| `POST /v1/chat/completions` | OpenAI Chat | OpenAI 系客户端 |
| `POST /v1/search/messages` | Anthropic Messages（搜索） | 联网搜索，返回 `web_search_tool_result` |
| `GET /v1/models` | — | 模型列表 |
| `GET /status` | — | 账号池状态 |

鉴权支持两种头：`Authorization: Bearer <key>` 或 `x-api-key: <key>`（Claude Code 默认发后者）。

## 配置速查

完整字段见 `config.example.json`（每项都有注释级默认值）。新人只需关心：

| 字段 | 默认 | 说明 |
|---|---|---|
| `api_key` | 空（自动生成随机） | 网关鉴权密钥，填进 Claude Code 的 `ANTHROPIC_AUTH_TOKEN`。**公网部署必须设置** |
| `listen` | `:7863` | 监听端口 |
| `schedule.checkin_enabled` | `true` | 自动签到 + 余额解冻，建议开 |
| `schedule.keepalive_enabled` | `true` | token 保活，建议开 |
| `pool.cross_domain_fallback` | `false` | 裸模型名跨域回退：cn 选不到号时自动试 global。开启后客户端只需写一个模型名 |
| `models.hidden` | `[]` | 模型黑名单，藏列表 + 拒点名调用 |

改 `config.json` 后用面板「配置」页保存（热生效），或 `docker compose restart`。

## 运维

```bash
docker compose logs -f          # 跟踪日志
docker compose restart          # 重启
docker compose down             # 停止（数据在 ./auths 与 ./data，不受影响）
docker compose up -d --build    # 升级代码后重建
```

面板地址：`http://localhost:7863/panel/` —— 账号管理、签到、积分、请求记录、模型可用性、手机端适配全在里面。

## 常见问题

**`11158 model ... is not allowed for user type`**：你的账号类型（如 `personal_free_user`）无权用该模型。换一个：国际版账号优先试 `global:default-model`、`global:minimax-m3.1-flash`；国内版试 `cn:glm-5.2`、`cn:deepseek-v4-flash`。

**`11102 model ... service info not found`**：模型名上游不认。检查是否带了多余前缀/后缀——上游只认裸名（如 `deepseek-v4-flash`），`cn:`/`global:` 前缀由网关剥离，`[1m]` 由 Claude Code 剥离，别手动拼成 `deepseek-v4-flash[1m]` 发给上游。

**Docker 登录后报 `permission denied` 写 `auths/*.json.tmp`**：容器以 uid 10001 运行，挂载目录属主不符。最简解法：`PUID=$(id -u) PGID=$(id -g) docker compose up -d`。

**账号被 Disable**：面板「账号池」找对应账号，点「启用」恢复；若上游限制（11140 `request illegal`），需在 WorkBuddy 官方渠道申诉，重登无效。

**切换 Claude Code 供应商**：

```bash
cat ~/.claude/settings.json_workbuddy2api-panel > ~/.claude/settings.json   # 用本地网关
cat ~/.claude/settings.json_<其他供应商>       > ~/.claude/settings.json   # 用其他
```

切换后新开会话生效。

## 使用声明

本项目仅限**个人管理自己的 CodeBuddy 账号**（签到、保活、给本地工具提供 API 入口）。禁止批量注册小号、对外售卖额度、二次加壳 / 卡密收费分发。`auths/` 存的是明文凭证，任何来路不明的「收费版」「卡密版」都可能回传窃取你的账号——本项目永远免费开源，不要购买任何篡改分发版本。

基于 MIT 协议开源；因使用非官方客户端导致的账号风控 / 封禁，由使用者自行承担。

## License

[MIT](LICENSE)
