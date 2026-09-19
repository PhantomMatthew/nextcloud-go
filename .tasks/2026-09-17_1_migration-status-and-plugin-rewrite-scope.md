# 背景
文件名：2026-09-17_1_migration-status-and-plugin-rewrite-scope.md
创建于：2026-09-17_21:26:40
创建者：matthew
主分支：main
任务分支：main（未创建功能分支，本任务目前仅为研究记录）
Yolo模式：Off

# 任务描述
对 nextcloud-go 迁移项目做一次现状盘点：
1. 对照 `docs/plans/*` 与 `docs/adr/*` 中承诺的各阶段范围，梳理尚未完成的任务。
2. 针对插件系统，梳理"不兼容 PHP app 生态、以 WASM 重写"这一既定路线下，从当前零代码状态到 `docs/specs/wasm-plugin-abi.md` 全量落地所涉及的改动范围。

# 项目概览
`nextcloud-go`：用 Go 对 Nextcloud Server（PHP）做 greenfield 重写，目标是对既有桌面/移动/CalDAV/CardDAV 客户端做线协议级兼容，不做数据/运行时兼容。五阶段计划（Phase 0–4，约 24 个月）。当前 20 次提交、83 个 Go 文件、约 5000 行非测试代码，`go test ./...` 全绿，`go.mod` 无任何第三方依赖。

⚠️ 警告：永远不要修改此部分 ⚠️
RIPER-5 协议摘要：
- 每个响应开头必须声明模式：[MODE: RESEARCH|INNOVATE|PLAN|EXECUTE|REVIEW]。
- 默认 RESEARCH；仅在收到 "ENTER XXX MODE" 明确信号后切换。
- RESEARCH：只读、只观察、只提问；禁止建议与实施。
- INNOVATE：只讨论可能性与利弊；禁止具体规划与代码。
- PLAN：输出精确到文件/函数签名的规范与编号实施清单；禁止写代码。
- EXECUTE：100% 忠实执行已批准清单；任何偏差立即回到 PLAN；每步后追加"任务进度"并请用户确认。
- REVIEW：逐行比对计划与实施，标记一切偏差，给出"完全匹配 / 偏离计划"结论。
- 常规交互用中文；模式声明、代码块、清单保持英文格式。禁用表情符号。
⚠️ 警告：永远不要修改此部分 ⚠️

# 分析

## A. 仓库整体现状（2026-09-17 快照）

提交历史（最新在上）：WebDAV MKCOL/DELETE/MOVE/COPY → GET/HEAD/PUT → PROPFIND 骨架 → Login Flow v2 MVP + CSRF → app-password 生命周期 → Basic auth/OCSMiddleware/cloud/user → capabilities → status.php → httpx 中间件 → OCS envelope → golden 工具链 → 仓库骨架 → 规划文档。

运行时状态完全在内存中：`auth.NewMemoryStore()`、`login.NewMemoryStore()`、`webdav.NewInMemoryFS()`；用户校验为硬编码 `auth.NewStaticVerifier("admin","admin","admin")`；`NCGO_SECRET` / `NCGO_INSTANCE_ID` 缺省时每次启动随机生成（token 与 file id 不跨重启）。

仅含 3 行 `doc.go`、无任何实现的包：
`internal/config`、`internal/database`、`internal/migrations`、`internal/cache`、`internal/storage`、`internal/jobs`、`internal/session`、`internal/modules`、`internal/plugins`、`pkg/api`、`pkg/pluginsdk`。
`cmd/ncgo-cli`、`cmd/ncgo-captest` 各 13 行空壳。`internal/observability` 仅三个版本变量。文档中的 `internal/eventbus` 包不存在。

ADR-0001 锁定的技术栈（chi、pgx、go-sql-driver/mysql、modernc sqlite、squirrel、golang-migrate、go-redis、ristretto、wazero、prometheus、otel、koanf、cobra、testify、testcontainers）均未引入。

## B. 各阶段未完成任务

### B.1 Phase 0（文档状态 Accepted，退出条件多数未满足）

| 退出条件 | 状态 |
|---|---|
| `ncgo` 用有效配置启动 | 部分：可启动，但无配置加载器，仅 `-addr` flag + 少量环境变量 |
| `GET /status.php` | 完成 |
| OCS `cloud/capabilities` | 完成（仅 `core` + `version` 块） |
| 迁移在 PG16/MySQL8/SQLite 干净运行 | 未开始：无 DB 层、无迁移文件、无驱动 |
| ≥50 golden case，≥10 可回放 | 6 个 case（status 1、capabilities 2、cloud-user 2、ocs 1），全部 synthetic；`tools/capture/` 不存在；无 HAR；`ncgo-captest` 空壳 |
| WASM hello-world 插件加载并打日志 | 未开始 |
| CI 矩阵 Linux/macOS × Go 1.22/1.23 | 已有 `.github/workflows/ci.yml` |
| golangci-lint / vet / staticcheck 干净 | 有 `.golangci.yml`，未验证 |
| 覆盖率 ≥60% | 未验证 |

### B.2 Phase 1（当前实际所处阶段）

已落地：`/status.php`；OCS envelope（V1/V2、XML/JSON）；capabilities；`cloud/user`；Basic auth；app password 签发/校验/撤销（`core/getapppassword`、`DELETE core/apppassword`）；Login Flow v2 五端点；CSRF PathBypass；maintenance 中间件（硬编码 false）；WebDAV `OPTIONS/PROPFIND/GET/HEAD`。

未完成（Phase 1 范围内）：
- 本地文件系统存储后端（当前仅 InMemoryFS）
- 自定义属性：已有 `oc:id`、`oc:fileid`、`oc:permissions`、`oc:size`；缺 `oc:checksums`、`oc:owner-id`、`oc:owner-display-name`、整个 `nc:` 命名空间
- quota 属性 `d:quota-used-bytes` / `d:quota-available-bytes`
- ETag 的 filecache 父链传播（`etag.go` 存在，但无 DB 支撑）
- Session cookie 与 Bearer token 认证器
- Argon2id 密码哈希与真实用户存储（`internal/users` 包未建，`cloud/user` 直接写在 `internal/ocs`）
- bruteforce 节流
- 蓝图规划 12 个 golden case，现有 6 个；要求 ≥5 个非 synthetic，现为 0
- 真实桌面客户端连接冒烟测试与录像证据（`docs/evidence/phase1/` 不存在）

### B.3 Phase 2

已提前落地（InMemoryFS 上）：`PUT/MKCOL/DELETE/MOVE/COPY`。

未开始：分块上传 v2（`PUT` 遇 `OC-Chunked` 头返回 501；无 `/remote.php/dav/uploads/` 路由；capabilities 无 `dav.chunking`）、`PROPPATCH`、`LOCK/UNLOCK`、trashbin、versions、公开链接分享、OCS Sharing API、S3 后端、后台任务框架、搜索。
工作区有未提交的 `docs/CHUNKED_UPLOAD_V2_SPEC.md`（2026-05-01），为 Phase 2 前置研究产物。

### B.4 Phase 3 / Phase 4

CalDAV/CardDAV、OCM 联邦、通知、活动流、插件系统、Admin UI、`ncgo-cli` 功能、`import-nextcloud`、加密、预览：全部零代码。

### B.5 文档与代码漂移

