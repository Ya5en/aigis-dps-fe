# 图标资源（ICO）接入需求文件

> 目标：为 `dpsmeter.exe` 提供统一的应用图标——exe 文件图标、任务栏/Alt+Tab 图标、托盘图标共用一份 ICO 资源。
> 现状（2026-11-22）：已按本文件实现（临时图标 😋 用于测试，正式图标待替换）；`assets/app.ico` 8 个尺寸 47.7KB，`versioninfo.json` + `rsrc_windows_amd64.syso` 已入库，版本 1.0.0。
> 兑换正式图标时只需替换 `assets/app.ico` 并重新生成 syso（命令见 README「构建与运行」），无需改代码。

## 1. 资源文件规格

| 项 | 要求 |
|---|---|
| 格式 | 标准 ICO 容器（多尺寸条目），**不允许** PNG 直接改名 `.ico` |
| 必含尺寸 | 16 / 20 / 24 / 32 / 48 / 256 px（20/24 供高 DPI 托盘：125%/150% 缩放下托盘取 20/24） |
| 可选尺寸 | 64 / 128 px（explorer 大视图用，可加） |
| 颜色深度 | 全部条目 32-bit BGRA + 8-bit alpha（半透明边缘）；禁止 4-bit/8-bit 调色板条目 |
| 256px 条目 | 允许 PNG 压缩存储（Vista+ 标准做法，减小体积） |
| 体积上限 | 整个 `.ico` ≤ 200 KB（exe 总量已 ~13MB，图标不应显著膨胀） |
| 命名/位置 | 资源文件 `assets/icon.ico`；设计源文件（SVG/工程文件）存 `assets/src/`（不入构建） |

## 2. 设计要求

- **16px 可辨识**：最小条目下主体形状清晰（单形轮廓 + 1~2 个强调色），不依赖文字
- 透明背景，主体外留 ≥1px 透明边距（避免任务栏/DPI 缩放裁边）
- 深色游戏画面上可见：主体带描边或高对比底形；同时在浅色任务栏上也成立
- 形状避免与常见 DPS 面板/统计类软件雷同；色弱友好（不单靠红绿区分）

## 3. 打包链路（零外部依赖优先）

| 项 | 要求 |
|---|---|
| 生成方式 | 首选 `josephspurrier/goversioninfo` 或 `akavel/rsrc`（纯 Go，`go run` 即可）由 `.ico` 生成 `rsrc_windows_amd64.syso`；手写 `.rc` + MinGW `windres` 为备选（用户已有 MSYS2 工具链） |
| .syso 位置 | 项目根目录，`go build` 自动拾取链接；**建议提交 git**（生成的二进制随库走，构建机无需再装工具） |
| 嵌入内容 | 图标资源（`IDI_APP=1`）+ 可选 Version Info（ProductName=aigis-dps、FileDescription、版本号） |
| 构建脚本 | `build.bat` 增加 syso 生成步骤（或约定 syso 已入库则跳过）；`go build` 命令不变 |
| 兼容性 | 无 `.syso` 时构建仍须通过（向后兼容）；`go:embed`（web dist、dotcdn）不受影响；单 exe 分发形态不变 |

## 4. 代码侧改动

| 位置 | 要求 |
|---|---|
| `window.go` 注册窗口类 | `hIcon`/`hIconSm` 用 `LoadImageW(hInstance, MAKEINTRESOURCE(1), IMAGE_ICON, 0, 0, LR_DEFAULTSIZE)` 加载资源图标；资源缺失时回退现有 `IDI_APPLICATION`（保持 headless/异常路径可用） |
| `window.go` 托盘 | `nid.hIcon` 同样改用资源图标（48px 条目按托盘实际 DPI 由系统缩放）；退出/托盘删除路径 `DestroyIcon` 防句柄泄漏 |
| 资源 ID | 统一常量 `iconResID = 1`（.rc 与 Go 两侧对齐，写进注释） |
| 版本信息（可选） | 任务管理器/悬停提示显示 `aigis-dps` 而非 `dpsmeter.exe`，随 syso 一并嵌入 |

## 5. 验收标准

1. explorer / 任务管理器 / 任务栏 / Alt+Tab / 托盘五处显示一致的新图标
2. DPI 100% / 125% / 150% 下托盘图标不糊、不裁边
3. `-headless` 模式启动正常（图标加载失败仅降级不崩溃）
4. `build.bat` 一键构建通过；exe 体积增量 ≤ 200 KB；仍为单文件分发
5. 无 `.syso`（如换机器 clone 未含二进制）时构建仍成功、行为同现状

## 6. 明确不做

- WebView2 页面内 favicon / UI 图标（前端已有自己的图标体系，与本需求无关）
- 窗口运行时换图标、动画图标
- Linux/macOS 资源（项目仅 Windows 目标）
