# bankcmp

跨库数据比对工具，用于银行核心系统从老库迁到新库后、上线前的"一条不差"证明。

它不做抽样。它把每张表按主键切成区间，逐区间比对聚合并只在区间不一致时下钻，
最终给出**每一条**差异行：多出、缺失、内容不同，以及具体是哪些列。差异清单可以
直接生成修复 SQL，执行前必须先预览。

```
┌──────────┐   keys/agg    ┌───────────────┐   mismatch    ┌──────────────┐
│ 源库 R/W │ ───────────▶  │ 分块校验和比对 │ ───────────▶  │ 区间二分下钻  │
└──────────┘               └───────────────┘               └──────┬───────┘
                                                                    │ 精确归并
┌──────────┐   keys/agg    ┌───────────────┐               ┌──────▼───────┐
│ 目标库   │ ───────────▶  │  规整层 canon  │               │ 行级差异清单  │
└──────────┘               └───────────────┘               └──────┬───────┘
                                                                   │
                         修复 SQL ◀── 预览 ◀── 差异分类 ◀──────────┘
```

## 设计要点（对应需求）

| # | 需求 | 实现 |
|---|------|------|
| 1 | 按主键分块，每块算校验和，不一致再细分 | `internal/engine`：`planChunks` 采样主键边界；每个区间对两侧计算「行数 + 128 位行指纹的加法聚合 + XOR 聚合」，三类聚合都相等才放过；不等则用两侧中位数递归二分，直到区间可精确归并 |
| 2 | 字段类型不同（金额精度、日期格式、字符集）先规整 | `internal/canon`：金额走 `decimal`（`123.4500 == 123.45`），日期/时间支持多格式并统一到 UTC，GBK/Big5/GB18030 等解码成 UTF-8，`CHAR(n)` 去掉填充空格，布尔多种写法归一 |
| 3 | 源库持续写入时也能比，区分真实差异与比对期间的变动 | 比对时记录每行指纹；结束后等待一个 settle 期重读差异行，只有**观察到源行内容确实变了**才标记 transient。真实丢失/改值的行保持 stable。`recheck_test.go` 与 `concurrent_test.go` 覆盖 |
| 4 | 大字段和二进制列只比摘要 | 列配置 `digest: true`：SQLite 用注册的 `bc_sha256*` UDF，PostgreSQL 用 `encode(sha256(...),'hex')`，只把 64 字节摘要传回 |
| 5 | 差异分多出/缺失/内容不同，列出具体列 | `missing`（源有目标无）、`extra`（目标多出）、`modified` + `columns[{column, source, target}]` |
| 6 | 根据差异生成修复 SQL，执行前先预览 | `internal/sqlgen`：INSERT/DELETE/UPDATE + 配套回滚 SQL。值来自**按主键重读的完整行**，不是界面预览；脱敏列和截断值会被拒绝生成；执行需 `{"confirm":"apply"}` |
| 7 | 比对限速，不能压垮生产库 | `internal/engine.Limiter`：行/秒与查询/秒双预算，按**实际扫描行数**计费，扫描流边读边等待；CLI 与看板都可配 |
| 8 | 报告给出每表行数、比对耗时和差异数 | `compare` 命令输出表格；看板「比对报告」页；`/api/tasks/{id}/report.csv` |
| — | 差异清单必须和埋点完全一致 | `internal/verify` 逐条核对 manifest：多一条、少一条、类型不符都判失败；`e2e_test.go` 是自动化金标准测试 |

## 实测结果（本机，SQLite 两侧）

