// aggregate.go —— 桥内全部聚合计算：实例分组、会话/实时 DPS（600 帧滑窗）、
// gf 秒桶、ty 分布、丢失对账、会话历史（最近 50 场）。
// 前端零计算：快照即渲染就绪数据。
package main

import (
	"fmt"
	"log"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	winFrames    = 600 // 实时 DPS 滑窗：600 帧 = 10s（gf 口径）
	framesPerSec = 60
	histCap      = 50 // 会话历史上限
	matrixCols   = 8  // 伤害矩阵保留的目标列数（其余并入 Other）
)

// instKey = ai<<16 | ag（均为 u16 语义，实例唯一）。
type instKey uint32

type winEv struct {
	gf uint32
	d  uint32
}

type instRow struct {
	ai, ag          uint32
	dmg, cnt        uint64
	gfFirst, gfLast uint32
	tyDmg           map[uint32]uint64 // 分类型伤害值（ACT 式行内分段柱）
	win             []winEv           // 滑窗内事件，按 gf 递增，随时剪枝
}

// vicRow 受击方身份行（vi = DLL 给出的身份 id，同 vi = 同一只怪）。
// vgs 记录观察到的独立实体句柄集合（len(vgs) = 实体计数）。
// 旧 DLL 无 vi 时回退：以 vg 为身份键（每个实体自成一行）。
type vicRow struct {
	id       uint32              // 身份 id（vi；回退 = vg）
	vgs      map[uint32]struct{} // 观察到的实体句柄集合
	dmg, cnt uint64
	gfFirst  uint32
	gfLast   uint32
	killers  map[instKey]uint64 // 攻击者 → 对该目标造成的伤害（伤害矩阵来源）
}

// session 一场战斗（epoch = writer 一次 init，gen 局内重计，不跨场合并）。
// sid = 桥本地会话流水号（重置 API 也开新 sid，显示与查找都用它）。
// 编码规则：实时/重置会话用全局 sidCounter（1..N）；导入会话用
// (batch<<32)|idx —— batch 为导入批次号（每次导入 +1），idx 为批内战斗序号。
// 两个空间永不相撞，且不同批次之间互不混用；查找复用同一 sid 键。
type session struct {
	sid        uint64
	epoch      uint32
	startedAt  time.Time
	dmg, cnt   uint64
	lost       uint64
	lostRanges int
	gfFirst    uint32
	gfLast     uint32
	hasGf      bool
	clockGf    uint32 // 战斗时钟（meta.last_gf/60 = 显示用时长；暂停停走）
	ty         map[uint32]uint64
	insts      map[instKey]*instRow
	vics       map[uint32]*vicRow // 受击方身份行（vi 索引）
	buckets    []uint64           // 每 gf 秒伤害（= 该秒 DPS）
	ver        uint64             // 快照版本号，ver 不变不推帧
	src        string             // 来源标记："import" = 导入重放的会话；空 = 真实 SHM
}

func newSession(epoch uint32) *session {
	return &session{
		epoch:     epoch,
		startedAt: time.Now(),
		ty:        map[uint32]uint64{},
		insts:     map[instKey]*instRow{},
		vics:      map[uint32]*vicRow{},
	}
}

