# Web Shell（Web Terminal）

Web Shell 是浏览器里的完整交互式终端（xterm.js + 真 PTY），与宝塔面板的终端体验一致：连接后直接出现 shell 提示符，可运行 vim、top、systemctl 等任何交互程序，支持窗口自适应缩放与 5000 行回滚。

缺省 `terminal_enabled = true`。显式配置 `false` 或 `LP_TERMINAL_ENABLED=false` 可关闭；只读模式始终拒绝连接。入口位于侧栏「Web Terminal」，点击「连接终端」即建立会话；离开页面或关闭标签会断开，不自动重连，重新连接会创建全新会话。

## 认证与安全模型

终端复用面板登录会话，不再有独立票据、连接额度、会话时长或输入输出上限：

1. 浏览器向 `GET /ws/terminal` 发起 WebSocket 升级，携带面板登录 cookie。未登录或登录过期返回 401。
2. 握手校验同源：浏览器总会携带 Origin，其 host 必须与请求 Host 一致，跨站页面无法借 cookie 连接（cookie 本身也是 SameSite=Strict）。非浏览器客户端（curl、wscat）不带 Origin，允许直连。
3. 退出登录立即撤销对应 WebSocket 会话（关闭原因 `session_revoked`），面板关闭时全部会话以 `server_shutdown` 结束。
4. shell 以**面板运行用户**身份执行（systemd 部署默认 `lightpanel`，root 部署即 root）。不继承 `LP_PASS_HASH` 等面板环境变量；工作目录与 `HOME`、`TERM=xterm-256color`、`PATH` 按常规登录环境设置。

生命周期审计（`terminal_audit`）记录连接开始/结束、拒绝原因、PID/UID 与持续时间，不含命令或输出正文。这不是恶意代码沙箱：拥有 shell 的管理员可以 `setsid` 脱离进程组，资源隔离由部署侧的 systemd 限制（`lightpanel.service` 自带）或容器承担。

## 协议

- 二进制帧：客户端 → 服务端为原始键盘输入字节；服务端 → 客户端为 PTY 原始输出字节（xterm.js 直接渲染）。
- 文本帧：仅 `{"type":"resize","rows":24,"cols":80}`，调整 PTY 窗口大小；未知控制帧忽略。

## 反向代理

须保留 Host、转发 WebSocket Upgrade，并把读超时设得足够长（长连接会话无服务端时限）。使用 HTTPS/WSS；不要在代理日志里记录 Cookie。

## 验证

`go test -race ./...` 覆盖认证、跨源拒绝、真实 PTY 输入输出、resize、退出登录撤销、服务关闭与进程组清理、审计脱敏。前端使用本地内置的 xterm.js（`static/vendor/xterm/`，CSP `script-src 'self'` 下可用）。
