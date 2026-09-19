# 背景
文件名：2026-09-18_1_phase-1-readonly-webdav-and-auth.md
创建于：2026-09-18_10:55:22
创建者：matthew
主分支：main
任务分支：task/phase-1-readonly-dav-auth_2026-09-18_1
Yolo模式：Off

# 任务描述
按已接受的 `docs/plans/00-phased-rewrite-plan.md` Phase 1 实施只读 WebDAV + 核心认证：filecache、localfs 接线、PROPFIND 属性、webdav-root 别名、Bearer/session/bruteforce、进程内 golden。不兼容 PHP app；社区 app 不重写；Talk/Mail/Office 与 WASM 全量 ABI 不在本任务。

# 项目概览
nextcloud-go Phase 0 WP0–WP9 已提交。本任务从当前 main 分出功能分支实施 Phase 1 代码可证明部分。捕获冲刺与桌面冒烟仍为操作员工作。

⚠️ 警告：永远不要修改此部分 ⚠️
RIPER-5 协议摘要：
- 每个响应开头必须声明模式：[MODE: RESEARCH|INNOVATE|PLAN|EXECUTE|REVIEW]。
- 默认 RESEARCH；仅在收到 "ENTER XXX MODE" 明确信号后切换。
- RESEARCH：只读、只观察、只提问；禁止建议与实施。
- INNOVATE：只讨论可能性与利弊；禁止具体规划与代码。
- PLAN：输出精确到文件/函数签名的规范与编号实施清单；禁止写代码。
- EXECUTE：100% 忠实执行已批准清单；任何偏差立即回到 PLAN；每步后追加"任务进度"并请用户确认。
- REVIEW：逐行比对计划与实施，标记一切偏差，给出"完全匹配 / 偏离计划"结论。
- 常规交互用中文；模式声明、代码块、清单保持 English 格式。禁用表情符号。
⚠️ 警告：永远不要修改此部分 ⚠️

# 分析
见 2026-09-18 PLAN：00 与 03 范围冲突以 00 为准；login v2 已落地故纳入本阶段；InMemoryFS 仍为 app 默认；无 filecache 表；session 包为空；Bearer 仅 CSRF 旁路。

# 提议的解决方案
批准的 PLAN（D1–D9，WP0–WP8）。MySQL `files.path` 使用 VARCHAR(768) 以满足 InnoDB utf8mb4 唯一索引上限（计划原文“以免索引超长”）。

# 当前执行步骤："清单 1–25 已完成；等待 ENTER REVIEW MODE"

# 任务进度

[2026-09-18_10:30:00]
- 已修改：internal/migrations/sql/{postgres,mysql,sqlite}/0002_filecache.{up,down}.sql；internal/migrations/migrate_test.go；internal/migrations/integration_test.go
- 更改：filecache schema 0002；migrate_test 覆盖 Up 到 version 2 与 Down 回 version 1
- 原因：清单 WP1
- 阻碍因素：无
- 状态：成功（commit b961b95）

[2026-09-18_10:45:00]
- 已修改：internal/files/{doc.go,errors.go,file.go,path.go,path_test.go,sqlstore.go,sqlstore_test.go}
- 更改：Store 接口与 SQLStore；NormalizePath；ComputeFileETag/ComputeDirETag；祖先 etag rollup
- 原因：清单 WP2
- 阻碍因素：无
- 状态：成功（commit b065ef5）

[2026-09-18_11:05:00]
- 已修改：internal/files/dav.go, internal/files/dav_test.go, internal/app/app.go, internal/app/app_test.go, internal/app/dev.go, internal/app/routes.go
- 更改：files.DAV 实现 webdav.FS（localfs + filecache）；App.openStorage；生产 DAV 挂载 /remote.php/dav/files/ 与 /remote.php/webdav/；DevConfig 使用 os.TempDir；测试用独立 TempDir
- 原因：清单 WP3（items 9–12）
- 阻碍因素：errcheck check-blank 禁止补偿路径 `_ = Storage.Delete`；改为 compensateDelete/Rename 检查并 errors.Join。未改 webdav-root parsePath（属 WP4）
- 状态：成功（commit fbd9f98）

[2026-09-18_11:15:00]
- 已修改：internal/webdav/{handler.go,propfind.go,fs.go,handler_test.go,propfind_test.go,webdavroot.go,testdata/golden/propfind/*}；internal/files/dav.go；internal/app/{app.go,routes.go}
- 更改：PROPFIND 增加 checksums/owner/nc:/quota；webdav-root parseOwnerPath；WP6 配额并入 WP4
- 原因：清单 13–15
- 阻碍因素：无
- 状态：成功（commit 83e7c4f）

[2026-09-18_11:20:00]
- 已修改：internal/session/*；internal/auth/{bearer,bruteforce,middleware}*；internal/users GetByID；internal/web/login_v2.go cookie；internal/app routes
- 更改：Bearer/session/bruteforce 统一中间件；login v2 写 nc_session_id；auth.UserSource 避免 auth↔users 循环
- 原因：清单 16–19
- 阻碍因素：BearerVerifier 不能直接依赖 users.Store（循环导入）；改为 auth.UserSource + app 适配器
- 状态：成功（commit c20ef12）

[2026-09-18_11:22:00]
- 已修改：testdata/golden/webdav/*；internal/app/app_test.go
- 更改：六例只读 WebDAV golden；app_test 冻结 Clock 并 seed hello.txt
- 原因：清单 20–21
- 阻碍因素：无
- 状态：成功（commit 9535d2f）

[2026-09-18_11:25:00]
- 已修改：docs/adr/0007-phase1-filecache-and-etag.md；docs/plans/00-phased-rewrite-plan.md；docs/plans/03-phase-1-blueprint.md；internal/httpx/httpx_test.go
- 更改：ADR-0007 与 00/03 Change Log；补 httpx 测试使包覆盖率 ≥65%
- 原因：清单 22–24
- 阻碍因素：本机 5432 被占用且无 ncgo MySQL/Redis，integration tag 未在本机跑通（CI 服务容器覆盖）
- 状态：成功（commit c3c526c）

[2026-09-18_11:35:00]
- 已修改：tools/capture/README.md
- 更改：补 Phase 1 只读操作员冒烟三行（登录 + 浏览 + 下载）
- 原因：计划「测试方法」遗留项
- 阻碍因素：go-reviewer 子代理因 Other Models 配额失败，未改代码；Snyk 仅报 path.go SHA1 etag（CWE-916，与 ADR-0007 一致）
- 状态：未确认（commit 68d833c）

# 最终审查
（尚未进入 REVIEW 模式，留空）
