# prism.openai.com 协议说明书

> 来源：`prism.openai.com.har`（2026-09-16 抓包，797,507 行）+ 站点自身发布的前端 JS 分片。
> 除标注 **【推断】** 外，本文所有 URL、方法、请求体字段、响应字段均为**逐字**摘自真实流量或官方前端代码，未做任何猜测。
> 关键突破：HAR 本身对响应体做了一定剥离，但**前端分片里带着完整的接口调用代码**。`11850yk199q3a.js` 是 AI 对话层，`0m_v952oghjll.js` 是沙箱/项目层。

---

## 一、认证

**是纯 Cookie 会话，没有 Bearer token。**

前端所有同源业务请求都带 `credentials: "include"`（少数 AI 接口为 `"same-origin"`），凭据由浏览器自动附带：

```js
// 11850yk199q3a.js —— 通用 AI 请求封装
async function j(e, t) {
  let i = await (0, B.fetchOnce)(e, t);          // 首次请求
  if (401 !== i.status) return i;                 // 非 401 直接返回
  try {
    let e = await (0, W.refreshPrismAuthState)(); // 401 → 刷新会话
    if (!e.user || !0 === e.user.is_anonymous) return i;
  } catch { return i; }
  return (0, B.fetchOnce)(e, t);                  // 刷新成功后重试一次
}
```

`GET /auth/session` 的真实响应（行 797,476，完整）：

```json
{"session":null,"user":{"id":"a80558b4-614e-5d20-bb5a-fee6f92b3420","email":"...","is_anonymous":false,
"app_metadata":{"user_id":"user-uVXTSCVQP7uI2G4CXbKC5shv","plan_type":null,"api_subscription_plan_types":[],"beta_program_enrolled":false},
"user_metadata":{"name":"..."}},
"policy":{"requires_openai_access_token_cookie":true,
  "account":{"auth_state":"siwc_linked","allowed_sign_in_providers":["openai"],"requires_migration_prompt":false},
  "user":{"id":"a80558b4-...","supabase_user_id":"a80558b4-...","prism_user_id":"prism_user_c4643049aeb88191ba7e9865984cd6af",
    "openai_user_id":"user-uVXTSCVQP7uI2G4CXbKC5shv","email":"...",
    "selected_workspace":{"account_id":"4f8703b4-0813-436f-8061-f9c80c1903f9","account_user_id":"user-uVXTSCVQP7uI2G4CXbKC5shv","kind":"personal"},
    "is_anonymous":false,"plan_type":null,"api_subscription_plan_types":[],"prism_chatgpt_account_consent_seen_at":null,"beta_program_enrolled":false,
    "identities":[{"provider":"openai","identity_data":{"email":"...","name":"..."}}]}},
"openAiLinkMigrationState":{"openAiIdentity":null,"legacyIdentities":[]},
"userTier":"logged_in",
"resolutionDiagnostics":{"reason":"signed_in","refreshed":true,"prismSessionReadFailure":null,"hadPrismSessionToken":true,"hadOpenAiRecoveryCredential":true},
"openAiRefreshAt":1790427925}
```

要点：

| 字段 | 含义 |
|---|---|
| `requires_openai_access_token_cookie: true` | 必须同时携带 **OpenAI access token cookie** |
| `hadPrismSessionToken: true` | 另有 **prism session token cookie** |
| `prism_user_id` | `prism_user_<hex>` |
| `openai_user_id` / `app_metadata.user_id` | `user-XXXX` —— **`/api/llm` 的 `metadata.userId` 用的就是这个** |
| `user.id` | 用户 UUID，用于 Yjs awareness / RUM |
| `selected_workspace.account_id` | 工作区，`/auth/workspaces` 用 |
| `userTier: "logged_in"` | 登录态判据 |

> ⚠️ **两个 cookie 的具体名字无法从 HAR 得到**——导出时 cookie 被整体剔除（所有条目 `"cookies": []`）。需要用浏览器 DevTools 或 `document.cookie` 之外的 HttpOnly 视角自行确认。前端只写 `credentials:"include"`，从不读 cookie 名。
> ⚠️ `401` 且响应头 `x-prism-auth-error: missing-openai-token` ⇒ OpenAI access token cookie 缺失/过期。

> ✅ **未登录 / 凭据无效时的实测形状（2026-09-17，经美国出口）**
> 不带任何 cookie：HTTP **200**，
> `{"session":null,"user":null,"policy":null,"openAiLinkMigrationState":null,"userTier":"logged_out","resolutionDiagnostics":{"reason":"no_session_credentials","refreshed":true,"prismSessionReadFailure":"missing","hadPrismSessionToken":false,"hadOpenAiRecoveryCredential":false}}`
> → **判登录态只能看 `userTier`，不能看 HTTP 状态码**（这里失败也是 200）。
> 带**无效** `prism_session_token`：HTTP **503**，`{"error":"auth-session-policy-unavailable"}`。

配额：`GET /auth/entitlements`（约 4 分钟轮询一次，`setInterval`）返回

```json
{"status":"resolved","subjectId":"a80558b4-...","planType":"free","apiSubscriptionPlanTypes":["free"],
 "source":"authoritative","serverTimeMs":1789565155628,"observedAtMs":1789565151322,
 "refreshAfterMs":1789565199322,"validUntilMs":1789651551322}
```

---

## 二、主机与基础路径

- 全部业务接口同源：**`https://prism.openai.com`**
- 前端应用 URL：`https://prism.openai.com/?u=<projectUuid>&pg=<page>&m=<mainFile>`（首页为 `/?pg=0`）
- 静态分片：`/_next/static/chunks/*.js`（Cloudflare 前置，源站在 Azure Blob）
- LaTeX 工具链：`https://crixet.s3.us-east-2.amazonaws.com/crixet_v4/dist`（`WORKSPACE_DIR = /code/crixet/workspace`，内部代号 **crixet**）
- 所有同源 API 请求都会带 `Referer: https://prism.openai.com/?u=<uuid>&pg=<n>&m=<file>`
- 响应头特征：`x-openai-proxy-wasm: v0.1`、`cf-cache-status: DYNAMIC`、`cache-control: no-store`；鉴权层另注入 `x-prism-auth-request-id`

---

## 三、AI 对话（核心）

来源：`11850yk199q3a.js`（HAR 行 ~605k 一带的 page_7 分片），逐字。

### 3.1 发起一轮对话

```js
async function Z(e, t, i, n, r) {          // e=input, t=previousResponseId, i=metadata, n=conversationId
  let o = performance.now();
  let s = await j("/api/llm/response_with_tools_start", {
    method: "POST",
    headers: { "content-type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ input: e, previousResponseId: t, metadata: i, conversationId: n })
  });
  if (!s.ok) throw Error(401 === s.status
    ? "Your OpenAI session has expired. Sign in again to continue."
    : `response_with_tools_start failed (${s.status} ${s.statusText})`);
  let a = await s.json();
  // 校验：a.request_id 必须是非空字符串
  if ("started" === a.status) {
    // a.turn_state 必须是对象
    return a;                              // → 进入轮询
  }
  if ("completed" === a.status && a.response && "object" == typeof a.response) {
    return a;                              // → 一轮就结束
  }
  throw Error("Invalid response_with_tools_start response");
}
```

**`POST /api/llm/response_with_tools_start`**

请求体：

```jsonc
{
  "input": [ /* OpenAI Responses API 风格的 item 数组，见 3.4 */ ],
  "previousResponseId": "…" ,      // 首轮为 undefined；后续轮用上一轮 payload.id
  "metadata": { /* 见 3.3 */ },
  "conversationId": "cdx1_…"       // 首轮为 null；后续轮用上一轮 payload.conversationId
}
```

响应体：

```jsonc
{
  "request_id": "…",               // 必填、非空字符串
  "status": "started" | "completed",
  "turn_state": { … },             // status=started 时必填，下轮原样回传
  "response": {                    // status=completed 时出现
    "status": "success" | "error",
    "payload": { … }               // success 见 3.5；error 见 3.6
  }
}
```

### 3.2 轮询