// apply 消费一条事件（事件 gf 单调不减到达，writer 发布序）。
func (s *session) apply(e ev) {
	s.dmg += uint64(e.D)
	s.cnt++
	s.ty[e.Ty] += uint64(e.D) // ty 汇总 = 分类型伤害值（非事件数）
	if !s.hasGf {
		s.hasGf = true
		s.gfFirst = e.Gf
	}
	if e.Gf > s.gfLast {
		s.gfLast = e.Gf
	}

	r := s.insts[instKey(e.Ai<<16|e.Ag)]
	if r == nil {
		r = &instRow{ai: e.Ai, ag: e.Ag, gfFirst: e.Gf, gfLast: e.Gf,
			tyDmg: map[uint32]uint64{}}
		s.insts[instKey(e.Ai<<16|e.Ag)] = r
	}
	r.dmg += uint64(e.D)
	r.cnt++
	r.tyDmg[e.Ty] += uint64(e.D)
	if e.Gf < r.gfFirst {
		r.gfFirst = e.Gf
	}
	if e.Gf > r.gfLast {
		r.gfLast = e.Gf
	}
	r.win = append(r.win, winEv{e.Gf, e.D})

	// 剪枝：窗口起点 = gfLast - 600 帧
	winStart := int64(r.gfLast) - winFrames
	cut := 0
	for cut < len(r.win) && int64(r.win[cut].gf) < winStart {
		cut++
	}
	if cut > 0 {
		r.win = append([]winEv(nil), r.win[cut:]...) // 复制，释放旧底层数组
	}

	// gf 秒桶
	idx := int(e.Gf / framesPerSec)
	for len(s.buckets) <= idx {
		s.buckets = append(s.buckets, 0)
	}
	s.buckets[idx] += uint64(e.D)

	// 受击方聚合（vi/vg 全 0 = 旧 DLL 无目标字段，跳过目标维度）
	if e.Vi != 0 || e.Vg != 0 {
		id := e.Vi
		if id == 0 {
			id = e.Vg // 旧 DLL 回退：无 vi 时按 vg 实体区分
		}
		v := s.vics[id]
		if v == nil {
			v = &vicRow{id: id, vgs: map[uint32]struct{}{}, gfFirst: e.Gf, gfLast: e.Gf,
				killers: map[instKey]uint64{}}
			s.vics[id] = v
		}
		v.vgs[e.Vg] = struct{}{} // 同 vi 多个 vg = 同一只怪的多实体观测（一般恒同值）
		v.dmg += uint64(e.D)
		v.cnt++
		if e.Gf > v.gfLast {
			v.gfLast = e.Gf
		}
		v.killers[instKey(e.Ai<<16|e.Ag)] += uint64(e.D)
	}

	s.ver++
}

// duration 战斗时长（秒，gf 口径）。
func (s *session) duration() float64 {
	if !s.hasGf {
		return 0
	}
	return float64(s.gfLast-s.gfFirst) / framesPerSec
}

func divDps(dmg uint64, sec float64) float64 {
	if sec < 1 {
		sec = 1 // 开场不足 1s 时按 1s 计，避免夸大
	}
	return float64(dmg) / sec
}

// winDmg 滑窗内伤害合计。
func (r *instRow) winDmg(s *session) uint64 {
	winStart := int64(s.gfLast) - winFrames
	var sum uint64
	for _, w := range r.win {
		if int64(w.gf) >= winStart {
			sum += uint64(w.d)
		}
	}
	return sum
}

// ==================== 快照（渲染就绪 JSON） ====================

type snapRow struct {
	N   string            `json:"n"`
	Ai  uint32            `json:"ai"`
	Ag  uint32            `json:"ag"`
	Dmg uint64            `json:"dmg"`
	Cnt uint64            `json:"cnt"`
	Dps float64           `json:"dps"`
	Rt  float64           `json:"rt"`
	Pct float64           `json:"pct"`
	Ty  map[uint32]uint64 `json:"ty,omitempty"` // 分类型伤害值（行内分段柱）
}

type snapHdr struct {
	Dmg   uint64            `json:"dmg"`
	Cnt   uint64            `json:"cnt"`
	GfFst uint32            `json:"gfFirst"`
	GfLst uint32            `json:"gfLast"`
	Dur   float64           `json:"dur"`   // 事件跨度时长（DPS 分母口径）
	Clock float64           `json:"clock"` // 显示用战斗时钟 = last_gf/60
	Dps   float64           `json:"dps"`
	Rt    float64           `json:"rt"`
	Ty    map[uint32]uint64 `json:"ty"`
	Lost  uint64            `json:"lost"`
	LostN int               `json:"lostRanges"`
}

type snapSrc struct {
	Ai  uint32 `json:"ai"`
	Ag  uint32 `json:"ag"`
	Dmg uint64 `json:"dmg"`
}

type snapVic struct {
	Vi   uint32    `json:"vi"`             // 身份 id（vi；旧 DLL 回退 = vg）
	Name string    `json:"name,omitempty"` // 显示名（aliases.json v 前缀 > 编号）
	N    int       `json:"n"`              // 实体计数（观察到的不同 vg 数）
	Dmg  uint64    `json:"dmg"`
	Cnt  uint64    `json:"cnt"`
	Atk  int       `json:"atk"`            // 打过它的不同攻击者实例数
	Pct  float64   `json:"pct"`            // 占全场伤害比例 0..1
	Srcs []snapSrc `json:"srcs,omitempty"` // 各攻击者对该目标的伤害（降序，承伤行展开用）
}

