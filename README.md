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

### `POST /events`

追加一条审计记录。请求体为 JSON 对象，`account`、`operation`、`resource`、`result`、`occurred_at` 为必填字符串（前四项去除两端空白后不能为空，存储时保留原文）；`occurred_at` 必须是带时区的 RFC3339 时间（拒绝闰秒），按 UTC 存储和返回并保留小数秒。`seq` 可省略，由服务分配；显式 `seq` 只接受不含小数及指数的正整数。请求禁止携带 `hash`、`prev_hash` 及任何未知字段。

成功时返回 HTTP 201 及完整记录（业务字段、`seq`、`prev_hash`、`hash`）。首条记录 `seq` 为 1、`prev_hash` 为 64 个 `0`；之后序号连续递增，`prev_hash` 等于上一条的 `hash`。`hash` 是对按 `seq、account、operation、resource、result、occurred_at、prev_hash` 顺序组成的紧凑 JSON 数组（UTF-8 字节）计算的 SHA-256 小写十六进制。

```json
{
  "account": "alice",
  "operation": "login",
  "resource": "console",
  "result": "ok",
  "occurred_at": "2026-10-05T00:30:00Z",
  "seq": 1,
  "prev_hash": "0000000000000000000000000000000000000000000000000000000000000000",
  "hash": "…"
}
```

输入非法（JSON 畸形、非对象、多值、字段缺失或为 null、类型不符、字符串或时间非法、含禁止字段、显式 `seq` 非下一序号）返回 HTTP 400，`code` 为 `invalid_audit_input`；显式 `seq` 已存在返回 HTTP 409，`code` 为 `audit_seq_conflict`；存储不可用或提交失败返回 HTTP 503，`code` 为 `storage_unavailable`。

### `GET /ledger/verify`

按序号升序校验整条账本链：确认序号从 1 连续递增、首条 `prev_hash` 为 64 个 `0`、其余记录的 `prev_hash` 等于上一条的 `hash`，并按追加时的字段顺序与编码规则从当前存储的字段重新计算 `hash` 与存储值比较。校验针对同一份已提交快照进行，只读不改。正常完成时返回 HTTP 200：

```json
{"valid":true,"checked":3,"first_invalid_seq":null}
```

空账本返回 `valid` 为 `true`、`checked` 为 0、`first_invalid_seq` 为 `null`；全部通过时 `checked` 等于记录总数。发现首个不符合条件的位置即停止：`valid` 为 `false`，`checked` 包含已检查的记录及触发失败的现存记录；序号不等于期望序号时 `first_invalid_seq` 为该期望序号，前链值或自身校验值不符时为该记录的序号。

请求携带任何查询参数返回 HTTP 400，`code` 为 `invalid_audit_input`（优先于存储错误）；数据库不可用或读取失败返回 HTTP 503，`code` 为 `storage_unavailable`，不返回部分结果。

### `GET /healthz`

返回服务与存储状态。正常时 HTTP 200：

```json
{"status":"ok","database":"ok"}
```

存储不可用时 HTTP 503：

```json
{"error":{"code":"storage_unavailable","message":"database is not available"}}
```

## 错误约定

所有错误响应都是单个顶层 `error` 对象，包含 `code` 与 `message` 两个字符串字段；`message` 不包含 SQL、堆栈或文件路径。
