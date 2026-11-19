# sub2api-mini — 仅信箱中继

这是 `mini` 分支：**只保留信箱（mailbox relay）功能**。原 `main` 分支的 sub2api 网关（3043 个 Go 文件、约 117 万行）已全部移除，仅留下信箱链路所需的最小实现（7 个 Go 文件、约 2200 行，编译产物 22MB）。

## 保留的能力

- `POST /v1/chat/completions`（流式 / 非流式）
- `POST /v1/responses`（含 `previous_response_id` 多轮上下文）
- `GET /v1/models`
- `GET /health`
- 纯 prompt 文件队列（spool）：`pending → claimed → answers`
- SSE 心跳保活
- reasoning 回传：流式 `delta.reasoning_content`，非流式 `message.reasoning_content` / Responses 的 reasoning item
- 工具意图回传：应答器只输出语义意图，网关映射成客户端声明的工具
- 应答器侧 opencode 使用 deny-all 的 `mailbox-relay` agent，**不执行任何工具**，只产出 JSON

## 删除的内容

前端、管理后台、支付、订阅、账号渠道、模型定价、OAuth、对象存储、Webhook、i18n、全部 `ent` 生成代码、全部其它 handler/service/repository 等——凡与信箱无关一律删除。数据库和 Redis 也不再需要，会话状态落在 spool 的 JSON 文件里。

## 构建与运行

```bash
cd backend
go build -o bin/mailbox ./cmd/mailbox

export MAILBOX_API_KEY=sk-your-key
export MAILBOX_SPOOL_DIR=/tmp/mailbox-spool
export SERVER_PORT=8080
./bin/mailbox
```

启动应答器（需 bun，opencode 在 PATH 或设 `OPENCODE_BIN`）：

```bash
cd answerer
./start-answerer.sh
```

## 测试

```bash
curl -s http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $MAILBOX_API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"hy4-flash-free","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

设计背景见 `MAILBOX.md`，应答器细节见 `answerer/README.md`。
