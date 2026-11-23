<script setup lang="ts">
import { computed, nextTick, onBeforeUnmount, onMounted, ref } from 'vue'
import type { ConnState, ShmState, SnapVic, Snapshot, SessionMeta } from './types'

const conn = ref<ConnState>('wait')
const state = ref<ShmState | null>(null)
const snap = ref<Snapshot | null>(null)
const panelEl = ref<HTMLElement | null>(null)
let es: EventSource | null = null

// 会话选择：LIVE=跟随实时；否则回看指定 sid 的历史快照
const LIVE = 0
const viewing = ref(LIVE)

function fmtNum(n: number): string {
  return Math.round(n).toLocaleString('en-US')
}
function fmtDps(n: number): string {
  return n >= 10000 ? (n / 1000).toFixed(1) + 'k' : Math.round(n).toLocaleString('en-US')
}
function fmtDur(sec: number): string {
  const s = Math.max(0, Math.round(sec))
  return `${String(Math.floor(s / 60)).padStart(2, '0')}:${String(s % 60).padStart(2, '0')}`
}

// 场次标题：实时/回看 = 「第N场」（sid 流水号）；导入 = 「导入B·第I场」
// （导入 sid 按 (batch<<32)|idx 编码，与实时流水号及其他批次互不混用）
const IMP_BASE = 2 ** 32
function sessTitle(sid: number): string {
  if (sid < IMP_BASE) return `第${sid}场`
  return `导入${Math.floor(sid / IMP_BASE)}·第${sid % IMP_BASE}场`
}
function sessLabel(m: SessionMeta): string {
  return `${sessTitle(m.sid)} ${fmtDur(m.clock)} · ${fmtDps(m.dps)}`
}

async function refreshState() {
  try {
    state.value = await fetch('/api/state').then((r) => r.json())
    conn.value = state.value.shm === 'ok' ? (state.value.stalled ? 'stalled' : 'live') : 'wait'
  } catch {
    conn.value = 'wait'
  }
}

async function doReset() {
  try {
    await fetch('/api/reset', { method: 'POST' })
    viewing.value = LIVE
    snap.value = null
    await refreshState()
  } catch {
    /* 桥不可达时保持原状 */
  }
}

// 场次分组：真实 SHM 会话 vs 导入的历史数据（SessionMeta.src === 'import'，导入功能落地后自动归组）
const realSess = computed(() => (state.value?.sessions ?? []).filter((s) => s.src !== 'import'))
const impSess = computed(() => (state.value?.sessions ?? []).filter((s) => s.src === 'import'))

async function viewSession(sid: number) {
  viewing.value = sid
  if (sid === LIVE) return // 实时快照由 SSE 自动跟随
  try {
    const r = await fetch(`/api/session?sid=${sid}`)
    if (!r.ok) throw new Error('no session')
    snap.value = (await r.json()) as Snapshot
  } catch {
    /* 拉取失败保持原快照 */
  }
}

onMounted(async () => {
  await refreshState()
  es = new EventSource('/events')
  es.onmessage = (e) => {
    const s = JSON.parse(e.data) as Snapshot
    // 实时帧只在「跟随实时」或帧恰为当前回看会话时应用；回看历史时不被打断
    if (viewing.value === LIVE || viewing.value === s.sid) snap.value = s
  }
  es.onerror = () => { conn.value = 'wait' }
  const t = window.setInterval(refreshState, 3000)
  onBeforeUnmount(() => window.clearInterval(t))
  document.addEventListener('click', closeCtx)
  document.addEventListener('keydown', onKeyClose)
  onBeforeUnmount(() => {
    document.removeEventListener('click', closeCtx)
    document.removeEventListener('keydown', onKeyClose)
  })
  if (panelEl.value) {
    syncHeight()
    new ResizeObserver(syncHeight).observe(panelEl.value)
  }
})

onBeforeUnmount(() => es?.close())