```js
async function X(e, t) {                   // e=request_id, t=turn_state
  let i = await j("/api/llm/response_with_tools_status", {
    method: "POST",
    headers: { "content-type": "application/json" },
    credentials: "same-origin",
    body: JSON.stringify({ request_id: e, turn_state: t })
  });
  if (!i.ok) throw Error(401 === i.status ? "Your OpenAI session has expired. Sign in again to continue."
    : `response_with_tools_status failed (${i.status} ${i.statusText})`);
  let n = await i.json();
  if ("pending" === n.status) {
    if (!n.turn_state || "object" != typeof n.turn_state) throw Error("Invalid response_with_tools_status pending payload");
    return n;                              // 继续轮询
  }
  if ("completed" === n.status && n.response && "object" == typeof n.response) return n;
  throw Error("Invalid response_with_tools_status payload");
}
```

**`POST /api/llm/response_with_tools_status`**

- 请求：`{ "request_id": "…", "turn_state": { … } }`
- 响应：`{ "status": "pending"|"completed", "turn_state": {…}, "response": {…} }`
- 轮询间隔：外层 `pollIntervalMs` 默认 **5000 ms**

外层驱动循环（逐字节选）：遇到 `response.payload.reason === SandboxReconnecting` 时**不算失败**，而是等待 `pollIntervalMs`、重新 `ensureSandboxConnection()`，然后**重发 start**：

```js
async function H(e) {
  let t = Math.max(0, Math.floor(e.pollIntervalMs ?? 5e3)), i = e.sleep ?? z;
  for (;;) {
    if (e.shouldAbort()) return { status: "aborted" };
    let n = await e.start();
    if (!(!(!n || "object" != typeof n || Array.isArray(n))
        && "completed" === n.status
        && "string" == typeof n.request_id && n.request_id.trim().length > 0
        && n.response?.status === "error"
        && n.response.payload?.reason === F.ResponseWithToolsErrorReason.SandboxReconnecting))
      return { status: "completed", response: n };
    for (e.onReconnectWait();;) {
      if (e.shouldAbort() || (await i(t), e.shouldAbort())) return { status: "aborted" };
      let r = await e.ensureSandboxConnection();
      if ("error" === r.status) return { status: "terminal_error", message: r.message, requestId: n.request_id };
      if (e.hasSandboxCredentials()) break;
    }
  }
}
// 调用点： H({ start, ensureSandboxConnection, hasSandboxCredentials, shouldAbort,
//              onReconnectWait, pollIntervalMs: 5e3 })
```

即：**start → 若 started 则 status 轮询 → 若 response.reason==SandboxReconnecting 则等沙箱就绪后重发 start**。

### 3.3 `metadata` 的确切构造

调用点原文（`t` 就是传给 start 的 `metadata`）：

```js
(let e = tw.current /* {projectId,userId,sandboxUrl,sandboxToken,sandboxState,sandboxProxyRequestDebug} */,
 t = { projectId: e.projectId, userId: e.userId, model: tn, reasoning_effort: tt },
 window.location?.origin && (t.frontend_origin = window.location.origin),
 e.sandboxUrl && (t.sandbox_url = e.sandboxUrl),
 e.sandboxToken && (t.sandbox_token = e.sandboxToken),
 e.sandboxProxyRequestDebug && (t.proxy_request_debug = JSON.stringify(e.sandboxProxyRequestDebug)),
 (n = r ? tY(r) : null) && (t.codex_listen_snapshot = JSON.stringify(n)))
```

| 字段 | 说明 |
|---|---|
| `projectId` | 项目 UUID —— **必须对应一个真实存在的项目** |
| `userId` | `user-XXXX`（来自 `/auth/session` 的 `openai_user_id`） |
| `model` | 见 3.7 |
| `reasoning_effort` | `low` / `medium` / `high` / `xhigh` |
| `frontend_origin` | `https://prism.openai.com` |
| `sandbox_url` | 沙箱代理基址 —— **AI 靠它执行工具（改文件、编译）** |
| `sandbox_token` | `gAAAAA…`（Fernet 风格） |
| `proxy_request_debug` | 可选，调试用 JSON 字符串 |
| `codex_listen_snapshot` | 可选，JSON 字符串 |

> ⚠️ **`sandbox_url` + `sandbox_token` 是把 LaTeX 编辑/编译能力交给服务端 Agent 的关键**。不带它们时服务端无法执行工具（很可能直接报错或退化为纯文本回答）。

### 3.4 `input` 数组的形状

`input` 是 **OpenAI Responses API 的 item 数组**。首元素是 system prompt：

```js
{
  type: "message", role: "system",
  content: [{ type: "input_text", text: makeSystemPrompt("ChatGPT", "Prism", i18n.language) }]
}
```

用户消息：

```js
{ type: "message", role: "user", content: [{ type: "input_text", text: "…" }] }
```

附件（逐字，`appendPromptAttachmentToMessage`）：

```js
// 文件
{ type: "input_file", filename: t.name, ...(t.projectPath ? { project_path: t.projectPath } : {}) }
// 图片
{ type: "detail": "auto", type: "input_image", image_url: t.url ?? "", file_name: t.name }
```

助手历史消息（逐字，来自 3.5 的 `output`）：`{type:"message", role:"assistant", content:[{type:"output_text", text, annotations:[]}]}`

**体积上限**：整个 `input` 序列化后若 > `18e5` 字节（1.8 MB），前端会把 system 消息里的 `input_text` 逐级截断（6000 → 3000 → 1500 字符）。

参考：真实的 `POST /api/codex/conversation-history` 请求体（HAR 行 240,716，逐字），可用于拉取历史会话：

```json
{"conversationId":"cdx1_6d6c2234-5155-4a19-90b1-234da4f5f716","order":"desc","limit":50,
 "userId":"user-uVXTSCVQP7uI2G4CXbKC5shv","projectId":"713f8452-fc7d-4919-b90c-c6bf422835cf"}
```
（注：该请求体由 HAR 逐字给出，其中 projectId 为 `713f8452-fc7d-4919-b90c-c6bf422835bf`，此处按原文抄录。）

### 3.5 成功响应的 `payload`

```js
let { output: U, id: q, conversationId: Y,
      codexDebug: ei, codexRequestDebug: er, codexListenSnapshot: ec,
      codexDeltaFiles: eh, codexExecMeta: ep } = V.payload;   // V = response, V.status === "success"
```

| 字段 | 说明 |
|---|---|
| `output` | item 数组，模型输出 |
| `id` | **下一轮的 `previousResponseId`**（`tR.current = q`） |
| `conversationId` | **下一轮的 `conversationId`**（变了就切换会话） |
| `codexDebug` | 调试对象 |
| `codexRequestDebug` | 调试对象 |
| `codexListenSnapshot` | `{conversation_id, workspace_session_id, codex_session_id, transcript_cursor}` |
| `codexDeltaFiles` | 数组，本轮改动的文件 |
| `codexExecMeta` | `{async_mode: "async"\|"blocking", async_fallback_used, async_job_id}` |

`output` 的 item 类型（逐字归纳）：

```jsonc
// 助手文本
{ "type":"message", "role":"assistant", "content":[
    { "type":"output_text", "text":"…", "annotations":[ {"type":"url_citation","url":"…","title":"…"} ] } ] }
// 思维链
{ "type":"reasoning", "summary":[ {"type":"summary_text","text":"…"} ] }
// 工具调用
{ "type":"function_call" | "function_call_output", … }
// 文件改动
{ "type":"apply_patch_call", "operation":{ "type":"create_file"|"update_file"|"delete_file", "path":"…", "diff":"…" } }
{ "type":"apply_patch_call_output", … }
```

> 提取纯文本：筛选 `type==="message" && role==="assistant"`，拼接 `content[].type==="output_text"` 的 `text`。前端还有一个「空回答」哨兵：若末条输出文本恰为 `"Codex did not produce an answer"`，会**重试一次**。

**✅ 2026-09-17 复核：真实成功响应在抓包里，逐字如下**（此前误记为"未捕获"）。`prism.openai.com.har` 里 `POST /api/llm/response_with_tools_status` 共 10 条，其中一条 HTTP 200、响应体 3109 B：

```jsonc
{"status":"completed","request_id":"16f9426b-6de9-44c5-8886-b552037caec5","codex_async_job_id":"1663",
 "response":{"status":"success","payload":{
   "id":"resp_mu44y9j3_g17m9dtz",
   "output":[{"id":"msg_mu44y9j3_cgo4zx0r","type":"message","role":"assistant","status":"completed",
              "content":[{"type":"output_text","text":"What would you like me to do with `main.tex`?","annotations":[]}]}],
   "conversationId":"cdx1_aa3f4d64-5035-4acc-88be-2bacb4bc7177",
   "codexDebug":null, "codexRequestDebug":{…}, "codexListenSnapshot":{…}, "codexDeltaFiles":[…]}}}
```

