# OpenCode + 飞书（Feishu）集成

让 OpenCode 变成一个飞书机器人：用户在飞书里发消息 → OpenCode 的 coding agent 处理 → 结果回发到飞书。

## 架构

```
飞书消息  →  HTTP 回调 (/feishu/callback)
                 ↓
         internal/feishu  (纯标准库实现，零外部依赖)
                 ↓  MessageFunc
         app.RunNonInteractiveCapture(prompt) → 文本
                 ↓
         通过飞书 OpenAPI 回复用户
```

新增文件：
- `internal/feishu/feishu.go` — 飞书回调服务 + OpenAPI 发消息（标准库 `net/http`）
- `cmd/feishu.go` — `opencode feishu` 子命令
- `internal/app/app.go` — 新增 `RunNonInteractiveCapture`（把 agent 结果写入 `strings.Builder`）

## 编译与运行

> 需要本机安装 Go（当前开发机未安装，装好后即可编译）。

```bash
cd opencode-feishu
go build -o opencode .
```

## 飞书后台配置

1. 打开 [飞书开放平台](https://open.feishu.cn/) → 创建「企业自建应用」
2. 获取 **App ID** 和 **App Secret**
3. 开通权限：`im:message`（读取/发送消息）、`im:message:send_as_bot`
4. 事件订阅：
   - 请求地址填：`https://<你的公网域名>/feishu/callback`
   - 订阅事件：`接收消息 v2.0`（`message`）
   - 复制「验证 Token」用于下面的 `FEISHU_VERIFICATION_TOKEN`
5. 将应用发布并添加到你的群或个人会话

> 本地调试可用 ngrok / frp 把 `:8080` 暴露到公网。

## 启动机器人

```bash
export FEISHU_APP_ID="cli_xxxxxxxx"
export FEISHU_APP_SECRET="xxxxxxxx"
export FEISHU_VERIFICATION_TOKEN="xxxxxxxx"
export FEISHU_PUBLIC_URL="https://your-domain.com"   # 仅日志提示用

./opencode feishu -c /path/to/your/project -a :8080
```

或在 `opencode.json` 里配置好 LLM provider（OpenAI / Anthropic / Gemini 等），机器人会自动复用。

## 说明

- 仅处理 `text` 类型消息；群聊会自动去掉 `@机器人` 前缀
- 回复超时设为 5 分钟
- 复用了 OpenCode 现有的 agent、权限（非交互模式自动放行）、会话与数据库能力
- 飞书 SDK 用官方 OpenAPI（tenant_access_token），未引入第三方依赖，便于离线编译