// ---- 渲染派生 ----
const hdr = computed(() => snap.value?.hdr ?? null)
// 状态提示统一为 4 字陈述、不带省略号：数据静止 / 等待数据
const waitTxt = computed(() => (conn.value === 'stalled' ? '数据静止' : '等待数据'))

// 行背景柱：宽度 = 该行伤害 / 第一名伤害；分段 = 行内各 ty 位组合的伤害占比
const tyKeys = computed(() =>
  Object.keys(hdr.value?.ty ?? {}).sort((a, b) => Number(a) - Number(b))
)
// ty 位语义（aigis-re/ty_bits.md §5）：ty 是位域——此处只观察 bit0（Magic）与
// bit7（Pierce，技能直伤引擎强制附加位），其余位不参与命名与配色。
const TY_BITS: Array<[number, string]> = [
  [0x000001, "Magic"], // bit0 魔法
  [0x000080, "Pierce"], // bit7 贯穿（技能直伤引擎位）
]
function tyName(v: number): string {
  const bits = TY_BITS.filter(([b]) => v & b).map(([, n]) => n)
  return bits.length ? bits.join("+") : "物理"
}
// 分段配色三级：Pierce 紫 > Magic 黄 > 其余一律白（无灰色语义）
function tyCls(v: number): string {
  if (v & 0x80) return "s-purple"
  if (v & 0x01) return "s-yellow"
  return "s-white"
}
// 伤害类型段 tooltip：ty 值 · 位名分解 · 伤害值 · 占比
const segTitle = (r: { ty?: Record<string, number>; dmg: number }, k: string) =>
  `ty 0x${Number(k).toString(16).toUpperCase().padStart(2, "0")}（${tyName(Number(k))}）· 伤害 ${fmtNum(r.ty?.[k] ?? 0)} · 占比 ${(((r.ty?.[k] ?? 0) / r.dmg) * 100).toFixed(1)}%`
const topDmg = computed(() => snap.value?.rows[0]?.dmg ?? 0)
function barW(r: { dmg: number }): string {
  return topDmg.value > 0 ? ((r.dmg / topDmg.value) * 100).toFixed(1) + '%' : '0%'
}
function segW(r: { dmg: number; ty?: Record<string, number> }, k: string): string {
  if (!r.dmg || !r.ty) return '0%'
  return (((r.ty[k] ?? 0) / r.dmg) * 100).toFixed(1) + '%'
}
const hasRows = computed(() => !!snap.value && snap.value.rows.length > 0)

// 承伤列表（按承伤降序）与行背景柱宽度
const vrows = computed(() => snap.value?.vrows ?? [])
const topVicDmg = computed(() => vrows.value[0]?.dmg ?? 0)
function vicBarW(v: { dmg: number }): string {
  return topVicDmg.value > 0 ? ((v.dmg / topVicDmg.value) * 100).toFixed(1) + '%' : '0%'
}

// ---- 选单切换 + 行展开（输出 / 承伤共用同一片区域） ----
const tab = ref<'out' | 'dmg'>('out')
const expOut = ref<string | null>(null) // 展开的输出行 key = ai#ag
const expVic = ref<number | null>(null) // 展开的承伤行 vi
function toggleOut(r: { ai: number; ag: number }) {
  const k = `${r.ai}#${r.ag}`
  expOut.value = expOut.value === k ? null : k
}
function toggleVic(v: { vi: number }) {
  expVic.value = expVic.value === v.vi ? null : v.vi
}
// 承伤行显示名：桥侧已按 v 别名解析（name > vi 编号）；多实体加 ×N 计数
function vicLabel(v: { name?: string; vi: number; n: number }): string {
  const base = v.name || String(v.vi)
  return v.n > 1 ? `${base}×${v.n}` : base
}

