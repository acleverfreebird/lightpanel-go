# Task Center Implementation Plan

**Goal:** 将任务中心放到最后一个侧边栏入口，提供与面板风格一致的任务列表与日志弹窗。

**Architecture:** 保留原生 dialog、ES modules 和两秒轮询。任务详情请求带选择版本校验；结束状态仍拉取最终输出。使用已有 CSS 变量，避免改动后端执行协议。

**Tech Stack:** Go templates、原生 JavaScript/CSS、Node test runner。

## Tasks

- [x] 在 scripts/tasks.test.mjs 使用现有 vm 测试模式重现结束任务日志未更新、详情乱序覆盖问题，运行 node --test scripts/tasks.test.mjs 确认失败。
- [x] 修改 templates/index.html：侧边栏最后加入任务入口；将弹窗改为列表、详情、日志工具栏；替换右下角文案。
- [x] 修改 static/tasks.js：默认选择执行中/最近任务，渲染状态与时间，防止旧请求覆盖新选择，结束后补取日志，支持复制和跟随滚动，清理后保持正确选择，提供局部错误反馈。
- [x] 修改 static/app.css：移除悬浮球样式，沿用主题变量实现双栏弹窗，窄屏上下排列。
- [x] 运行 node --test scripts/*.test.mjs 和 go test ./...；浏览器检查桌面/窄屏、成功/失败、空态、只读及滚动交互。检查 git diff --check。

## Acceptance

执行中任务转为结束后必须显示最终输出；切换任务后较早返回的请求不得覆盖当前详情。任务入口无需依赖接口成功才能出现。弹窗不改变所在页面，无任务时有清晰引导。后台未产生日志时不宣称逐行实时流。

## Verification notes

- 三个回归用例均先复现失败、修复后通过：最终日志缺失、详情乱序、旧列表覆盖新建任务选择。
- Node 测试共 10 项通过。
- Chromium 隔离测试使用模拟任务接口验证桌面/窄屏、结束状态、复制、滚动跟随、清理空态、Escape、只读、接口失败恢复；无浏览器异常。
- 独立代码审查发现的旧列表竞态已修复。
- Linux 完整 Go 测试通过（WSL，独立 GOCACHE，复用 Windows GOMODCACHE）；git diff --check 通过。