| 文档 | 代码 |
|---|---|
| `internal/plugin` | `internal/plugins` |
| `internal/db` | `internal/database` |
| `internal/obs` | `internal/observability` |
| 顶层 `modules/` | `internal/modules` |
| `cmd/ncgo-server` | `cmd/ncgo` |
| `test/golden/` | `testdata/golden/` |
| chi 路由（ADR-0001） | 自研 `httpx.NewRouter` |
| `plugin.json`（ADR-0003） | `plugin.toml`（ABI 规范） |
| `internal/X` 仅经 `pkg/api` 交互 | `pkg/api` 为空；`internal/webdav` 直接 import `internal/auth` |
| `examples/hello-plugin/` | 不存在 |

## C. 插件系统：不兼容重写路线的改动范围

前提事实："不兼容 PHP app、以 WASM 重写"是 ADR-0002/ADR-0003 已锁定的决策，不是待选项。当前插件相关代码为零，因此改动范围等于 `docs/specs/wasm-plugin-abi.md` 全文。按依赖自底向上：

### C.1 宿主运行时（`internal/plugins`）
引入 wazero；不挂载 WASI；模块编译 + 磁盘缓存；实例模型 `per_request / pooled / singleton`；fuel 计量、线性内存上限（默认 32 MiB、宿主硬顶 256）、单次调用 wall-clock 超时；trap 处理（销毁实例、带插件 ID 日志、HTTP 500 / job failed）；每实例 handle 表（流 64、DB rows 16、HTTP response 16）与跨实例 handle 拒绝。

### C.2 清单、打包、签名
`plugin.toml` 解析（ADR-0001 技术栈无 TOML 库，选型未做）；`.ncplugin` zip 读取；ed25519 签名校验；`ncgo-abi/1` 版本匹配与 N/N-1 兼容窗口；`on_upgrade` 时能力变更需重新审批。

### C.3 能力代理
默认拒绝；`db.read/write` 表名 glob；`storage.read/write` user/system 作用域；`http.outbound` host:port 白名单 + 私网 IP 阻断；`events.publish/subscribe` topic 与 `core.*` 保护；`routes.register/ocs.register` 的 `/apps/<plugin-id>/` 前缀强制；`webdav.props` 命名空间约束；`config.read/write` 键 glob；授权状态持久化（规范暗示 `module_config`，目前无 DB）。

### C.4 宿主函数目录（ABI §6）
约 50 个导出到 `ncgo` 模块的函数，13 组：log、ctx、config、db（事务 + handle 式 rows）、cache、storage（流式）、http、events、jobs、route/ocs 注册、webdav 属性注册、crypto。统一内存约定（插件导出 `ncgo_alloc/ncgo_free`，UTF-8 长度前缀字符串，结构化数据 MessagePack）。每函数需 capability 检查、边界检查、Prometheus counter/histogram、OTel span。

### C.5 SQL 安全层
按方言解析并做表名白名单：Postgres `pg_query_go`、MySQL Vitess parser、SQLite 自研 shim。
已识别冲突：`pg_query_go` 是 `libpg_query` 的 cgo 封装，与 ADR-0001 "No CGO" 硬约束矛盾；ABI 规范 §14 Open Question 1 亦承认解析器选型未定。

### C.6 宿主侧依赖子系统（目前全部缺失）
`internal/database`（DB/Tx/Querier + 三驱动）、`internal/cache`、`internal/storage`（Storage 接口 + localfs/S3）、`internal/jobs`（Runner）、`internal/eventbus`（包不存在；WebDAV PUT 目前不发 `files.uploaded`）、`internal/config`（`plugin:` 段、`install_dir`）、`httpx` 运行期挂载 `/apps/<id>/*` 与 OCS `/apps/<id>/api/*`、`webdav/propfind.go` 从硬编码属性改为属性注册表、`capabilities.Manager` 接受插件注册。Phase 0 蓝图标注这些接口"冻结于 Phase 0"，实际均未冻结。

### C.7 公共契约层
`pkg/api`：`Module`、`Host`/`ModuleHost`、`Route` 等接口；承担 `internal/X` 解耦职责，意味着现有 `internal/webdav → internal/auth` 等直接依赖需改造。
`pkg/pluginsdk`：`//go:build wasm` + `//go:wasmimport ncgo ...` 绑定、`ncgo_alloc/ncgo_free`、MessagePack 编解码、`Request/Response`、`DBQuery/DBExec/OCSRegister` 等高层 API；TinyGo 构建链；`examples/hello-plugin/`。

### C.8 生命周期与运维面
六个入口点（`on_install/on_uninstall/on_upgrade/on_request/on_job/on_event`）调度；插件表 DDL 仅限 install hook；config/cache/job 名自动前缀 `plugin.<id>.` / `plugin:<id>:`；`ncgo-cli plugin install/list/remove/approve`（cobra 未引入）；Phase 4 Admin UI 每插件仪表盘。

### C.9 "不兼容"的连带改动
捆绑 app 功能（trash、versions、sharing、CalDAV/CardDAV、activity、notifications）需作为一方模块在 `internal/modules` 重写（Phase 2–3 主体），并在 Phase 4 作为验证 ABI 的参考插件；`import-nextcloud` 需识别并跳过/警告不受支持 app 的 `oc_*` 表；ADR-0002 提及的前置 "compatibility report" 工具；GUI 插件为 v1 Non-Goal，Talk/Mail/Office 等重前端 app 无扩展点；ABI 规范 §14 六个 Open Question 全部悬置；§13 安全清单 10 项全未勾选。

## D. 待用户确认的问题
"不兼容重写"的含义：
1. 相对 PHP app 生态不兼容（既定决策，本文件 C 节基于此）；
2. 相对现有 ABI 规范草案不兼容，即推翻 wazero / raw imports / MessagePack 方案重新设计（如 Component Model/WIT，或 go-plugin 子进程模型）。
两者改动范围差异很大，尚待用户答复。

## E. 排序原则（用户于 2026-09-17 21:29 确定）："补齐 Phase 0 基础设施优先"

### E.1 Phase 0 蓝图给出的基础设施内部依赖顺序（`docs/plans/01-phase-0-blueprint.md` Week 1–8）
1. Week 1：`internal/config`（koanf 加载 + schema 校验）、Dockerfile、compose。
2. Week 2：`internal/db` 接口 + pgx/mysql/sqlite 三驱动；`migrations/0001_init.sql` 三方言；迁移集成到 `ncgo` 启动与 `ncgo-cli migrate`；起 reference Nextcloud + mitmproxy。
3. Week 3 / 5：捕获冲刺（`tools/capture/` mitmproxy 脚本、HAR → golden、≥50 case）。
4. Week 4：httpx / ocs（已完成）；`ncgo-captest` v0 回放。
5. Week 6：auth 基础类型 + Argon2id；`internal/cache` 分层缓存。
6. Week 7：WASM 插件 stub（wazero、`plugin.toml` 解析、`ncgo.log`、`pkg/pluginsdk` 三函数、`examples/hello-plugin/`、集成测试）。
7. Week 8：CI 矩阵扩展 {Postgres, MySQL, SQLite}、lint/vet/staticcheck 干净、覆盖率 ≥60%、文档定稿。
另有"核心接口冻结"要求：db / storage / cache / auth / jobs / plugin Host / Module 接口在 Phase 0 定稿。

