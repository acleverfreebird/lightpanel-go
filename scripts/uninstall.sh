#!/usr/bin/env bash
# LightPanel 卸载脚本。默认保留 /var/lib/lightpanel（更新暂存/数据目录）；--purge 连同删除。
set -euo pipefail

SERVICE_NAME="lightpanel"
HELPER_SERVICE_NAME="lightpanel-helper"
UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
HELPER_UNIT="/etc/systemd/system/${HELPER_SERVICE_NAME}.service"
PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

if [ "$(id -u)" -ne 0 ]; then
  printf '[lightpanel] 错误: 请用 root 运行: sudo bash %s\n' "$0" >&2
  exit 1
fi

for svc in "$SERVICE_NAME" "$HELPER_SERVICE_NAME"; do
  systemctl stop "$svc" 2>/dev/null || true
  systemctl disable "$svc" 2>/dev/null || true
done
rm -f "$UNIT" "$HELPER_UNIT"
systemctl daemon-reload 2>/dev/null || true
rm -rf /opt/lightpanel

if [ "$PURGE" -eq 1 ]; then
  rm -rf /var/lib/lightpanel
  printf '[lightpanel] 已删除 /opt/lightpanel、服务单元与 /var/lib/lightpanel\n'
else
  printf '[lightpanel] 已删除 /opt/lightpanel 与服务单元（含 lightpanel-helper）\n'
  printf '[lightpanel] 保留 /var/lib/lightpanel（如需一并删除: sudo bash %s --purge）\n' "$0"
fi
printf '[lightpanel] LightPanel 已卸载\n'
printf '[lightpanel] 系统用户 lightpanel 未删除；如确认不再使用可手动执行: userdel lightpanel\n'
