# 客户端连接参数（真实 Agent 测试用）

## 网关（OpenAI 兼容）

| 项 | 值 |
|---|---|
| Base URL | `http://127.0.0.1:18080/v1` （外部访问用 `http://172.28.0.12:18080/v1`） |
| API Key | `sk-279494a4c5ef36b2a9d615636a3d8aec1053f1daebfd8c90f336126b76d6e849` |
| Model | 11 个真实 free 模型之一（见下），或旧名 `meta/muse-spark-1.3-contributor` |
| 协议 | OpenAI Chat Completions + Responses（含流式 SSE） |

**可用模型（网关 `/v1/models` 返回，已刷新）**：
```
big-pickle, deepseek-v4.1-flash-free, hy4-flash-free, iceman-free,
inkling-2-small-free, longcat-2.5-free, flint-40b-free,
nemotron-3.5-super-vl-free, beam-free, mimo-v2.6-flash-free,
nemotron-3.5-lightning-free
```

> 客户端看到的模型菜单已与应答器的真实 free 模型池对齐。网关把客户端指定的 model 名透传到信里；应答器按自己的池 round-robin 作答（未必等于客户端指定的那个）。日志 `answerer.log` 里每条 `answered_by=opencode:prompt:<模型名>` 可追踪实际作答模型。要改变菜单用 `MAILBOX_MODEL_IDS` 环境变量（逗号分隔）重启网关。

## 返回格式（已支持）

- **流式**：`reasoning_content` 里先是等待心跳 `…`，随后是模型的真实思考；正文走 `content`；工具调用走 `function_call` / `tool_calls`
- **非流式**：message 带 `reasoning_content` 字段；工具调用在 `output`（Responses）或 `tool_calls`（Chat）里
- 工具调用是**语义意图**经网关映射后的结果：例如客户端声明 `exec_command(cmd)`，模型输出 `run_command` 意图 → 网关返回 `function_call{name:"exec_command",arguments:{"cmd":"..."}}`

## 直接用 curl 测

```bash
curl -X POST http://127.0.0.1:18080/v1/chat/completions \
  -H "Authorization: Bearer sk-279494a4c5ef36b2a9d615636a3d8aec1053f1daebfd8c90f336126b76d6e849" \
  -H 'Content-Type: application/json' \
  -d '{"model":"hy4-flash-free","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

## 给 opencode 客户端的配置片段

```jsonc
{
  "providers": {
    "mailbox": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "Sub2API Mailbox",
      "options": {
        "baseURL": "http://127.0.0.1:18080/v1",
        "apiKey": "sk-279494a4c5ef36b2a9d615636a3d8aec1053f1daebfd8c90f336126b76d6e849"
      },
      "models": {
        "meta/muse-spark-1.3-contributor": { "name": "Mailbox Relay" }
      }
    }
  }
}
```
> 注：opencode v2 配置键为 `providers`（设置项在 `settings` 下）；若你的客户端是 v1 风格则用 `provider` + `options`。Core Codex CLI 只需在 `~/.codex/config.toml` 配 `model_provider = "mailbox"` 指向该 baseURL/key。

## 日志（持续写入）

| 日志 | 路径 |
|---|---|
| 网关（请求/响应/状态） | `logs/gateway.log` |
| 应答器（收信/认领/回信） | `logs/answerer.log` |
| 进程 PID | `logs/gateway.pid`、`logs/answerer.pid` |

实时查看：
```bash
tail -f logs/gateway.log logs/answerer.log
```

## 收信链路

```
Agent 发请求 → sub2api :18080 → pending/<qid>.json（纯 prompt）
   → 适配器原子认领 → opencode 真实模型（hy4-flash-free）作答
   → answers/<qid>.json → 网关重组 → Agent 收到回信
```

## 注意

- 网关监听 `0.0.0.0:18080`，防火墙/安全组需放行；Key 是明文 Bearer，仅用于测试环境。
- 应答器密码取自 opencode 服务（4097）进程环境，服务重启后需重新读取并重启适配器。
- 默认 pending 超时 120s（真实模型一般 10–20s，足够）。
