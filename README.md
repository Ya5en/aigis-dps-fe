# aigis-dps-fe

千年戦争アイギス DPS 统计前端：SHM 只读消费 → Go 桥聚合 → overlay 面板 / 规则编辑器。
当前版本见仓库根 `VERSION`（`MAJOR.MINOR.PATCH`；exe 属性与 `-version` 输出均由它派生，
不在文档里另写一份，以免脱节）。

上游为 [aigis-dps](../aigis-dps) 的 DLL（进程内 hook，将伤害事件发布到共享内存
`Local\aigis_dps`，协议见 `aigis-dps/docs/SHM_PROTOCOL.md`）；本项目为独立的
只读消费端，与 DLL 无任何写入交互。

## 架构

```
DLL (aigis-dps)                 Go 桥 (本项目)                      前端
────────────────                ──────────────                      ────
SHM "DPS2"  ──1ms 轮询──▶ shm.go 读事件流
                                aggregate.go 聚合
                                  · 实例分组（ai#ag）
                                  · 会话/实时 DPS（600 帧滑窗）
                                  · 受击方按 vi 聚合（合并配置可跨 vi 归并）
                                  · 会话历史（50 场）
                                api.go /events SSE（200ms 渲染就绪快照）
                                                    ──────────▶  overlay 面板（WebView2）
                                                                  editor.html 规则编辑器
fetch.go 启动时在线拉 NameText 名字表（逐级哈希短路）
dot.go    敌方点阵预览（EnemyDot.aar → ALTX 图集 PNG + AOD 动画元数据）
```

单 exe（约 13MB），前端静态资源嵌入；点阵编号表不在构建期固化，与名字表同构：启动后台从 Worker 拉最新 index（XOR 0xDA 解密）抽取 `EnemyDot*.aar` 行生成，缓存于 `data/dotcache/dotcdn.json`（index URL 哈希短路，失败沿用缓存）。

## 构建与运行

依赖：Go 1.22+、Node.js（前端构建）、WebView2 Runtime（overlay 形态）。

```bat
build.bat   rem VERSION → versioninfo.json → syso → npm run build → go build → dpsmeter.exe
run.bat     rem 启动
```

`build.bat` 是一条完整链路：读 `VERSION` → 刷新 `versioninfo.json`（`tools/genver`）→
用 `go tool goversioninfo` 重生成 `rsrc_windows_amd64.syso` → 前端构建 → 编译并把
版本号注入二进制（`-X main.version`）。日常改版本只需改 `VERSION` 一个文件再跑 `build.bat`。

图标与版本信息由 `rsrc_windows_amd64.syso`（已入库）随 `go build` 自动链接，
内容来自 `versioninfo.json` + `assets/app.ico`。仅换图标时手动重生成资源即可：

```bat
go tool goversioninfo -64 -o rsrc_windows_amd64.syso versioninfo.json
```

（goversioninfo 已登记为 `go.mod` 的 tool 依赖，`go tool` 直接可用；v1.4.1 起才兼容新版
Go 链接器，v1.4.0 会报 `unknown relocation type 7`。`.syso` 缺失时构建仍通过，
窗口自动回退系统默认图标。）

