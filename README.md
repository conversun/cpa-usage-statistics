# CPA Usage Statistics

CLIProxyAPI 的持久化用量统计插件。记录每次请求的用量，写入本地 SQLite；提供查询/删除接口。字段命名对齐上游 `usage.Record` / `usage.Detail`。

当前版本：`0.3.0`

## 建议使用配套的前端面板

[配套面板](https://github.com/Fwindy/Cli-Proxy-API-Management-Center)

## 功能

- 接收上游用量记录并持久化到 SQLite（WAL，写入不阻塞查询）。
- 提供受管理鉴权保护的聚合查询、明细分页查询与删除接口。
- 可选按天数保留清理，后台批量执行，不占用请求路径。

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

### 聚合查询（推荐面板使用）

```
GET /v0/management/plugins/usage-statistics/usage/summary
    ?start=<RFC3339>&end=<RFC3339>&bucket=15m|hour|day|month
```

在 SQL 侧按「时间桶 × api_key × provider × model × source × auth_index」聚合，响应大小只取决于组合数而非请求量。`bucket` 缺省为 `day`。

包含 `source` / `auth_index` 是因为同一个 api_key 可能由多个凭证轮流承担，按 api_key 分组会把它们合并成一行，凭证维度的统计就无法还原。

**平均值只统计上报了该指标的请求**：`ttft_ms = 0` 表示该请求根本没测到首字节，把这些 0 算进平均会拉低结果。因此同时返回 `latency_samples` / `ttft_samples`，供调用方跨桶做加权平均（直接平均两个桶的 `avg_*` 是错的）。

```jsonc
{
  "buckets": [
    {
      "bucket": "2026-05-02",
      "api_key": "…",
      "provider": "anthropic",
      "model": "claude-sonnet-4-5",
      "source": "claude-code",
      "auth_index": "auth-1",
      "requests": 128,
      "failures": 3,
      "tokens": {
        "input_tokens": 10240, "output_tokens": 20480, "reasoning_tokens": 512,
        "cached_tokens": 0, "cache_read_tokens": 0, "cache_creation_tokens": 0,
        "total_tokens": 31232
      },
      "avg_latency_ms": 1250,
      "max_latency_ms": 8300,
      "latency_samples": 128,
      "avg_ttft_ms": 210,
      "max_ttft_ms": 900,
      "ttft_samples": 126
    }
  ],
  "bucket_by": "day",
  "truncated": false
}
```

`bucket=15m` 用于服务健康类的细粒度视图（小时桶无法在客户端再拆分），桶键形如 `2026-05-02T10:30`。请配合较短的时间范围使用。

### 明细查询（下钻，keyset 分页）

```
GET /v0/management/plugins/usage-statistics/usage/records
    ?start=<RFC3339>&end=<RFC3339>
    &api_key=&provider=&model=&failed=true
    &limit=100&cursor=<next_cursor>
```

按 `(timestamp, id)` 游标分页，翻到第 N 页与第 1 页开销相同。把响应里的 `next_cursor` 原样回传即可取下一页；`has_more` 为 `false` 时到底。`limit` 缺省 100、上限 1000。

列表里的 `failure_body` 只返回前 500 字节（按 UTF-8 边界截断，不会切断多字节字符）。要完整错误文本，用 `?id=<record-id>` 查单条。

```jsonc
{
  "records": [
    {
      "id": "…",
      "timestamp": "2026-05-02T10:30:00Z",
      "api_key": "…",
      "provider": "anthropic",
      "model": "claude-sonnet-4-5",
      "source": "claude-code",
      "latency_ms": 1250,
      "ttft_ms": 200,
      "tokens": { "input_tokens": 10, "output_tokens": 20, "total_tokens": 33 },
      "failed": true,
      "failure_status_code": 429,
      "failure_body": "rate limited"
    }
  ],
  "next_cursor": "…",
  "has_more": true
}
```

### 原始查询（旧接口，保持兼容）

```
GET /v0/management/plugins/usage-statistics/usage?start=<RFC3339>&end=<RFC3339>
```

响应按「分组键（api_key 或 provider）→ 模型」两层聚合，形状与 0.1.0 完全一致：

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

该接口返回原始记录，响应大小随请求量线性增长，因此**最多返回最新的 10000 条**。命中上限时响应头带 `X-Usage-Truncated: true` 与 `X-Usage-Row-Limit: 10000`，被丢弃的是范围内更旧的记录。宽时间范围请改用 `/usage/summary`（图表）与 `/usage/records`（明细表）。

### 删除

```
DELETE /v0/management/plugins/usage-statistics/usage
Content-Type: application/json

{"ids": ["<record-id>", "..."]}
```

响应：`{"deleted": 1, "missing": ["..."]}`。

## 说明

- 本插件仅使用上游 `UsageRecord` ABI 现有字段。失败判定为 `Failed || failure_status_code >= 400`。
- `failure_body` 写入时截断到 4096 字节（按 UTF-8 边界），避免单条异常错误撑爆库和响应。
- 数据库使用 WAL 模式。**备份不能只拷 `usage.db`** —— 还有 `usage.db-wal` / `usage.db-shm` 两个附属文件，只拷主文件会得到陈旧或损坏的快照。请用 `sqlite3 usage.db ".backup out.db"` 或 `VACUUM INTO`。
- 从 0.1.0 升级无需人工操作：首次打开时自动切到 WAL、补齐缺失列、建立 `idx_usage_ts_id` 并删除已冗余的旧索引。
