// 与 Go 桥 api.go / aggregate.go 的 JSON 契约一一对应（前端零计算，只映射渲染）

export interface SnapRow {
  n: string // 显示名（名#ag 或回退 ai#ag，桥内已填）
  ai: number
  ag: number
  dmg: number
  cnt: number
  dps: number
  rt: number // 实时 DPS（桥内 600 帧滑窗）
  pct: number // 伤害占比 0..1
  ty?: Record<string, number> // 分类型伤害值（行背景分段柱）
}

export interface SnapHdr {
  dmg: number
  cnt: number
  gfFirst: number
  gfLast: number
  dur: number // 秒（事件跨度，DPS 分母口径，gf 口径）
  clock: number // 显示用战斗时钟 = last_gf / 60（战斗开始清零、暂停停走）
  dps: number
  rt: number
  ty: Record<string, number>
  lost: number
  lostRanges: number
}

export interface SnapSrc {
  ai: number
  ag: number
  dmg: number
}

export interface SnapVic {
  vi: number // 受击者身份 id（同 vi = 同一只怪；旧 DLL 回退 = vg）
  name?: string // 显示名（aliases.json v 前缀 > vi 编号）
  n: number // 实体计数（观察到的不同 vg 数）
  dmg: number // 承伤合计
  cnt: number // 受击次数
  atk: number // 打过它的不同攻击者实例数
  pct: number // 占全场伤害比例 0..1
  srcs?: SnapSrc[] // 各攻击者对该目标的伤害（降序，承伤行展开用）
}

export interface SnapMatrixRow {
  ai: number
  ag: number
  cells: number[] // 与 matrix.cols 一一对应（0 = 未打该目标）
  other: number // 其余目标伤害合计
}

export interface SnapMatrix {
  cols: SnapVic[] // 前 N 目标（按承伤降序）
  rows: SnapMatrixRow[] // 与 rows 同序（按攻击者总伤降序）
}

export interface Snapshot {
  t: 'snap'
  sid: number // 会话键：实时 = 流水号 1..N；导入 = (batch<<32)|idx（≥2^32）
  epoch: number // writer init 序号（SHM 协议）
  ver: number
  started: number // Unix 秒
  hdr: SnapHdr
  rows: SnapRow[] // 已按 dmg 降序
  vrows?: SnapVic[] // 逐目标承伤，已按 dmg 降序（缺省 = 无目标数据）
  matrix?: SnapMatrix // 攻击者 × 受击者伤害矩阵
  buckets: number[] // 每 gf 秒伤害
}

export interface SessionMeta {
  sid: number
  epoch: number
  dmg: number
  cnt: number
  dur: number
  clock: number
  dps: number
  started: number
  src?: 'import' // 来源标记：缺省 = 真实 SHM 会话；'import' = 导入的历史数据（场次下拉分组用）
  batch?: number // 导入批次号（仅导入会话；sid = (batch<<32)|idx）
  idx?: number // 批内战斗序号（仅导入会话）
}

export interface ShmState {
  shm: 'ok' | 'none'
  stalled: boolean
  epoch: number
  seqTotal: number
  oldestSeq: number
  replaying: boolean // 历史回放进行中（启动串行追赶）
  replayLeft: number // 回放剩余条数
  sessions: SessionMeta[]
}

export type ConnState = 'wait' | 'live' | 'stalled'
