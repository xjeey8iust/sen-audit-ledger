# sen-audit-ledger

把审计事件的账号、操作、目标资源、结果与发生时间记录成只追加且可校验的服务，支持按账号与时间窗口分页查询记录并校验记录链的完整性。

## 运行要求

- Go 1.26 或以上
- SQLite（本服务自带存储，不需要外部数据库）

## 构建、测试与启动

```bash
go build ./...
go test ./...
go run .
```

服务默认监听 `127.0.0.1:8080`。可用环境变量覆盖：

| 变量 | 默认值 | 用途 |
|---|---|---|
| `ADDR` | `127.0.0.1:8080` | HTTP 监听地址 |
| `DB_PATH` | `sen-audit-ledger.db` | SQLite 数据库文件路径 |

## 已公开的入口

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

### `POST /events`

追加一条审计记录。请求体为单个 JSON 对象：

| 字段 | 类型 | 说明 |
|---|---|---|
| `account` | string | 必填，去除两端空白后不能为空，存储时保留原文 |
| `operation` | string | 必填，同上 |
| `resource` | string | 必填，同上 |
| `result` | string | 必填，同上 |
| `occurred_at` | string | 必填，带时区的 RFC3339 时间（拒绝闰秒），按 UTC 存储和返回，保留小数秒 |
| `seq` | number | 可选，只接受不含小数及指数的正整数；省略时由服务分配 |

请求禁止携带 `hash`、`prev_hash` 及任何未知字段。

成功返回 HTTP 201 及完整记录（业务字段、`seq`、`prev_hash`、`hash`）。首条记录 `seq` 为 1、`prev_hash` 为 64 个 `0`；之后序号连续递增，`prev_hash` 等于上一条的 `hash`。`hash` 是 SHA-256 小写十六进制，输入为按 `seq、account、operation、resource、result、occurred_at、prev_hash` 顺序组成的紧凑 JSON 数组的 UTF-8 字节；字符串只转义双引号、反斜杠与控制字符，控制字符使用小写十六进制的 `\u00xx`。

追加在事务内以单写者提交：并发省略 `seq` 的合法请求都会成功且序号无空洞；失败不消耗序号、不改变链尾；同一 `DB_PATH` 重开服务后继续原有序号和链。

错误响应：

- HTTP 400 `invalid_audit_input`：JSON 畸形、非对象、多值、字段缺失或为 null、类型不符、字符串或时间非法、含禁止字段、显式 `seq` 非法或跳号
- HTTP 409 `audit_seq_conflict`：显式 `seq` 已存在
- HTTP 503 `storage_unavailable`：存储不可用或提交失败

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