### E.2 当前构建与 CI 约束
- `go.mod`：`go 1.23`，零依赖。CI 矩阵 Go 1.22 / 1.23（1.22 runner 将触发 toolchain 自动下载）。本机 Go 1.27.1。
- golangci-lint：CI 固定 v1.61，配置为 v1 格式，启用 40 个 linter，含 forbidigo（禁 `fmt.Print*`、禁 `panic(`）、sloglint（attr-only、context scope）、gofumpt extra-rules、gosec、errcheck check-blank。`internal/webdav/handler.go` 现有 `panic("webdav: prefix must start with /")`，与 forbidigo 规则冲突（lint 是否已在本地跑过未知）。
- CI 另有 tidy check 与 govulncheck job。本机未安装 govulncheck、tinygo；已安装 docker、golangci-lint、staticcheck。
- `Makefile` 已定义 build / test / cover / lint / verify / docker / dev-up 等目标，构建三个二进制。
- `deploy/docker/docker-compose.dev.yml` 只有 postgres + redis + ncgo，缺蓝图中的 mysql、minio、reference-nextcloud、mitmproxy；其中 `NCGO_DB_DRIVER / NCGO_DB_DSN / NCGO_CACHE_DRIVER / NCGO_CACHE_DSN / NCGO_HTTP_LISTEN / NCGO_LOG_LEVEL` 六个环境变量目前无任何代码读取。
- `internal/app` 包（蓝图中的依赖装配层）不存在，装配全部在 `cmd/ncgo/main.go`。

### E.3 goldentest 现有能力
`internal/goldentest` 已具备 `Load`、`ParseRequest/ParseResponse`、`Normalize`、`RunHandler`（进程内）、`RunHTTP`（对 baseURL）、`Diff`；case 格式为 `case.yaml + request.http + response.http`，schema v1，含 `provenance.capture`、`replayable`、`assertions` 字段。`cmd/ncgo-captest` 只需接线，不需从零实现比对逻辑。`tools/golden-gen` 已有 `import-har` 子命令。

### E.4 技术栈各库当前版本（2026-09-17 `go list -m -versions` 查询）
| 模块 | 最新版 |
|---|---|
| github.com/knadh/koanf/v2 | v2.3.6 |
| github.com/jackc/pgx/v5 | v5.11.0 |
| github.com/go-sql-driver/mysql | v1.10.1 |
| modernc.org/sqlite | v1.59.0 |
| github.com/golang-migrate/migrate/v4 | v4.20.1 |
| github.com/Masterminds/squirrel | v1.5.4 |
| github.com/tetratelabs/wazero | v1.12.0 |
| github.com/dgraph-io/ristretto/v2 | v2.4.2 |
| github.com/redis/go-redis/v9 | v9.23.0-beta.1（最新稳定版需另查） |
| github.com/spf13/cobra | v1.10.2 |
| github.com/stretchr/testify | v1.12.1 |
| github.com/testcontainers/testcontainers-go | v0.44.0 |
| github.com/prometheus/client_golang | v1.24.1 |
| github.com/BurntSushi/toml | v1.6.0 |
| github.com/pelletier/go-toml/v2 | v2.4.3 |
| golang.org/x/crypto | v0.57.0 |

### E.5 用户决定（2026-09-17 21:34）：构建约束改为 Go 1.27 最新版
当前 Go 1.27 系列稳定版：go1.27.0、go1.27.1（最新）。该决定牵连的现有引用点：
- `go.mod:3` `go 1.23`
- `.github/workflows/ci.yml:24` 矩阵 `["1.22","1.23"]`；`:48` 覆盖率上传条件 `matrix.go == '1.23'`；`:62` `:76` `:93` lint / tidy / govulncheck job 各自 `go-version: "1.23"`
- `.github/workflows/ci.yml` lint job 使用 `golangci/golangci-lint-action@v6` + `version: v1.61`：v1.61 以 Go 1.23 构建，无法分析 Go 1.27 代码。golangci-lint 最新为 v2.13.2（v2 系列配置格式与 v1 不兼容：需 `version: "2"`，`linters.disable-all` 改为 `linters.default: none`，`gofmt/gofumpt/goimports` 迁至 `formatters` 段，`gosimple/stylecheck` 并入 `staticcheck`，`tenv` 已移除，`output.formats` 结构变化）。本机安装的是 v1.64.8。
- `Dockerfile:6` `FROM golang:1.23-alpine`
- `CONTRIBUTING.md:27` "1.22 or 1.23"
- `docs/plans/00-phased-rewrite-plan.md:65`、`docs/plans/01-phase-0-blueprint.md:471,485`、`docs/plans/03-phase-1-blueprint.md:344` 中的 CI 矩阵描述（plans 为 living document，可更新并记 Change Log）
- `docs/plans/03-phase-1-blueprint.md:183` "Go 1.22+ ServeMux" 为最低版本说明，与 1.27 不冲突

### E.6 制定计划前尚待决定的事实性缺口
- TOML 库未选型（ADR-0001 未覆盖）。
- hello-plugin 的构建方式：本机无 TinyGo；规范要求不暴露 WASI，标准 Go 的 `wasip1` 目标依赖 WASI import，与规范冲突；可选路径包括 CI 安装 TinyGo、或提交预编译 `.wasm` 二进制、或测试中以手写 wasm 字节构造最小模块。
- 捕获冲刺依赖真实客户端与 reference Nextcloud 实例，属于需要人工操作/外部环境的项目，无法纯代码完成。
- 数据库集成测试需 Docker（testcontainers）或 CI service containers；SQLite 可无外部依赖。

# 提议的解决方案

## P0. 范围与边界
目标：关闭 Phase 0 蓝图（`docs/plans/01-phase-0-blueprint.md`）中所有可由代码关闭的退出条件，并把构建约束提升到 Go 1.27.x。
包含：工具链升级；`internal/config`；`internal/database` + `internal/migrations`；`internal/cache`；`internal/storage`（接口 + localfs）；`internal/jobs`（仅接口）；`pkg/api`（Module/Route/ModuleHost/Host 契约）；auth 持久化 + Argon2id + `internal/users`；WASM 插件 stub（`internal/plugins`、`pkg/pluginsdk`、`examples/hello-plugin`）；`internal/app` 装配层；`cmd/ncgo` / `cmd/ncgo-cli` / `cmd/ncgo-captest` 接线；`tools/capture` 与 compose 捕获 profile；CI 矩阵与覆盖率门禁；ADR-0006 与 plans Change Log。
不包含（Phase 1+）：WebDAV 接入 storage/DB、Session cookie/Bearer 认证器、bruteforce、metrics/otel、jobs 实现、S3、插件 ABI 除 `ncgo.log` 外的宿主函数。
无法由代码关闭、需人工执行的退出条件：≥50 个真实捕获 golden case；真实客户端冒烟。本计划只交付捕获工具与 runbook。

