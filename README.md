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

### `GET /events`

按账号与发生时间窗口分页读取审计记录。查询参数均可省略，省略筛选条件时查询全部记录：

| 参数 | 说明 |
|---|---|
| `account` | 按解码后原文精确匹配（区分大小写、保留两端空白）；纯空白值非法 |
| `from` | 发生时间下界（包含），格式与追加入口相同的 RFC3339，按实际时刻比较 |
| `to` | 发生时间上界（不包含），格式同上；同时提供时 `from` 必须早于 `to` |
| `limit` | 每页条数，默认 50，范围 1 至 100；仅接受 ASCII 十进制数字，允许前导零 |
| `after_seq` | 分页游标，只返回 `seq` 大于该值的记录，默认 0，范围为非负 int64，格式同上 |

时间按实际时刻比较：任意长度的小数秒都参与比较，表示同一时刻的不同写法（如 `.5` 与 `.50`、`+08:00` 与换算后的 `Z`）视为相等。结果按 `seq` 升序返回，每项复用追加成功响应结构并保留已存原值。一次请求在同一个只读事务内读取，只展示同一已提交视图；后续分页可看到之后追加的记录。查询不修改任何记录或校验值。

成功返回 HTTP 200，正文仅含 `events` 与 `next_after_seq`：

```json
{
  "events": [
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
  ],
  "next_after_seq": 1
}
```

有更多匹配记录时 `next_after_seq` 为本页末条序号，否则为 `null`；空账本、无匹配或游标越过末尾均返回空数组和 `null` 游标。

重复或未知参数、显式空值、查询串解码失败、非法时间、上下界顺序错误或非法分页数值返回 HTTP 400，`code` 为 `invalid_audit_input`（参数错误优先于存储错误）；数据库不可用或读取失败返回 HTTP 503，`code` 为 `storage_unavailable`，不返回部分记录。

### `GET /ledger/verify`

校验整个账本。请求不接受任何查询参数。一次校验在同一个只读事务内按 `seq` 升序读取，针对同一个已提交视图；校验期间发生追加时，看到的是追加前或追加后的完整链，不会混合读取。

逐条确认：序号从 1 起连续递增；首条记录的 `prev_hash` 为 64 个 `0`，其余记录的 `prev_hash` 等于上一条记录存储的 `hash`；并按追加入口相同的字段顺序（`seq、account、operation、resource、result、occurred_at、prev_hash`）与紧凑 JSON 数组编码，依据当前存储字段重新计算 SHA-256，与该条存储的 `hash` 比较（已存文本与时间的小数秒形式原样参与计算，含中文、引号、反斜杠和控制字符的合法记录同样通过）。

发现首个不符合条件的记录即停止。`checked` 为已检查记录数，包含触发失败的那条现存记录；序号不等于期望序号时 `first_invalid_seq` 为期望序号，序号连续但前链值或自身 `hash` 不符时为该记录的序号。只保留合法链前缀的账本仍判定为有效。校验不修改任何记录、不补洞、不把重算值写回。

正常完成（账本为空、全部通过或发现断链）均返回 HTTP 200：

```json
{"valid":true,"checked":0,"first_invalid_seq":null}
```

```json
{"valid":false,"checked":3,"first_invalid_seq":3}
```

携带任何查询参数返回 HTTP 400，`code` 为 `invalid_audit_input`（参数非法优先于存储错误）；数据库不可用或读取失败返回 HTTP 503，`code` 为 `storage_unavailable`，不返回部分检查结果。

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