- **注意 `status` 在两层**：外层 `"completed"` 表示这一轮结束，内层 `response.status` 是 `"success"` / `"error"`。两层都要判。
- 全文见 [`prism/testdata/status_completed.json`](../prism/testdata/status_completed.json)（**原始字节，未做任何改动**），`prism/prism_test.go` 的 `TestRealCompletedStatusResponse` 直接把它喂给解析器做回归。

**`start` 的响应形状（逐字，同样来自抓包）**：

```jsonc
{"status":"started","request_id":"16f9426b-…","conversation_id":"cdx1_aa3f4d64-…",
 "turn_state":{"version":1,"conversation_id":"cdx1_aa3f4d64-…","snapshot_id":null, …}}
```

拿到 `"started"` 就**必须轮询** `response_with_tools_status` 并把 `turn_state` 原样回传——这就是那个端点存在的理由。另外也观察到 `start` 直接返回 `{"status":"completed", …, "response":{"status":"error","payload":{"reason":"unknown","message":"Error while processing conversation (500 Inter…"}}}` 的情形，即还没开跑就失败。

### 3.6 失败响应

```js
if ("error" === V.status) {
  // V.payload: { reason, message, rootCause, messageKey, httpStatus, codexRequestDebug }
}
```
`reason === ResponseWithToolsErrorReason.SandboxReconnecting` → 走 3.2 的重连重试。
`reason === ResponseWithToolsErrorReason.ConversationTooLarge` → 需要开新会话。

**还有一种失败在「外层」，不在 `response.payload` 里（2026-09-17 实测）**：HTTP **500**，顶层 body 直接是

```json
{"status":"error","message":"auth-session-policy-unavailable"}
```

没有 `request_id`、没有 `turn_state`。实测于凭据无效时。

> 解析器必须在检查 `request_id` **之前**先看顶层 `status`。否则会把它误报成"未返回 request_id"（丢掉真正原因），并且轮询循环会把这类**终止性**错误一直重试到超时——`response_with_tools_status` 也可能返回同样的顶层 `status:"error"`。

### 3.7 模型与思考档位

- 模型列表：前端 `useAvailableCodexModels()`；默认项 `DEFAULT_CODEX_MODEL_OPTION`（模块 312670，字面量未在本轮读取中捕获）。
- 思考档位（逐字）：`[{value:"low"},{value:"medium"},{value:"high"},{value:"xhigh"}]`
- 自定义 `sandbox_url`/`sandbox_token` 由 `metadata` 透传，因此**模型名由 `metadata.model` 决定**，与 URL 无关。

### 3.8 中断

```js
// POST /api/llm/response_with_tools_stop
body: JSON.stringify({ request_id: e.requestId, conversation_id: e.conversationId, turn_state: e.turnState })
```

---

## 四、项目

### 4.1 创建项目

```js
// DEFAULT_PROJECT_NAME = "New Project"
async function l_(e, t, i) {                // e=title, t=project_uuid, i=file_uuids
  let o = await fetch("/api/projects", {
    method: "POST",
    headers: { accept: "application/json", "content-type": "application/json" },
    body: JSON.stringify({ project_uuid: t, title: e, file_uuids: i }),
    credentials: "include"
  });
  let r; try { r = await o.json(); } catch { r = undefined; }
  if (!o.ok) return { status: "error", message: /* r.error | r.detail | `Failed to create project: ${o.status} ${o.statusText}` */ };
  if (typeof r?.uuid !== "string") return { status: "error", message: "Invalid project response" };
  return { status: "success", payload: r };
}
```

- `POST /api/projects`，body `{ project_uuid, title, file_uuids }` → 响应必须含 `uuid`
- **`project_uuid` 由客户端生成**（UUID v4），不是服务端分配的

**✅ 已由真实流量验证（2026-09-17）**，与上面的源码逐字段一致：

```
请求  {"project_uuid":"713f8452-fc7d-4919-b90c-c6bf422835bf","title":"新建项目","file_uuids":[]}
响应  {"uuid":"713f8452-fc7d-4919-b90c-c6bf422835bf","created_at":"2026-09-16T13:11:42.440675+00:00",
       "deleted":false,"owner":"a80558b4-614e-5d20-bb5a-fee6f92b3420",
       "public_role":null,"thumbnail_url":null,"thumbnail_uuid":null,"title":"新建项目"}
```

（响应 200。`title` 由前端传入，网页端默认是 `"新建项目"`。）

### 4.2 访问权

```js
await fetch(`/api/project-access?d=${encodeURIComponent(e)}`, {
  method: "GET", credentials: "include", cache: "no-store",
  headers: { Accept: "application/json" }
});
// 403 → {accessible:false, project:null, userRole:null, accessDenied:true}
```

200 响应（HAR 行 796,825，逐字）：

```json
{"accessible":true,"project":{"uuid":"1a1084c2-d548-453e-b521-808a23826ca3",
 "created_at":"2026-09-16T12:51:36.380317+00:00","deleted":false,
 "owner":"a80558b4-614e-5d20-bb5a-fee6f92b3420","public_role":null,
 "thumbnail_url":null,"thumbnail_uuid":"3930d0ca-6590-494a-8959-986d53cb7938","title":"New Project"},
 "userRole":null}
```

### 4.3 缩略图

```js
await fetch(`/api/projects/${encodeURIComponent(e)}/thumbnail`, {
  method: "PATCH",
  headers: { accept: "application/json", "content-type": "application/json" },
  body: JSON.stringify({ thumbnail_uuid: t })
});
// 响应须含 project 字段，否则报 "Thumbnail update response was missing project metadata"
```

### 4.4 文件上传 / 读取

```js
await fetch("/api/project-files/upload", {
  method: "POST",
  headers: {
    "content-type": e.type || "application/octet-stream",
    "x-prism-file-id": t,                              // 客户端生成的 UUID
    "x-prism-file-name": encodeURIComponent(e.name || t),
    "x-prism-file-size": String(e.size),
    "x-prism-project-id": i,
    "x-prism-require-project-edit-access": n ? "true" : "false"
  },
  body: e,                                             // 原始 File/Blob
  credentials: "include"
});
// 响应须含 id → 返回 o.id
```

- 文件内容：`GET /api/projects/{uuid}/files/{fileId}/content`
- 并发上传 10，重试 3 次、退避 `1000*(n+1)` ms
- 缩略图上传后调 4.3

### 4.5 其他项目接口（逐字）

```
GET    /api/projects/{uuid}/collaborators
GET    /api/projects/{uuid}/mentionable-users
POST   /api/projects/{uuid}/user-avatars        body {owner_uuids:[…]} → {users:[…]}
PATCH  /api/projects/{uuid}/invites/{id}        body {role}
DELETE /api/projects/{uuid}/invites/{id}
PATCH  /api/projects/{uuid}/collaborators/{id}  body {role}
DELETE /api/projects/{uuid}/collaborators/{id}
POST   /api/project-invites/{uuid}/accept
GET    /api/projects/{uuid}/render-results/latest?main_document=<file>
POST   /api/metrics                             body {type,name,value,tags?}
```

`/api/projects/{uuid}/render-results/latest` 响应（前端逐字用法）：

```js
let u = await l.json();
let d = typeof u.output_pdf_object_name === "string" ? u.output_pdf_object_name : undefined;
// 拿到 object name 后再 getFile(...) 取 PDF 字节
let m = { content: new Uint8Array(await c.arrayBuffer()), status: "success", isBeamer: u.is_beamer ?? false };
```
未编译过时返回 **404**（`content-type: application/json`，约 198 字节）。前端 1s 一次、最多重试 10 次。

### 4.6 文件管理（首页/侧栏）

```
GET    /api/file-management/groups
POST   /api/file-management/groups                       body {name} → {group}
PATCH  /api/file-management/groups/{id}                  body {name} → {group}
DELETE /api/file-management/groups/{id}
GET    /api/file-management/projects?section=your_projects
PUT    /api/file-management/projects/{id}/folder         body {group_id} → {placement}
DELETE /api/file-management/projects/{id}/folder
```

### 4.7 工作区

```
GET  /auth/workspaces   (credentials:"include", cache:"no-store") → {workspaces:[…]}
POST /auth/workspaces   body {accountId}   → 切换后 window.location.assign("/")
```
仅在 Statsig gate `prism_workspace_switcher` 打开且 `requires_openai_access_token_cookie` 时启用。

