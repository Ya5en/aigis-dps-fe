# aigis-dps-fe

千年戦争アイギス（千年战争 Aigis）实时 DPS 统计 overlay。上游 DLL（aigis-dps）
将伤害事件发布到共享内存 `Local\aigis_dps`（协议 v2，magic `"DPS2"`），本项目作为
**只读消费端**以 1ms 轮询读取事件流，在 Go 侧完成全部聚合，通过 SSE 推送到
WebView2 overlay 面板与浏览器规则编辑器——前端零计算，游戏进程零注入零写入。

当前版本见仓库根 [`VERSION`](VERSION)（唯一真源；exe 属性、启动日志、`-version`
输出均由它派生）。

## 功能

- **实时统计**：每实例 DPS（600 帧滑窗）、受击方按 vi 聚合、会话历史（50 场）
- **命名与合并**：我方/敌方改名、跨 id 归并（双侧独立并查集），`aliases.json`
  mtime 热载，另有在线规则编辑器（含敌方点阵动画预览，预览后一键落成命名规则）
- **规则订阅**：从社区维护的订阅 URL 拉取别名规则（ETag 条件请求、大小上限、
  schema 校验净化），分层合并——本地 `aliases.json` 永远优先，订阅只补漏
- **名字表在线更新**：启动后台从 Worker 拉最新资源索引（XOR 0xDA 解密），逐级
  URL 哈希短路，失败沿用磁盘缓存
- **敌方点阵预览**：EnemyDot aar → ALTX 图集 + AOD 时间轴动画（Stand/Walk/Attack/
  Damage），编号表运行时从 index 抽取生成，不固化在二进制里
- **导入 / 导出**：`.ashm`（zstd 压缩的 SHM 16MB 全量映像）导出后可重放导入，
  导入会话与实时数据编号空间完全隔离
- **overlay 形态**：无边框置顶、默认点击穿透不挡游戏，托盘 + 全局热键控制，
  可被 OBS 窗口捕获（无 `WS_EX_TOOLWINDOW`）

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

产物为单 exe（约 13MB），前端静态资源嵌入。

## 运行

依赖：WebView2 Runtime（overlay 形态）。从 [Releases](../../releases) 下载
`dpsmeter.exe`，与上游 DLL（发布 SHM 的 aigis-dps）配合使用。

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:9832` | HTTP 监听地址（仅本地） |
| `-data` | `data` | 数据目录（unit_names.json / aliases.json 所在） |
| `-poll` | `1` | SHM 轮询间隔（毫秒） |
| `-headless` | 关 | 仅启动本地服务不开窗，浏览器访问面板 |
| `-size` | `520x420` | overlay 初始尺寸 WxH |
| `-version` | 关 | 打印版本号后退出（`dpsmeter X.Y.Z`，值与 `VERSION` 同源） |
| `-log-level` | `warning` | 日志级别：`info` / `warning` / `error`；默认只输出警告与错误 |
| `-v` | 关 | 显示 INFO 日志（等价 `-log-level info`） |

**操作**：默认点击穿透（不挡游戏）；托盘菜单或 `Ctrl+Alt+L` 切换穿透；
`Ctrl+Alt+H` / 托盘 / 双击托盘图标切换显隐；解锁后拖页面顶部拖条移动窗口；
面板高度随内容自适应（240–800px）。窗口关闭 = 进程退出。

**右键菜单**（解锁后或浏览器中）：规则编辑器 / 导入战斗数据 / 导出战斗数据 /
退出；点击任意条目后自动收起，弹出位置越界时自动翻转防出窗。

## 构建

依赖：Go 1.22+、Node.js。

```bat
build.bat   rem VERSION → versioninfo.json → syso → npm run build → go build → dpsmeter.exe
run.bat     rem 启动
```

`build.bat` 是一条完整链路：读 `VERSION` → 刷新 `versioninfo.json`（`tools/genver`）→
用 `go tool goversioninfo` 重生成 `rsrc_windows_amd64.syso` → 前端构建 → 编译并把
版本号注入二进制（`-X main.version`）。日常改版本只需改 `VERSION` 一个文件再跑
`build.bat`。

图标与版本信息由 `rsrc_windows_amd64.syso`（已入库）随 `go build` 自动链接。
仅换图标时手动重生成资源：

```bat
go tool goversioninfo -64 -o rsrc_windows_amd64.syso versioninfo.json
```

（goversioninfo 已登记为 `go.mod` 的 tool 依赖，`go tool` 直接可用；v1.4.1 起才兼容
新版 Go 链接器，v1.4.0 会报 `unknown relocation type 7`。`.syso` 缺失时构建仍通过，
窗口自动回退系统默认图标。）

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
规则完全一致：链式合并、相同源先声明者优先、循环吸收丢弃环边、错阵营源视为非法。
目标自身实例一并归入合一行。

可视化编辑：面板右键菜单「规则编辑器」（或浏览器开
`http://127.0.0.1:9832/editor.html`），支持命名/合并行编辑、保存校验，以及单位
预览——输入敌方 vi 即拉取对应 `EnemyDot<vi>.aar`（CDN，缓存于 `data/dotcache/`），
按逆向定案的 ALTX 帧表 + AOD 时间轴播放动画（锚点对齐），亦可看全图集；
预览后可一键落成命名规则。

