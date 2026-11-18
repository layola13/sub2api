# 信箱中继（Mailbox Relay）— 需求、处理步骤与问题记录

本文档记录信箱中继服务的需求口径、本次代码推送的完整处理步骤，以及过程中遇到的问题与解决办法。随代码一并入库，仓库 layola13/sub2api。

## 1. 需求

### 1.1 背景与形态

客户问题经一个中继服务转给智能端作答：

- 客户端把问题提交到中继邮箱，**纯用户 prompt 进文件队列（spool）**，中继不掺入其它数据；
- 客户端连接以 **SSE + 心跳** 保持，慢回答不超时断开；
- 智能端**定时经 SSH 进中继取件、作答、投递**回队列，客户端从同一条连接收到答案；
- 多轮上下文用 `previous_response_id` 链式衔接。

### 1.2 约束与既定决策（2026-11-12 用户定）

- 独立小型服务，不依赖 sub2api 本体（无 sub2api 的小型独立版）；
- 目标部署环境是小 VPS：BuyVM 1 核 / 约 580 MB RAM / 3.0 GB 磁盘；
- 对外入口经 Cloudflare：`https://mailbox.zig.best`；
- 终局形态为纯 SLA 单程序实现。**本次入库的是先行落地的原型**：在 sub2api 网关接缝上实现的信箱模式（Go），纯 SLA 独立版仍为既定方向、尚未动工。

### 1.3 验收记录（2026-11-12，验收项 (a)–(e) 全部通过）

验收在当时的运行环境上以真实 Codex CLI 完成，实测证据：

- 两轮问答均返回 rc=0；
- 一次 65 秒的慢回答全程经 SSE 保持连接，15 秒间隔心跳（ping）不断线；
- spool 中只有 prompt 本身（prompt-only）；
- 第二轮经 `previous_response_id` 正确衔接第一轮上下文。

配套材料（保留在本地主本 `~/workspace/mailbox-proto-sub2api/`，未入库）：`box/` 部署脚本与 `mailbox-overlay.patch`、`answerer/` 智能端取件/作答脚本（SSH 轮询认领）、`cf-tunnel/` 域名链路、9 份 RESULT/INTERIM 记录。

注：原运行栈于 2026-11-13 随其宿主环境一并删除，目前无运行实例；恢复 = 从本地主本重建，不是重启旧栈。

## 2. 本次推送的处理步骤（2026-11-18）

目标：把信箱模式代码推入 layola13/sub2api 的 main。

1. **核查本地代码**：本地主本 `~/workspace/mailbox-proto-sub2api/src/` 完整（上游 Wei-Shaw/sub2api 钉在 a9240d5，信箱模式原为未提交的工作区改动）。
2. **本地落提交**：在 a9240d5 之上提交 `3098d9cc7`「feat: mailbox mode — prompt-only file-queue relay on the gateway seam」，10 个文件、+1741/−3，含新文件 `backend/internal/handler/mailbox_handler.go`。
3. **核对目标仓现状**：远端 main 只有用户建仓时的一笔 bootstrap 提交 `8d89d57e`（树里仅 README.md，且与本地 README 逐字节相同）。
4. **盒上复刻提交**：在 Colab 盒（25024）克隆上游、检出 a9240d5，用 `git am` 应用本地导出的补丁——复刻出的提交 sha 与本地 `3098d9cc7` **逐字一致**（补丁经 base64 管道传输，sha256 双侧核对）。
5. **剔除二进制**：按 §3 问题 4 的规则，从推送树移除 37 个二进制文件并写入 `.gitignore`，生成快照树。
6. **快照提交并推送**：以 bootstrap `8d89d57e` 为父提交生成快照提交，按 §3 问题 5 的方法在盒上一条命令内取 token 完成推送（快进、未强推），main：`8d89d57e` → `a4c8e1d5`。
7. **回读核验**（经 PAT 只读回查）：main 头 = `a4c8e1d5`；`mailbox_handler.go` 在仓、blob 哈希与本地一致；被剔除的二进制文件抽查返回 404。
8. **本地主本同步规则**：本地落规则提交 `4d63a18e`（`.gitignore` 写入同一份忽略清单 + 37 文件移出索引；文件仍在本地盘上）。

## 3. 遇到的问题与解决

### 问题 1：普通推送被拒（non-fast-forward）

- **现象**：本地向上游式完整历史推送时被远端拒绝。
- **原因**：本地仓库是上游的完整历史，远端 bootstrap 是独立的根提交，两边历史不相干，不存在快进关系。
- **解决**：不强推、不丢弃远端提交——把信箱代码做成一笔快照提交，挂在 bootstrap 提交之后快进推送。

### 问题 2：部署密钥（deploy key）路线走不通

- **现象**：经 API 给仓库加 deploy key 返回 403。
- **原因**：现用的 fine-grained PAT 没有管理 deploy key 的权限。
- **解决**：弃用该路线，改走盒上推送（问题 5 的方法）。

### 问题 3：Git Data API 逐 blob 上传慢且卡

- **现象**：用 API 逐个上传整棵树（4,468 个 blob）极慢，中途被叫停时只传了 304 个。
- **原因**：逐 blob 串行上传本身低效，且把二进制文件也一并逐个传。
- **解决**：按用户路线序弃用该路（远端引用从未被动，304 个已传 blob 成为无引用孤儿，无损）；改盒上 git 原生推送，一次传完。

### 问题 4：二进制文件不应进仓

- **用户定规（2026-11-18）**：「写入忽略列表，不要推送二进制，二进制是自己编译」——二进制由使用方自行编译/恢复，不进仓库。
- **处理**：全树清点出 37 个二进制文件（36 张 `assets/partners/logos/` 合作商 logo 图 + 1 个 openspec 源码冻结包 `aicodex-prompt-audit-untracked.tar.gz`，共 3.03 MB），全部写入 `.gitignore` 并从推送树剔除；回读抽查 404 确认。需要这些文件时从上游 `Wei-Shaw/sub2api@a9240d5` 恢复。

### 问题 5：盒上 SSH 会话取不到 GIT_TOKEN

- **现象**：Colab 盒的 ssh shell 环境里没有 GIT_TOKEN，userdata API 从 ssh 也调不通，一度误判「CLI 盒无 token」。
- **实证（2026-11-18，25024 盒）**：token 在盒上常驻进程（opencode、codex 等）的进程环境里，ssh 登录 shell 只是不继承。
- **解决方法（用户 2026-11-18 亲述定规，现行标准做法）**：同步/推送在盒内**一条命令里完成**——遍历 `/proc/<pid>/environ` 找到带 `GIT_TOKEN=` 的进程环境，就地取进该命令的 shell 变量，立刻拼 `x-access-token:<token>` 的 base64 串，用 `git -c http.extraheader="Authorization: Basic …" push` 一次性推送。token **不打印、不写文件、不进 git 配置与远端 URL**，命令结束即消失。
- **路线序**：盒推为正路；取不到 token 或推送失败时，才回退本机 PAT 逐笔落地。

## 4. 现状

- 远端：layola13/sub2api main = `a4c8e1d5`（本文档提交后以仓库最新头为准）；历史 = bootstrap + 信箱快照提交，共两笔，树内无二进制文件。
- 本地主本：`~/workspace/mailbox-proto-sub2api/src/`，提交链 `3098d9cc7`（信箱模式）→ `4d63a18e`（忽略二进制规则）→ 本文档提交；本地保留完整上游历史与全部文件。
- 运行状态：无部署实例（见 §1.3 注）；纯 SLA 独立版未动工。