| 数据量 | 区间大小 | 埋点差异 | 比对结果 | 耗时 |
|--------|----------|----------|----------|------|
| 3 表 × 300 万行（共 900 万） | 20000 | 740 | 740 条，逐条一致，0 多余 0 缺失 | 51 s |
| 同上，仅 transactions（300 万） | 500 | 267 | 267 条，逐条一致 | 12.5 s |
| 同上，限速 20000 行/秒 | 20000 | 422 | 422 条，逐条一致 | 389 s |
| PostgreSQL → PostgreSQL，2 表 × 30 万 | 20000 | 56 | 56 条，逐条一致 | 2.7 s |
| PostgreSQL → PostgreSQL，2 表 × 40 万 | 20000 | 74 | 74 条，逐条一致 | 7.4 s |
| PostgreSQL 源库边比边写入（12031 次更新） | 5000 | 74 | 74 条稳定差异全部命中，写入行判为 transient | 27 s |

另外用 SQLite 直接 join 计算"真实差异"与引擎输出对拍（8 组随机变异，每组
100+ 删除/新增/改值），逐类差异数完全一致；这项对拍已固化为
`internal/engine/differential_test.go`。

修复闭环也做了端到端验证：对埋点数据生成修复 SQL 并执行，再次比对差异为 0
（`internal/verify/repair_e2e_test.go`）。

## 快速开始

```bash
# 1. 造一对库（老库文本金额/GBK、新库 numeric/date），并埋入已知差异
go run ./cmd/seedtest -dir ./testdata -rows 200000

# 2. 不开界面，直接比对并对照埋点验证（不一致时退出码 1）
go run ./cmd/bankcmp compare \
  -config ./testdata/config.yaml \
  -verify ./testdata/manifest.json

# 3. 起看板
go run ./cmd/bankcmp -addr 127.0.0.1:8080 -journal ./bankcmp.db
#   浏览器打开 http://127.0.0.1:8080
```

`testdata/config.yaml` 由造数命令生成，可直接改。

### 用 PostgreSQL

两侧都跑 PostgreSQL：

```bash
go run ./cmd/seedtest -pg 'postgres://user@host/postgres' \
  -dir ./pgtest -rows 300000
go run ./cmd/bankcmp compare -config ./pgtest/config.yaml -verify ./pgtest/manifest.json
```

一侧 SQLite、一侧 PostgreSQL 也可以，只要 `source.driver` / `target.driver` 分别设置。
集成测试需要设置 `BANKCMP_TEST_PG` 指向一个可建库的 PostgreSQL：

```bash
BANKCMP_TEST_PG='postgres://postgres@/postgres?host=/tmp&port=5432' go test ./...
```

## 任务配置

```yaml
source:
  driver: sqlite            # sqlite | postgres
  dsn: /path/to/legacy.db
  charset: gbk              # 该库文本的默认字符集（可被列级覆盖）
target:
  driver: postgres
  dsn: postgres://user@host/newdb
options:
  workers: 2                # 同时比对的表数
  chunk_workers: 4          # 每张表的并发区间数
  chunk_rows: 20000         # 粗粒度区间大小
  diff_max_rows: 50000      # 单个区间一次物化的差异上限，超出继续二分
  rows_per_second: 0        # 0 = 不限；按扫描行数限速
  queries_per_second: 0
  recheck: true             # 重读差异行判断稳定性
  settle_ms: 300            # 重读前的等待时间
tables:
  - name: accounts
    column_mode: all        # all（默认，比较全部共有列）| listed（只比列出的列）
    columns:
      - name: balance
        type: decimal       # 文本金额 vs numeric
      - name: open_date
        type: date
      - name: cust_name
        source_charset: gbk # 仅该列是 GBK
      - name: memo
        type: string
        digest: true        # 只比摘要
    # where: "status <> 'DEL'"            # 可选过滤条件（两侧同方言时）
    # chunk_rows: 50000
```

### 列级选项

| 选项 | 作用 |
|------|------|
| `type` | 逻辑类型：`int decimal float string bytes bool date datetime time uuid` |
| `target_name` | 两侧列名不同 |
| `ignore` | 不比较该列 |
| `digest` | 大字段/二进制只比服务端摘要 |
| `source_charset` | 源侧该列的字符集 |
| `scale` | 金额四舍五入到 N 位后再比 |
| `float_tolerance` | 浮点容差（只在行级归并时使用；区间校验和仍按精确值） |
| `trim_space` / `collapse_space` / `case_fold` | 文本归一 |
| `null_equals_empty` | NULL 与空串等价 |
| `layouts` | 额外的日期格式 |
| `timezone` | 无时区时间按该时区解释 |

