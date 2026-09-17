# hostcheck — 在**真实宿主**里加载插件

`plugincheck` 用一个复刻宿主 loader 的小程序来验插件；这里更进一步：
把插件加载进**真正的 CLIProxyAPI v7.2.50 二进制**，看宿主自己怎么说。

```bash
# 1. 构建宿主（在用户的 CLIProxyAPI checkout 里，产物落到本目录，不污染他的仓库）
cd "/c/Users/zjj/Documents/New project/CLIProxyAPI"
CGO_ENABLED=0 GOFLAGS=-mod=readonly go build -o "C:\zjj\cpa-gptlatex-plugin\tools\hostcheck\cpa-host.exe" ./cmd/server

# 2. 换上新构建的插件
cp ../../prism/prism-provider.dll run/plugins/

# 3. 起服务（必须带代理，否则插件打上游会被 Cloudflare 403）
cd run
HTTPS_PROXY=socks5://127.0.0.1:15732 ../cpa-host.exe -config "$(cygpath -w "$PWD/config.yaml")" -local-model

# 4. 查询
curl -s -H "Authorization: Bearer hostcheck-mgmt-key" http://127.0.0.1:8319/v0/management/plugins
```

> 这台机器上 `127.0.0.1:15732` 是 ViewTurbo 提供的 HTTP/SOCKS5 代理，也是系统代理。
> 不加 `HTTPS_PROXY` 时插件会走直连，上游被 Cloudflare 403 拦下——**与 cookie 无关**（详见
> [`../../docs/prism-protocol.md`](../../docs/prism-protocol.md) 的 A1 表与 `prism/README.md` §4）。

Windows 上 `internal/pluginhost/loader_windows.go` 的构建约束是 `//go:build windows`，
**不要求 CGO**（只有 Linux/macOS 的 `loader_unix.go` 才要求），所以这个宿主用 `CGO_ENABLED=0` 构建即可。
注意插件本身**仍然必须是 cgo 动态库**。

## 已验证的结论（2026-09-16）

宿主日志：

```
pluginhost: plugin loaded     plugin_id=prism-provider
pluginhost: plugin registered plugin_id=prism-provider plugin_name=prism-provider version=0.1.0
```

管理 API `/v0/management/plugins`：

```json
{"id":"prism-provider","configured":true,"registered":true,"enabled":true,
 "effective_enabled":true,"supports_oauth":true,"oauth_provider":"prism-provider",
 "config_fields":[{"name":"sandbox","type":"boolean",...},
                  {"name":"reasoning_effort","type":"enum","enum_values":["low","medium","high","xhigh"],...}]}
```

放入 `run/auth/prism-test.json` 后，宿主调用 `auth.parse` + `model.for_auth`：

```
auth file changed (CREATE): prism-test.json, processing incrementally
Registered new model gpt-6-astra from provider prism-provider
Registered client prism-test.json from provider prism-provider with 2 models
```

`GET /v1/models` 返回 `gpt-6-astra` 与宿主按 `Prefix` 拼出的别名 `prism/gpt-6-astra`。

带**占位 cookie** 打一发真实对话，走通了 `executor.execute` 全链路直到上游：

```json
{"error":{"message":"prism POST /api/llm/response_with_tools_start 被 Cloudflare 拦截 (HTTP 403)：
 cookie 里缺少有效的 cf_clearance / __cf_bm，请在浏览器重新登录 https://prism.openai.com 后复制完整 Cookie 再更新配置"}}
```

这一条顺带证明了三件事：`prism.openai.com` 从本机可达（DNS/TCP/TLS 都通）、
`/auth/session` 这一跳通过了、以及**唯一剩下的缺口就是真实凭据**。

## 目录说明

| 路径 | 说明 |
|---|---|
| `cpa-host.exe` | CLIProxyAPI v7.2.50 的本地构建（58 MB，没 git 也能用；删了按上面第 1 步重建） |
| `run/config.yaml` | 隔离配置：127.0.0.1:8319、`plugins.enabled: true`、指向 `run/plugins` |
| `run/plugins/prism-provider.dll` | 被测插件 |
| `run/auth/prism-test.json` | **占位**凭据，仅用于验证 parse 链路，不含真实 cookie |

改完 `run/config.yaml` 宿主会热重载；换 `.dll` 必须重启进程（动态库不热替换）。