// 攻击者名映射（矩阵行与 rows 同序，仍按 ai#ag 兜底查）
const rowName = computed(() => {
  const m = new Map<string, string>()
  for (const r of snap.value?.rows ?? []) m.set(`${r.ai}#${r.ag}`, r.n)
  return (ai: number, ag: number) => m.get(`${ai}#${ag}`) ?? `${ai}#${ag}`
})

// 输出行展开：该单位对各目标的伤害分布（来源=桥内伤害矩阵，-1=其余目标）
const mtxMap = computed(() => {
  const m = new Map<string, { cells: number[]; other: number }>()
  for (const mr of snap.value?.matrix?.rows ?? []) m.set(`${mr.ai}#${mr.ag}`, mr)
  return m
})
function outDist(r: { ai: number; ag: number; dmg: number }): { vi: number; name?: string; d: number }[] {
  const mr = mtxMap.value.get(`${r.ai}#${r.ag}`)
  if (!mr || !snap.value?.matrix || r.dmg <= 0) return []
  const items = snap.value.matrix.cols
    .map((c, i) => ({ vi: c.vi, name: c.name, d: mr.cells[i] ?? 0 }))
    .filter((x) => x.d > 0)
  if (mr.other > 0) items.push({ vi: -1, d: mr.other })
  items.sort((a, b) => (a.vi < 0 ? 1 : 0) - (b.vi < 0 ? 1 : 0) || b.d - a.d) // 伤害降序；「其他目标」固定沉底
  return items
}
// 承伤行展开：该目标受到的各攻击者伤害分布（桥侧已降序，此处再保险一次）
function vicDist(v: SnapVic): { ai: number; ag: number; dmg: number }[] {
  return (v.srcs ?? [])
    .filter((x) => x.dmg > 0)
    .sort((a, b) => b.dmg - a.dmg)
}
// 输出子行 tooltip：目标 · vi（写规则用）· 伤害 · 占比（-1=其他目标，无 id）
function outSubTitle(r: { dmg: number }, it: { vi: number; name?: string; d: number }): string {
  const who = it.vi < 0 ? '其他目标' : `目标 ${it.name || '#' + it.vi} · vi=${it.vi}`
  return `${who} · 伤害 ${fmtNum(it.d)} · 占其输出 ${((it.d / r.dmg) * 100).toFixed(1)}%`
}
// 承伤子行 tooltip：攻击者 · id（写规则用）· 伤害 · 占比
function vicSubTitle(v: { dmg: number }, src: { ai: number; ag: number; dmg: number }): string {
  return `${rowName.value(src.ai, src.ag)} · id=${src.ai}#${src.ag} · 伤害 ${fmtNum(src.dmg)} · 占其承伤 ${((src.dmg / v.dmg) * 100).toFixed(1)}%`
}
// 子条宽度：行内相对强度
function subBarW(v: number, max: number): string {
  return max > 0 ? ((v / max) * 100).toFixed(1) + '%' : '0%'
}

// overlay 拖动：顶部拖条（仅 WebView2 内注入了 overlayMove 时有效，浏览器中静默无效）
const dragging = ref(false)
// 规则编辑器：交给系统浏览器打开（overlay 太小放不下编辑区）
async function openEditor() {
  try { await fetch('/api/openeditor', { method: 'POST' }) } catch { /* 桥不可达时忽略 */ }
}
function onStripDown(e: PointerEvent) {
  dragging.value = true
  ;(e.currentTarget as HTMLElement).setPointerCapture(e.pointerId)
}
function onStripUp() {
  dragging.value = false
}
function onStripMove(e: PointerEvent) {
  if (!dragging.value) return
  const api = (window as unknown as { overlayMove?: (x: number, y: number) => void }).overlayMove
  if (api) api(e.movementX, e.movementY)
}

// 窗口高度自适应：内容每变化就上报实际高度（拖条 12px + 余量），Go 侧只改高度
let lastSentH = 0
function syncHeight() {
  const api = (window as unknown as { overlayResize?: (h: number) => void }).overlayResize
  if (!api || !panelEl.value) return
  const h = Math.round(panelEl.value.getBoundingClientRect().height + 12 + 4)
  if (Math.abs(h - lastSentH) >= 2) {
    lastSentH = h
    api(h)
  }
}