主键列不允许使用会改变排序的归一选项（会与数据库排序不一致），也不允许 NULL。

**文本列存数字主键**：老库常把金额/编号存成文本。若两侧物理类型不同（一侧文本、一侧
numeric），直接比会因"字典序 vs 数值序"被拒绝。在列上显式声明类型即可，工具会在 SQL 里
同样做 cast，保证两侧排序一致：

```yaml
tables:
  - name: ledger
    columns:
      - name: acct_seq      # 老库 TEXT，新库 bigint
        type: int
    # 也可用 key_columns 列出主键，并在 columns 中为它声明 type
```

**UUID 主键**：PostgreSQL `uuid` 列按其 16 字节排序，工具不会对它加文本排序规则。
若 UUID 存成文本，必须是规范的小写 `8-4-4-4-12` 形式，否则排序无法与数据库一致，会被拒绝。

## 看板

五个页面：连接配置 / 比对任务 / 差异明细 / 修复脚本 / 比对报告。

- **连接配置**：保存、编辑、测试连接（测试只读）。
- **比对任务**：选两侧连接 + YAML，创建即跑；可调限速；详情页有每表行数/差异数/耗时和运行日志。
- **差异明细**：按表、类型、稳定性过滤，按主键或内容搜索；`modified` 行列出具体列及两侧值。
- **修复脚本**：选表与差异类型 → 预览 SQL 与回滚 SQL → 二次确认后执行。
- **比对报告**：汇总与逐表统计，可导出 CSV。

修复默认跳过 `transient` 行（比对期间还在变的行），避免把过期数据写回。

## 保证与边界

**能证明的**：在两侧数据在比对窗口内不再变化时，输出的差异行集合与真实差异**逐行一致**，
并给出每行的差异类型与列。

**有限制的**：

- 表必须有稳定且非空的主键（或全列 NOT NULL 的唯一索引）。没有主键的表会被拒绝，而不会
  退化成"整行文本比较"——那种方式在重复行和 NULL 上不可靠。
- 文本主键要求两侧连接使用同一字符集：不同字符集下相同字符的字节序不同，区间切分就失去意义。
- 源库持续写入时，差异行会经过 settle 期重读：能证明的是"重读时仍然存在"，不能证明
  "任意时刻都不存在"。`transient` 标记是保守的——只有观察到源行变化才标记。
- 摘要比对只在服务端做 SHA-256，摘要相等即视为相等（与行级归并的指纹不同，后者不依赖数据库函数）。

## 目录

```
cmd/bankcmp        CLI（compare 子命令）+ HTTP 服务 + 内嵌看板
cmd/seedtest       造数：埋入已知差异并写 manifest
internal/canon     值规整：decimal/日期/字符集/布尔/NULL
internal/dialect   两侧 SQL 方言差异（标识符、占位符、排序、字面量）
internal/source    连接、内省、SQLite 摘要 UDF
internal/plan      把表配置解析成两侧可执行的计划（键、列、排序一致性校验）
internal/engine    分块校验和、区间二分、精确归并、稳定性重读、限速
internal/sqlgen    修复 SQL 生成（含回滚）
internal/journal   任务/表/差异/修复/日志的持久化
internal/app       任务调度与表级编排
internal/api       HTTP JSON API
internal/verify    manifest 金标准核对
internal/seed      造数与埋点
```

## 测试

```bash
go test ./...                       # 单元 + 端到端（SQLite）
BANKCMP_TEST_PG=... go test ./...   # 追加 PostgreSQL 集成
go test -race ./internal/engine     # 并发路径
```

`internal/verify/e2e_test.go` 会造数、比对、逐条对照 manifest；`internal/engine`
下的 `concurrent_test.go` 验证写入期间的瞬态判定。
