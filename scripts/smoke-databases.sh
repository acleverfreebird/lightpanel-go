#!/usr/bin/env bash
# 数据库管理的端到端烟雾测试：以 root 面板 + 真实 MariaDB/MySQL 走完
# 登录 → 列表 → 一步建库 → SQL 控制台 → 备份（任务） → 备份列表/下载 → 恢复 → 删库 全流程。
# 用法：ENGINE=mariadb LP_PASS_HASH=<bcrypt> bash scripts/smoke-databases.sh（仓库根目录；root 运行）。
set -euo pipefail

PORT=18899
ENGINE=${ENGINE:-mysql}
ORIGIN="http://127.0.0.1:${PORT}"
SMOKE_DIR=$(mktemp -d /tmp/lp-smoke.XXXXXX)
PASS_HASH=${LP_PASS_HASH:?需要 LP_PASS_HASH 环境变量（用 ./lightpanel -hash-password 生成）}

cp dist/lp-smoke-linux-amd64 "$SMOKE_DIR/lightpanel"
chmod 755 "$SMOKE_DIR/lightpanel"
cat > "$SMOKE_DIR/config.toml" <<EOF
host = "127.0.0.1"
port = $PORT
admin_user = "admin"
password_hash = "$PASS_HASH"
public_origin = "$ORIGIN"
read_only = false
EOF

"$SMOKE_DIR/lightpanel" -c "$SMOKE_DIR/config.toml" >"$SMOKE_DIR/panel.log" 2>&1 &
PANEL_PID=$!
trap 'kill $PANEL_PID 2>/dev/null || true' EXIT
sleep 2

JAR="$SMOKE_DIR/cookies.txt"
curl -s -c "$JAR" "$ORIGIN/login" -o /dev/null
LOGIN_CODE=$(curl -s -b "$JAR" -c "$JAR" -H "Origin: $ORIGIN" -H "Referer: $ORIGIN/login" -d "username=admin&password=local-test-only-password" "$ORIGIN/login" -o /dev/null -w '%{http_code}')
INDEX=$(curl -s -b "$JAR" "$ORIGIN/" -w '\n%{http_code}')
INDEX_CODE=$(printf '%s' "$INDEX" | tail -1)
CSRF=$(printf '%s' "$INDEX" | grep -o 'name="csrf-token" content="[^"]*"' | sed 's/.*content="//;s/"//')
[ -n "$CSRF" ] || { echo "未取得 CSRF token：login=$LOGIN_CODE index=$INDEX_CODE"; cat "$SMOKE_DIR/panel.log"; exit 1; }

api_post() { curl -s -b "$JAR" -H "X-CSRF-Token: $CSRF" -H "Origin: $ORIGIN" -d "$1" "$ORIGIN$2"; }
api_post_enc() { local path=$1; shift; curl -s -b "$JAR" -H "X-CSRF-Token: $CSRF" -H "Origin: $ORIGIN" "$@" "$ORIGIN$path"; }
api_get() { local path=$1; shift; curl -s -b "$JAR" "$ORIGIN$path" "$@"; }
wait_task() {
  STATE=running
  for _ in $(seq 1 60); do
    STATE=$(api_get "/api/tasks/$1" | python3 -c 'import json,sys;print(json.load(sys.stdin)["state"])')
    [ "$STATE" != running ] && break
    sleep 1
  done
  [ "$STATE" = done ] || { echo "任务 $1 状态 $STATE"; api_get "/api/tasks/$1"; exit 1; }
}

# 幂等：清掉上次运行残留（真实部署不会有这一步）。
mariadb -e "DROP DATABASE IF EXISTS shop; DROP USER IF EXISTS 'shop_user'@'localhost';" 2>/dev/null || true
rm -f /var/lib/lightpanel/db/credentials.json

echo "== 引擎与列表 =="
api_get "/api/databases" | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("engines:", [(e["engine"], e["installed"], e["running"]) for e in d["engines"]])
print("databases:", {k: [r["name"] for r in v] for k, v in d["databases"].items()})
'

echo "== 一步建库（shop + shop_user + utf8mb4 + localhost）=="
api_post "engine=$ENGINE&name=shop&user=shop_user&password=s3cret'x&charset=utf8mb4&host=localhost" "/api/databases/create"
echo

echo "== 列表应含 shop、容量与凭据 =="
api_get "/api/databases" | python3 -c '
import json,sys
d=json.load(sys.stdin)
rows=d["databases"]["'$ENGINE'"]
row=next(r for r in rows if r["name"]=="shop")
assert row["charset"]=="utf8mb4", row
assert row["user"]=="shop_user" and row["password"] == "s3cret'"'"'x", row
print("列表 OK:", row["name"], row["charset"], row["size"], row["user"])
'

echo "== SQL 控制台：建表 + 插入 + 查询 =="
api_post_enc "/api/databases/query" -d "engine=$ENGINE&name=shop" --data-urlencode "sql=CREATE TABLE t1(id INT PRIMARY KEY, v TEXT); INSERT INTO t1 VALUES (1,'hello'),(2,'world');"
echo

echo "== 备份（后台任务）=="
TASK=$(api_post "engine=$ENGINE&name=shop" "/api/databases/backup" | python3 -c 'import json,sys;print(json.load(sys.stdin)["task_id"])')
wait_task "$TASK"

echo "== 备份列表与下载 =="
api_get "/api/databases/backups?engine=$ENGINE&name=shop"
FILE=$(api_get "/api/databases/backups?engine=$ENGINE&name=shop" | python3 -c 'import json,sys;print(json.load(sys.stdin)["backups"][0]["file"])')
api_get "/api/databases/backup/download?engine=$ENGINE&name=shop&file=$FILE" -o "$SMOKE_DIR/dump.sql.gz"
gzip -t "$SMOKE_DIR/dump.sql.gz" && echo "gzip 校验 OK: $FILE"
ls -la /var/backups/lightpanel/databases/

echo "== 清空表后恢复备份 =="
api_post_enc "/api/databases/query" -d "engine=$ENGINE&name=shop" --data-urlencode "sql=DROP TABLE t1;" >/dev/null
RESTORE=$(api_post "engine=$ENGINE&name=shop&file=$FILE" "/api/databases/backup/restore" | python3 -c 'import json,sys;print(json.load(sys.stdin)["task_id"])')
wait_task "$RESTORE"
api_post_enc "/api/databases/query" -d "engine=$ENGINE&name=shop" --data-urlencode "sql=SELECT COUNT(*) FROM t1;"

echo "== 非法备份文件名应被拒绝 =="
CODE=$(curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -H "X-CSRF-Token: $CSRF" -H "Origin: $ORIGIN" -d "engine=$ENGINE&name=shop&file=../../etc/passwd" "$ORIGIN/api/databases/backup/delete")
[ "$CODE" = 400 ] || { echo "delete 非法文件名返回 $CODE，应为 400"; exit 1; }
echo "路径校验 OK"

echo "== 删库（凭据备忘应一并清除）=="
api_post "engine=$ENGINE&name=shop" "/api/databases/delete"
api_get "/api/databases" | python3 -c '
import json,sys
d=json.load(sys.stdin)
assert not [r for r in d["databases"]["'$ENGINE'"] if r["name"]=="shop"]
print("删库 OK")
'
echo "SMOKE PASS"