启动参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:9832` | HTTP 监听地址（仅本地） |
| `-data` | `data` | 数据目录（unit_names.json / aliases.json 所在） |
| `-poll` | `1` | SHM 轮询间隔（毫秒） |
| `-headless` | 关 | 仅启动本地服务不开窗，浏览器访问面板 |
| `-size` | `520x420` | overlay 初始尺寸 WxH |
| `-version` | 关 | 打印版本号后退出（`dpsmeter X.Y.Z`，值与 `VERSION` 同源） |

overlay 操作：默认点击穿透（不挡游戏）；托盘菜单或 `Ctrl+Alt+L` 切换穿透；
`Ctrl+Alt+H` / 托盘 / 双击托盘图标切换显隐；解锁后拖页面顶部拖条移动窗口；
面板高度随内容自适应（240–800px）。窗口关闭 = 进程退出。

**右键菜单**（解锁后或浏览器中）：规则编辑器 / 导入战斗数据 / 导出战斗数据 /
退出；点击任意条目后自动收起，弹出位置越界时自动翻转防出窗。

窗口为普通无边框置顶窗（无 `WS_EX_TOOLWINDOW`）：任务栏有按钮、Alt+Tab
可见、可被 OBS 等工具的窗口捕获选中（录屏演示面板时直接窗口捕获即可）；
点击穿透与置顶行为不受影响。

## 版本号约定

**唯一真源**：仓库根 `VERSION`（单行纯文本，不带 `v` 前缀，如 `1.0.0`）。
其它位置的版本号全部派生，不手工编辑：

| 出现位置 | 来源 | 形式 |
|---|---|---|
| exe 属性 FileVersion / ProductVersion | `tools/genver` 写入 `versioninfo.json` | 四段 `X.Y.Z.0`（Build 恒 0） |
| StringFileInfo 的 FileVersion / ProductVersion 字符串 | 同上 | 完整语义化版本（含 `-rc.1` 之类后缀） |
| `dpsmeter.exe -version` 输出 | `build.bat` 的 `-X main.version` | `dpsmeter X.Y.Z` |
| 启动日志 | 同上 | `[*] dpsmeter vX.Y.Z 启动…` |

**格式**：`MAJOR.MINOR.PATCH`，可选预发布后缀 `-rc.1` / `-beta.2`（预发布仅供自测，
不打正式 tag）。校验正则：`^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$`。

**何时升哪一位**（SemVer 判定，按影响面而非改动行数）：

| 位 | 触发条件（本项目语境） |
|---|---|
| MAJOR | 破坏性变更：SHM 协议版本不兼容、HTTP API 破坏性改动、`aliases.json` 语义改变、`.ashm` 格式变更 |
| MINOR | 向后兼容的新功能：新 API 端点、新面板能力、新数据文件/新导入来源 |
| PATCH | 向后兼容的修复：bug、文案、样式、性能、日志可观测性、面向使用者的文档改动；内部工具 / 注释等无感更新不占位（见下节） |

**更新流程**

1. 改 `VERSION`（只改这一处），且**与代码改动同批**——不许事后补
2. 跑 `build.bat`——自动刷新 `versioninfo.json` → 重生成 `rsrc_windows_amd64.syso` →
   前端构建 → 编译并注入版本号
3. 提交时 `VERSION`、`versioninfo.json`、`rsrc_windows_amd64.syso` 三者一起进同一次 commit
   （三者必须同源，否则 exe 属性与文件内容会脱节）
4. 一致性自检：`go run ./tools/genver -check`（一致打印版本，不一致非零退出，可挂 CI / 提交钩子）

**约定**：README 与代码注释不写死版本号（本节示例除外）；版本只在 `VERSION` 与构建产物中体现。

### 铁律：用户可感知的修改都必须如实更新版本号

> **任何一次「用户可感知」的修改，都要在同一个变更集里把 `VERSION` 升到位。**
> 版本号描述的是**交付面**（exe、前端产物、数据格式、对外文档）的状态，
> 不是提交计数——漏升、迟升、虚升一律按交付缺陷处理，产物作废。

**「用户无感」判定（不升版，只进 git 历史）**：改动不会改变 exe / 前端产物的任何行为、
外观、数据格式或对外文档内容，即为用户无感。典型：`tools/` 内部脚本（分析、侦察、
报告生成）、内部笔记 / 报告 md、代码注释、不参与构建的辅助脚本。
无感更新只依赖 git 提交记录追溯，**禁止为此升 `VERSION`**。
拿不准时按「可感知」处理。

| # | 规则 | 说明 / 反例 |
|---|---|---|
| 1 | **随改随升** | `VERSION` 增量必须与可感知改动同批（同一次构建 / 提交）。✗ 先改代码跑通、隔天想起来才补版本号 |
| 2 | **如实定级** | 位数只由改动性质决定（见上表），不看改动行数。✗ 把 `.ashm` 格式变更、API 破坏性改动塞进 PATCH 蒙混；✗ 无破坏性却升 MAJOR 充数 |
| 3 | **一次一位** | 一次改动只升一位，且只升该升的那一位；多次独立改动不得攒着合并记。✗ 三个 bugfix 记成 1.0.1（应是 1.0.1→1.0.2→1.0.3）；✗ bugfix 记 1.1.0 |
| 4 | **无感不升版** | 用户无感更新（内部工具 / 内部文档 / 注释 / 不参与构建的脚本）不占版本位，由 git 历史记录；README 中面向使用者的说明改动**可感知**，占一次 PATCH |
| 5 | **产物自证** | 只有 `dpsmeter.exe -version`、启动日志的 `v` 值、exe 属性的 FileVersion/ProductVersion 全部与 `VERSION` 逐字一致，该产物才算有效；不一致立即作废重构建 |
| 6 | **派生不得手改** | 只改 `VERSION`，`versioninfo.json` / `rsrc_windows_amd64.syso` 一律由 `build.bat` 重新生成；手改派生文件视为违规 |
| 7 | **未交付可并入** | 仅当该版本**尚未到达交付点**（构建产物实跑验证通过）时，同一变更集内的追加改动可并入而不另升位；一旦达交付点即冻结，后续可感知改动必须新升一位 |
| 8 | **回滚不回退** | 回滚可感知改动时版本号继续**向前**走一位 PATCH，并在变更记录里写明「回滚了 X」；已发布的号段永不复用、永不减号。回滚无感改动不动版本 |

**自检口径**：用户可感知的改动已落地（源码、前端、对外文档任一）却看不到 `VERSION`
的同步变化，且该版本已达交付点 → 视为违规，先补版本号再构建。核对时先跑
`go run ./tools/genver -check` 看派生文件是否与 `VERSION` 一致，再看 `VERSION`
是否解释了本次改动。内部工具 / 报告的改动不在此列，只要求 git 提交信息说清楚。

> ⚠ `build.bat` 已内置守卫：`VERSION` 缺失或为空直接失败，绝不产出无版本号的 exe。

## 配置：data/aliases.json

mtime 热载，保存后新事件立即生效（已聚合历史不回溯重算，重置后干净开局）。

```json
{
  "names": {
    "a590":  "我方改名",
    "v4755": "蛇魔"
  },
  "merge": {
    "a590":  ["1469", "1470"],
    "v4755": "4756",
    "590":   ["1469"]
  }
}
```

**命名（names）**：key 必须 `a`/`v` 前缀 + 数字 id。`a` = 我方（显示名优先于
NameText，亦可为查不到名的新 id 补名）；`v` = 敌方（按受击身份 id vi 生效）。

**合并（merge）**：key = 吸收方（目标），value = 被吸收源列表（或单源字符串简写）；
空列表 = 仅同 id 自合并。目标 key 支持 `a`/`v` 前缀（无前缀 = 我方，向后兼容）——
我方按攻击者 CardID（ai）归并输出行，敌方按 vi 归并受击行，两侧独立并查集，
规则完全一致：链式合并、相同源先声明者优先、循环吸收丢弃环边、
错阵营源视为非法。目标自身实例一并归入合一行。

可视化编辑：面板右键菜单「规则编辑器」（或浏览器开
`http://127.0.0.1:9832/editor.html`），支持命名/合并行编辑、保存校验，
以及单位预览——输入敌方 vi 即拉取对应
`EnemyDot<vi>.aar`（CDN，缓存于 `data/dotcache/`），按逆向定案的 ALTX 帧表 +
AOD 时间轴播放动画（Stand/Walk/Attack/Damage，锚点对齐），亦可看全图集；
预览后可一键落成命名规则。