---

## 五、沙箱与 LaTeX 编译

内部代号 **crixet**。沙箱代理基址（沙箱 URL 的 pathname 就是 `/s/sandboxes/proxy`）：

```
/s/sandboxes/proxy[/…]   →  /s/sandbox-resources[/…]     (getSandboxResourcesBaseUrlFromSandboxUrl)
/sandboxes/proxy[/…]     →  /sandbox-resources[/…]
```
每个沙箱请求都会追加 `prism_cache_bust=<random>`（`Math.floor(Math.random()*Number.MAX_SAFE_INTEGER)`），**只对 `/s/sandboxes/proxy*` 路径生效**。

### 5.1 拿沙箱 token

```
POST /api/projects/{uuid}/sandbox/resources-token
     headers: credentials:"include", Content-Type: application/json
     body:    { sandbox_session_id: <string|null>, sandbox_token: <gAAAAA…> }
     retries: 3, baseDelayMs 1000, backoffFactor 1
     shouldRetry: 429 || (500..599)
  → 200 { access_token: <string>, resources_base_url?: <string>,
          expires_at?: <unix秒>, max_age_seconds?: <秒> }
```

拿到 `access_token` 后再与沙箱代理交换：

```
POST {sandboxUrl}resources-token
     headers: X-Crixet-Sandbox-Token: <sandboxToken>, Content-Type: application/json
     body:    { token: <access_token>, resourceBaseUrl: <resources_base_url 或换算得到>, projectId: <uuid> }
     retries: 3, baseDelayMs 1000, backoffFactor 1
     shouldReturnResponse: 501, shouldRetry: 502
  ok  → credentialToken = access_token（用于后续工具/文件访问）
  501 | 404 | (502 && isSandbox502ReprovisionFallbackEnabled) → 需要重新 provisioning
```

同步 access token（用于把宿主 token 灌进沙箱）：

```
POST {sandboxUrl}token
     headers: X-Crixet-Sandbox-Token: <sandboxToken>, Content-Type: application/json
     body:    <normalized 对象，url/baseUrl 会被 s9() 归一化>
     retries: 1, shouldRetry: 404, 超时 10s
```

### 5.2 心跳（保活）

```js
let u = new Headers({ "Content-Type": "application/json" });
u.set("X-Crixet-Sandbox-Token", m.sandboxToken);
let f = <sandboxUrl 末尾补 "/" 后接 "heartbeat">，再过 addSandboxProxyCacheBust();
await fetch(f, { method: "GET", headers: u, signal });     // 超时 25s
```
- 默认间隔 `HEARTBEAT_INTERVAL_MS = 1e4`（10s）
- 502 重新 provisioning 最多 3 次；连接错误指数退避上限 60s
- 真实抓包：单次阻塞 4–19 秒（响应头 `openai-processing-ms`），返回体 2 字节 `{}`
- 响应头 `x-session-id` = 沙箱会话 ID（`getSessionIdFromResponseHeaders` 读 `x-session-id` 或 `X-Session-Id`）

错误判据（逐字）：

```js
isSandboxExpiredResponse(e) = 502 === e.status && e.headers.get("x-crixet-sandbox-expired")?.toLowerCase() === "true"
isSandbox502ReprovisionFallbackEnabled(e) = e.headers.get("x-crixet-sandbox-502-reprovision-fallback")?.toLowerCase() !== "disabled"
// 另有：502 且 x-caas-proxy-internal-error === "connection_error" → 退避
//       401 且 x-prism-auth-error === "missing-openai-token"  → 认证缺失
//       502 + x-crixet-sandbox-expired: true → 需要新沙箱
```

沙箱获取（新后端）走 Worker 消息 `crixet:new-backend`，锁定超时 30s；对应抓包里的 `POST /api/backend/1/new`。

### 5.3 编译（render）—— 异步 202 协议

```js
// 基址 t = <sandboxUrl>（已 ensureTrailingSlash）
// 首次请求：POST {t}render  —— 响应可能是 PDF（直接成功）或 JSON 进度对象
function sR(e) { return (200 === e.status && e.headers.get("Content-Type")?.toLowerCase().startsWith("application/pdf")) ?? false; }
function sL(e) { return !!e && "object" == typeof e && "rendering" === e.status; }
function sz(e) { return !!e && "object" == typeof e && "complete"  === e.status; }
async function sF(e) {   // 解析 JSON 进度
  if (e.headers.get("Content-Type")?.toLowerCase().includes("application/json")) {
    try { let t = await e.clone().json(); if (sL(t) || sz(t)) return t; } catch { return; }
  }
}

// 轮询循环（sq）
let i;                                  // 覆盖 pollAfterMs
if (sR(e)) return e;                    // 已是 PDF，直接成功
let o = await sF(e), s = sL(o) ? o : undefined;
if (!s) return e;                       // 不是 rendering → 原样返回（可能是错误）
let l = s;                              // 最近一次 rendering 对象，用于取 cancelPath/statusPath
const u = () => fetch(l.cancelPath ? new URL(l.cancelPath, t) : `${t}render-cancel`,
                      { method: "POST", retries: 1, headers: { "X-Crixet-Sandbox-Token": a } }).catch(() => {});
const c = Date.now();
for (; s;) {
  if (Date.now() - c > 84e4) { u(); return 504 render_status_timeout; }   // 14 分钟上限
  await sU(i ?? s.pollAfterMs ?? 1e3, r);                                // 默认 1s
  i = undefined;
  let e = await n(s.statusPath ? new URL(s.statusPath, t).toString() : `${t}render-status`, {
    signal: r, retries: 5, baseDelayMs: 1e3, backoffFactor: 1.5,
    shouldReturnResponse: e => 504 === e.status,
    headers: { "X-Crixet-Sandbox-Token": a }
  });
  if (504 === e.status) { i = 1e3; s = l; continue; }                    // 重试，沿用旧状态
  if (202 === e.status && e.headers.get("Content-Type")?.toLowerCase().startsWith("application/pdf")) { s = l; continue; }
  if (sR(e)) return e;                                                    // 变 PDF → 成功
  let o = await sF(e);
  if (sz(o)) {                                                            // complete → 取结果
    l = undefined;
    return await n(o.resultPath ? new URL(o.resultPath, t).toString() : `${t}render-result`,
                   { retries: 5, baseDelayMs: 1e3, backoffFactor: 1.5,
                     headers: { "X-Crixet-Sandbox-Token": a } });
  }
  if ((s = sL(o) ? o : undefined) && (l = s), !s) return e;
}
```

**协议总结**

| 步骤 | 请求 | 响应 |
|---|---|---|
| 触发编译 | `POST <sandboxBase>/render`（头 `X-Crixet-Sandbox-Token`） | `200` + `application/pdf`（直接成功）<br>或 `202` + JSON `{status:"rendering", statusPath?, pollAfterMs?}` |
| 轮询 | `GET <sandboxBase>/render-status`（或 `statusPath`） | `{status:"rendering", pollAfterMs?}`<br>`{status:"complete", resultPath?}`<br>`504`（重试）<br>`202` + PDF（继续）<br>`200` + PDF（成功） |
| 取结果 | `GET <sandboxBase>/render-result`（或 `resultPath`） | PDF 字节 |
| 取消 | `POST <sandboxBase>/render-cancel`（或 `cancelPath`） | — |

- `sandboxBase` 形如 `https://prism.openai.com/s/sandboxes/proxy/<sandboxId>/`（`ensureTrailingSlash` 后）
- 轮询默认 **1000 ms**（`pollAfterMs` 可覆盖），重试 5 次、退避 1.5×
- 总超时 **840000 ms（14 分钟）**，超时返回 `{success:false, error_code:"render_status_timeout", message:"Timed out waiting for sandbox render status."}` + HTTP 504
- 抓包里 `GET /s/sandboxes/proxy/heartbeat?prism_cache_bust=…` 是同一套信封；`POST /s/sandboxes/proxy/render` 返回 **202** 与用户描述一致
- **稳定性技巧**（逐字）：路径末段为 `render` / `render-status` / `render-result` 且响应是 PDF 时，把内部重试计数清零

### 5.4 结构化沙箱错误

```jsonc
{ "error_code": "…",
  "error": { "error_message": "…", "error_name": "…" },
  "message": "…",
  "request": { "method": "…", "path": "…" },
  "extra": { "stage": "…" },
  "debug_state": …, "recent_errors": […] }
```