## P1. 关键决策（批准计划即批准以下决策）
D1 Go 版本：`go.mod` 写 `go 1.27.1`；所有 CI job 用 `go-version-file: go.mod`；矩阵仅保留 OS 维度。
D2 lint：golangci-lint v2.13.2，`golangci/golangci-lint-action@v8`；`.golangci.yml` 迁移到 v2 格式（`golangci-lint migrate` 后人工核对）。
D3 TOML 库：`github.com/pelletier/go-toml/v2`（插件清单）。配置文件为 YAML（蓝图 schema），经 koanf 加载。
D4 环境变量映射：前缀 `NCGO_`，`__`（双下划线）表示层级，单下划线保留在键名内：`NCGO_DATABASE__DSN` → `database.dsn`。兼容别名：`NCGO_SECRET`→`instance.secret`、`NCGO_INSTANCE_ID`→`instance.id`、`NCGO_MAINTENANCE`→`maintenance.enabled`。compose 文件同步改名。
D5 DB 访问路径：统一走 `database/sql`（pgx 经 `pgx/v5/stdlib`、go-sql-driver/mysql、modernc.org/sqlite），一方 SQL 统一用 `?` 占位符，Postgres 由 `database.Rebind` 重写为 `$n`；squirrel 用于 store 层，占位符格式由 `database.Placeholder(dialect)` 提供。
D6 时间戳列：三方言统一 `BIGINT` 存 Unix 毫秒（偏离蓝图 `TIMESTAMPTZ`，理由：跨驱动扫描一致，记入 ADR-0006）。
D7 集成测试：不引入 testcontainers（延后，记入 ADR-0006）；Postgres/MySQL/Redis 测试置于 `//go:build integration`，从 `NCGO_TEST_POSTGRES_DSN` / `NCGO_TEST_MYSQL_DSN` / `NCGO_TEST_REDIS_ADDR` 读取，缺省 skip；CI ubuntu job 用 service containers 提供。SQLite 测试无条件运行。
D8 插件资源限制：wazero v1 无 fuel 计量 API；Phase 0 stub 仅实现内存页上限 + wall-clock 超时（`WithCloseOnContextDone`），fuel 项记入 ADR-0006 与 ABI 规范 Change Log 作为 Phase 4 待解。
D9 hello-plugin 测试策略：`internal/plugins` 单测使用 `internal/plugins/internal/wasmgen` 手工编码生成的最小 wasm 二进制（不依赖 TinyGo）；`examples/hello-plugin` 为 TinyGo 源码（`//go:build tinygo`），由可选 CI job（`continue-on-error: true`）构建并用 `ncgo-cli plugin check` 加载验证。
D10 引导管理员：取消硬编码 `admin/admin`。`ncgo serve --dev` 使用共享内存 SQLite + 引导 `admin/admin` + 临时密钥；非 dev 模式下用户表为空且未配置 `auth.bootstrap_admin` 时启动失败并给出明确错误。
D11 `login.Store.DeleteExpired` 签名改为 `(int, error)`；`webdav.NewHandler` 改为返回 `(*Handler, error)` 以消除 `panic`（forbidigo）。
D12 `pkg/api` 在 Phase 0 只承载 `Module` / `Route` / `ModuleHost` / `Host` 四个契约；DB/Storage/Cache/Jobs 接口按蓝图放在各自 `internal/<pkg>` 中。架构文档中"internal/X 仅经 pkg/api 交互"的规则在 Phase 0 不强制，记入 ADR-0006 作为待议项。

## P2. 工作包规范

### WP0 工具链升级
文件：`go.mod`、`.github/workflows/ci.yml`、`.golangci.yml`、`Dockerfile:6`、`CONTRIBUTING.md:27`、`internal/webdav/handler.go`（NewHandler）及其调用方。
- `go.mod`：`go 1.27.1`。
- `ci.yml`：`test` job 矩阵 `os: [ubuntu-latest, macos-latest]`，`setup-go` 用 `go-version-file: go.mod`；覆盖率上传条件改为 `matrix.os == 'ubuntu-latest'`；`lint` job 升 action@v8 + `version: v2.13.2`；`tidy`、`govulncheck` job 改用 `go-version-file`。
- `.golangci.yml`：v2 格式；`linters.default: none`；保留原 enable 列表中在 v2 仍存在者，`gosimple/stylecheck` 删除（并入 staticcheck），`tenv` 替换为 `usetesting`；`gofmt/gofumpt/goimports` 迁入 `formatters`；`exclusions.paths` 加 `testdata`、`bin`、`dist`、`examples`；原 `exclude-rules` 迁为 `exclusions.rules`。
- `webdav.NewHandler(prefix string, fs FS, instanceID string) (*Handler, error)`；错误 `ErrInvalidPrefix`。
- 运行 `golangci-lint run` 修复全部现存告警；修复必须行为保持；若某项修复需要改动导出 API（除 D11），回到 PLAN。
- 运行 `make cover` 记录当前覆盖率基线到任务文件。
验收：`go build ./...`、`go vet ./...`、`go test -race ./...`、`golangci-lint run` 本地全绿。

### WP1 `internal/config`
文件：`config.go`、`defaults.go`、`load.go`、`validate.go`、`config_test.go`、`testdata/full.yaml`、`testdata/minimal.yaml`。
类型（`koanf:"..."` tag 与 YAML 键一致）：
```
type Config struct {
    Server        ServerConfig        // listen, trusted_proxies []string, trusted_domains []string, base_url
    Database      DatabaseConfig      // driver, dsn, max_open_conns, max_idle_conns, conn_max_lifetime, auto_migrate bool
    Cache         CacheConfig         // l1_max_items, l1_max_cost_mb, redis_addr, redis_db, redis_password
    Storage       StorageConfig       // default_backend, backends map[string]BackendConfig{type, root, endpoint, bucket, access_key_id, secret_access_key, region}
    Auth          AuthConfig          // session_ttl, app_password_ttl, password_hash, argon2id{memory_kb, iterations, parallelism}, bootstrap_admin{uid, password, display_name}
    Jobs          JobsConfig          // workers, poll_interval
    Plugin        PluginConfig        // enabled, install_dir, default_memory_limit_mb, default_cpu_timeout_ms
    Observability ObservabilityConfig // log_level, log_format, metrics_listen, otel_endpoint
    Maintenance   MaintenanceConfig   // enabled, needs_db_upgrade
    Instance      InstanceConfig      // id, secret
}
type LoadOptions struct { Path string; EnvPrefix string; Overrides map[string]any }
func Default() *Config
func Load(opts LoadOptions) (*Config, error)
func (c *Config) Validate() error
```
优先级：Default < 文件（存在时）< 环境变量 < Overrides（flag）。
校验：`database.driver ∈ {postgres,mysql,sqlite}`；`database.dsn` 非空；`server.listen` 可被 `net.SplitHostPort` 解析；`observability.log_level ∈ {debug,info,warn,error}`、`log_format ∈ {json,text}`；argon2id 三参数 >0；`plugin.default_memory_limit_mb ≤ 256`、`default_cpu_timeout_ms ≤ 30000`；`storage.default_backend` 必须存在于 `backends`。错误类型 `*ValidationError{Field, Reason}`，多错误用 `errors.Join`。`instance.secret` 为空不报错（由 app 层生成临时值并 WARN）。
测试：默认值快照；文件 + env 覆盖顺序；`__` 层级映射；别名；每条校验规则的表驱动。