## 规则订阅

社区共同维护的别名规则（新单位改名、合并组）通过订阅 URL 分发，用户只需订阅，
不必手编 JSON。推荐订阅源形态：GitHub 仓库里的 `aliases.json` +
jsDelivr CDN（`https://cdn.jsdelivr.net/gh/<user>/<repo>@main/aliases.json`），
社区提 PR、维护者合并即发布。

配置 `data/subscriptions.json`（编辑器「规则订阅」区可视化编辑）：

```json
{
  "enabled": true,
  "intervalMin": 30,
  "urls": ["https://cdn.jsdelivr.net/gh/<user>/<repo>@main/aliases.json"]
}
```

**分层合并模型**（生效结果 = 订阅层叠加在本地上）：

- 合并结果 = 订阅层（URL 依序，先者优先）← 覆盖 ← 本地 `aliases.json`（最高优先级）
- 命名（names）：本地同名键永远赢，订阅只补本地缺失的 id
- 合并（merge）：规则按「本地声明在前、订阅在后」拼接——并查集「先声明者优先」
  的冲突裁决天然让本地规则压过订阅，订阅补本地未覆盖的源
- 订阅文件格式与 `aliases.json` 相同（`names` / `merge` 段，二者至少其一），
  可另带 `meta` 字段（如 `{"version": "...", "updated": "..."}`，Go 侧忽略）

**拉取策略**：启动后台刷一轮 + 按 `intervalMin` 定时刷新（另有配置 mtime 变化
即刷）；ETag/If-None-Match 条件请求，304 短路；单文件上限 1MB；schema 校验
+ 名称净化（去控制字符、单名 ≤64 字符），坏文件拒载沿用上次缓存。
**失败回退**：拉取失败沿用上次成功缓存（原文落盘 `data/subcache/`），再不行
只用本地文件——订阅任何环节出问题都不影响记录主功能。停用/清空 URL 即移除
订阅层，回到纯本地。
**隐私**：订阅拉取会向订阅源服务器暴露本机 IP 与刷新节奏；不希望外联时
`enabled: false` 即可完全关闭。

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
| `GET /api/subs` | 订阅状态（URL 列表 + 逐条拉取状态 + 订阅层计数） |
| `POST /api/subsfile` | 保存订阅配置 `subscriptions.json`（校验 URL 后写回，立即触发刷新） |
| `POST /api/subs/refresh` | 手动立即刷新订阅 |
| `POST /api/openeditor` | 系统浏览器打开规则编辑器 |
| `POST /api/export` | SHM 原样 dump（16MB 全量映像）→ zstd 压缩落盘 `data/exports/`，扩展名 `.ashm`；`zstd -d` 可解 |
| `POST /api/import` | 导入 `.ashm` 重放事件；产生的会话带 `src=import`；缺省从 Go 侧弹系统文件选择框，`?name=` 可程序化指定 exports 内文件；SHA-256 去重，重复导入 409 |
| `GET /api/dotindex` | 全部可用敌方点阵编号 |
| `GET /api/vdot?v=` | vi → 点阵编号解析 |
| `GET /api/dot.png?d=` | 点阵图集 PNG（`?v=` 直接按 vi） |
| `GET /api/dotmeta?d=` | 帧表 + 动作时间轴（动画元数据） |

