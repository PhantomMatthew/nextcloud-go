# 背景
文件名：2026-09-18_3_phase-2b-trashbin.md
创建于：2026-09-18_13:05:00
创建者：matthew
主分支：main
任务分支：task/phase-2b-trashbin_2026-09-18_1
Yolo模式：On

# 任务描述
按已批准的 Phase 2b 计划实施回收站：DELETE 入 trashbin、PROPFIND/恢复/永久删除、files.undelete。不含 versions/PROPPATCH/LOCK/sharing/S3/jobs/search。

# 项目概览
Phase 2a 已在 HEAD f63c9de。本任务从该提交拉出 2b 分支。

⚠️ 警告：永远不要修改此部分 ⚠️
RIPER-5 协议摘要：
- 每个响应开头必须声明模式：[MODE: RESEARCH|INNOVATE|PLAN|EXECUTE|REVIEW]。
- 默认 RESEARCH；仅在收到 "ENTER XXX MODE" 明确信号后切换。
- EXECUTE：100% 忠实执行已批准清单；Yolo On 时连续执行不逐步询问状态。
- 常规交互用中文；模式声明、代码块、清单保持 English 格式。
⚠️ 警告：永远不要修改此部分 ⚠️

# 分析
见 2026-09-18 PLAN D1–D15。

# 提议的解决方案
批准的 PLAN（D1–D15，清单 1–9）。

# 当前执行步骤："9. 按 WP 提交"

# 任务进度

[2026-09-18_13:40:00]
- 已修改：分支 task/phase-2b-trashbin_2026-09-18_1；.tasks/2026-09-18_3_phase-2b-trashbin.md
- 更改：从 f63c9de 拉出 2b 分支并写入任务文件
- 原因：清单 1
- 阻碍因素：无
- 状态：成功

[2026-09-18_13:42:00]
- 已修改：internal/migrations/sql/{postgres,mysql,sqlite}/0004_trash.{up,down}.sql；migrate_test.go；integration_test.go
- 更改：trash_items 表；Up 到 version 4；Down 回 3 后 uploads 仍在
- 原因：清单 2
- 阻碍因素：无
- 状态：成功

[2026-09-18_13:50:00]
- 已修改：internal/files/{trashstore.go,trash.go,dav.go,uploads.go} 及测试
- 更改：SQLTrashStore；Trash FS；MoveToTrash/Restore/PurgeLocation；DAV.Remove 入回收站；Purge 硬删；MOVE/COPY 覆盖与 checksum 失败走 Purge
- 原因：清单 3–5
- 阻碍因素：无
- 状态：成功

[2026-09-18_13:55:00]
- 已修改：internal/webdav/{fs.go,handler.go,propfind.go}；internal/capabilities/files.go；internal/app/{app.go,routes.go,app_test.go}
- 更改：Handler.Restore；trash PROPFIND 属性；files.undelete；挂载 /remote.php/dav/trashbin/
- 原因：清单 6
- 阻碍因素：无
- 状态：成功

[2026-09-18_14:00:00]
- 已修改：testdata/golden/webdav/017–020；capabilities/ocs goldens；docs/adr/0009；docs/plans/00 Change Log
- 更改：回收站 golden；undelete capability；ADR-0009
- 原因：清单 7–8
- 阻碍因素：无
- 状态：成功


# 最终审查
（尚未进入 REVIEW 模式，留空）
