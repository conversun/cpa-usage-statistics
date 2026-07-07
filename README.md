# CPA Usage Statistics

CLIProxyAPI 的持久化用量统计插件。以 UsagePlugin 能力接收每次请求的用量记录，写入本地 SQLite；以 ManagementAPI 能力提供查询/删除接口。字段命名对齐上游 `usage.Record` / `usage.Detail`，可直接运行在纯上游宿主上。

当前版本：`0.1.0`

## 功能

- 接收上游用量记录并持久化到 SQLite（纯 Go 驱动 `modernc.org/sqlite`，无需 CGO SQLite）。
- 提供受管理鉴权保护的查询/删除接口。
- 可选按天数保留清理。
- 字段与上游命名一致：`reasoning_effort` / `ttft_ms` / `failure_status_code` / `failure_body`，并补齐 `cache_read_tokens` / `cache_creation_tokens`。

## 安装

下载对应平台的 release zip，将动态库放到 CLIProxyAPI 的插件目录：

```text
plugins/linux/amd64/usage-statistics.so
plugins/windows/amd64/usage-statistics.dll
plugins/darwin/arm64/usage-statistics.dylib
```

放好后重启 CLIProxyAPI。

## 配置

```yaml
plugins:
  enabled: true
  configs:
    usage-statistics:
      enabled: true
      priority: 100
      # 可选：usage.db 存放目录；缺省为 ~/.cli-proxy-api/plugins/usage-statistics
      data_dir: ""
      # 可选：保留天数，超过则清理旧记录；0 表示不清理
      retention_days: 0
```

`data_dir` 也可用环境变量 `USAGE_STATISTICS_DIR` 指定。中文配置键 `用量保留天数`、`数据目录` 同样可用。

## API

均为受管理鉴权保护的管理路由（宿主自动加 `/v0/management` 前缀）：

### 查询

```
GET /v0/management/plugins/usage-statistics/usage?start=<RFC3339>&end=<RFC3339>
```

响应按「分组键（api_key 或 provider）→ 模型」两层聚合：

```jsonc
{
  "<api_key 或 provider>": {
    "<model>": [
      {
        "id": "…",
        "timestamp": "2026-05-02T10:30:00Z",
        "provider": "anthropic",
        "source": "claude-code",
        "auth_index": "auth-1",
        "reasoning_effort": "high",
        "service_tier": "priority",
        "latency_ms": 1250,
        "ttft_ms": 200,
        "tokens": {
          "input_tokens": 10, "output_tokens": 20, "reasoning_tokens": 3,
          "cached_tokens": 4, "cache_read_tokens": 0, "cache_creation_tokens": 0,
          "total_tokens": 33
        },
        "failed": true,
        "failure_status_code": 429,
        "failure_body": "rate limited"
      }
    ]
  }
}
```

### 删除

```
DELETE /v0/management/plugins/usage-statistics/usage
Content-Type: application/json

{"ids": ["<record-id>", "..."]}
```

响应：`{"deleted": 1, "missing": ["..."]}`。

## 从源码构建

```bash
# c-shared 需要 CGO_ENABLED=1 与 C 工具链（gcc / mingw-w64）
bash ./build.sh
```

发布由 `v*.*.*` tag 触发 GitHub Action，跨 linux/darwin/windows 构建，产物 `usage-statistics_<版本>_<goos>_<goarch>.zip` + `checksums.txt`。

## 说明

- 本插件仅使用上游 `UsageRecord` ABI 现有字段。上游未提供 `endpoint`、`request_id`，故不记录；失败判定为 `Failed || failure_status_code >= 400`。