// ---- 右键菜单（规则编辑/战斗数据导入导出/窗体退出；点击穿透解锁时或浏览器中可用）----
// 弹出后测量实际尺寸：越出窗口边缘时翻转到指针另一侧，仍越界则夹紧到窗口内
const ctx = ref<{ x: number; y: number } | null>(null)
const ctxEl = ref<HTMLElement | null>(null)
async function openCtx(e: MouseEvent) {
  ctx.value = { x: e.clientX, y: e.clientY }
  await nextTick()
  const el = ctxEl.value
  const c = ctx.value
  if (!el || !c) return
  const r = el.getBoundingClientRect()
  if (c.x + r.width > window.innerWidth) c.x = Math.max(0, e.clientX - r.width)
  if (c.y + r.height > window.innerHeight) c.y = Math.max(0, e.clientY - r.height)
  ctx.value = { x: c.x, y: c.y }
}
function closeCtx() { ctx.value = null }
function onKeyClose(e: KeyboardEvent) { if (e.key === 'Escape') closeCtx() }
// 战斗数据导出：桥侧将 SHM 原样 dump（16MB 全量映像）经 zstd 压缩落盘
// data/exports/ 并打开资源管理器定位；结果无需在 overlay 内展示
async function exportData() {
  try { await fetch('/api/export', { method: 'POST' }) } catch { /* 桥不可达时忽略 */ }
}
// 战斗数据导入：桥侧弹出系统文件选择框选择 .ashm（zstd SHM 映像）重放事件，
// 产生的会话带 src=import，归入场次下拉「导入」分组；重复导入被桥侧拒绝。
// 用户取消选择框时桥返回 canceled，前端静默不做任何变化
async function importData() {
  try {
    const r = await fetch('/api/import', { method: 'POST' })
    const d = (await r.json()) as { ok?: boolean; canceled?: boolean }
    if (d.canceled) return
    await refreshState() // 会话列表即时刷新，导入场次立即可回看
  } catch { /* 桥不可达时忽略 */ }
}
async function doExit() {
  const api = (window as unknown as { exitApp?: () => void }).exitApp
  if (api) { api() // WebView2 内走绑定回调（与托盘退出同路径）
  } else {
    try { await fetch('/api/exit', { method: 'POST' }) } catch { /* 桥不可达时忽略 */ }
  }
}
</script>