// snapMatrix 伤害矩阵（攻击者 × 受击者）。
// Cols = 按承伤降序的前 MatrixCols 个目标；每行 Cells 与 Cols 对齐，
// 溢出目标（未进前列的）合并进 Other —— 保证行和 = 该攻击者总伤害。
type snapMatrix struct {
	Cols []snapVic       `json:"cols"` // 前 N 目标（已按承伤降序）
	Rows []snapMatrixRow `json:"rows"` // 与快照 rows 同序（按攻击者总伤降序）
}

type snapMatrixRow struct {
	Ai    uint32   `json:"ai"`
	Ag    uint32   `json:"ag"`
	Cells []uint64 `json:"cells"` // 与 cols 一一对应（0 = 未打该目标）
	Other uint64   `json:"other"` // 其余目标伤害合计
}

type snapshot struct {
	T       string      `json:"t"`   // 恒 "snap"
	Sid     uint64      `json:"sid"` // 桥本地会话流水号
	Epoch   uint32      `json:"epoch"`
	Ver     uint64      `json:"ver"`
	Started int64       `json:"started"` // 会话开始 Unix 秒
	Hdr     snapHdr     `json:"hdr"`
	Rows    []snapRow   `json:"rows"`  // 已按 dmg 降序
	Vrows   []snapVic   `json:"vrows"` // 逐目标承伤，已按 dmg 降序
	Matrix  *snapMatrix `json:"matrix,omitempty"`
	Buckets []uint64    `json:"buckets"`
}

