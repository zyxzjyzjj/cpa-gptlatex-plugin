# 发布到 CLIProxyAPI 插件商店

三条路，按"改的东西多少"排序。**证书/签名类要求一条都没有**，只有命名和内容布局的硬约束。

## 0. 一个硬约束先记住

产物名和压缩包内容由**安装器**校验，错了会被直接拒：

| 项 | 要求 |
|---|---|
| zip 名 | `<id>_<version>_<goos>_<goarch>.zip`，`<version>` 是**去掉 `v`** 的 tag（`prism-provider_0.1.0_windows_amd64.zip`） |
| zip 内容 | **动态库必须在压缩包根目录**，名字必须是 `<id>.<ext>`（`prism-provider.dll`）。嵌套目录、额外文件、绝对路径、zip-slip 一律拒收 |
| `checksums.txt` | `<sha256>  <文件名>`，**裸文件名**。写成 `./x.zip` 会"能解析但查不到"（宿主只剥 `*`，不剥 `./`） |
| 插件 ID | 由**文件名**决定（去掉扩展名），必须匹配 `[A-Za-z0-9][A-Za-z0-9._-]{0,127}` |
| 目录布局 | `plugins/<GOOS>/<GOARCH>/<id>.<ext>`；查找顺序 `plugins/<os>/<arch>-<variant>` → `plugins/<os>/<arch>` → `plugins`，同 ID 时**优先级高者胜** |

## 1. 打包

```bash
git tag v0.1.0 && git push origin v0.1.0      # tag 必须是 v<点分数字版本>
bash tools/package-release.sh 0.1.0           # 产出 release-assets/
```

`tools/package-release.sh` 会先做**版本一致性前置检查**：tag 去掉 `v` 后，必须与 `prism/main.go` 里的 `var version` 和 `registry.json` 里的 `version` 三者相同，否则直接失败。这条检查是为了防止"发出去的清单版本和二进制自己报的版本不一致"——商店的 `version` 只是**显示兜底**，真实版本取的是 GitHub release tag。

跨平台编译需要对应平台的 C 工具链（插件是 C ABI 的 c-shared 库）。按平台给编译器即可，编不出来的平台会被跳过：

```bash
CC_linux_amd64=gcc CC_windows_amd64=x86_64-w64-mingw32-gcc \
  PLATFORMS="linux/amd64 windows/amd64" bash tools/package-release.sh 0.1.0
```

本机（Windows + 可移植 MinGW）实测产物：

```
release-assets/prism-provider_0.1.0_windows_amd64.zip   （内含根目录一个 prism-provider.dll）
release-assets/checksums.txt                            （裸文件名，sha256 校验通过）
```

`GOOS=linux` 交叉编译出 `.so` 需要一个 Linux 目标工具链（本机没装，所以本地只出了 Windows 那份）。CI 里用 `gcc-mingw-w64-x86-64` + `gcc` 出两个平台。

## 2. 发布

`.github/workflows/release.yml` 在推 `v*` tag 时触发，依次：校验 tag 与两处版本号一致 → 装交叉工具链 → `go test -race` / `vet` / `gofmt` → 打包 → **校验产物结构、checksums 和四个导出符号**（`cliproxy_plugin_init` / `cliproxyPluginCall` / `cliproxyPluginFree` / `cliproxyPluginShutdown`）→ 建或更新 release。也可以手动触发并指定已存在的 tag。

## 3. 进商店

### 方式 A：提 PR 到官方商店

Fork [`CLIProxyAPI-Plugins-Store`](https://github.com/router-for-me/CLIProxyAPI-Plugins-Store)，把 `registry-entry.json` 的内容加进 `registry.json` 并发 PR。必填 `id` / `name` / `description` / `author` / `repository`；可选 `version` / `logo` / `homepage` / `license` / `tags`。宿主强制的校验：

- `schema_version` 必须是 `1`（`2` 才支持 `direct` 安装计划）
- `id` 匹配上面的字符集且唯一
- `repository` 必须**恰好**是 `https://github.com/{owner}/{repo}`
- `version` 不能以 `v` 开头

### 方式 B：自建第三方源（不用等审核）

仓库根目录的 `registry.json` 就是一份完整的源。推到 GitHub 后：

```yaml
plugins:
  enabled: true
  store-sources:
    - "https://raw.githubusercontent.com/zyxzjyzjj/cpa-gptlatex-plugin/master/registry.json"
```

> ⚠️ **用 `http://` 会被拒。** 宿主对非 HTTPS 的商店地址要求一条匹配的 auth 规则，否则报
> `insecure plugin store url requires matching allow-insecure auth rule`：
>
> ```yaml
> plugins:
>   store-auth:
>     - match: "http://127.0.0.1:8791/"
>       type: none
>       allow-insecure: true
> ```
>
> 官方源永远存在且不可移除。源 ID 由 URL 哈希派生，同一个文件挂两个 URL 算两个源，ID 撞车是硬错误。

## 4. 安装

```bash
curl -H "Authorization: Bearer $ADMIN_KEY" http://localhost:8317/v0/management/plugin-store
curl -X POST -H "Authorization: Bearer $ADMIN_KEY" \
  "http://localhost:8317/v0/management/plugin-store/prism-provider/install?source=<source_id>"
```

安装会写入动态库并**把该插件的 `enabled` 置 true**，但**不会**打开全局 `plugins.enabled`。同一 ID 出现在多个源时，`?source=<sourceID>` 必填。Windows 上已加载的 DLL 不能被覆盖，所以更新运行中的插件会提示重启冲突。

## 5. 已经验证 / 还没验证

**已验证**（本地实测）：打包产物结构合规、`checksums.txt` 为裸文件名且哈希正确、`registry.json` 通过宿主**真实解析器**（`GET /v0/management/plugin-store` 返回 `source_errors: NONE`，`prism-provider` 带全字段出现在列表里）。

仓库地址为 `https://github.com/zyxzjyzjj/cpa-gptlatex-plugin`，已写入 `main.go` 的 `repoURL`、`registry.json` 与 `registry-entry.json`，与 `origin` remote 一致；改仓库名时这三处要同步改（workflow 会校验一致性）。