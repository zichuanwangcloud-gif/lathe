#!/bin/sh
# clean-test-db.sh —— 清理共享开发库里的测试残留。
#
# 问题：测试跑在一个共享的真实 Postgres 上（make dev-infra 起的那个），
# 而生产查询是全局的：task.Machine.ClaimReady 按 (priority DESC, id) 取
# 全局最早的 queued 行，queue.Reconcile 把库里【所有】在途任务重新入队。
# 测试进程被杀时（Ctrl-C、CI 超时、编排器 kill）t.Cleanup 不执行，
# fixture 造的 user/repo/task 就原样留下 —— 下一轮测试的全局查询会捞到
# 这些孤儿行，断言「领到的是自己那条」当场崩。
#
# 2026-09-09 实测过两种崩法（docs/08-debt-cleanup.md §7 有完整取证）：
#   1. cmd/lathe：Reconcile 把孤儿在途任务一起入队，ClaimReady 按 id
#      升序把它优先领走 —— 日志里的 requeued_inflight=2 就是铁证
#   2. internal/runner：fixture 的 email 当时无随机量 + ON CONFLICT
#      DO UPDATE，孤儿 user 被复用，固定 issue key CR-777 撞上部分唯一
#      索引 tasks_one_active_per_issue（SQLSTATE 23505）
#
# 测试侧已经修了（fixture 加随机量、断言按归属过滤、领单前排空），所以
# 孤儿行不再让测试变红。本脚本管的是另一件事：把已经堆积的残留清掉，
# 别让开发库无限膨胀。做成脚本而不是手工 SQL，是因为手工 SQL 下次还得再想一遍。
#
# 判别条件：**email 以 @example.com 结尾**。RFC 2606 把 example.com 保留
# 给文档与测试，真实用户不会用它，所以这个条件既充分又安全。删 user 会
# 级联带走它的 repos / tasks / task_events / verifications / agent_events
# 与团队成员关系；之后再删「没有任何成员的团队」（孤儿团队，f12）。
#
# 磁盘上的 worktree 目录不在本脚本职责内 —— 数据库行会被级联带走，目录
# 不会。那是 worktree 收割机（docs/08-debt-cleanup.md T6）的事。
#
# 用法：
#   scripts/clean-test-db.sh          # 干跑，只报告要删什么
#   scripts/clean-test-db.sh --yes    # 真删
#
# 连接串取 LATHE_TEST_DSN，未设时用 make dev-infra 的默认库。输出第一节永远是
# current_database()：跑之前先看一眼连的是哪个库。

set -eu

DSN="${LATHE_TEST_DSN:-postgres://lathe:lathe@127.0.0.1:55432/lathe?sslmode=disable}"
APPLY=""
[ "${1:-}" = "--yes" ] && APPLY=1

# 库名从 DSN 里解析：docker 回退分支借容器里的 psql，只能按库名连，不能直接吃 DSN。
# 解析不出就退出，不回落到任何默认库 —— 以前这里写死 -d lathe，LATHE_TEST_DSN
# 指向别的库时（并行功能线各用各的测试库）脚本照样连共享库 lathe，越界（f12 AC-43）。
DBNAME=$(printf '%s' "$DSN" | sed -nE 's#^postgres(ql)?://[^/]+/([A-Za-z0-9_]+)([?].*)?$#\2#p')
if [ -z "$DBNAME" ]; then
	echo "无法从 LATHE_TEST_DSN 解析出库名：$DSN" >&2
	exit 1
fi

# psql 优先用宿主机的；没有就借开发容器里的（本仓常见情形：宿主无 psql）。
if command -v psql >/dev/null 2>&1; then
	run_sql() { psql "$DSN" -v ON_ERROR_STOP=1 "$@"; }
elif command -v docker >/dev/null 2>&1 && docker ps --format '{{.Names}}' | grep -qx lathe-postgres-dev; then
	run_sql() { docker exec -i lathe-postgres-dev psql -U lathe -d "$DBNAME" -v ON_ERROR_STOP=1 "$@"; }
else
	echo "找不到 psql，也没有在跑的 lathe-postgres-dev 容器；先 make dev-infra" >&2
	exit 1
fi

# 第一条输出永远是实际连上的库名，让人（和 AC-43 测试）看得见脚本动的是哪个库。
run_sql -c "SELECT current_database();"
echo

echo "== 测试残留（email 以 @example.com 结尾的 fixture 用户）=="
run_sql -c "
SELECT count(*) AS 用户数,
       (SELECT count(*) FROM repos r JOIN users u ON u.id=r.user_id
         WHERE u.email LIKE '%@example.com') AS 仓库数,
       (SELECT count(*) FROM tasks t JOIN users u ON u.id=t.user_id
         WHERE u.email LIKE '%@example.com') AS 任务数
FROM users WHERE email LIKE '%@example.com';"

echo
echo "== 孤儿团队（没有任何成员的团队；fixture 用户删掉后其团队会变成这样）=="
run_sql -c "
SELECT count(*) AS 孤儿团队数
FROM teams t WHERE NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id);"

echo
echo "== 非终态任务里【不属于】测试 fixture 的部分（需人工判断，本脚本不动）=="
run_sql -c "
SELECT t.id, t.state, t.external_key, u.email, t.updated_at
FROM tasks t JOIN users u ON u.id = t.user_id
WHERE t.state NOT IN ('merged','failed','cancelled')
  AND u.email NOT LIKE '%@example.com'
ORDER BY t.id;"

if [ -z "$APPLY" ]; then
	echo
	echo "（干跑，未删除任何东西。确认无误后加 --yes 真删）"
	exit 0
fi

echo
echo "== 删除中 =="
# 顺序不能反：先删 fixture 用户，team_members 随 ON DELETE CASCADE 消失，fixture
# 团队这才成为孤儿；先删孤儿团队的话，fixture 团队此刻仍有成员，会残留。
# 第二条也会删掉非测试用户留下的空团队 —— 正常路径上不会出现空团队（owner 不能
# 退出），出现了就是残留；干跑段的「孤儿团队数」供人先判断。
# 两条 DELETE 用下面的标记夹住，internal/store 的 TestCleanTestDBDeleteSQLInRollback
# 按标记抽出原文在回滚事务里验证（测的就是这里写的）。
# lathe:delete-sql-begin
run_sql -c "DELETE FROM users WHERE email LIKE '%@example.com';"
run_sql -c "DELETE FROM teams t WHERE NOT EXISTS (SELECT 1 FROM team_members m WHERE m.team_id = t.id);"
# lathe:delete-sql-end
echo "完成。剩余非终态任务："
run_sql -c "
SELECT t.id, t.state, t.external_key, u.email
FROM tasks t JOIN users u ON u.id = t.user_id
WHERE t.state NOT IN ('merged','failed','cancelled')
ORDER BY t.id;"