### WP2 `internal/database`
文件：`db.go`、`dialect.go`、`open.go`、`rebind.go`、`errors.go`、`sqldb.go`、`db_test.go`、`integration_test.go`（tag integration）。
```
type Dialect string  // DialectPostgres="postgres" DialectMySQL="mysql" DialectSQLite="sqlite"
type Querier interface { Query(ctx, q string, args ...any) (Rows, error); QueryRow(ctx, q string, args ...any) Row; Exec(ctx, q string, args ...any) (Result, error) }
type DB interface { Querier; Begin(ctx) (Tx, error); Close() error; Ping(ctx) error; Dialect() Dialect }
type Tx interface { Querier; Commit() error; Rollback() error }
type Rows interface { Next() bool; Scan(dest ...any) error; Close() error; Err() error }
type Row interface { Scan(dest ...any) error }
type Result interface { RowsAffected() (int64, error) }
type Config struct { Driver Dialect; DSN string; MaxOpenConns, MaxIdleConns int; ConnMaxLifetime time.Duration }
func Open(ctx, cfg Config) (DB, error)
func Unwrap(db DB) (*sql.DB, bool)
func Rebind(d Dialect, query string) string
func Placeholder(d Dialect) squirrel.PlaceholderFormat
func IsUniqueViolation(d Dialect, err error) bool
var ErrUnsupportedDriver, ErrNoRows(=sql.ErrNoRows)
```
实现：`sqlDB{db *sql.DB; d Dialect}` 在 `Query/QueryRow/Exec` 前对 Postgres 调 `Rebind`；`Rebind` 需跳过字符串字面量内的 `?`。SQLite：`Open` 自动追加 `_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)`；DSN 含 `mode=memory` 时强制 `MaxOpenConns=1`。MySQL：DSN 缺 `parseTime` 不强加（时间为 BIGINT）。唯一约束错误码：pg `23505`、mysql `1062`、sqlite `2067/1555`。
测试：SQLite 建表/插入/事务回滚/Ping/唯一冲突；Rebind 表驱动（含字面量内 `?`）；integration 对 PG/MySQL 重复同一套。

### WP3 `internal/migrations`
文件：`embed.go`、`migrate.go`、`migrate_test.go`、`integration_test.go`、`sql/postgres/0001_init.up.sql|.down.sql`、`sql/mysql/...`、`sql/sqlite/...`。
```
func Up(ctx, db *sql.DB, d database.Dialect, logger *slog.Logger) (applied int, err error)
func Down(ctx, db *sql.DB, d database.Dialect, steps int, logger *slog.Logger) error
func Version(ctx, db *sql.DB, d database.Dialect) (version uint, dirty bool, err error)
var ErrDirty
```
golang-migrate `source/iofs` + `database/pgx/v5`、`database/mysql`、`database/sqlite`。`0001_init`：`users`、`groups`、`group_members`、`sessions`、`app_passwords`（id TEXT PK、user_id FK、token_hash UNIQUE、login_name、name、type INT、created_at、last_used_at NULL、expires_at NULL）、`login_flows`（poll_token PK、login_token UNIQUE、state_token UNIQUE、client_name、state INT、server、login_name、app_password NULL、created_at、expires_at）、`jobs`、`module_config`。时间列全部 `BIGINT`（D6）；MySQL 中做索引的文本列用 `VARCHAR(255)`；SQLite 主键 `INTEGER PRIMARY KEY AUTOINCREMENT`。
测试：SQLite Up → 校验 `sqlite_master` 含全部表 → Down 全部 → 再 Up；`Version`；integration 对 PG/MySQL。

### WP4 auth 持久化、Argon2id、`internal/users`
文件：`internal/auth/password.go`、`password_test.go`、`sqlstore.go`、`sqlstore_test.go`；`internal/login/sqlstore.go`、`sqlstore_test.go`、`v2.go`（D11）；`internal/users/{doc.go,user.go,sqlstore.go,verifier.go,bootstrap.go,*_test.go}`。
```
// auth
type PasswordHasher interface { Hash(pw string) (string, error); Verify(hash, pw string) (bool, error); NeedsRehash(hash string) bool }
type Argon2idParams struct { MemoryKB, Iterations uint32; Parallelism uint8; SaltLen, KeyLen uint32 }
func NewArgon2id(p Argon2idParams) *Argon2id       // PHC 格式 $argon2id$v=19$m=,t=,p=$salt$hash
var ErrInvalidHash
func NewSQLStore(db database.DB) *SQLStore          // 实现 auth.Store（app_passwords）
// login
type Store interface { ...; DeleteExpired(ctx, now time.Time) (int, error) }
func NewSQLStore(db database.DB) *SQLStore          // 实现 login.Store（login_flows）
// users
type User struct { ID int64; UID, DisplayName, Email, PasswordHash string; QuotaBytes *int64; Enabled bool; CreatedAt, UpdatedAt time.Time }
type Store interface { Create(ctx, *User) error; GetByUID(ctx, uid string) (*User, error); UpdatePasswordHash(ctx, id int64, hash string) error; Count(ctx) (int64, error) }
func NewSQLStore(db database.DB) *SQLStore
func NewPasswordVerifier(store Store, h auth.PasswordHasher) *PasswordVerifier   // 实现 auth.Verifier；Enabled=false → auth 失败
type BootstrapAdmin struct { UID, Password, DisplayName string }
func EnsureBootstrapAdmin(ctx, store Store, h auth.PasswordHasher, b BootstrapAdmin, logger *slog.Logger) error
var ErrNotFound, ErrExists, ErrNoUsers
```
`EnsureBootstrapAdmin`：`Count==0` 且 `b.UID!=""` → 创建；`Count==0` 且未配置 → `ErrNoUsers`；`Count>0` → 无操作。`StaticVerifier` 保留仅供测试。
测试：Argon2id 往返/错误密码/篡改 hash/参数变更触发 NeedsRehash；SQL store 用 SQLite 覆盖 auth/login/users 全部方法；`PasswordVerifier` 禁用用户、不存在用户、错误密码返回 `auth` 现有错误语义（与 `StaticVerifier` 测试对齐）。

### WP5 `internal/cache`
文件：`cache.go`、`memory.go`、`redis.go`、`tiered.go`、`memory_test.go`、`tiered_test.go`、`redis_integration_test.go`。
```
type Cache interface { Get(ctx, key string) ([]byte, error); Set(ctx, key string, val []byte, ttl time.Duration) error; Delete(ctx, key string) error; Increment(ctx, key string, delta int64) (int64, error) }
var ErrMiss
type MemoryConfig struct { MaxItems int64; MaxCostBytes int64 }
func NewMemory(cfg MemoryConfig) (*Memory, error)        // ristretto/v2 存字节；Increment 用独立 mutex map[string]int64
type RedisConfig struct { Addr, Password string; DB int }
func NewRedis(cfg RedisConfig) (*Redis, error)           // go-redis/v9；Close() error
func NewTiered(l1, l2 Cache) *Tiered                     // Get: L1→L2→ErrMiss，命中 L2 回填 L1（ttl 取 L2 TTL 不可得时用默认 30s）；Set 写穿两层；Delete 两层；Increment 有 L2 则 L2 权威并覆盖 L1
```
测试：Memory Get/Set/TTL 过期/Delete/Increment 并发；Tiered 用两个 Memory 验证回填与写穿；Redis integration。

### WP6 storage / jobs / pkg/api 接口冻结
文件：`internal/storage/storage.go`、`errors.go`、`localfs/localfs.go`、`localfs/localfs_test.go`；`internal/jobs/jobs.go`；`pkg/api/module.go`、`pkg/api/host.go`。
```
// storage
type FileInfo struct { Path string; Size int64; ModTime time.Time; IsDir bool }
type Storage interface { Stat(ctx, p string) (*FileInfo, error); Open(ctx, p string) (io.ReadSeekCloser, error); Create(ctx, p string, size int64) (io.WriteCloser, error); Delete(ctx, p string) error; List(ctx, p string) ([]*FileInfo, error); Rename(ctx, src, dst string) error; Mkdir(ctx, p string) error }
var ErrNotFound, ErrExists, ErrNotEmpty, ErrIsDir, ErrNotDir, ErrInvalidPath
func localfs.New(root string) (*FS, error)     // 路径规范化 + 越界拒绝（Clean 后必须以 root 为前缀）；Create 写临时文件后 rename 原子替换
// jobs（仅接口）
type Job interface { Name() string; Run(ctx, payload []byte) error }
type Runner interface { Register(job Job) error; Enqueue(ctx, name string, payload []byte, runAt time.Time) error; Start(ctx) error; Stop(ctx) error }
var ErrDuplicateJob, ErrUnknownJob
// pkg/api
type Route struct { Method, Pattern string; Handler http.Handler; RequiresAuth bool; Transport string }
type ModuleHost interface { Logger() *slog.Logger }
type Module interface { ID() string; Init(ctx, host ModuleHost) error; Routes() []Route }
type Host interface { Install(ctx, archive io.Reader) (*Manifest, error); Uninstall(ctx, pluginID string) error; Invoke(ctx, pluginID, function string, payload []byte) ([]byte, error); PublishEvent(ctx, topic string, payload []byte) error }
type Manifest struct { ID, Name, Version, ABI string }   // pkg/api 侧的最小投影
```
测试：localfs 全方法 + 越界路径（`../`、绝对路径、符号链接指向外部）拒绝。

