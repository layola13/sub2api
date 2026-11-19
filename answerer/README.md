# mailbox-local — 本机信箱应答器（brain 模式）

给 `backend/internal/handler/mailbox_handler.go` 配套的**应答器**实现，针对本机 opencode 1.19.6（V1 API）校准，解决三件事：

1. **reasoning 回传**：模型把 `text` / `reasoning` 写进严格 JSON，适配器解析后写入 `answer.reasoning`；网关在流式里输出 `delta.reasoning_content`、非流式输出 `message.reasoning_content`。
2. **工具意图回传**：模型的 `tool_calls` 只写语义意图（`read_file/list_dir/search_text/run_command/write_file/edit_file`），由网关映射成客户端声明的工具；流式输出 `delta.tool_calls`，非流式输出 `message.tool_calls`。
3. **应答器不执行工具**：`opencode.json` 里的 `mailbox-relay` agent 用 V1 `permission` 规则全部 `deny`，只产出 JSON，不调用任何工具。工具由原始客户端执行。

> 注意：仓库 `tools/mailbox-opencode-answerer/opencode.json.example` 用的是 V2 `permissions` 格式，opencode 1.19.6 会报 `V2 permissions are not supported by OpenCode V1`；这里改用 V1 `permission` 写法。

## 用法

```bash
# 1. 网关需开启 mailbox 模式并建好 platform=mailbox 的分组 + API Key
MAILBOX_ENABLED=true MAILBOX_SPOOL_DIR=/tmp/mailbox-spool ...

# 2. 启动应答器（需 bun，opencode 在 PATH 或用 OPENCODE_BIN 指定）
./start-answerer.sh
```

可用环境变量：`MAILBOX_SPOOL_DIR`、`OPENCODE_BIN`、`OPENCODE_MODEL`、`OPENCODE_PROVIDER`、`MAILBOX_OPENCODE_AGENT`、`MAILBOX_ANSWER_TIMEOUT_MS`。

## 文件

- `adapter.js`：在原适配器基础上改造 CLI 模式 —— `spawn` + 忽略 stdin 规避非交互挂起，解析 brain JSON，保留无 JSON 时的纯文本兜底。
- `opencode.json`：V1 兼容的 deny-all `mailbox-relay` agent 配置。
- `start-answerer.sh`：启动脚本。
