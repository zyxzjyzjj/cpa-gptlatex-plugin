# prism-provider — 把 prism.openai.com 网页订阅接入 CLIProxyAPI

这是一个 **CLIProxyAPI (CPA) provider 插件**，注册名为 `prism-provider`。
它把 OpenAI 的 LaTeX 编辑器 [prism.openai.com](https://prism.openai.com) 的网页订阅，
包装成 OpenAI 兼容的 `chat-completions` 上游，于是你可以在 CPA 里像管理其它订阅一样集中管理它。

协议细节见 [`docs/prism-protocol.md`](docs/prism-protocol.md) —— 全文逐字摘自抓包与站点自身的前端代码。

---

## 1. 它做了什么

```
客户端 ──chat/completions──► CLIProxyAPI ──插件──► prism.openai.com
                                                    POST /api/llm/response_with_tools_start
                                                    POST /api/llm/response_with_tools_status (轮询)
```

一次请求的完整流程：

1. 用你提供的 **cookie** 调 `GET /auth/session`，取到 `openai_user_id`（形如 `user-…`）。
2. 选一个项目：凭据/配置里指定过就用它，否则复用本插件此前创建的（标题匹配 `project_title`，
   列表里最新那个），都没有才 `POST /api/projects` 新建（`project_uuid` 由客户端生成）。
   复用是必要的：缓存只在内存里，重启后就没了——2026-09-17 实测如此攒下了 14 个项目；
   而且 prism 在项目存储不可用时会用 503 拒掉**新建**，读取却正常，复用它就能照常工作。
3. 把 `messages` 转成 Prism 的 Responses 风格 `input` 数组。
4. `POST /api/llm/response_with_tools_start`，body 为
   `{input, previousResponseId, metadata:{projectId, userId, model, reasoning_effort, frontend_origin}, conversationId}`。
5. 若 `status=="started"`（或 `"pending"`）就每 5 秒 `POST /api/llm/response_with_tools_status`，
   body 为 `{request_id, turn_state}`，直到 `status=="completed"`。
6. 从 `response.payload.output` 里取 `type=="message" && role=="assistant"` 的 `output_text`，拼成回答。

`response.payload.reason == "SandboxReconnecting"` **不是错误**：网页端会等沙箱就绪后重发同一轮请求，插件同样重试最多 3 次。

---

## 2. 构建

需要 **Go 1.26**（与宿主保持一致；c-shared 动态库跨 Go 版本加载风险最大），
并且**插件本身必须用 CGO 构建**——宿主加载后只查找 `cliproxy_plugin_init` 一个符号。

宿主那边不一定需要 CGO：Windows 的 `loader_windows.go` 构建约束是 `//go:build windows`，
用 `syscall.LoadDLL` 就够；只有 Linux/macOS 的 `loader_unix.go` 才带 `cgo` 约束。

```bash
cd prism
./build.sh            # 自动找 C 编译器，产出 prism-provider.dll / .so / .dylib
make build            # 有 make 时等价
```

Windows 上也可以直接用批处理（会自动建好 `plugins\windowsmd64\`）：

```bat
build.bat
```

手动等价命令：

```bash
CGO_ENABLED=1 go build -trimpath -buildmode=c-shared -o prism-provider.dll .
```

**cgo 需要一个 C 编译器。** 这台机器上 `gcc` 不在 PATH 里（只有 Git 自带的 `mingw64` 目录，不含编译器），
已额外解包了一份可移植 MinGW-w64 到 `C:\zjj\toolchain\mingw64`，`build.sh` / `Makefile` 会自动回退到它。
要换编译器就设 `CC`：

```bash
CC=/path/to/gcc ./build.sh
```

> 扩展名必须对：macOS `.dylib`、Linux `.so`、Windows `.dll`。平台不匹配宿主会直接加载失败。
> 构建产物约 13 MB（Go 运行时是静态链进动态库的）。

### 版本与 ABI 约束（改代码前务必先读）

插件**刻意不依赖 `github.com/router-for-me/CLIProxyAPI/v7`**，`main.go` 里自己镜像了 ABI 常量。
原因不是洁癖，而是依赖它会导致插件**完全无法加载**：

- 宿主会拒绝 `schema_version` 高于自己的插件
  （`internal/pluginhost/rpc_client.go`：`plugin schema version %d is not supported`）。
- 而 `pluginabi.SchemaVersion` 随 CPA 版本漂移：本地 checkout 是 **v7.2.50 → 1**，
  v7.2.129 → 3，v7.3.4 → 6。之前 `go.mod` 锁的是 v7.2.129，
  于是插件对外声称 schema_version=3，被 v7.2.50 的宿主在注册阶段直接拒绝。
- 因此常量固定在 `schemaVersion = 1`：它是**唯一**能被上述所有宿主接受的取值，
  也正是本插件实际实现的协议版本。`abiVersion = 1` 同理。

删掉 CPA 依赖还顺带把依赖树从 50 MB 缩到只剩 `gopkg.in/yaml.v3`，并且不再需要 go1.26 工具链切换。

---

## 2.5 自检

两个层次的自检都是离线、不需要 cookie 的：

```bash
cd prism
go test ./...          # 17 个单元测试：配置解析、chat→prism 映射、响应封装、凭据
make check             # 真·加载动态库，按宿主 loader 的方式跑全部 RPC
```

`make check` 调用 [`tools/plugincheck`](tools/plugincheck)，
它按 `internal/pluginhost/loader_windows.go` 的**同一套结构体布局与回调约定**加载 `.dll`，
真的走 `cliproxy_plugin_init` → `cliproxyPluginCall`，覆盖 27 项断言：
`plugin.register`（含空配置、坏配置、`ConfigField.Type` 合法性、schema_version 上限）、
`auth.parse`（含拒绝异种凭据）、`model.static` / `model.for_auth`、
`executor.count_tokens`、未知方法、**空指针 method**（不做判空会段错误）、`plugin.shutdown`。

唯一测不到的是真正打到 `prism.openai.com` 的那几跳——那需要你的 cookie。

第三个层次是**真实宿主**：把 `.dll` 塞进一个本地构建的 CLIProxyAPI v7.2.50 里跑，
看宿主自己怎么报告。详见 [`tools/hostcheck`](tools/hostcheck)。

---

### 发布（Windows）

```bat
release.bat 0.1.1 --dry-run    :: 只做校验，不改任何东西
release.bat 0.1.1              :: 确认后：同步版本号 → 提交 → 推 master → 打 tag → CI 出包
release.bat 0.1.1 --yes        :: 跳过确认
```

它先用 `tools/set-version.ps1` 把 `main.go` 的 `var version`、`registry.json`、`registry-entry.json` 三处版本号同步成同一个值，再推 tag 触发 `release.yml`。前置校验：分支必须是 `master`、`origin` 必须存在、本地与远端都不能已有该 tag、版本号必须是点分数字形式。完整说明见 [docs/plugin-store.md](docs/plugin-store.md)。

---

## 3. 安装与配置

把动态库放到 CPA 的插件目录（按平台分目录更规范）：

```
<CLIProxyAPI 根目录>/
├── config.yaml
└── plugins/
    ├── prism-provider.so
    ├── linux/amd64/prism-provider.so
    └── windows/amd64/prism-provider.dll
```

**插件 ID = 文件名去掉扩展名**，也就是 `prism-provider`——`config.yaml` 里的配置键必须与它一致
（注意：是文件名，不是插件注册的 provider 名，两者这里刚好相同）。

`config.yaml`：

```yaml
plugins:
  enabled: true            # 全局开关，必须打开（商店安装不会自动打开它）
  dir: "plugins"
  configs:
    prism-provider:
      enabled: true
      priority: 1

      # ── 必填 ──────────────────────────────────────────────
      # 浏览器登录 prism.openai.com 后，把该站点的 Cookie 请求头整行复制过来。
      # 实测必需的两个（名称已从 Fiddler 抓包确认）：
      #   prism_session_token      —— 会话（GET /auth/session 会续期，Max-Age=43200）
      #   prism_oai_access_token   —— OpenAI 访问令牌（RS256 JWT）
      # 建议一并带上 cf_clearance / __cf_bm，否则可能被 Cloudflare 拦。
      cookies: "prism_session_token=...; prism_oai_access_token=...; cf_clearance=...; __cf_bm=...; __cflb=...; oai-did=...; prism-did=..."

      # ── 选填 ──────────────────────────────────────────────
      user_id: ""              # 形如 user-xxxx；留空自动从 /auth/session 探测
      project_uuid: ""         # 复用的项目 UUID；留空则首次调用时自动创建
      project_title: "CLIProxyAPI"
      # 领取 LaTeX 沙箱并完成工作区同步。**建议保持开启**：
      # 实测（2026-09-17）不提供沙箱时服务端只会一直返回 sandbox_reconnecting，
      # 整轮永远拿不到答案。插件会自动做完整握手（资源令牌 → y-sweet 令牌 →
      # Yjs socket 同步 → wait-for-sync），并按项目缓存复用，所以只有首次请求
      # 会多等几秒。默认开。
      sandbox: true
      reasoning_effort: "medium"   # low | medium | high | xhigh
      system_prompt: ""            # 客户端没给 system 消息时用这个
      # default_model / models 留空 = 用 prism 自己下发的模型清单（推荐）。
      # 只在需要固定模型时才填：
      # default_model: "gpt-5.6-sol"
      # models: ["gpt-5.6-sol", "gpt-5.6-terra"]
```

> **模型清单来自上游，不要写死。** prism 没有 `/models` 端点，网页端的可选模型是一次
> Statsig 动态配置（`prism_codex_models`，经同源 `/api/ff/initialize` 下发）。插件会带上
> 凭据去取同一份清单并缓存 30 分钟。2026-09-17 实测：抓包里还在用的 `gpt-6-astra`
> 隔天就被服务端以 `400 Error while processing conversation` 拒掉，线上实际可用的是
> `gpt-5.6-sol` / `gpt-5.6-terra`——写死就会这样无声失效。

> `models` 三种写法都认：单行 JSON 数组字符串（CPA 文档的写法）、真正的 YAML 列表、以及带 `id` 字段的对象数组。

> **模型清单是上游下发的，`models` 留空即可**；填了才覆盖（见上）。
>
> **首次跑通建议：把 `user_id` 和 `project_uuid` 都填上。** 两个都非空时会短路掉两次多余往返——`metadata.userId` 直接用你给的值（不再调 `GET /auth/session`），项目直接用你给的 UUID（不再调 `POST /api/projects`）。既省一次往返，也让你明确复用浏览器里已有的那个项目。
>
> 这两个端点本身**已经和抓包核对过**：`POST /api/projects` 的请求体/响应体逐字段一致（见 [`docs/prism-protocol.md`](docs/prism-protocol.md) §4.1 与 A6），`GET /auth/session` 的字段名也实测确认（同上）。
>
> `project_uuid` 就是浏览器地址栏里 `?u=` 后面那段：`https://prism.openai.com/?u=<project_uuid>&pg=1&m=main.tex`。

> **必须让 CPA 走代理，否则一定 403。** 本机直连 prism.openai.com 时出口 IP 会被 Cloudflare 判定为异常流量，**连首页都打不开**，且与 cookie 无关（详见 §4「如果返回 403」）。这是网络层的事，**不要写进插件配置**——给 CPA 进程设 `HTTPS_PROXY` 即可（插件用 Go 默认 transport，自动读取）。

改完配置后**重启（或重新安装）CPA**——动态库不会热替换。

---

## 4. 验证

管理 API 的路由是 **`/v0/management/...`**（不是 `/v1/`），且**即使本机访问也要带 `remote-management.secret-key`**，
不带 key 时所有 `/v0/management` 路由都是 404：

```bash
curl -s -H "Authorization: Bearer $CPA_MGMT_KEY" \
  http://127.0.0.1:8319/v0/management/plugins \
  | jq '.plugins[] | select(.id=="prism-provider")'
```

关键字段（下面这段是**在真实宿主里实测到的**输出，不是期望值）：

```json
{
  "id": "prism-provider",
  "configured": true,
  "registered": true,
  "enabled": true,
  "effective_enabled": true,
  "supports_oauth": true,
  "oauth_provider": "prism-provider",
  "config_fields": [
    {"name": "sandbox", "type": "boolean"},
    {"name": "reasoning_effort", "type": "enum", "enum_values": ["low","medium","high","xhigh"]}
  ]
}
```

模型列表（模型由 `model.for_auth` 挂在导入的凭据上，所以**必须先导入 cookie**，否则是空的）：

```bash
curl -s -H "Authorization: Bearer $CPA_KEY" http://127.0.0.1:8319/v1/models | jq
# {"data":[{"id":"gpt-5.6-sol","object":"model","owned_by":"prism-provider"}, ...]}
```

真实对话：

```bash
curl -s http://127.0.0.1:8319/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5.6-sol",
       "messages":[{"role":"user","content":"用一句话解释 LaTeX 的 \\label 有什么用"}]}'
```

### 让 Codex / ZCode 读取本机项目文件

插件默认开启**客户端工具桥**（`client_tools: true`）。远程 CPA 不直接读取你电脑的磁盘；它把 Prism 输出的工具请求翻译成标准 OpenAI `function_call` / `custom_tool_call`，由本机 Codex、ZCode 或其他 agent 客户端执行，再把工具结果发回下一轮。

```text
Prism 模型 → 标准 tool call → 本地 Codex/ZCode 执行 → tool result → Prism 最终回答
```

这适合远程 CPA + 本地工作区：Codex CLI 0.154.0 把工具定义放在 Responses 的 `input[].additional_tools` 中（含 namespace/custom `exec`），插件会识别该形状，也兼容标准 Chat/Responses `tools`。只有请求实际声明工具时才激活；普通聊天无额外提示词和解析开销。

安全边界：

- 工具由**客户端本机**执行，CPA 服务器不会得到本机文件系统权限。
- 插件只会返回客户端自己声明过的工具；模型伪造的未声明工具会被拒绝。
- 工具调用 ID 稳定，可用 `function_call_output` / `custom_tool_call_output` 或 Chat `role: tool` 回灌。
- 模型输出格式错误时最多纠正 `client_tools_max_rounds` 次（默认 2），不会执行任何工具。
- 若客户端只是普通网页、没有本地工具执行器，仍需要本地 companion/MCP Client；远程服务器无法凭空读取浏览器所在电脑的目录。

配置：

```yaml
plugins:
  configs:
    prism-provider:
      client_tools: true
      client_tools_max_rounds: 2
```

真实验收已覆盖：Responses `function_call`、Codex `additional_tools → custom_tool_call exec`、两种工具结果回灌，以及 Chat Completions `tool_calls`。

### 如果返回 403 / Cloudflare

**这不是 cookie 的问题，是出口 IP 的问题。** 2026-09-17 实测：本机直连时，**连首页 `/` 都会被拦**，返回 4577 字节的 `Attention Required!` 拦截页，而且**带不带 cookie 响应逐字节相同**；改走美国出口后，同一个请求 `/auth/session` 直接 200、首页 132 KB 正常渲染。所以补 `cf_clearance` 没有用，也**不需要**任何 TLS 指纹伪装。

修法是给 **CPA 进程**设代理环境变量。插件用的是 Go 默认 transport，会自动读这两个变量，**不需要改插件、也不要把代理写进插件配置**：

```bash
# Linux / macOS
HTTPS_PROXY=socks5://127.0.0.1:15732 ./cli-proxy-api -config config.yaml

# Windows（PowerShell）
$env:HTTPS_PROXY="socks5://127.0.0.1:15732"; .\cli-proxy-api.exe -config config.yaml
```

`http://`、`socks5://` 都支持；本机实测可用的是 `127.0.0.1:15732`（ViewTurbo，同时提供 HTTP 与 SOCKS5，也是这台机器的系统代理）。

设好之后，插件会正常打到 prism 的应用层。若此时仍报错，看到的就是 prism 自己的错误了，例如：

```json
{"error":{"message":"prism POST /api/llm/response_with_tools_start 返回 500: {\"status\":\"error\",\"message\":\"auth-session-policy-unavailable\"}"}}
```

这条的意思是**凭据无效**——也就是说网络已经通了，剩下才是 cookie 的事。

---

## 5. 已知限制

- **cookie 会过期。** 目前 `auth.refresh` 只做校验并把 `NextRefreshAfter` 定在 12 小时后；cookie 失效时会返回 401，需要重新粘贴 cookie。
- **流式连接已使用 CPA 的异步 `host.stream.emit/close`。** 插件立即返回合法的 Responses/Chat 首事件，后台等待 Prism，并每 10 秒发送保活；长请求实测 197 秒仍保持 HTTP 200、TTFB 约 5ms。Prism 当前仍主要在终态给出完整正文，所以不是逐 token 流；下一步可把 `codex_live_progress.eventPreviews` 直接翻成增量事件。
- **`executor.count_tokens` 是桩，恒返回 `total_tokens: 0`。** Prism 不回报 usage，也没有分词器；这里选择与 CPA 自带参考插件完全一致的返回，而不是编一个估算值。若客户端依赖它做上下文预检，会看到 0。
- **沙箱是必需的，不是可选项。** 插件执行完整握手（项目资源令牌 → y-sweet token → Yjs socket → `wait-for-sync`），每 10 秒 heartbeat 保活，按项目缓存 45 分钟；连续心跳失败或上游明确要求重连时才重新领取。
- **项目 UUID 与登录凭据绑定。** 凭据/配置中的 UUID 只作为候选，插件会先验证它属于当前账号；失效的旧项目会自动替换为当前账号已有项目并写回凭据，避免换账号后稳定 404。
- **历史仍由插件携带。** 普通请求保留文本消息；Responses replay 中无文本的 reasoning/tool 结构会跳过。客户端工具桥会把工具调用和结果改写成自洽的文本协议，并按 1.8MB 上限从最旧历史开始裁剪。
- **上游调用用标准库 `net/http`，没有走 `host.http.do` 桥。** 它会读取 CPA 进程的 `HTTP_PROXY`/`HTTPS_PROXY`，但不会进入宿主的统一请求日志策略。
- **模型清单由上游 Statsig 动态配置读取并缓存。** 请求模型会对当前目录校验；过期名称会在打上游前返回可用清单。

---

## 6. 文件一览

| 文件 | 作用 |
|---|---|
| `main.go` | C ABI 导出、方法分发、注册清单、信封编解码、宿主回调（`host.log`） |
| `config.go` | 解析 `plugins.configs.prism-provider` YAML |
| `auth.go` | `auth.parse` / `auth.refresh` / `model.static` / `model.for_auth` |
| `prism.go` | Prism HTTP 客户端：会话、建项目、start/status 轮询、沙箱 |
| `executor.go` | Chat/Responses 请求归一化、异步流、请求去重和响应封装 |
| `client_tools.go` | 客户端工具桥：工具定义归一化、提示协议、调用解析、结果回灌与历史裁剪 |
| `prism_test.go` / `client_tools_test.go` | 配置、消息、项目绑定、工具桥、响应协议和凭据回归测试 |
| `build.sh` | 找编译器并构建（无 make 时用这个） |
| `build.bat` / `release.bat` | Windows 上的构建与发布脚本 |
| `tools/set-version.ps1` | 把 tag 版本号同步进 main.go 与两份 registry |
| `tools/package-release.sh` | 打出商店要求的 zip 与 checksums.txt |
| `registry.json` / `registry-entry.json` | 第三方商店源与官方商店条目 |
| `Makefile` | 构建 / 测试 / 自检 / fmt / vet |
