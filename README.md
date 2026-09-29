# Sub2API OpenAI Basis Points 插件

这是把 [cpa-plugin-oai-basispoints](https://github.com/JaxsonWang/cpa-plugin-oai-basispoints) 适配到 [Sub2API](https://github.com/Wei-Shaw/sub2api) 插件协议的独立插件工程。

## 能力范围

- 实现 Sub2API `openai.oauth.outbound_transport.v1`，入口为独立 Go 子进程和本机 gRPC 流。
- 使用 Sub2API 已经选中的 OpenAI OAuth 账号请求头，不读取或保存 refresh token。
- 将 OpenAI Responses 请求转换为 Basis Points 请求，保留原项目的模型映射、完整历史输入、客户端工具中继、推理等级、元数据和图片附件上传逻辑。
- 通过 HTTP/SSE 返回原始上游响应；响应解析、错误映射、SSE、用量统计、计费和账号调度继续由 Sub2API 负责。
- 使用账号级灰度路由。Sub2API 当前的插件绑定按 OpenAI OAuth 账号命中，不是按模型名命中；因此建议给专用于 Basis Points 的 OAuth 账号启用插件绑定。
- WebSocket 不在插件进程内直连。Sub2API 命中插件的 OAuth WebSocket 请求会走宿主现有 HTTP Bridge，避免绕过 v1 HTTP 插件协议。

## 复用 Sub2API 账号管理中的登录账号

插件不需要、也不允许在配置页面填写 OAuth Token。请求到达插件后，插件使用请求中的 `account_id` 通过 Sub2API `HostService.ResolveOutboundIdentity` 获取该账号当前有效的短期 `access_token`、账号请求头和代理地址；Token 不写入插件配置或日志。

请先在 Sub2API「账号管理」中完成以下条件：

1. 平台选择 `OpenAI`；
2. 类型选择 `OAuth`，并完成 ChatGPT/Codex 登录；
3. 账号状态为有效且可调度；
4. 在插件绑定或灰度设置中将该账号命中 `OpenAI OAuth` 出站传输插件。

如果账号未命中插件绑定、不是 OpenAI OAuth、已暂停或已失效，宿主不会把它解析为插件出站身份。

## 与原 CPA 插件的差异

本工程不再包含 CPA 动态库 ABI、CPA 认证解析、CPA 管理菜单和 CPA 宿主回调。Sub2API 负责 OAuth 登录/刷新、账号选择、代理、请求重试和生命周期；插件只负责 Basis Points 出站 HTTP 适配。

原配置中的 `upstream_transport`、`ws_handshake_timeout_seconds`、`data_dir` 不再使用。配置改为 JSON，由 Sub2API 加密保存：

```json
{
  "responses_url": "https://bps.openai.com/basispoints/api/responses",
  "upstream_model": "gpt-6-astra",
  "models": ["gpt-6-astra-basispoints", "gpt-6-sol", "gpt-6-luna", "gpt-6-terra", "gpt-6-astra"],
  "model_mappings": {
    "gpt-6-astra-basispoints": "gpt-6-astra",
    "gpt-6-sol": "gpt-6-sol",
    "gpt-6-luna": "gpt-6-luna",
    "gpt-6-terra": "gpt-6-terra",
    "gpt-6-astra": "gpt-6-astra"
  },
  "timeout_seconds": 300,
  "max_response_bytes": 67108864,
  "max_request_bytes": 33554432,
  "auth_mode": "chatgpt",
  "tools_version_id": ""
}
```

`models` 是插件接受的客户端模型别名；`model_mappings` 的值是实际发送给 Basis Points 的模型。未写映射的别名使用 `upstream_model`。客户端未填写 `model` 时使用 `models` 第一项。

升级已有安装时，插件会在运行时兼容迁移旧的默认配置，但不会直接覆盖 Sub2API 已保存的配置；仍建议在插件配置页确认并保存上述五个模型和对应映射，或直接粘贴上面的完整 JSON。

## 构建

需要 Go 1.26 或更高版本、Python 3.10 或更高版本和 Git。依赖下载由构建环境自行完成；本仓库不提交真实 OAuth 凭据。

```powershell
# 单元测试：不访问真实 Basis Points 服务
$env:PATH = "C:\\path\\to\\go\\bin;$env:PATH"
go test ./internal/basispoints

# 构建当前平台二进制
python tools/build.py --goos windows --goarch amd64 --allow-unsigned
```

也可以分两步：

```powershell
go build -trimpath -ldflags "-s -w" -o dist/runtimes/windows-amd64/plugin.exe ./cmd/oai-basispoints
python tools/package.py --binary dist/runtimes/windows-amd64/plugin.exe --goos windows --goarch amd64 --allow-unsigned --output dist/oai-basispoints-windows-amd64.s2plugin
```

`--allow-unsigned` 只用于本地开发。生产 Sub2API 默认要求 `signature.json`，应使用发布者 Ed25519 私钥对生成的 `manifest.json` 原始字节签名，再把签名文件传给打包器；私钥不能放入仓库或插件包。

## 安装到 Sub2API

1. 在 Sub2API 管理页的插件管理中上传生成的 `.s2plugin`。
2. 开发包需要宿主开启 `plugins.allow_unsigned`；生产包应先配置受信发布者公钥并使用签名包。
3. 安装后启用 `OpenAI OAuth` 绑定，先使用小比例灰度验证，再扩大到目标账号。
4. 打开插件配置 UI，保存 `responses_url`、模型别名和映射。
5. 确认被插件路由命中的 OpenAI OAuth 账号已经在「账号管理」中登录且可调度；客户端使用 `models` 中的别名调用 `/v1/responses`。无需在插件配置中再次填写 Token。

插件的配置测试只验证 JSON、URL 和范围，不发送带 OAuth 凭据的探测请求。真实上游联通性需要在已授权的测试账号和测试站点上验证。

本发布包的签名信任配置如下（生产环境保持 `allow_unsigned: false`）：

```yaml
plugins:
  allow_unsigned: false
  trusted_publishers:
    oai-basispoints-publisher-v1: "IfhRYx9hUb3gz4G+jgEdWHEpAqCT/x3ol/WvdrFPScU="
```

## 目录

- `cmd/oai-basispoints/main.go`：插件进程入口。
- `internal/basispoints/`：从原项目复用并裁剪后的请求转换、工具中继、身份和图片附件逻辑。
- `internal/transport/`：Sub2API gRPC TransportPlugin 实现、配置和 HTTP 代理连接池。
- `pkg/pluginapi/v1/`：与 Sub2API 0.2.9 对齐的公开插件协议生成代码。
- `ui/index.html`：sandbox UI Bridge 配置页。
- `manifest.source.json`：打包前清单源文件。
- `tools/package.py`：生成 `.s2plugin`、文件哈希和可选签名文件。
- `tools/build.py`：构建二进制并调用打包器。

## 验证边界

已在本工程执行的本地验证只覆盖配置规范化、模型/Responses 请求转换、OAuth 请求头构造和错误拒绝路径。没有使用真实 OAuth 凭据，也没有宣称已完成 Basis Points 线上调用或 Sub2API 生产验收。

## 许可

本适配层沿用原项目的 MIT License；协议生成文件与 Sub2API 的公开插件协议保持一致，具体以 `pkg/pluginapi/v1/` 来源仓库为准。