func (s *session) snapshot(names map[string]string, an aliasNames) snapshot {
	snap := snapshot{
		T:       "snap",
		Sid:     s.sid,
		Epoch:   s.epoch,
		Ver:     s.ver,
		Started: s.startedAt.Unix(),
		Buckets: s.buckets,
	}
	dur := s.duration()
	snap.Hdr = snapHdr{
		Dmg: s.dmg, Cnt: s.cnt,
		GfFst: s.gfFirst, GfLst: s.gfLast,
		Dur: dur, Clock: float64(s.clockGf) / framesPerSec,
		Dps: divDps(s.dmg, dur),
		Ty:  s.ty, Lost: s.lost, LostN: s.lostRanges,
	}

	// 全局实时 DPS：所有实例滑窗伤害 / 窗口实际秒数
	var winSum uint64
	for _, r := range s.insts {
		winSum += r.winDmg(s)
	}
	if s.hasGf {
		winStart := s.gfLast - winFrames
		base := s.gfFirst
		if base < winStart {
			base = winStart
		}
		snap.Hdr.Rt = divDps(winSum, float64(s.gfLast-base)/framesPerSec)
	}

	rows := make([]snapRow, 0, len(s.insts))
	for _, r := range s.insts {
		row := snapRow{
			Ai:  r.ai,
			Ag:  r.ag,
			Dmg: r.dmg,
			Cnt: r.cnt,
			Dps: divDps(r.dmg, float64(r.gfLast-r.gfFirst)/framesPerSec),
			Pct: 0,
			Ty:  r.tyDmg,
		}
		if s.dmg > 0 {
			row.Pct = float64(r.dmg) / float64(s.dmg)
		}
		// 显示名解析顺序（十进制 id）：
		// ① aliases "a" 前缀（我方名，含未来新 id 补名）→ ② NameText(%04d) → ③ ai 编号
		// 合并行（Ag 归零）不挂实例后缀；普通实例行 = 名#ag
		dec := strconv.FormatUint(uint64(r.ai), 10)
		label := ""
		if nm, ok := an.Ally[dec]; ok {
			label = nm
		} else if nm, ok := names[fmt.Sprintf("%04d", r.ai)]; ok {
			label = nm
		} else {
			label = dec
		}
		if r.ag == 0 {
			row.N = label
		} else {
			row.N = fmt.Sprintf("%s#%d", label, r.ag)
		}
		if s.hasGf {
			winStart := s.gfLast - winFrames
			base := r.gfFirst
			if base < winStart {
				base = winStart
			}
			row.Rt = divDps(r.winDmg(s), float64(r.gfLast-base)/framesPerSec)
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Dmg != rows[j].Dmg {
			return rows[i].Dmg > rows[j].Dmg
		}
		return rows[i].Ag < rows[j].Ag // 稳定序：同伤按 ag
	})
	snap.Rows = rows

	// ---- 逐目标承伤（按 vi 合并，承伤降序）----
	if len(s.vics) > 0 {
		vrows := make([]snapVic, 0, len(s.vics))
		for _, v := range s.vics {
			// 各攻击者对该目标的伤害分布（降序），供承伤行展开
			srcs := make([]snapSrc, 0, len(v.killers))
			for k, d := range v.killers {
				srcs = append(srcs, snapSrc{Ai: uint32(k >> 16), Ag: uint32(k & 0xffff), Dmg: d})
			}
			sort.Slice(srcs, func(i, j int) bool { return srcs[i].Dmg > srcs[j].Dmg })
			vr := snapVic{Vi: v.id, N: len(v.vgs), Dmg: v.dmg, Cnt: v.cnt,
				Atk: len(v.killers), Srcs: srcs}
			// 敌方显示名：aliases.json "v" 前缀 > vi 编号（vi 是敌方 id 空间，不查 NameText）
			dec := strconv.FormatUint(uint64(v.id), 10)
			if nm, ok := an.Enemy[dec]; ok {
				vr.Name = nm
			} else {
				vr.Name = dec
			}
			if s.dmg > 0 {
				vr.Pct = float64(v.dmg) / float64(s.dmg)
			}
			vrows = append(vrows, vr)
		}
		sort.Slice(vrows, func(i, j int) bool {
			if vrows[i].Dmg != vrows[j].Dmg {
				return vrows[i].Dmg > vrows[j].Dmg
			}
			return vrows[i].Vi < vrows[j].Vi
		})
		snap.Vrows = vrows

		// ---- 伤害矩阵：列 = 前 matrixCols 个目标，行 = 攻击者（与 rows 同序）----
		nCols := len(vrows)
		if nCols > matrixCols {
			nCols = matrixCols
		}
		cols := vrows[:nCols]
		colIdx := make(map[uint32]int, nCols)
		for i, c := range cols {
			colIdx[c.Vi] = i
		}
		m := &snapMatrix{Cols: cols, Rows: make([]snapMatrixRow, 0, len(rows))}
		for _, r := range rows {
			mr := snapMatrixRow{Ai: r.Ai, Ag: r.Ag, Cells: make([]uint64, nCols)}
			key := instKey(r.Ai<<16 | r.Ag)
			for _, v := range s.vics {
				d := v.killers[key]
				if d == 0 {
					continue
				}
				if ci, ok := colIdx[v.id]; ok {
					mr.Cells[ci] = d
				} else {
					mr.Other += d
				}
			}
			m.Rows = append(m.Rows, mr)
		}
		snap.Matrix = m
	}
	return snap
}

// ==================== 聚合器（含会话历史 + 名字映射） ====================

type agg struct {
	mu         sync.Mutex
	cur        *session
	hist       []*session // 含 cur，按时间序
	sidCounter uint64
	impBatch   uint64 // 最近导入批次号（每次导入 +1，导入会话 sid 高 32 位）
	names      map[string]string

	namesPath string
	namesMod  time.Time

	aliasPath string
	aliasMod  time.Time
	alias     aliasNames // 命名配置（a=覆盖我方 / 未标记=兜底 / v=敌方）
	uf        unionFind  // 我方伤害归属并查集（merge 组，按 ai）
	ufV       unionFind  // 敌方受击归并并查集（merge 组，按 vi）
	subMod    time.Time  // 订阅合并产物 mtime（变更触发叠加重载）
}

func newAgg(namesPath, aliasPath string) *agg {
	a := &agg{
		namesPath: namesPath,
		aliasPath: aliasPath,
		names:     map[string]string{},
		alias:     emptyAliasNames(),
		uf:        unionFind{parent: map[uint32]uint32{}, reps: map[uint32]bool{}},
		ufV:       unionFind{parent: map[uint32]uint32{}, reps: map[uint32]bool{}},
	}
	a.reloadNames()
	a.reloadAliases()
	return a
}