<template>
  <div class="panel" ref="panelEl" @contextmenu.prevent="openCtx">
    <!-- overlay 拖条 -->
    <div
      class="dragstrip"
      title="拖动窗口"
      @pointerdown="onStripDown"
      @pointermove="onStripMove"
      @pointerup="onStripUp"
    ></div>

    <!-- 单行状态条：点 + 场次/时长/DPS，异常信息仅异常时出现 -->
    <div class="bar">
      <i class="dot" :class="conn"></i>
      <span v-if="conn !== 'live'" class="dim">{{ waitTxt }}</span>
      <span v-if="snap" class="num">
        {{ sessTitle(snap.sid) }} {{ fmtDur(hdr!.clock) }} · DPS <b>{{ fmtDps(hdr!.dps) }}</b>
      </span>
      <span v-if="hdr && hdr.lost > 0" class="lost" :title="`丢失 ${hdr.lostRanges} 个区间`">
        丢 {{ fmtNum(hdr.lost) }}
      </span>
      <span class="tools">
        <span v-if="viewing !== LIVE" class="review">回看</span>
        <select
          v-model="tab"
          class="sel"
          title="切换统计视图"
        >
          <option value="out">输出</option>
          <option value="dmg">承伤</option>
        </select>
        <!-- 场次选择：原生下拉，optgroup = 不可点击的分组标签（SHM / 导入） -->
        <select
          v-if="state && state.sessions.length > 1"
          class="sel"
          :value="viewing"
          title="切换战斗场次"
          @change="viewSession(Number(($event.target as HTMLSelectElement).value))"
        >
          <optgroup label="SHM">
            <option :value="LIVE">实时</option>
            <option v-for="m in realSess" :key="m.sid" :value="m.sid">
              {{ sessLabel(m) }}
            </option>
          </optgroup>
          <optgroup label="导入">
            <template v-if="impSess.length">
              <option v-for="m in impSess" :key="'i' + m.sid" :value="m.sid">
                {{ sessLabel(m) }}
              </option>
            </template>
            <option v-else disabled>暂无导入数据</option>
          </optgroup>
        </select>
        <button class="reset" @click="doReset">重置</button>
      </span>
    </div>

    <!-- 历史回放过渡：串行追赶期间显示不定长进度条 -->
    <div v-if="state?.replaying" class="replay" title="正在重建启动前的战斗记录">
      <i class="replay-ind"></i>
      <span class="replay-text">重建历史{{ state.replayLeft > 0 ? ` · 剩余 ${fmtNum(state.replayLeft)} 条` : '' }}</span>
    </div>

    <!-- 输出统计：点击行展开 → 此单位对各目标的伤害分布 -->
    <div v-if="tab === 'out' && hasRows" class="rank">
      <div class="r-head">
        <span>名#ag</span><span>伤害</span><span>DPS</span><span>占比</span>
      </div>
      <template v-for="r in snap!.rows" :key="r.ai + '#' + r.ag">
        <div
          class="r-row"
          :class="{ exp: expOut === r.ai + '#' + r.ag }"
          :title="`${r.n} · id=${r.ai}#${r.ag} · 攻击 ${fmtNum(r.cnt)} 次 · 实时 ${fmtDps(r.rt)} · 占全场 ${(r.pct * 100).toFixed(1)}% · ${expOut === r.ai + '#' + r.ag ? '点击收起' : '点击展开'}`"
          @click="toggleOut(r)"
        >
          <i class="fill" :style="{ width: barW(r) }">
            <i
              v-for="k in tyKeys"
              :key="k"
              :class="tyCls(Number(k))"
              :style="{ width: segW(r, k) }"
              :title="segTitle(r, k)"
            ></i>
          </i>
          <span class="name">{{ r.n }}</span>
          <span class="num dmg">{{ fmtNum(r.dmg) }}</span>
          <span class="num dps">{{ fmtDps(r.dps) }}</span>
          <span class="num pct">{{ (r.pct * 100).toFixed(1) }}%</span>
        </div>
        <div v-if="expOut === r.ai + '#' + r.ag" class="sub">
          <div
            v-for="it in outDist(r)"
            :key="it.vi"
            class="sub-row"
            :title="outSubTitle(r, it)"
          >
            <i class="fill" :style="{ width: subBarW(it.d, Math.max(...outDist(r).map((x) => x.d))) }"></i>
            <span class="sub-name">{{ it.vi < 0 ? '其他目标' : it.name || '#' + it.vi }}</span>
            <span class="num dmg">{{ fmtNum(it.d) }}</span>
            <span class="num pct">{{ ((it.d / r.dmg) * 100).toFixed(1) }}%</span>
          </div>
          <p v-if="outDist(r).length === 0" class="hint sub">暂无目标分布</p>
        </div>
      </template>
    </div>
    <p v-else-if="tab === 'out'" class="hint block">暂无伤害数据</p>

    <!-- 承伤统计：点击行展开 → 此目标受到的各攻击者伤害分布 -->
    <div v-else-if="vrows.length" class="rank">
      <div class="r-head v-head">
        <span>目标</span><span>承伤</span><span>攻击者</span><span>占比</span>
      </div>
      <template v-for="v in vrows" :key="v.vi">
        <div
          class="r-row"
          :class="{ exp: expVic === v.vi }"
          :title="`目标 ${vicLabel(v)} · vi=${v.vi} · 受击 ${fmtNum(v.cnt)} 次 · 占全场 ${(v.pct * 100).toFixed(1)}% · ${expVic === v.vi ? '点击收起' : '点击展开'}`"
          @click="toggleVic(v)"
        >
          <i class="fill" :style="{ width: vicBarW(v) }"></i>
          <span class="name">{{ vicLabel(v) }}</span>
          <span class="num dmg">{{ fmtNum(v.dmg) }}</span>
          <span class="num dps">{{ v.atk }}</span>
          <span class="num pct">{{ (v.pct * 100).toFixed(1) }}%</span>
        </div>
        <div v-if="expVic === v.vi" class="sub">
          <div
            v-for="src in vicDist(v)"
            :key="src.ai + '#' + src.ag"
            class="sub-row"
            :title="vicSubTitle(v, src)"
          >
            <i
              class="fill"
              :style="{ width: subBarW(src.dmg, Math.max(...vicDist(v).map((x) => x.dmg))) }"
            ></i>
            <span class="sub-name">{{ rowName(src.ai, src.ag) }}</span>
            <span class="num dmg">{{ fmtNum(src.dmg) }}</span>
            <span class="num pct">{{ ((src.dmg / v.dmg) * 100).toFixed(1) }}%</span>
          </div>
          <p v-if="vicDist(v).length === 0" class="hint sub">暂无攻击者分布</p>
        </div>
      </template>
    </div>
    <p v-else class="hint block">暂无承伤数据</p>

    <!-- 右键菜单：规则编辑 / 历史战斗数据导入导出 / 退出
         （条目点击不 stop 冒泡，交给 document click 统一收起菜单） -->
    <div
      v-if="ctx"
      ref="ctxEl"
      class="ctxmenu"
      :style="{ left: ctx.x + 'px', top: ctx.y + 'px' }"
      @contextmenu.prevent.stop
    >
      <button class="ctx-item" title="在系统浏览器打开规则编辑器" @click="openEditor">规则编辑器</button>
      <div class="ctx-sep"></div>
      <button class="ctx-item" title="弹出文件选择框选择 .ashm 导入（重复导入自动拒绝）" @click="importData">导入战斗数据</button>
      <button class="ctx-item" title="SHM 原样 dump + zstd 压缩（.ashm），保存到 data/exports" @click="exportData">导出战斗数据</button>
      <div class="ctx-sep"></div>
      <button class="ctx-item" @click="doExit">退出</button>
    </div>
  </div>
