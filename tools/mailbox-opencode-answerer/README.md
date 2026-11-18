# mailbox-opencode-answerer

让 [OpenCode](https://opencode.ai) 充当 sub2api **mailbox relay（信箱中继）** 的外部应答器（收信方）。

## 它是什么

sub2api 的 mailbox 模式（`backend/internal/handler/mailbox_handler.go`）把 `platform=mailbox` 分组的请求改写成一个**文件信箱**：

```
pending/<qid>.json   应答器来认领（pending -> claimed 原子 rename）
claimed/<qid>.json   已被认领
answers/<qid>.json   应答器写回答案 ← 网关在这里轮询
sessions/<sid>.json  被剥离的 instructions/tools（应答器看不到）
done/<qid>.json      问答归档
```

本目录的适配器负责 `pending → claimed → opencode 作答 → answers` 这半边，网关负责其余部分。两端只靠磁盘上的 JSON 文件通信，**不需要改网关一行代码**。

## 设计选择：brain 模式

原协议要求应答器只写**语义意图**（`read_file / list_dir / search_text / run_command / write_file / edit_file`），**从不执行工具**；真正的工具由最初的客户端执行，网关负责把意图映射到客户端声明的工具。

因此 `opencode.json.example` 里给专用 agent 配了：

- `permissions: [{action:"*", resource:"*", effect:"deny"}]` —— 禁止 opencode 自己执行任何工具；
- 严格的 `system` 提示词 —— 强制只输出 `{"text","tool_calls"}` JSON（opencode v2 没有原生 json_schema 输出，只能靠提示词 + 解析）。

如果你的场景只需要纯文本问答、不需要工具循环，可以允许工具执行并让 answer 的 `tool_calls` 留空。

## 输出格式：reasoning + 工具意图（brain 模式）

应答器默认用 `MAILBOX_ANSWER_MODE=prompt`（`session.prompt` + 读 assistant 消息），返回三类内容：

- `text` → 答案正文
- `reasoning` → **模型的真实思考**（写入 `answer.reasoning`，网关在流式里以 `reasoning_content` / reasoning item 输出，非流式写入 message 的 `reasoning_content` 字段）
- `tool_calls` → **语义意图**（模型不执行工具，只在 JSON 里表达；网关映射成客户端实际声明的工具，返回 `function_call`）

为此给模型注入了 `STRICT_JSON` 指令（拼在 prompt 里）：禁止调用工具，只输出
`{"text","reasoning","tool_calls":[{"intent","args"}]}`。语义意图词汇表是 6 种
（`read_file/list_dir/search_text/run_command/write_file/edit_file`），网关负责映射回
客户端工具——包括把 `run_command` 映射到用 `cmd` 参数的 shell 工具（如 Codex 的 `exec_command`）。

> 这样既不重复执行工具（opencode 端不执行），又让客户端收到真正的 `function_call`，符合原 mailbox 的 brain 设计。

## 模型池（真实 free 模型列表）

应答器默认使用 **OpenCode 的真实 free 模型列表**（11 个，取自 `/api/model`），每封信 round-robin 轮换一个模型：

```
big-pickle, deepseek-v4.1-flash-free, hy4-flash-free, iceman-free,
inkling-2-small-free, longcat-2.5-free, flint-40b-free,
nemotron-3.5-super-vl-free, beam-free, mimo-v2.6-flash-free,
nemotron-3.5-lightning-free
```

- 实测 10/11 可用；`inkling-2-small-free` 在某些区域报 `not available in your country`，适配器会**自动跳过并轮换到下一个模型**。
- 每个模型建独立 session（session 绑定模型），失败时丢弃 session 以便下次重建。
- 自定义模型池：`OPENCODE_MODELS="model-a,model-b"`（逗号分隔）或单个 `OPENCODE_MODEL=xxx`。
- 日志里每条回信都带 `answered_by=opencode:http:<模型名>`，可追踪是哪个模型答的。

## 模型接入方式

适配器支持两种模式（`MAILBOX_ANSWER_MODE`）：

- **`http`（默认，推荐）**：直连 OpenCode HTTP 服务，用 `session.generate`。这是无状态的一次性生成——**不执行任何工具**，完美对应 mailbox 的 brain 模式。配 `OPENCODE_BASE_URL` / `OPENCODE_SERVER_PASSWORD` / `OPENCODE_MODEL` / `OPENCODE_PROVIDER`。
  > 本环境实测：opencode 服务跑在 `4097`（`--service`，共享 harness 活跃会话额度），用 `hy4-flash-free` 模型；`generate/text` 这类 stateless 端点会 429，但**新建 session + session.generate** 可正常拿到真实模型输出。密码从服务进程环境读取：`tr '\0' '\n' < /proc/<pid>/environ | grep OPENCODE_SERVER_PASSWORD`。
- **`cli`**：`opencode run --standalone --agent <agent> --format json`，需要一个带 `mailbox-relay` agent 的 opencode 配置（`OPENCODE_CONFIG`）。

两种模式都在 **prompt 里内嵌严格 JSON 指令**（`STRICT` 常量），因为 opencode v2 没有原生 `json_schema` 输出。

## 用法

### 1. 启用 sub2api mailbox 模式

```yaml
# config.yaml
mailbox:
  enabled: true
  spool_dir: "/tmp/mailbox-spool"
  pending_timeout_seconds: 600
  keepalive_seconds: 15
  poll_interval_millis: 500
```

（也可用 `MAILBOX_ENABLED=true` 等环境变量。）建一个 `platform=mailbox` 的分组并给它发 API Key。该分组的 `/v1/models` 暴露真实 free 模型列表（见模型池一节，可用 `MAILBOX_MODEL_IDS` 覆盖）；旧名 `meta/muse-spark-1.3-contributor` 仍兼容。

### 2a. HTTP 模式（真实模型，默认）

```bash
export OPENCODE_BASE_URL=http://127.0.0.1:4097
export OPENCODE_SERVER_PASSWORD='<从 opencode 服务进程环境读取>'
export OPENCODE_MODEL=hy4-flash-free
export OPENCODE_PROVIDER=opencode
bun adapter.js /tmp/mailbox-spool 500
```

### 2b. CLI 模式（备选）

```bash
cp opencode.json.example opencode.json
# 编辑 providers 段，填入真实模型供应商的 apiKey/baseURL/model
export OPENCODE_CONFIG=/abs/path/opencode.json
MAILBOX_ANSWER_MODE=cli bun adapter.js /tmp/mailbox-spool 500
```

### 4. 发信

```bash
curl -X POST http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer sk-..." -H 'Content-Type: application/json' \
  -d '{"model":"hy4-flash-free",
       "messages":[{"role":"user","content":"你好"}],"stream":false}'
```

curl 会挂起直到适配器把答案写回 `answers/`，然后收到标准 OpenAI 响应。

## 实测记录

在真实部署（Go 1.27.4 + Postgres 16 + Redis 7 + sub2api）上，**用真实模型 `opencode/hy4-flash-free`** 验证通过：

- 单轮问答：curl 发"用一句话解释量子纠缠" → 信落盘（仅纯 prompt）→ opencode 真实模型生成中文解释 → 写回 → 标准响应返回，耗时约 13.6s，`answered_by=opencode:http`；
- 多轮：`previous_response_id` 链式衔接正确（第二轮 prompt 含两轮历史）；
- 工具意图：真实模型产出 `search_text {pattern:"TODO", is_regex:false}` → 网关映射成客户端声明的 `grep_search`，并按其 schema 把参数整形为 `{"pattern":"TODO"}`（客户端未声明 `is_regex`，被丢弃）；
- 流式：SSE 心跳（reasoning_content）+ 答案 chunk + `[DONE]` 正常。

> 早期用 mock 模型验证过管线；最终已切换到真实模型（HTTP 模式）。本环境里 opencode 的 `generate/text` 等 stateless 端点因免费额度耗尽返回 429，但新建 session + `session.generate` 可正常调用真实模型，适配器据此实现。

## 文件

- `adapter.js` —— 文件信箱轮询 / 原子认领 / opencode 调用 / 答案校验与写回
- `opencode.json.example` —— 专用 agent（deny-all + 严格 JSON）与 provider 配置示例
- `README.md` —— 本文档
