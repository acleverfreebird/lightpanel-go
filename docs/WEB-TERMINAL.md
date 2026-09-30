# 终端（Web Terminal）

面板内置一个宝塔风格的网页终端：侧栏进入「终端」即自动连接，整页黑色控制台由本地内置的 xterm.js（`static/vendor/xterm/`，CSP `script-src 'self'` 下可用）渲染，背后是一个真实的 Linux PTY。可以照常运行 vim、top、systemctl 等任何交互程序，支持窗口自适应与 5000 行回滚。

使用方式只有三条：

1. 打开「终端」页自动建立会话；切到其他模块会话保持，回到终端页自动恢复；关闭标签页或离开面板即断开，不自动重连。
2. 「重新连接」随时放弃当前会话并新建一个全新会话；「清屏」清空当前屏幕。
3. 缺省 `terminal_enabled = true`；显式配置 `false` 或 `LP_TERMINAL_ENABLED=false` 可整体关闭；只读账号始终不可用。

## 认证与安全模型

终端复用面板登录会话，没有独立票据、连接额度、会话时长或输入输出上限：

1. 浏览器向 `GET /ws/terminal` 发起 WebSocket 升级，携带面板登录 cookie。未登录或登录过期返回 401。
2. 握手校验同源：浏览器的 Origin 必须匹配 `public_origin`，且其 host 必须与请求 Host 一致；通配配置按 `public_origin` 的协议与实际请求 Host 校验。TLS 由反代终止时同样有效，不信任客户端转发头。跨站页面无法借 cookie 连接（cookie 本身也是 SameSite=Strict）。非浏览器客户端（curl、wscat）不带 Origin，允许直连。
3. 退出登录立即撤销对应 WebSocket 会话（关闭原因 `session_revoked`），面板关闭时全部会话以 `server_shutdown` 结束。
4. 会话始终是 **root 登录 shell**（与宝塔面板一致）。面板进程以 root 运行时，PTY 在面板进程内直接派生；非 root 部署（systemd 默认）经 `lightpanel-helper` 中继：helper 以 root 派生 PTY 并在连接存续期间泵送字节，连接断开即回收整个 root 会话。中继受 `[helper] allow_terminal` 控制（缺省开启），helper 不可用或拒绝时终端以明确错误结束，不会静默降级为面板账号 shell；仅在完全没有 `[helper]` 段的旧部署中保持旧行为（面板账号本地 shell）。不继承 `LP_PASS_HASH` 等面板环境变量；`HOME=/root`、`TERM=xterm-256color`、`PATH` 按常规登录环境设置。

生命周期审计（`terminal_audit`）记录连接开始/结束、拒绝原因、PID/UID、模式（`local`/`helper`）与持续时间，不含命令或输出正文。这不是恶意代码沙箱：拥有 shell 的管理员可以 `setsid` 脱离进程组，资源隔离由部署侧的 systemd 限制（`lightpanel.service` 自带）或容器承担。

## 协议

浏览器到面板的 WebSocket 协议不变：

- 二进制帧：客户端 → 服务端为原始键盘输入字节；服务端 → 客户端为 PTY 原始输出字节（xterm.js 直接渲染）。
- 文本帧：仅 `{"type":"resize","rows":24,"cols":80}`，调整 PTY 窗口大小；未知控制帧忽略。

非 root 面板在面板与 helper 之间增加一段中继（`OpTerminal`，协议 v3）：JSON 请求/应答握手后，面板 → helper 为帧（1 字节类型 + 4 字节大端长度 + 载荷；`0x01` = 键盘输入，`0x02` = resize，载荷为 4 字节 rows/cols），helper → 面板为 PTY 原始输出字节，EOF 即会话结束。

## 反向代理

须保留 Host、转发 WebSocket Upgrade，并把读超时设得足够长（长连接会话无服务端时限）。使用 HTTPS/WSS；不要在代理日志里记录 Cookie。

TLS 由反代终止时，设置 `public_origin = "https://面板域名"`，后端可以使用 HTTP。完整 Nginx 示例见 [README](../README.md#https-与反向代理)。如果面板能登录但终端无法连接，检查 `/ws/terminal` 是否返回 `101 Switching Protocols`；`403` 通常表示 Origin/Host 与配置不符，`400` 通常表示代理未转发 Upgrade/Connection。

## 验证

`go test -race ./...` 覆盖认证、跨源拒绝、真实 PTY 输入输出、resize、退出登录撤销、服务关闭与进程组清理、审计脱敏。前端 `node --test scripts/*.test.mjs` 覆盖打开即连、会话保持、重连、清屏与只读/关闭拦截。