## 导入 / 导出格式

`.ashm` = zstd 压缩的 SHM 16MB 全量映像（头 + 槽表 + 事件环，字节级原样），
导入时在映像副本上按 SHM 协议重放（复用 poll 全套校验：槽表发布位、ABA、
gf 回退分段），会话标记 `src=import` 与实时数据隔离。事件 payload 由标准库
`encoding/json` 解析：`gf/ai/ag/vi/vg/d/ty` 字段顺序无关，上游新增的未知字段
（如 v10.53+ 的 `"ex":{...}` 调试对象）自动忽略，内部同名键不参与统计；
畸形载荷明确报错并丢弃。导入入口从 Go 侧弹出
系统文件选择框（comdlg32 `GetOpenFileNameW`，零依赖；overlay 形态下
WebView2 的 JS 文件选择不可靠），用户取消返回 `{ok:false, canceled:true}`。

**场次编号空间隔离**：实时/重置会话用全局流水号（1..N）；每次导入分配独立
批次号 `batch`，批内第 `idx` 场的 sid 编码为 `(batch<<32)|idx`——实时与各批次
之间互不混用。`sessionMeta` 携带 `batch`/`idx`，前端显示「导入N·第M场」。
同内容映像按 SHA-256 去重，重复导入 409。

可扩展点（预留，未实现）：zstd 流首可放置 skippable frame（magic
`0x184D2A50..5F`，12B 头 + 负载）承载 JSON 元数据（工具版本 / 内容校验和等）——
klauspost 解码器透明跳过、标准 `zstd` CLI 兼容（`tools/skipframe` 实验已验证），
将来需要时接入。

## 数据文件（data/，运行时生成，不入库）

| 文件 | 来源 | 说明 |
|---|---|---|
| `unit_names.json` | fetch.go 在线拉取 | CardID → 全名缓存 |
| `names_meta.json` | fetch.go | 资源 URL 哈希（短路检查） |
| `aliases.json` | 用户/编辑器 | 命名 + 合并配置 |
| `subscriptions.json` | 用户/编辑器 | 规则订阅配置（enabled / intervalMin / urls） |
| `subcache/` | subscribe.go | 订阅层产物：`sub_merged.json`（合并后订阅规则，agg 叠加用）+ 各 URL 原文缓存 + `meta.json`（ETag/状态） |
| `exports/` | `/api/export` | SHM 原样 dump（`.ashm`，导入功能读回用） |
| `dotcache/` | dot.go | dot 编号表（`dotcdn.json` + `_meta`）+ EnemyDot aar 原包 + 图集 PNG + 动画元数据 |

日志：`dpsmeter.log`（exe 同级，追加）+ stderr。三级标签：

| 级别 | 含义 | 例子 |
|---|---|---|
| `[INFO]` | 正常里程碑 | 启动、名字表/编号表在线更新、配置热载、导入导出、HTTP 请求行（不含 SSE） |
| `[WARN]` | 降级但可用 | 沿用缓存、配置条目非法被忽略、缓存写盘失败、订阅拉取失败 |
| `[ERROR]` | 功能不可用 / 进程退出 | HTTP 服务起不来、数据目录建不了、WebView2 初始化失败 |

**默认只输出 `[WARN]` 与 `[ERROR]`**——一切正常时日志文件可以一直是空的，
这是特性不是故障。排查问题加 `-log-level info`（或 `-v`）即可看到 INFO 明细；
`-log-level error` 则进一步安静到只剩错误。

## 版本号约定

**唯一真源**：仓库根 `VERSION`（单行纯文本，不带 `v` 前缀）。其它位置全部派生，
不手工编辑：

| 出现位置 | 来源 | 形式 |
|---|---|---|
| exe 属性 FileVersion / ProductVersion | `tools/genver` 写入 `versioninfo.json` | 四段 `X.Y.Z.0`（Build 恒 0） |
| StringFileInfo 的 FileVersion / ProductVersion 字符串 | 同上 | 完整语义化版本（含 `-rc.1` 之类后缀） |
| `dpsmeter.exe -version` 输出 | `build.bat` 的 `-X main.version` | `dpsmeter X.Y.Z` |
| 启动日志（`-log-level info`） | 同上 | `[INFO] dpsmeter vX.Y.Z 启动…` |

