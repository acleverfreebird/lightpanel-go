#!/usr/bin/env bash
# LightPanel 卸载脚本。默认保留文件沙箱 /var/lib/lightpanel；--purge 连同删除。
set -euo pipefail

SERVICE_NAME="lightpanel"
UNIT="/etc/systemd/system/${SERVICE_NAME}.service"
PURGE=0
[ "${1:-}" = "--purge" ] && PURGE=1

if [ "$(id -u)" -ne 0 ]; then
  printf '[lightpanel] 错误: 请用 root 运行: sudo bash %s\n' "$0" >&2
  exit 1
fi

systemctl stop "$SERVICE_NAME" 2>/dev/null || true
systemctl disable "$SERVICE_NAME" 2>/dev/null || true
rm -f "$UNIT"
systemctl daemon-reload 2>/dev/null || true
rm -rf /opt/lightpanel

if [ "$PURGE" -eq 1 ]; then
  rm -rf /var/lib/lightpanel
  printf '[lightpanel] 已删除 /opt/lightpanel、服务单元与 /var/lib/lightpanel\n'
else
  printf '[lightpanel] 已删除 /opt/lightpanel 与服务单元\n'
  printf '[lightpanel] 保留文件沙箱 /var/lib/lightpanel（如需一并删除: sudo bash %s --purge）\n' "$0"
fi
printf '[lightpanel] LightPanel 已卸载\n'