### WP7 WASM 插件 stub
文件：`internal/plugins/{manifest.go,host.go,abi.go,plugin.go,errors.go,manifest_test.go,host_test.go}`、`internal/plugins/internal/wasmgen/{wasmgen.go,wasmgen_test.go}`、`pkg/pluginsdk/{abi.go,log_wasm.go,mem_wasm.go,log_stub.go}`、`examples/hello-plugin/{main.go,plugin.toml,README.md}`、`Makefile`（`example-plugin` 目标）。
```
// manifest
type Manifest struct { Plugin PluginSection; Runtime RuntimeSection; Capabilities map[string]any; EntryPoints EntryPointsSection }
type PluginSection struct { ID, Name, Version, ABI, Description, Author, Homepage, License string }
type RuntimeSection struct { InstanceModel string; PoolSize int; MemoryLimitMB int; CPUTimeoutMS int; FuelPerCall uint64 }
type EntryPointsSection struct { Module, OnInstall, OnUninstall, OnUpgrade, OnRequest, OnJob, OnEvent string }
func ParseManifest(r io.Reader) (*Manifest, error)
func (m *Manifest) Validate() error     // id ^[a-z0-9]+(\.[a-z0-9-]+)+$；abi=="ncgo-abi/1"；instance_model ∈ {per_request,pooled,singleton}（缺省 per_request）；memory ≤256；timeout ≤30000；module 非空
// host
type HostConfig struct { DefaultMemoryLimitMB int; DefaultCallTimeout time.Duration }
func NewHost(ctx, cfg HostConfig, logger *slog.Logger) (*Host, error)   // wazero.NewRuntimeWithConfig(WithCloseOnContextDone(true), WithMemoryLimitPages)；注册 host module "ncgo"（仅 log）；不实例化 WASI
func (h *Host) Load(ctx, m *Manifest, wasm []byte) (*Plugin, error)     // 编译；拒绝任何 import 模块名 != "ncgo"（ErrForbiddenImport，含 wasi_snapshot_preview1）；校验导出 ncgo_abi_version/ncgo_alloc/ncgo_free（ErrMissingExport）
func (h *Host) Close(ctx) error
func (p *Plugin) Install(ctx) error       // 新实例；调 ncgo_abi_version 必须==1（ErrABIMismatch）；若 EntryPoints.OnInstall 非空则调用；非零返回 → *PluginError{Code int32}；超时/trap → ErrTrap 包装
func (p *Plugin) Close(ctx) error
// abi.go：ncgo.log(level i32, ptr i32, len i32) i32；level ∉[0,3] 或 ptr/len 越界 → ErrCodeInvalidArgument；len > 65536 → ErrCodeTooLarge；正常 → slog 对应级别，attrs plugin.id、plugin.version；返回 ErrCodeOK
// errors.go：ErrCode* 十三个常量（0..-12，与规范 §6.2 一致）；ErrManifestInvalid、ErrABIMismatch、ErrMissingExport、ErrForbiddenImport、ErrTrap
// wasmgen（test-only 内部包）
func HelloModule(msg string) []byte      // 手工编码：import ncgo.log；memory 1 页 export "memory"；global bump 指针；导出 ncgo_abi_version→1、ncgo_alloc、ncgo_free、ncgo_on_install（调 log(1, ptr, len) 并返回其结果）；data 段放 msg
func WASIModule() []byte                 // import wasi_snapshot_preview1.fd_write，用于 ErrForbiddenImport 测试
func NoExportsModule() []byte            // 用于 ErrMissingExport 测试
// pkg/pluginsdk
abi.go（无 tag）：const ABIVersion int32 = 1；LevelDebug..LevelError；ErrCode* 常量（与 internal/plugins 共用，internal/plugins 导入此处常量）
log_wasm.go（//go:build wasm）：//go:wasmimport ncgo log；func Debug/Info/Warn/Error(msg string)
mem_wasm.go（//go:build wasm）：//go:wasmexport ncgo_abi_version / ncgo_alloc / ncgo_free（bump 分配器）
log_stub.go（//go:build !wasm）：同名 no-op，保证宿主侧可编译
```
`examples/hello-plugin/main.go`（`//go:build tinygo`）：`//go:wasmexport ncgo_on_install` 调 `pluginsdk.Info("hello from wasm")` 返回 0。Makefile：`example-plugin: tinygo build -o examples/hello-plugin/hello.wasm -target=wasm-unknown -no-debug ./examples/hello-plugin`。
测试：manifest 解析/每条校验；Host 加载 HelloModule → Install → 用 slog 测试 handler 捕获到 `hello from wasm` 且 attrs 正确；WASIModule → ErrForbiddenImport；NoExportsModule → ErrMissingExport；超时（HelloModule 变体带无限循环 `LoopModule()`）→ ErrTrap 且耗时 ≈ 超时；越界 ptr → log 返回 -2。

### WP8 装配层、CLI、captest、捕获工具
文件：`internal/app/{app.go,dev.go,routes.go,app_test.go}`；`cmd/ncgo/main.go`（重写为 cobra）；`cmd/ncgo-cli/main.go` + `cmd/ncgo-cli/{migrate.go,user.go,plugin.go}`；`cmd/ncgo-captest/main.go`；`internal/goldentest/{runner.go,compare.go,discover.go}`；`tools/capture/{mitmproxy_har.py,README.md}`；`deploy/docker/docker-compose.dev.yml`。
```
// app
type App struct { Cfg *config.Config; Logger *slog.Logger; DB database.DB; Cache cache.Cache; Users users.Store; Router *httpx.Router; PluginHost *plugins.Host }
func New(ctx, cfg *config.Config, logger *slog.Logger) (*App, error)  // 顺序：DB Open → (auto_migrate) migrations.Up → stores → EnsureBootstrapAdmin → cache → plugin host（enabled 时）→ routes（现 main.go 内容迁入 routes.go，逻辑不变）
func (a *App) Handler() http.Handler
func (a *App) Run(ctx) error          // httpx.NewServer + Run
func (a *App) Close(ctx) error        // 逆序关闭
func DevConfig() *config.Config        // sqlite "file:ncgo-dev?mode=memory&cache=shared"，bootstrap admin/admin，log text/debug
// cmd/ncgo：`ncgo serve [--config path] [--addr] [--dev]`、`ncgo version`
// cmd/ncgo-cli：`migrate up|down --steps N|version`、`user add <uid> --display-name --password-stdin`、`plugin check <dir>`（读 plugin.toml + module，Load+Install，输出结果）；均接受 --config
// cmd/ncgo-captest：`run --cases DIR [--base-url URL] [--tag T]... [--replayable-only] [--json]`；无 --base-url 时进程内启动 app.New(DevConfig()) 用 Handler；输出每例 PASS/FAIL/SKIP 与汇总；有 FAIL 退出码 1
// goldentest 重构（行为不变）
func Discover(root string) ([]string, error)                       // 含 case.yaml 的目录，排序
func Execute(ctx, c *Case, do func(*http.Request) (*http.Response, error)) (*ParsedResponse, error)
func Compare(c *Case, want, got *ParsedResponse) error             // compareOrFail 改为调用 Compare
```
`tools/capture/mitmproxy_har.py`：mitmproxy addon，按环境变量 `CAPTURE_SCENARIO` 命名输出 `/captures/<scenario>-<ts>.har`。`README.md`：蓝图 Week 3/5 场景清单 runbook。compose：新增 `mysql`、`minio`、`reference-nextcloud`、`mitmproxy` 服务，全部置于 `profiles: [capture]`；`ncgo` 服务环境变量改为 D4 命名；Makefile 增 `capture-up/capture-down`。
测试：`app_test.go` 用 DevConfig 启动并对现有 6 个 golden case 跑 `RunHandler`（保证重构未改行为）；captest 进程内模式对 `testdata/golden` 全绿。