---

## 六、文件同步（Yjs）

### 6.1 拿 socket token

```js
await fetch("/api/y", {
  method: "POST",
  credentials: "include",
  headers: { "Content-Type": "application/json" },
  body: JSON.stringify({ docId: e, requestContext: { ...context, maxAttempts: n, requestSeriesId: i } }),
  retries: n /* 默认 5 */, baseDelayMs: 500,
  shouldRetry: e => (429 === e.status || (e.status >= 500 && e.status < 600))
});
// 失败信息：{error:"Failed to get client token: HTTP <status>"}
```

### 6.2 WebSocket

```
wss://prism.openai.com/y/d/{docId}/ws/{docId}?token=<base64>
```
- 请求头：`Upgrade: websocket`、`Origin: https://prism.openai.com`、`Sec-WebSocket-Version: 13`、`Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits`
- **无 cookie**——鉴权全靠 `?token=`（由 `/api/y` 签发，base64 里明文含 docId 与 userId）
- 响应 `101 Switching Protocols`
- 协议是**标准 y-websocket / y-protocols 二进制帧**（`opcode: 2`），非 JSON：
  - 首帧 SyncStep1 `AAAIAYOzh8cE8wI=` → 服务端回 SyncStep2/全量 doc state
  - `AAECAAA=` = sync step 2（空 state vector）
  - `ZgEA` / `ZgEB` = 3 字节保活，约 2–2.5s 一次，双向
  - `AZUBAcDb2NQJ…` = awareness 更新，payload 内嵌明文 JSON：
    `{"user":{"userId":"a80558b4-…","name":"…","backgroundColor":"#640002","foregroundColor":"#FF5659"}}`

### 6.3 文档数据模型

每个文件是一个 Yjs 文档。共享类型里的 key（从抓包文档同步帧逐字解出）：

```jsonc
{
  "content$<fileUuid>": {
    "id":       "<fileUuid>",
    "filename": "main.tex",          // 文件夹为 "root"
    "type":     "folder" | "text",
    "inFolder": "<父文件夹 uuid>",     // 可选
    "deleted":  true|false,
    "settings": …,                    // 初始化时出现
    "comments": …,
    "content":  "\\documentclass…"     // 仅 type=text；LaTeX 源码本体
  }
}
```

抓包实例（page_3，13:16:07）：文件夹 `edbbf1db-f779-4bb8-9482-4a48a591883e`（filename `root`）内含文本文件 `d49c6eab-f202-4fef-899e-e7b125d62685`（filename `main.tex`），`content` 即 LaTeX 源码；另有 `866fbfcb-227b-4893-af94-6fde3517650e`（filename `1111.tex`，`type: text`，空 content）。

> **要写 .tex 文件，就得走这条 Yjs 通道**（`/api/y` 拿 token → 连 wss → 改 `content$<uuid>.content`）。`POST /api/project-files/upload` 是另一条路（上传二进制/附件）。

---

## 七、其余端点

```
GET  /auth/session                      会话与身份
GET  /auth/entitlements                 配额（约 4 分钟轮询）
GET  /auth/workspaces / POST /auth/workspaces
GET  /api/maintenance                   前端每 30s 轮询（仅页面可见时），{mode:"off"|"banner"|"full",message:null|string}
POST /api/metrics                       {type:"count"|"distribution",name,value,tags?}
GET  /api/backend/1/new  (POST 1/new)   新沙箱后端
POST /api/codex/conversation-history    {conversationId,order:"desc",limit:50,userId,projectId}
GET  /api/codex/runtime/debug?conversation_id=… → {ok,conversation_id,snapshot}
```

Next.js Server Action（部分写操作走这里，不是 REST）：`POST /?u=<uuid>&pg=<n>&m=<file>`，带 `next-action: <hash>` 头。抓包中出现过 `next-action: 006ca0b20631d0bf126aca7fdd7ec262cdc8c5956e`，body `[]` → 响应 `1:{"exists":false}`；以及 body `[{"projectId":"…","conversationIds":["cdx1_…"]}]`。

---

## 八、实现要点与坑

1. **鉴权只能靠 cookie**（`credentials:"include"`）。要程序化调用，必须把浏览器里那套 cookie（prism session token + OpenAI access token cookie）原样带上；缺 OpenAI 那个会拿到 `401 x-prism-auth-error: missing-openai-token`。
2. **`/api/llm/*` 用 `credentials:"same-origin"`**，同源场景下与 `include` 等价。
3. **401 会自动刷新重试一次**——客户端也应实现：401 → 刷新会话 → 重试一次。
4. **`metadata.userId` 用的是 `user-XXXX`**（`openai_user_id`），不是用户 UUID。搞混会导致会话归属错误。
5. **`sandbox_url` / `sandbox_token` 必须放进 `metadata`**，否则服务端 Agent 没有工具执行环境。
6. **所有同源请求都要带 `Referer: https://prism.openai.com/?u=<uuid>&pg=<n>&m=<file>`**，抓包中无一例外。
7. **`project_uuid` 由客户端生成**（v4），创建项目时一并提交。
8. **编译是异步 202**：`render` → `render-status`（默认 1s）→ `render-result`，总超时 14 分钟，任何时刻可 `render-cancel`。
9. **沙箱请求都要加 `prism_cache_bust`**（随机整数），且只在 `/s/sandboxes/proxy*` 路径上加。
10. **心跳 10s 一次**，单次会阻塞数秒到十几秒；响应头 `x-session-id` 是沙箱会话 ID，需回填到后续 `resources-token` 调用里。
11. **`input` 上限 1.8 MB**，超了要截断 system 消息。
12. **`output` 里混着 message / reasoning / function_call / apply_patch_call**，取纯文本要过滤 `type==="message" && role==="assistant"`。
13. **`SandboxReconnecting` 不是错误**，是「等沙箱就绪后重发 start」的信号。
14. 前端分片名是内容哈希，会随发版变化；本文引用的 `11850yk199q3a.js` / `0m_v952oghjll.js` 是 2026-09-16 的版本。
15. **HAR 导出缺陷**：`response.content.text` 在多数 API 条目里被剥离（只剩 `size`/`mimeType`），`postData` 也常在 POST 上缺失。本次能补全协议，靠的是站点自身发布的前端 JS。后续再抓包建议直接导出 `log.entries[]` 为每行一条 JSON，或关闭 `_initiator` 栈采集。

---

# 附：第二次抓包实测（`111.har`）

第二份抓包是 **Fiddler** 导出（`exported @ 2026/9/16 23:20:27`），与第一份 WebInspector 导出互补：**它保留了真实 cookie 和完整请求/响应体**。以下均为逐字实测值，用于替换/补强上文中的推断。

## A1. Cookie 名字（本次确认）

| Cookie | 说明 |
|---|---|
| `prism_session_token` | prism 会话 JWT。`GET /auth/session` 会 `set-cookie` 续期，`Max-Age=43200` |
| `prism_oai_access_token` | OpenAI 访问令牌，RS256 JWT，`aud=["https://api.openai.com/v1"]`，`client_id=app_jqKb52JverFFcl5GP4axT8QY`，含 `chatgpt_account_id`、`chatgpt_plan_type`、`email`、`exp` |
| `prism_oai_refresh_token` | 只在 `GET /auth/entitlements` 上附带（`rt.1.…`），并配 `prism_oai_earliest_refresh_at=<unix>` |
| `cf_clearance` / `__cf_bm` / `__cflb` / `_cfuvid` | Cloudflare。**⚠️ 2026-09-17 更正：这里 403 的根因是出口 IP 被判定为异常流量，不是缺 cookie。** 实测：本机直连（无代理）时连首页 `/` 都是同一个 4577 字节的 `Attention Required!` 拦截页，而且**带不带 cookie 响应逐字节相同**（同为 4577 字节）；改走美国出口后，同一个 `GET /auth/session` 直接 **200**、首页返回 132 KB 正常渲染。所以修复方式是**给进程设 `HTTPS_PROXY`**，而不是去补 `cf_clearance`。这一步同时排除了"需要 TLS 指纹伪装"的猜测 |
| `oai-did` / `prism-did` | 设备 id |
| `oai-sc` | 会话凭据（`0gAAAAAB…`） |
| `YSWEET_OFFLINE_KEY` | Yjs 离线缓存键 |
| `_dd_s` / `_ga` / `_uetvid` / `oaicom-stable-id` / `__obi` / `crixet_light_mode_preference_v1` | 遥测/偏好，可省 |