## HTTP API（127.0.0.1）

| 端点 | 说明 |
|---|---|
| `GET /api/state` | 桥状态：SHM 接入/静止判定 + 会话列表元信息 |
| `GET /api/session?sid=` | 指定场次快照（sid=0 = 实时） |
| `GET /events` | SSE：ver 变化推渲染就绪快照帧，5s 心跳 |
| `POST /api/reset` | 清空统计开新会话（不动 SHM 与游标） |
| `GET /api/names` | 当前名字表 |
| `GET /api/alias` | 当前生效别名配置（含并查集解析结果） |
| `GET/POST /api/aliasfile` | 读/写 aliases.json（编辑器用） |
| `POST /api/openeditor` | 系统浏览器打开规则编辑器 |
| `POST /api/export` | SHM 原样 dump（16MB 全量映像）→ zstd 压缩落盘 `data/exports/`，扩展名 `.ashm`；`zstd -d` 可解 |
| `POST /api/import` | 导入 `.ashm`（zstd 压缩的 SHM 映像）重放事件；产生的会话带 `src=import`；缺省从 Go 侧弹系统文件选择框，`?name=` 可程序化指定 exports 内文件；SHA-256 去重，重复导入 409 |
| `GET /api/dotindex` | 全部可用敌方点阵编号 |
| `GET /api/vdot?v=` | vi → 点阵编号解析 |
| `GET /api/dot.png?d=` | 点阵图集 PNG（`?v=` 直接按 vi） |
| `GET /api/dotmeta?d=` | 帧表 + 动作时间轴（动画元数据） |

## 数据文件（data/）

