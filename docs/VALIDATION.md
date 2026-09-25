# 验证记录

日期：2026-09-24。Windows 交叉编译，WSL Ubuntu 24.04.4 / Linux 6.6.114.1 / amd64 实际执行。编译器 Go 1.26.2，发布构建 CGO_ENABLED=0。

## 自动检查

- `go test -race -count=1 ./...`：4 个包全部通过，18 个测试，无竞态报告。
- `GOOS=linux CGO_ENABLED=0 go vet ./...`：通过。
- `go mod verify`：全部依赖通过校验。
- Linux amd64 和 arm64 静态构建通过。amd64 已运行；arm64 未在 ARM 设备执行。
- 覆盖配置校验、明文公网监听拒绝、登录/session/过期/撤销、Secure cookie、按 IP 限流、Host/Origin/CSRF、只读权限、XSS 转义/附件下载、审计、路径遍历/符号链接/硬链接/FIFO/根删除/覆盖上传拒绝、上传超限清理、目录替换、权限位限制、指标解析/缓存、进程名称解析、真实自建子进程的 pidfd 身份检查和 TERM、命令白名单/失败传播/输出上限。

## 实际二进制冒烟测试

```bash
python3 scripts/smoke.py dist/lightpanel-linux-amd64
```

Python 仅用于可选开发验证，面板运行不需要 Python。脚本创建自己的临时目录和回环监听实例，使用测试凭据，结束后清理；不会更改宿主机服务或防火墙。

最终产物本次结果：

```json
{
  "startup_ready_ms": 53.02,
  "rss_kib": 11316,
  "peak_rss_kib": 11316,
  "metrics_100_requests_ms": 70.04,
  "cpu_seconds_for_100_metrics": 0.04,
  "binary_bytes": 9195682,
  "file_roundtrip_bytes": 1114112,
  "graceful_shutdown": true,
  "audit_verified": true
}
```

这是单次轻载测量，不是性能 SLA。就绪探测粒度为 20 ms；指标请求大部分命中缓存；RSS 在登录、约 1.06 MiB 文件往返和指标请求后读取。没有并发、TLS 或生产规模进程表压测。浏览器期间另观测约 10–12 MiB RSS；20–50 MiB 常驻目标应在目标主机复测。

WSL 从 Windows NTFS 目录执行刚被 Windows 重建的同路径 ELF 时曾发生段错误；复制到 Linux 临时文件系统的新 inode 后正常。冒烟脚本采用临时副本执行，以避开挂载缓存问题。

## 浏览器检查

真实浏览器访问 WSL 回环实例，验证登录、概览、进程搜索、只读按钮禁用、文件目录、真实 systemctl 列表、journalctl 日志和退出；最终发布构建重新登录验证。概览截图未发现布局溢出，未捕获到 JS error/warn。

独立审查发现并修复：无 TLS 的公网后端配置、systemd 旧配置路径、并发切换标签漏加载。

## 发布产物

| 目标 | 字节数 | SHA256 |
|---|---:|---|
| linux-amd64 | 9195682 | 5a34fa39ab723153378ed41f6ee472ddce0b2f17112d26f3f902b16d58097f7b |
| linux-arm64 | 8585378 | 99e809b1ee2cc68fd150a96a9cdac65206e0ab2f67b91e1363f4465900734deb |

## 验证限制

- 未在每种发行版实机认证，未安装或启用系统级 lightpanel.service。
- 服务启停和防火墙写入使用受控 runner 测试；未修改宿主机服务或网络规则。
- TLS 使用标准库实现，未进行公网证书、ACME 或真实反代部署测试。
- 可选 Web Terminal 及第二阶段功能未实现。
- 旧内核缺少 pidfd 时进程结束返回不支持。文件管理面向整个文件系统，等同管理员文件权限；面板进程不是多租户隔离机制。

## 2026-09-25 文件管理功能增强复验