> 服务端还有一个 `POST /auth/session`（注意是 **POST**）用于创建/刷新会话；n 匿名态由 `POST /api/auth/anonymous-session` 建立，之后 `POST /api/auth/claim-anonymous-projects` 认领匿名项目。

## A2. 沙箱领取 —— 就是这一步拿到 `sandbox_url` / `sandbox_token`

上文 §3.3 说「`sandbox_url`+`sandbox_token` 必须放进 metadata」，但没说它们从哪来。实测答案是：

```
POST https://prism.openai.com/api/backend/1/new
     Cookie: <会话 cookie>          # 无 body、无自定义头
  → 200 {"url":"https://prism.openai.com/s/sandboxes/proxy",
         "token":"gAAAAAB…" }
```

注意响应键是 **`url` / `token`**（不是 `sandbox_url` / `sandbox_token`）。冷启动耗时约 2.5 s。

> ⚠️ 这里的 `url` **结尾没有斜杠**，而放进 `metadata.sandbox_url` 的是**带斜杠**的 `https://prism.openai.com/s/sandboxes/proxy/`（见 §3.3 的逐字 metadata）——**客户端要自己补**，否则服务端拼不出正确的子系统地址。
>
> 2026-09-17 复核：`111.har` 里三次 `/api/backend/1/new` 的响应体均为逐字 `{"url":"https://prism.openai.com/s/sandboxes/proxy","token":"gAAAAAB…"}`（bodySize 941 / 936 / 943），确认无斜杠。

拿到后再换资源令牌（这才是 §5.1 的真实请求体）：

```
POST https://prism.openai.com/s/sandboxes/proxy/resources-token?prism_cache_bust=2703035207070789
     X-Crixet-Sandbox-Token: <上一步的 token>
     Content-Type: application/json
     body: {"token":"eyJhbGci…(sandbox_resources JWT)…",
            "resourceBaseUrl":"https://prism.openai.com/s/sandbox-resources/",
            "projectId":"ee44e860-80ab-4366-aed1-df9748f3007e"}
  → 200 {"status":"success"}   响应头 x-session-id: 97a661cb98754906b0111617c7ac5d9c
```

## A3. `POST /api/llm/response_with_tools_start` —— 实测请求体

**模型是 `gpt-6-astra`**（本次实测值），`reasoning_effort: "medium"`。

```jsonc
{
  "input": [
    // [0] system：编辑器快照 JSON（openFile.content_preview / selectedText / request 等）
    { "type": "message", "role": "system",
      "content": [ { "type": "input_text", "text": "{\"openFile\":{…\"main.tex\"…},\"selectedText\":…,\"request\":…}" } ] },
    // [1] user
    { "type": "message", "role": "user",
      "content": [ { "type": "input_text", "text": "111" } ] }
  ],
  "previousResponseId": "resp_mu48x3s7_r327xltp",
  "conversationId": "cdx1_4d842a0e-1cc7-4dda-95b6-4f1743b5b1d9",
  "metadata": {
    "projectId": "f2c27dfa-1189-4d6f-a566-6ca0441c98f0",
    "userId": "user-uVXTSCVQP7uI2G4CXbKC5shv",          // 注意是 user-… 句柄
    "model": "gpt-6-astra",
    "reasoning_effort": "medium",
    "frontend_origin": "https://prism.openai.com",
    "sandbox_url": "https://prism.openai.com/s/sandboxes/proxy/",
    "sandbox_token": "gAAAAAB…",
    "proxy_request_debug": "…JSON 字符串…",
    "codex_listen_snapshot": "…JSON 字符串…"            // 内含内部集群地址 crixet-backend.oai-science.svc.cluster.local:8081
  }
}
```

**`input` 只有 2 项**——网页端并不把整段历史重复发上去，而是靠 `previousResponseId` / `conversationId` 在服务端续接。这与「每次带全量 messages」的 chat-completions 语义不同：**要么只用首轮（`previousResponseId` 省略、`conversationId` 省略）做一次性问答，要么自己维护 `resp_*` / `cdx1_*` 做多轮。**

## A4. `POST /api/llm/response_with_tools_status` —— 实测

```
body: { "request_id": "81f72fb7-…",
        "turn_state": { …含 sandbox_url / sandbox_token / user_id / project_id /
                           workspace_session_id / codex_session_id /
                           session_file_path:"/home/sandbox/.codex/sessions/2026/09/16/rollout-…jsonl" /
                           async_job_id:"5533" / snapshot_id:"file_0000…" … } }
→ 200 { "status": "pending",
        "response": …,
        "codex_live_progress": …,
        "codex_listen_snapshot": { "line_offset": 17, … },
        "model_context_window": 258400,
        "server_proxy_origin": "https://crixet-frontend.gateway.unified-4.api.openai.com" }
```

完成态返回 `{"status":"completed", "response":{"status":"success","payload":{"id":"resp_mu48x3s7_r327xltp", … "codexDeltaFiles":[{"…diff…"}]}}}`。

**关键推论**：`turn_state` 是服务端回传的不透明状态（含沙箱凭证、rollout 文件路径、异步 job id），**必须原样回传**，不能自己构造。所以客户端只需保存第一次 `start`/`status` 返回的 `turn_state` 并循环回传即可。

## A5. LaTeX 编译 —— 实测完整参数

```
POST /s/sandboxes/proxy/render?renderMode=async&renderStatusMode=json&renderResultMode=stream-v1&prism_cache_bust=5521541541728076
     X-Crixet-Sandbox-Token: gAAAAAB…
     body: {"mainDocument":"main.tex",
            "clientStateVector":"AcOS1JEJ8wI=",        // Yjs state vector (base64)
            "clientDeleteSetUpdate":"AAA="}            // Yjs delete set (base64)
  → 202 {"status":"rendering",
         "pollAfterMs":0,
         "statusPath":"render-status?waitMs=10000&renderResultMode=stream-v1&renderStatusMode=json",
         "cancelPath":"render-cancel",
         "cancelOnAbort":false}

GET  /s/sandboxes/proxy/render-status?waitMs=10000&renderResultMode=stream-v1&renderStatusMode=json&prism_cache_bust=…
     X-Crixet-Sandbox-Token: gAAAAAB…
  → 200 application/pdf  39,393 B
    响应头：x-crixet-render-id: 3c6a3810-65e5-4f21-b667-bb893a563287
            x-crixet-pdf-sha256: 5956d6d9…ccf7
            x-crixet-rendered-project-id: f2c27dfa-1189-4d6f-a566-6ca0441c98f0
            x-is-beamer: false
            x-session-id: b105a89eeff44844ab68c6d707a320ba
            x-crixet-proxy-endpoint-sha1: 3d25fb3810f9a045fd2c3ea291a90132d659259a
```

这就对上你最初列的 `GET /s/sandboxes/proxy/render-status?waitMs=10000` —— `waitMs=10000` 是**长轮询等待毫秒数**（`statusPath` 里自带），`render-status` 直接返回 PDF 字节即编译完成，不需要再单独取 `render-result`。

编译日志：

```
GET /s/sandboxes/proxy/logs  →  {"pdfTexLog":…,"bibTexLog":"","latexmkLog":…}   (约 24 KB)
```

`render` 的请求体需要 **Yjs 的 state vector 与 delete set**，也就是说客户端必须先连上 §6 的 Yjs socket 才有东西可编译。

## A6. 其他实测补充