| 文件 | 来源 | 说明 |
|---|---|---|
| `unit_names.json` | fetch.go 在线拉取 | CardID → 全名缓存 |
| `names_meta.json` | fetch.go | 资源 URL 哈希（短路检查） |
| `aliases.json` | 用户/编辑器 | 命名 + 合并配置 |
| `exports/` | `/api/export` | SHM 原样 dump（`.ashm` = zstd 压缩的映像，导入功能读回用） |
| `dotcache/` | dot.go | dot 编号表（`dotcdn.json` + `_meta`）+ EnemyDot aar 原包 + 图集 PNG + 动画元数据 |

`tools/` 只保留构建/自检工具链。点阵 CDN 索引已不在仓库内（旧嵌入版快照在
`../aigis-dps-lab/data/dotcdn_embedded_snapshot.json`），编号表运行时从 Worker 拉取。
逆向研究材料（资源样本、分析脚本、报告、构建产物）已移至仓库外的兄弟目录
`../aigis-dps-lab/`（`scripts/` 分析脚本、`data/` 样本与中间数据、`reports/` 报告、
`artifacts/` 旧产物），完全不进 git。dot 渲染优先级：`tools/EnemyDotNNNN.aar`
本地样本（如有，手工从 lab 拷回即生效）→ `data/dotcache` → CDN。

### 导入 / 导出格式说明

`.ashm` = zstd 压缩的 SHM 16MB 全量映像（头 + 槽表 + 事件环，字节级原样），
导入时在映像副本上按 SHM 协议重放（复用 poll 全套校验：槽表发布位、ABA、
gf 回退分段），会话标记 `src=import` 与实时数据隔离。导入入口从 Go 侧弹出
系统文件选择框（comdlg32 `GetOpenFileNameW`，零依赖；overlay 形态下
WebView2 的 JS 文件选择不可靠），用户取消返回 `{ok:false, canceled:true}`。

**场次编号空间隔离**：实时/重置会话用全局流水号（1..N）；每次导入分配独立
批次号 `batch`，批内第 `idx` 场的 sid 编码为 `(batch<<32)|idx`——三个空间
（实时 / 各批次之间）互不混用。`sessionMeta` 携带 `batch`/`idx`，前端显示
「导入N·第M场」。同内容映像按 SHA-256 去重，重复导入 409。
可扩展点（预留，未实现）：zstd 流首可放置 skippable frame（magic `0x184D2A50..5F`，
12B 头 + 负载）承载 JSON 元数据（工具版本 / 内容校验和等）——klauspost 解码器
透明跳过、标准 `zstd` CLI 兼容（`tools/skipframe` 实验已验证），将来需要时接入。

## 日志

`dpsmeter.log`（exe 同级，追加）+ stderr。启动接入、名字表更新、配置热载、
CDN 拉取失败、导入文件选择（选择/取消/失败）等均有记录；
HTTP 请求行排除 SSE 长连接防刷屏。

## 目录结构

```
main.go      启动/参数
shm.go       SHM 只读接入（协议 v2，"DPS2"）
aggregate.go 全部聚合计算（前端零计算）
api.go       HTTP/SSE 服务 + 名字/别名热载 + 导入导出
alias.go     用户配置层（命名 + 双侧并查集）
fetch.go     NameText 在线拉取（逐级短路）
dot.go       敌方点阵解码（ALAR/ALTX/AOD）+ 渲染缓存
dialog.go    系统文件选择框（comdlg32 GetOpenFileNameW，导入入口）
window.go    overlay：无边框置顶 + 点击穿透 + 托盘/热键（图标取 exe 资源）
web/         Vue3 + Vite 前端（overlay 面板）
web/public/editor.html  规则编辑器（原生 HTML）
assets/app.ico          应用图标（多尺寸 16–256），图标源文件在 assets/src/
VERSION                 语义化版本唯一真源（单行，如 1.0.0）
versioninfo.json        版本信息 + 图标路径（由 VERSION 生成，勿手改版本字段）
rsrc_windows_amd64.syso 由上面两者生成的资源包（入库，go build 自动链接）
tools/       构建/自检工具链（非运行依赖）：
               genver/    VERSION → versioninfo.json（含 -check 校验）
               pdot/      任意 PlayerDot/EnemyDot aar → 图集 PNG
               iconcheck/ 校验 exe 图标资源与窗口 WM_GETICON
               bench/     压缩算法基准
               skipframe/ zstd skippable frame 行为实验
```

仓库外兄弟目录 `../aigis-dps-lab/`（不进 git）存放逆向研究材料：
`scripts/` 分析脚本 / `data/` 资源样本与中间数据 / `reports/` 分析报告 /
`artifacts/` 历史构建产物与测试 dump。