</template>

<style scoped>
.bar {
  display: flex; align-items: center; gap: 8px;
  padding: 2px 4px 6px; font-size: 12px; color: var(--dim);
}
/* 历史回放过渡：不定长进度条（动画纯装饰，完成即消失） */
.replay {
  display: flex; align-items: center; gap: 6px;
  padding: 0 6px 6px; font-size: 11px; color: var(--dim);
}
.replay-ind {
  flex: 1; height: 3px; border-radius: 2px; overflow: hidden;
  background: color-mix(in srgb, var(--dim) 25%, transparent);
  position: relative;
}
.replay-ind::after {
  content: ""; position: absolute; top: 0; left: -30%;
  width: 30%; height: 100%; border-radius: 2px;
  background: var(--dim);
  animation: replay-slide 1.2s ease-in-out infinite;
}
@keyframes replay-slide {
  to { left: 100%; }
}
@media (prefers-reduced-motion: reduce) {
  .replay-ind::after { animation: none; left: 35%; }
}
/* 右键菜单 */
.ctxmenu {
  position: fixed; z-index: 1000;
  min-width: 88px; padding: 3px;
  background: var(--bg, #ffffff); border: 1px solid var(--line, #e2e2e2);
  border-radius: 5px; box-shadow: 0 4px 14px rgb(0 0 0 / 18%);
}
.ctx-item {
  display: block; width: 100%; padding: 4px 10px;
  border: 0; border-radius: 3px; background: none;
  font: inherit; font-size: 12px; color: var(--fg, #333);
  text-align: left; cursor: pointer;
}
.ctx-item:hover:not(:disabled) { background: color-mix(in srgb, var(--dim, #888) 18%, transparent); }
.ctx-item:disabled { color: color-mix(in srgb, var(--dim, #888) 55%, transparent); cursor: default; }
.ctx-sep {
  height: 1px; margin: 3px 6px;
  background: var(--line, #e2e2e2);
}
.dot { width: 7px; height: 7px; border-radius: 50%; background: #bbb; flex: none; }
.dot.live { background: #4a9c5d; }
.dot.stalled { background: var(--warn); }
.dot.wait { background: #bbb; }
.bar b { color: var(--fg); font-weight: 600; }
.lost { color: var(--warn); }
.tools { margin-left: auto; display: flex; align-items: center; gap: 6px; flex: none; }
.review {
  color: var(--dim); font-size: 11px;
  border: 1px solid var(--line); padding: 0 6px; border-radius: 2px;
}
.sel {
  background: #fff; border: 1px solid var(--line); color: var(--dim);
  font-size: 11px; padding: 1px 2px; max-width: 150px;
  cursor: pointer; outline: none;
}
.sel:hover { color: var(--fg); border-color: #aaa; }
.reset {
  background: none; border: 1px solid var(--line); color: var(--dim);
  font-size: 12px; padding: 1px 8px; cursor: pointer;
}
.reset:hover { color: var(--fg); border-color: #aaa; }

/* 排行表 */
.rank { width: 100%; }
.r-head, .r-row {
  display: grid;
  grid-template-columns: 1fr 84px 60px 52px;
  gap: 0 8px;
  align-items: center;
  padding: 2px 6px;
}
.r-head span {
  text-align: right; color: var(--dim); font-size: 11px;
  border-bottom: 1px solid var(--line); padding-bottom: 2px;
}
.r-head span:first-child { text-align: left; }
.r-row { position: relative; overflow: hidden; }
.r-row > span { position: relative; z-index: 1; text-align: right; }
.r-row .name {
  text-align: left; min-width: 0;
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.fill {
  position: absolute; inset: 0 auto 0 0; z-index: 0;
  display: flex; overflow: hidden;
}
.fill i { display: block; height: 100%; }
.fill .s-white { background: #ffffff; box-shadow: inset 0 0 0 1px #c8c8c8; }
.fill .s-yellow { background: #f0d75e; }
.fill .s-purple { background: #b48ad6; }

/* 提示文字统一：同一字号/颜色，仅排版位置分两档（block=区块空态 / sub=展开区空态） */
.hint { color: var(--dim); font-size: 11px; }
.hint.block { text-align: center; padding: 14px 0; }
.hint.sub { padding: 0 6px 2px 26px; }

/* 行展开：▸/▾ 指示 + 分布子行 */
.r-row { cursor: pointer; }
.r-row.exp .name { font-weight: 600; }
.r-row .name::before {
  content: '▸'; color: #bbb; font-size: 10px; margin-right: 4px;
  display: inline-block; width: 8px;
}
.r-row.exp .name::before { content: '▾'; }
.sub { padding: 0 0 4px 0; }
.sub-row {
  position: relative; overflow: hidden;
  display: grid; grid-template-columns: 1fr 84px 52px; gap: 0 8px;
  align-items: center; padding: 1px 6px 1px 26px; font-size: 12px;
}
.sub-row > span { position: relative; z-index: 1; text-align: right; }
.sub-row .sub-name {
  text-align: left; color: var(--dim);
  overflow: hidden; text-overflow: ellipsis; white-space: nowrap;
}
.sub-row .fill {
  position: absolute; inset: 0 auto 0 0; z-index: 0;
  background: #ececec;
}

</style>