### WP9 CI 与文档
- `ci.yml`：ubuntu job 增 `services: postgres:16-alpine / mysql:8 / redis:7-alpine`，设 `NCGO_TEST_*` 环境变量，增加 `go test -race -tags integration ./internal/database/... ./internal/migrations/... ./internal/cache/...`；覆盖率门禁 step：`go tool cover -func=coverage.out | tail -1` 解析总覆盖率，< 60 失败；新增可选 job `example-plugin`（`acifani/setup-tinygo@v2` + `make example-plugin` + `go run ./cmd/ncgo-cli plugin check examples/hello-plugin`，`continue-on-error: true`）。
- `docs/adr/0006-phase0-toolchain-and-scope-adjustments.md`：记录 D1–D12 中偏离既有 ADR/蓝图的项（Go 最新稳定版策略、pelletier TOML、BIGINT 时间戳、无 fuel 计量、testcontainers 延后、database/sql 统一路径、pkg/api 规则暂缓）。
- `docs/plans/00-phased-rewrite-plan.md:65`、`01-phase-0-blueprint.md:471,485`、`03-phase-1-blueprint.md:344`：CI 矩阵改为 "Linux/macOS × Go 1.27"；各加 Change Log 条目。`01` 的退出条件复选框按实际状态勾选。
- `docs/specs/wasm-plugin-abi.md` §15 路径改为 `internal/plugins`，Change Log 记录 fuel 计量待解。
- `CONTRIBUTING.md:27` 改为 "1.27+"。
- `README.md` 增 `ncgo serve --dev` 快速开始一节。

## P3. 依赖清单（`go get` 时以此版本为准，`@latest` 解析结果若更高则记录）
koanf/v2 v2.3.6、koanf/providers/file、koanf/providers/env/v2、koanf/parsers/yaml；pgx/v5 v5.11.0；go-sql-driver/mysql v1.10.1；modernc.org/sqlite v1.59.0；golang-migrate/migrate/v4 v4.20.1；Masterminds/squirrel v1.5.4；tetratelabs/wazero v1.12.0；dgraph-io/ristretto/v2 v2.4.2；redis/go-redis/v9 最新稳定版（非 beta）；spf13/cobra v1.10.2；stretchr/testify v1.12.1；golang.org/x/crypto v0.57.0；pelletier/go-toml/v2 v2.4.3。不引入：testcontainers、prometheus、otel、S3 SDK、chi。

## P4. 测试方法
单测：每个 WP 自带表驱动测试，SQLite 路径无外部依赖。集成：`integration` tag + env DSN，CI service containers。回归：`app_test.go` 对现有 golden case 全量回放，保证装配重构零行为变化。端到端：`ncgo-captest run --cases testdata/golden` 进程内全绿；`ncgo serve --dev` 手工 `curl /status.php`。质量门：`go vet`、`golangci-lint run`（v2.13.2）、`go test -race`、覆盖率 ≥60%、`go mod tidy` 无 diff。

## P5. 风险
R1 golangci v2 迁移暴露大量既有告警，修复量不可预估 → WP0 单独提交，超出 D11 的 API 改动回 PLAN。
R2 TinyGo 对 Go 1.27 支持滞后 → 该 CI job 非阻塞（D9）。
R3 `//go:wasmexport` 在 TinyGo `wasm-unknown` 目标的可用性 → 若不可用，改用 `//export`，属实现细节不回 PLAN。
R4 wazero 手工编码 wasm 出错 → wasmgen 自带测试用 wazero 编译校验。
R5 go-redis 最新 stable 版本号需在 `go get` 时确认。

## P6. 实施清单
见任务文件末尾"实施清单"与响应中的编号列表（两者一致）。