// reloadNames 热载 data/unit_names.json（mtime 变化才重读）。
func (a *agg) reloadNames() {
	st, err := statFile(a.namesPath)
	if err != nil {
		return // 文件不存在：全部走回退名
	}
	if !st.ModTime().After(a.namesMod) {
		return
	}
	m, err := readNamesFile(a.namesPath)
	if err != nil {
		return
	}
	a.namesMod = st.ModTime()
	a.names = m
}

// reloadAliases 热载 data/aliases.json（自定义改名 + 伤害归属并查集）。
// 叠加订阅层 data/subcache/sub_merged.json（subscribe.go 维护）：
// 命名本地优先、订阅补漏；合并规则本地声明在前、订阅在后。
// 注意：合并/改名只影响新消费的事件，已聚合历史不回溯重算。
func (a *agg) reloadAliases() {
	st, err := statFile(a.aliasPath)
	if err != nil {
		return
	}
	subPath := a.subMergedPath()
	subSt, subErr := statFile(subPath)
	subChanged := subErr == nil && subSt.ModTime().After(a.subMod)
	if subErr != nil && !a.subMod.IsZero() {
		// 合并产物被移除（停用/清空订阅）→ 触发一次重载回到纯本地
		subChanged = true
	}
	if !st.ModTime().After(a.aliasMod) && !subChanged {
		return
	}
	cfg, an, nNames := loadAlias(a.aliasPath)
	a.aliasMod = st.ModTime()
	if subChanged {
		if subErr == nil {
			a.subMod = subSt.ModTime()
		} else {
			a.subMod = time.Time{}
		}
	}

	// 订阅层（文件不存在 = 无订阅/停用，纯本地）
	nSub := 0
	subCfg, readErr := readAliasFile(subPath)
	if readErr == nil {
		nSub = fillSubNames(&an, subCfg)
		a.uf, a.ufV = splitUnionFindCombined(cfg.Merge, subCfg.Merge)
	} else {
		a.uf, a.ufV = splitUnionFind(cfg.Merge)
	}
	a.alias = an
	nMerge := len(a.uf.reps) + len(a.ufV.reps)
	if nNames > 0 || nMerge > 0 || nSub > 0 {
		log.Printf("[*] 别名配置已载入：我方名 %d / 敌方名 %d / 合并 %d 组（我方 %d + 敌方 %d） / 订阅补入 %d 条",
			len(an.Ally), len(an.Enemy), nMerge, len(a.uf.reps), len(a.ufV.reps), nSub)
	}
}

// subMergedPath 订阅层合并产物路径（data/subcache/sub_merged.json）。
func (a *agg) subMergedPath() string {
	return filepath.Join(filepath.Dir(a.aliasPath), "subcache", "sub_merged.json")
}

func (a *agg) currentNames() map[string]string { return a.names }

// currentAlias 当前生效的别名配置（含并查集解析后的组代表），供 /api/alias 核对。
func (a *agg) currentAlias() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	mk := func(uf unionFind) map[string]uint32 {
		m := map[string]uint32{}
		for id := range uf.parent {
			m[strconv.FormatUint(uint64(id), 10)] = uf.find(id)
		}
		return m
	}
	return map[string]any{
		"names": map[string]map[string]string{
			"ally":  a.alias.Ally,  // "a" 前缀
			"enemy": a.alias.Enemy, // "v" 前缀
		},
		"merge": map[string]map[string]uint32{
			"ally":  mk(a.uf),  // 我方合并组（key=被吸收源 → val=组代表）
			"enemy": mk(a.ufV), // 敌方合并组（按 vi）
		},
	}
}

// newSess 开新会话（sid 单调递增，归档入历史）。
func (a *agg) newSess(epoch uint32) *session {
	a.sidCounter++
	s := newSession(epoch)
	s.sid = a.sidCounter
	a.hist = append(a.hist, s)
	if len(a.hist) > histCap {
		a.hist = a.hist[len(a.hist)-histCap:]
	}
	return s
}

// reset 清空所有 DPS 统计（含历史），立即开新空会话。不动 SHM 与 reader 游标。
func (a *agg) reset(epoch uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hist = nil
	a.cur = a.newSess(epoch)
}

// ---- Sink 实现（reader 回调）----

func (a *agg) OnSessionStart(epoch uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.cur = a.newSess(epoch)
}

