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
- 旧内核缺少 pidfd 时进程结束返回不支持。文件空间须为独立可信目录，不得包含额外挂载；不是多租户隔离机制。