**格式**：`MAJOR.MINOR.PATCH`，可选预发布后缀 `-rc.1` / `-beta.2`（预发布仅供
自测，不打正式 tag）。

**何时升哪一位**（SemVer 判定，按影响面而非改动行数）：

| 位 | 触发条件（本项目语境） |
|---|---|
| MAJOR | 破坏性变更：SHM 协议版本不兼容、HTTP API 破坏性改动、`aliases.json` 语义改变、`.ashm` 格式变更 |
| MINOR | 向后兼容的新功能：新 API 端点、新面板能力、新数据文件/新导入来源 |
| PATCH | 向后兼容的修复：bug、文案、样式、性能、日志可观测性、面向使用者的文档改动；内部工具 / 注释等无感更新不占位（见下） |

**铁律：用户可感知的修改都必须如实更新版本号**——版本号描述的是**交付面**
（exe、前端产物、数据格式、对外文档）的状态，不是提交计数。

1. **随改随升**：`VERSION` 增量与可感知改动同批（同一次构建/提交），不许事后补
2. **如实定级**：位数只由改动性质决定，不看改动行数
3. **一次一位**：一次改动只升一位，多次独立改动不攒记（三个 bugfix =
   1.0.1→1.0.2→1.0.3）
4. **无感不升版**：内部工具/内部文档/注释等不影响交付面的更新不占版本位，由
   git 历史记录；拿不准时按「可感知」处理
5. **产物自证**：`-version`、exe 属性（以及 `-log-level info` 时的启动日志行）必须与 `VERSION` 逐字一致，
   否则产物作废重构建
6. **派生不得手改**：只改 `VERSION`，`versioninfo.json` / `.syso` 由 `build.bat`
   重新生成
7. **未交付可并入**：构建产物实跑验证通过前，同一变更集的追加改动可并入；
   达交付点即冻结，后续可感知改动必须新升一位
8. **回滚不回退**：回滚可感知改动时版本号继续向前走一位 PATCH，号段永不复用、
   永不减号；回滚无感改动不动版本

自检：`go run ./tools/genver -check`（一致打印版本，不一致非零退出）。
`build.bat` 内置守卫：`VERSION` 缺失或为空直接失败。

## 目录结构

```
main.go      启动/参数
shm.go       SHM 只读接入（协议 v2，"DPS2"）
aggregate.go 全部聚合计算（前端零计算）
api.go       HTTP/SSE 服务 + 名字/别名热载 + 导入导出
alias.go     用户配置层（命名 + 双侧并查集）
fetch.go     NameText 在线拉取（逐级短路）
subscribe.go 规则订阅（Worker/CDN 拉取 + 分层合并 + 条件请求缓存）
dot.go       敌方点阵解码（ALAR/ALTX/AOD）+ 编号表运行时拉取 + 渲染缓存
dialog.go    系统文件选择框（comdlg32 GetOpenFileNameW，导入入口）
window.go    overlay：无边框置顶 + 点击穿透 + 托盘/热键（图标取 exe 资源）
web/         Vue3 + Vite 前端（overlay 面板）
web/public/editor.html  规则编辑器（原生 HTML）
assets/app.ico          应用图标（多尺寸 16–256），图标源文件在 assets/src/
VERSION                 语义化版本唯一真源
versioninfo.json        版本信息 + 图标路径（由 VERSION 生成，勿手改版本字段）
rsrc_windows_amd64.syso 由上面两者生成的资源包（入库，go build 自动链接）
tools/       构建/自检工具链（非运行依赖）：
               genver/    VERSION → versioninfo.json（含 -check 校验）
               pdot/      任意 PlayerDot/EnemyDot aar → 图集 PNG
               iconcheck/ 校验 exe 图标资源与窗口 WM_GETICON
               bench/     压缩算法基准
               skipframe/ zstd skippable frame 行为实验
```

## 相关

- SHM 协议 v2（`"DPS2"`：头 + 槽表 + 事件环）由上游 DLL（aigis-dps）发布；
  本仓库只实现只读消费侧。
- 逆向研究材料（资源样本、格式分析脚本与报告）不在本仓库内维护。