- **Yjs socket**：`GET /y/d/{projectId}/ws/{projectId}?token=ASRmMmMyN2Rm…`（101 Switching Protocols）。`docId` 就是 project UUID。
- **落盘走 Next.js Server Action**：`POST /?u={uuid}&pg=1&m=main.tex`，头 `next-action: 60cf02d49514d47d45ad768880e55c79081f230368`，body `["<project-uuid>",{"workspaceUpdateBase64":"ARDDktSRCQAnAQ…","compiledAt":1789571888502}]` → `1:{"status":"success","data":{"entry":{"versionNumber":1,"kind":"edit","updateCount":4}}}`
- **文件内容**：`GET /api/projects/{uuid}/files/{fileUuid}/content` → **307 重定向**到 blob 存储的 `location`。
- **心跳**：`GET /s/sandboxes/proxy/heartbeat?prism_cache_bust=6434278067561803`（`prism_cache_bust` 在一次会话内是常量），响应体 `OK`。
- ⚠️ **2026-09-17 更正：`POST /api/projects` 与 `POST /api/y` 两个抓包其实都捕获到了**，原文"没有捕获到"的结论是错的（当时应该是模式匹配没命中，`111.har` 的 JSON 被重排成键值分行，`"url":` 在行尾、值在下一行）。逐字实测如下：
  - `POST /api/projects` → 200，请求体 `{"project_uuid":"713f8452-fc7d-4919-b90c-c6bf422835bf","title":"新建项目","file_uuids":[]}`（94 字节），响应体 `{"uuid":"713f8452-fc7d-4919-b90c-c6bf422835bf","created_at":"2026-09-16T13:11:42.440675+00:00","deleted":false,"owner":"a80558b4-614e-5d20-bb5a-fee6f92b3420","public_role":null,"thumbnail_url":null,"thumbnail_uuid":null,"title":"新建项目"}`（243 字节）。**与 §4.1 的前端源码逐字段一致**，所以 §4.1 不再是"推断"，是已验证。
  - `POST /api/y` → 200，请求体 `{"docId":"1a1084c2-d548-453e-b521-808a23826ca3"}`（48 字节）—— 印证下面那条"`docId` 就是 project UUID"。
  - 来源：`prism.openai.com.har`（各有 3 条 / 20 条）与 `111.har`（243 条 prism 请求，是真正的 Fiddler 导出，另含 `/api/auth/anonymous-session`、`/api/auth/popup-callback`、`/auth/signout`、`/api/metrics`、`/api/projects/<uuid>/files/<uuid>/content` 等第一份没有的端点）。
- 抓包中还出现了 `/api/file-management/projects?section=your_projects`、`/api/file-management/groups`、`/api/metrics`、`/api/maintenance`、`/auth/entitlements`、`/api/ff/*`、`/api/codex/runtime/debug`，与上文一致。

## A7. `response_with_tools_start` / `status` 实测样本（第三次抓包，curl 导出）

> 下列样本中的 token / cookie / JWT 一律以 `<…>` 占位，**任何凭证都不要写进文档或代码**。

### A7.1 `POST /api/llm/response_with_tools_start`

`input` **只有 2 项**。system 项的 `text` 是一段**编辑器快照 JSON 字符串**（不是自然语言提示词）：

```jsonc
{
  "input": [
    { "type": "message", "role": "system",
      "content": [ { "type": "input_text", "text": "{\"openFile\":{\"status\":\"success\",\"payload\":{\"filename\":\"main.tex\",\"type\":\"text\",\"content_preview\":\"…\",\"content_length\":356,\"content_truncated\":false}},\"selectedText\":{\"status\":\"success\",\"payload\":{\"text\":\"\",\"selection\":{\"startLineNumber\":25,\"startColumn\":1,\"endLineNumber\":25,\"endColumn\":1}}},\"request\":{\"source\":\"user\",\"promptTextLength\":1,\"timestampUtcIso\":\"2026-09-16T15:45:47.239Z\",\"timestampUtcMs\":1789573547239,\"selectionKind\":\"cursor\",\"cursorPosition\":{\"lineNumber\":25,\"column\":1},\"selection\":{…},\"selectionTextLength\":0}}" } ] },
    { "type": "message", "role": "user",
      "content": [ { "type": "input_text", "text": "1" } ] }
  ],
  "previousResponseId": "resp_mu48y0z1_zinca9r0",
  "conversationId": "cdx1_4d842a0e-1cc7-4dda-95b6-4f1743b5b1d9",
  "metadata": {
    "projectId": "f2c27dfa-1189-4d6f-a566-6ca0441c98f0",
    "userId": "user-uVXTSCVQP7uI2G4CXbKC5shv",
    "model": "gpt-6-astra",
    "reasoning_effort": "medium",
    "frontend_origin": "https://prism.openai.com",
    "sandbox_url": "https://prism.openai.com/s/sandboxes/proxy/",
    "sandbox_token": "<gAAAAA…>",
    "proxy_request_debug": "{\"requestUrl\":\"…/s/sandboxes/proxy/logs?prism_cache_bust=…\",\"requestMethod\":\"GET\",…}",
    "codex_listen_snapshot": "{\"user_id\":\"…\",\"project_id\":\"…\",\"conversation_id\":\"…\",\"sandbox_url\":\"http://crixet-backend.oai-science.svc.cluster.local:8081/sandboxes/proxy/\",\"workspace_session_id\":\"…\",\"codex_session_id\":\"…\",\"last_turn_id\":\"…\",\"transcript_cursor\":30,…}"
  }
}
```

请求头只有：`accept: */*`、`accept-language`、`content-type: application/json`、`origin`、`priority`、`referer`、`sec-ch-ua*`、`sec-fetch-*`、`user-agent`，加上 **cookie**（无 `authorization`、无 sandbox 头）。

要点：
- `proxy_request_debug` / `codex_listen_snapshot` 是**字符串化的 JSON**，由客户端自报，**可以省略**。
- `sandbox_url` 这里是**公开地址**；服务端在 `turn_state` 里会换成内部集群地址。
- 由于 `input` 里 system 项只是个普通 `input_text`，**把它换成自然语言 system prompt 在结构上完全等价**——这正是本仓库插件采用的做法（见 `prism/executor.go` 的 `buildInput`），便于把 prism 当通用 chat API 用。

### A7.2 `POST /api/llm/response_with_tools_status`

```jsonc
{
  "request_id": "4b7d5780-1a7f-49c1-a9f3-28f56b30654b",
  "turn_state": {
    "version": 1,
    "conversation_id": "cdx1_4d842a0e-1cc7-4dda-95b6-4f1743b5b1d9",
    "snapshot_id": "file_00000000662c82109fa88d5ae805d389",
    "allow_context_clear_notice": true,
    "prompt": "Context:\n{…编辑器快照 JSON…}\n\nUser request:\n1",
    "reasoning_effort": "medium",
    "sandbox_url": "http://crixet-backend.oai-science.svc.cluster.local:8081/sandboxes/proxy/",
    "sandbox_token": "<gAAAAA…>",
    "workspace_session_id": "4d842a0e-1cc7-4dda-95b6-4f1743b5b1d9",
    "codex_session_id": "01a0aacc-6d54-7172-85b2-5fce74c776d0",
    "last_turn_id": "77540b764a5b426687695e5a48303fa4",
    "endpoint_identity": null,
    "last_exec_at": "2026-09-16T15:45:50.745Z",   // 首次为 epoch 字符串 "1789573549.7057388"
    "session_file_path": "/home/sandbox/.codex/sessions/2026/09/16/rollout-2026-09-16T15-18-39-01a0aacc-6d54-7172-85b2-5fce74c776d0.jsonl",
    "line_offset": 35,             // 首次 30，随后 35
    "turn_cursor_start": 30,
    "transcript_cursor": 35,       // 首次 30，随后 35
    "user_id": "user-uVXTSCVQP7uI2G4CXbKC5shv",
    "project_id": "f2c27dfa-1189-4d6f-a566-6ca0441c98f0",
    "async_job_id": "13474",
    "turn_started_at": "2026-09-16T15:45:47.885Z",
    "turn_started_at_ms": 1789573547885
  }
}
```

**关键契约**：

1. `turn_state` 是**服务端签发的不透明状态**，客户端只负责回传。其中 `sandbox_token`、`session_file_path`、`async_job_id` 都不是客户端能构造的。
2. **轮询之间 `line_offset` / `transcript_cursor` / `last_exec_at` 会被更新**（实测 30 → 35）。客户端从上一轮响应里的 `codex_listen_snapshot.transcript_cursor` 回填这几个游标，用于增量展示进度。
   - 对不关心流式进度的代理：**原样回传 start 拿到的 `turn_state` 即可**，这些游标只影响 `codex_live_progress` 的增量上报。
3. `request_id` 与 `conversation_id` 在整轮里保持不变；`request_id` 由 `start` 返回。
4. 请求头同样只有 `content-type` + cookie；**sandbox token 放在 body 里，不作为头**。

### A7.3 本轮实测的其余确认