// OnClock 战斗时钟推进（reader 每 poll 一次，gf 变化才 ver++）。
func (a *agg) OnClock(gf uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil || gf == a.cur.clockGf {
		return
	}
	a.cur.clockGf = gf
	a.cur.ver++ // 时钟变化也要推帧（无伤害事件期间时钟照走，暂停停走自然冻结）
}

func (a *agg) OnEvent(e ev) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil { // 兜底（正常时 reader 接入即回调 OnSessionStart）
		a.cur = a.newSess(0)
	}
	a.applyEvent(a.cur, e)
}

// applyEvent 合并规则解析（并查集归并）+ 事件入指定会话。实时与导入重放共用。
func (a *agg) applyEvent(s *session, e ev) {
	if a.uf.grouped(e.Ai) {
		e.Ai = a.uf.find(e.Ai)
		e.Ag = 0
	}
	if a.ufV.grouped(e.Vi) {
		e.Vi = a.ufV.find(e.Vi)
	}
	s.apply(e)
}

// newImportedSession 开一个导入来源的会话（src=import，前端场次下拉「导入」分组）。
// sid 按 (batch<<32)|idx 编码，与实时场次、其他批次互不混用。
func (a *agg) newImportedSession(epoch uint32, batch uint64, idx uint32) *session {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.newSess(epoch)
	s.src = "import"
	s.sid = uint64(batch)<<32 | uint64(idx)
	return s
}

// nextImportBatch 分配下一个导入批次号（每次实际导入调用一次）。
func (a *agg) nextImportBatch() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.impBatch++
	return a.impBatch
}

// applyImported 导入重放专用：事件入指定导入会话，战斗时钟取事件 gf（导入侧
// 没有 meta.last_gf 实时源，以最后事件 gf 近似战斗时长显示）。
func (a *agg) applyImported(s *session, e ev) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.applyEvent(s, e)
	if e.Gf > s.clockGf {
		s.clockGf = e.Gf
	}
	s.ver++
}

func (a *agg) applyLost(s *session, n uint64, ranges int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s.lost += n
	s.lostRanges += ranges
	s.ver++
}

func (a *agg) OnLost(n uint64, ranges int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur != nil {
		a.cur.lost += n
		a.cur.lostRanges += ranges
		a.cur.ver++
	}
}

// snapshotOf 取指定 sid 会话快照（0 = 当前会话）。
func (a *agg) snapshotOf(sid uint64) (snapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reloadNames()
	a.reloadAliases()
	if a.cur == nil {
		return snapshot{}, false
	}
	if sid == 0 || sid == a.cur.sid {
		return a.cur.snapshot(a.names, a.alias), true
	}
	for i := len(a.hist) - 1; i >= 0; i-- {
		if a.hist[i].sid == sid {
			return a.hist[i].snapshot(a.names, a.alias), true
		}
	}
	return snapshot{}, false
}

// sessionMetas /api/state 的会话列表。
type sessionMeta struct {
	Sid     uint64  `json:"sid"`
	Epoch   uint32  `json:"epoch"`
	Dmg     uint64  `json:"dmg"`
	Cnt     uint64  `json:"cnt"`
	Dur     float64 `json:"dur"`
	Clock   float64 `json:"clock"`
	Dps     float64 `json:"dps"`
	Started int64   `json:"started"`
	Src     string  `json:"src,omitempty"`  // "import" = 导入重放会话（场次下拉「导入」分组）
	Batch   uint64  `json:"batch,omitempty"` // 导入批次号（仅导入会话）
	Idx     uint32  `json:"idx,omitempty"`   // 批内战斗序号（仅导入会话）
}

func (a *agg) metas() []sessionMeta {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]sessionMeta, 0, len(a.hist))
	for _, s := range a.hist {
		m := sessionMeta{
			Sid: s.sid, Epoch: s.epoch, Dmg: s.dmg, Cnt: s.cnt,
			Dur: s.duration(), Clock: float64(s.clockGf) / framesPerSec,
			Dps:     divDps(s.dmg, s.duration()),
			Started: s.startedAt.Unix(),
			Src:     s.src,
		}
		if s.src == "import" { // 反解 sid = (batch<<32)|idx
			m.Batch = s.sid >> 32
			m.Idx = uint32(s.sid)
		}
		out = append(out, m)
	}
	return out
}

func (a *agg) currentEpoch() uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return 0
	}
	return a.cur.epoch
}