文件空间从"浏览/上传/下载/删除/权限"扩展为完整管理能力：新建文件夹（`MkdirAll`，0750）、重命名/移动（`os.Root.Rename`）、在线文本编辑（≤1 MiB、拒绝含 NUL 的二进制、先写临时文件再原子替换、拒绝符号链接目标）、递归删除（显式 `recursive=true`，拒绝删除根）、目录列表新增 `modified` 字段、上传上限改为 `max_upload_mb` 配置（1..2048，默认 32）。前端补充排序、当前页筛选、多文件上传和编辑器对话框，全部动态文本仍走 `textContent`，无内联脚本。

WSL Ubuntu 24.04（Go 1.27.1）实测：

- `go test ./...` 与 `go test -race ./pkg/sysinfo/ ./config/` 全部通过（含新增 `TestFileLifecycle`、`TestWriteTooLarge`、`TestListReportsModified` 与配置项校验用例）。
- `GOOS=linux go vet ./...`、`gofmt` 通过。
- 重建 linux-amd64 后 `python3 scripts/smoke.py` 通过：启动 31.8 ms、RSS 约 11.5 MiB、文件往返 1.06 MiB、优雅关闭与审计验证均成功。
- 保存路径的符号链接契约与全站一致：编辑读取、写入目标均拒绝符号链接（`TestFileLifecycle` 覆盖）。

## 2026-09-25 在线更新功能复验

新增半自动更新链路：面板概览页「版本与更新」卡片（`GET /api/update/check` + `POST /api/update/apply`）、`main.version` 构建注入（Makefile 与 GitHub Actions `release.yml`，推送 `v*` 标签自动发布 amd64/arm64 二进制与 `SHA256SUMS`）、可选 `update_repo`/`update_mirror` 配置。

更新实现要点与验证：

- 检查：调 GitHub Releases API，无发布版本时返回空 `latest`（前端提示"仓库还没有发布版本"），404 不作为错误。
- 应用：仅支持 amd64/arm64；二进制下载到 `/proc/self/exe` 同目录（保证同文件系统原子 rename），先校验 `SHA256SUMS` 再 0755 替换，响应返回后以新会话 `systemctl restart lightpanel`；版本相同或更旧时跳过下载并明确返回。
- 单测（WSL Ubuntu 24.04，Go 1.27.1，`go test -race` 通过）：假 Release 服务器覆盖检查、成功替换（内容/权限/临时文件清理/重启调度）、坏校验和拒绝且不动原二进制、已最新跳过、无 release、版本比较（多段数字、pre-release、`dev` 回退）。
- 浏览器实测：登录实例显示注入的版本号，点击「检查更新」经真实 GitHub API 返回"仓库还没有发布版本"（仓库尚无 Release），链路完整。
- 安全边界：更新接口同样受登录 + CSRF + 只读模式约束并计入审计；校验和防传输损坏，不防上游 Release 被篡改（已在 README 声明）。

## 2026-09-25 文件管理改为全盘访问复验

文件管理从 `sandbox_root` 受限目录改为管理**整个文件系统**：所有接口只接受绝对路径（内部仍经 `os.Root` 钉住 `/` 走 openat，`..` 组件、反斜杠、NUL 拒绝）；目录列表新增 `symlink` 字段并返回归一化绝对路径；符号链接若最终指向普通文件允许下载（`/bin/sh` 等）；chmod 扩展到目录并继续拒绝符号链接与 setuid/setgid；`/proc`、`/sys`、`/dev`、`/run` 拒绝删除与移动；前端面包屑以 `/` 为根，支持绝对路径跳转、绝对路径重命名/移动与新建多级目录。`sandbox_root` 配置项废弃（旧配置仍可解析，字段被忽略），systemd 单元移除 `ProtectSystem`/`ProtectHome`/`PrivateTmp`/`ReadWritePaths` 以放行全盘访问，保留 `NoNewPrivileges` 与内核防护项。