- `PATCH /api/projects/{uuid}/thumbnail`，body `{"thumbnail_uuid":"8aca6042-82d6-4e2f-b03a-d53c6f18a49b"}` —— 与 §4.3 一致。
- `GET /s/sandboxes/proxy/heartbeat?prism_cache_bust=6434278067561803`，头 `Referer: …/turbopack-worker-1r2nq6-dwes2z.js` + `User-Agent` + `x-crixet-sandbox-token` + `content-type: application/json` —— 与 §5.2 一致。
- `POST /api/ff/rgstr` 上报 `ai_assistance_used`（`metadata.conversation_mode: "main"`）—— 遥测，可忽略。

## A8. 端到端实测（2026-09-17，用 `111.har` 里的真实 cookie 直打线上）

> 凭据来自 `111.har`（Fiddler 导出保留了真实 cookie），会话 token 当时仍有 10.3 小时有效期。**所有凭证只用于本次实测，未写入任何文件。**

### A8.1 已验证通过的

- `GET /auth/session` → 200，`userTier:"logged_in"`，`policy.user.openai_user_id` = `user-uVXTSCVQP7uI2G4CXbKC5shv` —— **确认了 `sessionResponse.OpenAIUserID()` 读的字段是对的**。
- `POST /api/llm/response_with_tools_start` 用**无状态全量历史**（不带 `previousResponseId`/`conversationId`）发送，被服务端正常接受（HTTP 200），说明请求体形状合法。
- `POST /api/backend/1/new` → 200 `{"url":"https://prism.openai.com/s/sandboxes/proxy","token":"gAAAAAB…"}`，1.7 s。

### A8.2 实测发现的严重问题

**1）`reason` 的真实线值是 snake_case，不是前端枚举的驼峰名。**

```json
{"status":"completed","request_id":"…","response":{"status":"error",
 "payload":{"reason":"sandbox_reconnecting",
            "message":"Reconnecting to sandbox. Your request will resume automatically once the sandbox is ready."}}}
```

前端源码里的枚举名是 `SandboxReconnecting`，但**线上传的是 `sandbox_reconnecting`**。任何按驼峰名比较的客户端都不会重试，而会把它当致命错误抛出去。`ConversationTooLarge` 同理需按线值核对。

**2）不领沙箱的话，这一轮永远不会成功。**

实测：不带 `metadata.sandbox_url`/`sandbox_token` 时，连续重发 3 次（间隔 6 s）全部返回 `sandbox_reconnecting`；**服务端不会自己补沙箱**。带沙箱才会继续往下走。见 `codexRequestDebug.sandbox_url_input: null`。

**3）领沙箱之后还有两步必做，否则工作区同步永远不完成。**

网页端在 `/api/backend/1/new` 和 `response_with_tools_start` 之间还有：

```
POST /api/projects/{uuid}/sandbox/resources-token
     body: {"sandbox_session_id":null,"sandbox_token":"gAAAAAB…"}
  → 200 {"access_token":"eyJ…","resources_base_url":"https://prism.openai.com/s/sandbox-resources",
         "expires_at":…,"max_age_seconds":…,"scopes":…}

POST {sandboxUrl}resources-token?prism_cache_bust=<常数>
     header: X-Crixet-Sandbox-Token: <第 1 步的 token>
     body:   {"token":"<上一步 access_token>","resourceBaseUrl":"<上一步 resources_base_url>","projectId":"<uuid>"}
  → 200 {"status":"success"}

GET {sandboxUrl}wait-for-sync?wait_ms=10000&prism_cache_bust=<常数>
     header: X-Crixet-Sandbox-Token: <第 1 步的 token>
  → 200 {"readinessCapabilities":["current_y_sweet_provider"],"status":"synced","tokens":{…}}
```

`prism_cache_bust` 在一次会话内是常量。

**4）但做完上面两步仍然不够——真正的门槛是 y-sweet（Yjs）provider 同步。**

把前 3 步都做掉之后，`wait-for-sync` 仍**一直停在 `syncing`**。逐字段对比说明差在哪：

| `tokens` 字段 | 只做了 resources-token | 抓包里的 `synced` |
|---|---|---|
| `hasResourceToken` / `hasResourceBaseUrl` / `hasResourceProjectId` | `true` | `true` |
| `hasCurrentYSweetToken` | **`false`** | `true` |
| `hasSyncedYSweetProvider` | **`false`** | `true` |
| `fileCredentialSource` | `"resources-token"` | `"resources-token"` |

沙箱要等 `hasSyncedYSweetProvider` 为真才算就绪。若不等就发 `start`，`codex_v2_restore_start` 会等**沙箱工作区文件同步**并在 **123.5 s（prism 网关超时）**后失败：

```
rootCause: POST http://crixet-backend.oai-science.svc.cluster.local:8081/codex_v2_restore_start failed (504 Gateway Timeout)
bodyText : {"error":{"message":"504: Timed out waiting for sandbox workspace file synchronization."}}
```

**结论（对任何非浏览器客户端都是硬约束）：**
prism 的 AI 永远跑在沙箱 agent 里，而沙箱的工作区靠 **Yjs 文档同步**灌进去——即 §6 那条 `POST /api/y {"docId":"<projectUuid>"}` 拿 token → 连 `wss://…/y/d/{projectId}/ws/{projectId}?token=…` → 同步文档。**只做 HTTP 轮询的代理拿不到答案**；要么实现 y-sweet/Yjs 的同步握手，要么走浏览器辅助传输。

### A8.3 ✅ 完整配方（2026-09-17 实测跑通，拿到了真实回答）

按顺序执行，缺任何一步都拿不到答案：

```
1. POST /api/backend/1/new                                    → 200 {"url":"…/s/sandboxes/proxy","token":"gAAAAAB…"}
2. POST /api/projects/{uuid}/sandbox/resources-token           → 200 {"access_token":"eyJ…","resources_base_url":"…/s/sandbox-resources"}
   body {"sandbox_session_id":null,"sandbox_token":"<第1步 token>"}
3. POST {第1步 url}/resources-token?prism_cache_bust=<常数>     → 200 {"status":"success"}
   header X-Crixet-Sandbox-Token: <第1步 token>
   body {"token":"<第2步 access_token>","resourceBaseUrl":"<第2步 resources_base_url>/","projectId":"<uuid>"}
4. POST /api/y {"docId":"<uuid>"}                              → 200 {"url":"wss://…/y/d/{uuid}/ws","baseUrl":…,"docId":…,"token":"ASRm…","authorization":"full"}
4b. POST {第1步 url}/token?prism_cache_bust=<常数>              → 200 {"success":true,"message":"Token received"}
   header X-Crixet-Sandbox-Token: <第1步 token>
   body  = 【第 4 步响应的原样整份 JSON】                       ← 关键：把 y-sweet 凭据交给沙箱。漏掉这步，沙箱的
                                                                 hasCurrentYSweetToken 永远是 false
5. 连 wss://…/y/d/{uuid}/ws/{uuid}?token=<第4步 token>          ← 注意路径是 /ws/{docId}；/api/y 返回的 url 少了这一段
   第一条二进制消息 = base64 解开的第4步 token（本身就是 y-sweet 的 Auth 消息：01 <len> <docId>）
   再发 Yjs SyncStep1（空 state vector：00 00 01 00），收到服务端 SyncStep1 后用空 SyncStep2（00 01 01 00）回应
   **这条连接必须一直保持**，沙箱靠它判定 provider 已同步
6. GET {第1步 url}/wait-for-sync?wait_ms=10000&prism_cache_bust=<常数>  → 直到 {"status":"synced",
     "tokens":{"hasResourceToken":true,"hasCurrentYSweetToken":true,"hasSyncedYSweetProvider":true}}
7. POST /api/llm/response_with_tools_start（metadata 带 sandbox_url/sandbox_token）→ {"status":"started",…,"turn_state":{…}}
8. POST /api/llm/response_with_tools_status {request_id, turn_state}  轮询 → {"status":"completed",
     "response":{"status":"success","payload":{"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"…"}]}]}}}
```

实测耗时：第 6 步在第 4b 步之后就绪（首次即 `synced`），第 7 步 2.6 s 返回 `started`，随后轮询到 `success` 并正确提取出回答文本。

`ref` 那类无沙箱尝试的对照：不发 `sandbox_url` → 永远 `sandbox_reconnecting`；发了但没做 4b → `wait-for-sync` 卡在 `syncing`，第 7 步在 **123.5 s** 后以 `codex_v2_restore_start` 504 失败。
- `x-crixet-sandbox-token` 与 `metadata.sandbox_token` 是**同一个值**，即 `POST /api/backend/1/new` 发的那一个，整轮不轮换。