# 实施清单
1. WP0：`go.mod` 改 `go 1.27.1`；`go mod tidy`。
2. WP0：本地升级 golangci-lint 至 v2.13.2；`golangci-lint migrate` 生成 v2 配置并按 WP0 规范人工核对 `.golangci.yml`。
3. WP0：`ci.yml` 按 WP0 规范修改（go-version-file、矩阵、lint action@v8 v2.13.2、覆盖率上传条件）。
4. WP0：`Dockerfile` 改 `golang:1.27-alpine`；`CONTRIBUTING.md:27` 改 "1.27+"。
5. WP0：`webdav.NewHandler` 改为返回 `(*Handler, error)`，新增 `ErrInvalidPrefix`，更新调用方与测试。
6. WP0：运行 `golangci-lint run`，修复全部告警（行为保持）；运行 `make cover`，把覆盖率基线记入任务文件。
7. WP0：commit `chore(toolchain): move to Go 1.27 and golangci-lint v2`。
8. WP1：新建 `internal/config/{config.go,defaults.go,load.go,validate.go}`，引入 koanf 及 file/env/yaml 子模块。
9. WP1：新建 `internal/config/testdata/{full,minimal}.yaml` 与 `config_test.go`，覆盖优先级、`__` 映射、别名、全部校验规则。
10. WP1：commit `feat(config): koanf-based configuration loader`。
11. WP2：新建 `internal/database/{db.go,dialect.go,open.go,rebind.go,errors.go,sqldb.go}`，引入 pgx/stdlib、mysql、modernc sqlite、squirrel。
12. WP2：新建 `db_test.go`（SQLite）与 `integration_test.go`（tag integration，PG/MySQL）。
13. WP2：commit `feat(database): database/sql facade with postgres/mysql/sqlite drivers`。
14. WP3：新建 `internal/migrations/sql/{postgres,mysql,sqlite}/0001_init.{up,down}.sql`（表结构见 WP3）。
15. WP3：新建 `internal/migrations/{embed.go,migrate.go}`，引入 golang-migrate iofs + 三数据库驱动。
16. WP3：新建 `migrate_test.go`（SQLite up/down/up、Version）与 `integration_test.go`。
17. WP3：commit `feat(migrations): embedded golang-migrate schema 0001_init for three dialects`。
18. WP4：新建 `internal/auth/password.go` + `password_test.go`（Argon2id，x/crypto）。
19. WP4：新建 `internal/auth/sqlstore.go` + 测试（app_passwords）。
20. WP4：`internal/login/v2.go` 中 `Store.DeleteExpired` 改为 `(int, error)`，更新 `MemoryStore`、`StartGC` 与测试；新建 `internal/login/sqlstore.go` + 测试。
21. WP4：新建 `internal/users/{doc.go,user.go,sqlstore.go,verifier.go,bootstrap.go}` + 测试。
22. WP4：commit `feat(auth,users): Argon2id hashing, SQL-backed stores, bootstrap admin`。
23. WP5：新建 `internal/cache/{cache.go,memory.go,redis.go,tiered.go}` + `memory_test.go`、`tiered_test.go`、`redis_integration_test.go`，引入 ristretto/v2、go-redis/v9。
24. WP5：commit `feat(cache): tiered ristretto + redis cache`。
25. WP6：新建 `internal/storage/{storage.go,errors.go}`、`internal/storage/localfs/{localfs.go,localfs_test.go}`。
26. WP6：新建 `internal/jobs/jobs.go`（接口）；新建 `pkg/api/{module.go,host.go}`。
27. WP6：commit `feat(storage,jobs,api): freeze Phase 0 core interfaces; add localfs backend`。
28. WP7：新建 `pkg/pluginsdk/{abi.go,log_wasm.go,mem_wasm.go,log_stub.go}`。
29. WP7：新建 `internal/plugins/{errors.go,manifest.go,manifest_test.go}`，引入 pelletier/go-toml/v2。
30. WP7：新建 `internal/plugins/internal/wasmgen/{wasmgen.go,wasmgen_test.go}`（HelloModule、WASIModule、NoExportsModule、LoopModule）。
31. WP7：新建 `internal/plugins/{host.go,abi.go,plugin.go,host_test.go}`，引入 wazero。
32. WP7：新建 `examples/hello-plugin/{main.go,plugin.toml,README.md}`；Makefile 增 `example-plugin` 目标。
33. WP7：commit `feat(plugins): wazero host stub with ncgo.log ABI and hello plugin`。
34. WP8：`internal/goldentest` 增 `Discover`、`Execute`、`Compare`，`RunHandler/RunHTTP` 改为调用它们（行为不变），补测试。
35. WP8：新建 `internal/app/{app.go,dev.go,routes.go,app_test.go}`，把 `cmd/ncgo/main.go` 的装配逻辑迁入，`app_test.go` 回放现有 golden case。
36. WP8：重写 `cmd/ncgo/main.go` 为 cobra（`serve`、`version`），引入 cobra。
37. WP8：实现 `cmd/ncgo-cli`（`migrate`、`user add`、`plugin check`）。
38. WP8：实现 `cmd/ncgo-captest run`；本地对 `testdata/golden` 进程内全绿。
39. WP8：新建 `tools/capture/{mitmproxy_har.py,README.md}`；修改 `deploy/docker/docker-compose.dev.yml`（D4 环境变量、capture profile 四服务）；Makefile 增 `capture-up/capture-down`。
40. WP8：commit `feat(app,cli,captest): wiring layer, operator CLI, golden replay runner, capture tooling`。
41. WP9：`ci.yml` 增 service containers、integration 测试 step、覆盖率门禁、可选 `example-plugin` job。
42. WP9：新建 `docs/adr/0006-phase0-toolchain-and-scope-adjustments.md`。
43. WP9：更新 `docs/plans/00`、`01`、`03` 的矩阵描述与 Change Log，勾选 `01` 退出条件；更新 `docs/specs/wasm-plugin-abi.md` §15 与 Change Log；`README.md` 增快速开始。
44. WP9：全量质量门：`go vet`、`golangci-lint run`、`go test -race ./...`、`go test -race -tags integration ...`（本地有 Docker 时）、`make cover` ≥60%、`go mod tidy` 无 diff。
45. WP9：commit `ci,docs: Phase 0 exit gates, ADR-0006, plan change logs`。

# 当前执行步骤："45. WP9 已提交；质量门已验证"

# 任务进度
2026-09-17_21:26:40
- 已修改：新建 `.tasks/2026-09-17_1_migration-status-and-plugin-rewrite-scope.md`
- 更改：将 RESEARCH 阶段的现状盘点与插件系统改动范围观察写入任务文件
- 原因：用户要求将观察整理成任务文件
- 阻碍因素：无
- 状态：未确认

2026-09-17_21:54:29
- 已修改：`go.mod`、`.github/workflows/ci.yml`、`.golangci.yml`、`Dockerfile`、`CONTRIBUTING.md`、`internal/webdav/handler.go` 及调用方/测试、若干 lint 修复
- 更改：WP0 清单 1–7。Go 1.27.1 + golangci-lint v2.13.2；`webdav.NewHandler` 返回 `(*Handler, error)`；lint 全绿。覆盖率基线 **57.8%**（`go tool cover -func`）。提交 `9e771bc chore(toolchain): move to Go 1.27 and golangci-lint v2`
- 原因：清单 WP0
- 阻碍因素：无（该提交在 ENTER EXECUTE MODE 之前已落在 main 上，本步核验并补记基线）
- 状态：成功

2026-09-17_21:59:00
- 已修改：`internal/config/{config,defaults,load,validate,config_test}.go`、`internal/config/testdata/{full,minimal}.yaml`、`go.mod`、`go.sum`
- 更改：WP1 清单 8–10。`Load` 按 Default < 文件 < `NCGO_` 环境（`__` 嵌套 + SECRET/INSTANCE_ID/MAINTENANCE 别名）< Overrides 合并，随后 `Validate`。提交 `773f853 feat(config): koanf-based configuration loader`
- 原因：清单 WP1
- 阻碍因素：无
- 状态：成功

2026-09-17_22:05:00
- 已修改：`internal/database/{db,dialect,errors,open,rebind,sqldb,db_test,integration_test,doc}.go`、`go.mod`、`go.sum`
- 更改：WP2 清单 11–13。`database/sql` facade（pgx stdlib / mysql / modernc sqlite）；`Rebind` 跳过引号内 `?`；SQLite 追加 pragma；`mode=memory` 强制 MaxOpenConns=1；`IsUniqueViolation` 覆盖 23505/1062/2067/1555。提交 `ea43089 feat(database): database/sql facade with postgres/mysql/sqlite drivers`
- 原因：清单 WP2
- 阻碍因素：无
- 状态：未确认

2026-09-17_22:55:00
- 已修改：WP3–WP8 已在此前提交（`658e762` … `58fbe4c`）。WP9 质量门期间修复 MySQL `groups` 保留字、DB exercise `VARCHAR(255)` UNIQUE、Redis increment 键隔离，提交 `847fd1f`。WP9 文档/CI/OCS 金标对齐，提交 `f2949e6`。
- 更改：清单 14–45。本地 `go vet`、`golangci-lint run` 0 issues、`go test -race ./...` 全绿、覆盖率 **65.9%**、`go mod tidy` 无 diff。integration：Postgres 16（55432）、MySQL 8、Redis 7 全绿。`ncgo-captest` testdata/golden pass=6 fail=0 skip=0。
- 原因：`/goal 完成所有WP`
- 阻碍因素：本机 5432 被其他容器占用，Postgres 集成改映射 55432；未 push，GitHub Actions 未实际跑过。Capture ≥50 HAR 仍为操作员工作（01 退出条件保持未勾选）。
- 状态：成功

# 最终审查
（尚未进入 REVIEW 模式，留空）
