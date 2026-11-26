// alias.go —— 用户配置层：单位命名 + 伤害归属并查集。
// 配置文件 data/aliases.json（mtime 变化热载，改配置后新事件立即生效；
// 已聚合的历史不回溯重算——需要时按重置按钮重开）。
//
// 格式：
//
//	{
//	  "names": {
//	    "a590":  "我方改名",   // "a" 前缀 = 我方单位：显示名优先于 NameText；
//	                           //   NameText 查不到的未来新 id 也用 "a" 补名
//	    "v8001": "敌人名"      // "v" 前缀 = 敌方单位名（按 vi 生效）
//	  },
//	  "merge": {
//	    "a590":  ["1469", "1470"],  // 我方合并：目标 ← 源（同 names 的 a/v 前缀约定）
//	    "v4755": "4756",            // 敌方合并：按 vi 归并受击方，规则与我方完全一致
//	    "590":   ["1469"]           // 无前缀 = 我方（向后兼容）
//	  }
//	}
//
// 合并方向固定：key = 吸收方（目标），value = 被吸收方（源）；目标自身实例一并合并。
// 源列表为空 [] = 仅同 id 自合并；兼容简写 "590": "1469" = 单源列表。
// 冲突裁决：相同源多条规则 → 先声明者优先（后者丢弃并警告）；
// 循环吸收 → 丢弃构成环的那条边并报错日志，不中断运行。
// 并查集支持链式：{"300": ["100"], "200": ["300"]} → 100/300 都归到 200（带环保护）。
// 阵营说明：事件流恒为「我方对敌输出」（DLL 侧已过滤），ai 恒为我方 CardID；
// 敌方侧按 vi（身份 id）聚合受击，v 前缀合并即跨 vi 归并受击行。
// 两侧 id 空间独立：我方合并/命名按 ai，敌方合并/命名按 vi，互不串扰。
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"strconv"
)

type aliasConfig struct {
	Names json.RawMessage `json:"names"` // key 必须带 a/v 前缀：a590=我方名 / v8001=敌方名
	Merge json.RawMessage `json:"merge"` // a/v 前缀（或无前缀=我方）目标 id → 源数组，声明顺序 = 优先级
}

// aliasNames 解析后的命名配置（按阵营分流，key 为规范化十进制 id）。
type aliasNames struct {
	Ally  map[string]string // "a" 前缀：我方单位名（优先于 NameText，含未来新 id 补名）
	Enemy map[string]string // "v" 前缀：敌方单位名（按 vi 生效）
}

// parseAliasNames 解析 names 段：key 必须以 a/v 开头，值为显示名字符串。
// 返回 (解析结果, 有效条目数)。
func parseAliasNames(raw json.RawMessage) (aliasNames, int) {
	an := aliasNames{Ally: map[string]string{}, Enemy: map[string]string{}}
	if len(raw) == 0 {
		return an, 0
	}
	var rawMap map[string]string
	if err := json.Unmarshal(raw, &rawMap); err != nil {
		logWarn("names 段解析失败（忽略）: %v", err)
		return an, 0
	}
	n := 0
	for key, name := range rawMap {
		if len(key) < 2 || name == "" {
			logWarn("names[%s] 格式非法（忽略，key 须为 a/v + 数字，值须非空）", key)
			continue
		}
		id, err := strconv.ParseUint(key[1:], 10, 32)
		if err != nil {
			logWarn("names[%s] id 非法（忽略）", key)
			continue
		}
		ids := strconv.FormatUint(id, 10) // 规范化（去前导零）
		switch key[0] {
		case 'a':
			an.Ally[ids] = name
		case 'v':
			an.Enemy[ids] = name
		default:
			logWarn("names[%s] 前缀非法（忽略，仅支持 a/v）", key)
			continue
		}
		n++
	}
	return an, n
}

// unionFind 伤害归属并查集（合并组代表 = 链终点 id）。
type unionFind struct {
	parent map[uint32]uint32
	reps   map[uint32]bool // 合并组代表集合（含自合并 id 与链终点）
}

// parseMergeTarget 解析 merge 单条 value：源 id 数组或单源字符串简写。
func parseMergeTarget(raw json.RawMessage) ([]string, bool) {
	var list []string
	if err := json.Unmarshal(raw, &list); err == nil {
		return list, true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return []string{s}, true
	}
	return nil, false
}

// mergeRule 保序的单条 merge 规则（Go map 会打乱 key 顺序，必须走 Decoder）。
type mergeRule struct {
	Key string
	Val json.RawMessage
}

// parseMergeOrdered 按 JSON 文档顺序解析 merge 段（声明顺序 = 优先级）。
func parseMergeOrdered(raw json.RawMessage) []mergeRule {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		logWarn("merge 段须为对象（忽略）")
		return nil
	}
	var rules []mergeRule
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			break
		}
		key, _ := kt.(string)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			break
		}
		rules = append(rules, mergeRule{Key: key, Val: v})
	}
	return rules
}

// reaches 从 t 沿 parent 链上溯，判断 s 是否可达（环检测）。
func (u unionFind) reaches(t, s uint32) bool {
	cur := t
	for i := 0; i < 64; i++ {
		if cur == s {
			return true
		}
		p, ok := u.parent[cur]
		if !ok {
			return false
		}
		cur = p
	}
	return false // 异常长链视为不可达（构建期已防环，理论上到不了）
}

// parseMergeKey 解析 merge 条目 key："a590" → (590, 我方)，"v4755" → (4755, 敌方)，
// 无前缀纯数字 → 我方（向后兼容）；id 须为合法非零 uint32。
func parseMergeKey(key string) (id uint32, enemy bool, ok bool) {
	enemy = false
	s := key
	if len(s) >= 2 && (s[0] == 'a' || s[0] == 'v') {
		enemy = s[0] == 'v'
		s = s[1:]
	}
	u, err := strconv.ParseUint(s, 10, 32)
	if err != nil || u == 0 {
		return 0, false, false
	}
	return uint32(u), enemy, true
}

// parseMergeSrc 解析 merge 源 id：纯数字，或与目标同阵营的前缀（错阵营前缀视为非法）。
func parseMergeSrc(s string, enemy bool) (uint32, bool) {
	if len(s) >= 2 && (s[0] == 'a' || s[0] == 'v') {
		if (s[0] == 'v') != enemy {
			return 0, false // 阵营不匹配（如我方目标的源写了 v 前缀）
		}
		s = s[1:]
	}
	u, err := strconv.ParseUint(s, 10, 32)
	if err != nil || u == 0 {
		return 0, false
	}
	return uint32(u), true
}

// splitMergeSides 按目标 key 前缀把 merge 规则分流到我方/敌方两侧（保声明顺序）。
func splitMergeSides(raw json.RawMessage) (ally, enemy []mergeRule) {
	for _, rule := range parseMergeOrdered(raw) {
		id, isV, ok := parseMergeKey(rule.Key)
		if !ok {
			logWarn("merge 目标 %q 非法（忽略，须为 a/v + 数字或纯数字）", rule.Key)
			continue
		}
		r := mergeRule{Key: strconv.FormatUint(uint64(id), 10), Val: rule.Val}
		if isV {
			enemy = append(enemy, r)
		} else {
			ally = append(ally, r)
		}
	}
	return ally, enemy
}

// newUnionFindFromRules 由分流后的保序规则构建并查集（key 已为规范十进制，enemy=敌方侧）。
func newUnionFindFromRules(rules []mergeRule, enemy bool) unionFind {
	u := unionFind{parent: map[uint32]uint32{}, reps: map[uint32]bool{}}
	tgts := map[uint32]bool{}
	for _, rule := range rules {
		tid, _ := strconv.ParseUint(rule.Key, 10, 32)
		tgts[uint32(tid)] = true
		srcs, ok := parseMergeTarget(rule.Val)
		if !ok {
			logWarn("merge[%s] 值须为源 id 数组或单源字符串（忽略该条）", rule.Key)
			continue
		}
		for _, s := range srcs {
			sid, ok := parseMergeSrc(s, enemy)
			if !ok {
				logWarn("merge[%s] 源 %q 非法（忽略）", rule.Key, s)
				continue
			}
			u.addEdge(sid, uint32(tid), rule.Key)
		}
	}
	for id := range u.parent {
		u.reps[u.find(id)] = true
	}
	for id := range tgts { // 目标自身实例也并入合一行（空列表目标 = 仅同 id 自合并）
		u.reps[u.find(id)] = true
	}
	return u
}

// splitUnionFind 由 merge 配置构建两侧独立并查集（规则完全一致，id 空间互不串扰）。
func splitUnionFind(rawMerge json.RawMessage) (unionFind, unionFind) {
	ally, enemy := splitMergeSides(rawMerge)
	return newUnionFindFromRules(ally, false), newUnionFindFromRules(enemy, true)
}

// splitUnionFindCombined 订阅分层合并：规则列表 = 本地声明在前 + 订阅在后，
// 「先声明者优先」的冲突裁决天然让本地规则压过订阅同名源；
// 订阅只补本地未覆盖的源 id。
func splitUnionFindCombined(local, sub json.RawMessage) (unionFind, unionFind) {
	ally, enemy := splitMergeSides(local)
	subAlly, subEnemy := splitMergeSides(sub)
	return newUnionFindFromRules(append(ally, subAlly...), false),
		newUnionFindFromRules(append(enemy, subEnemy...), true)
}

// fillSubNames 订阅命名补漏：只填本地未定义的 key（本地同名键永远赢）。
// 返回补入条数。
func fillSubNames(base *aliasNames, subCfg aliasConfig) int {
	subAn, _ := parseAliasNames(subCfg.Names)
	n := 0
	for k, v := range subAn.Ally {
		if _, ok := base.Ally[k]; !ok {
			base.Ally[k] = v
			n++
		}
	}
	for k, v := range subAn.Enemy {
		if _, ok := base.Enemy[k]; !ok {
			base.Enemy[k] = v
			n++
		}
	}
	return n
}

// addEdge 添加一条 源→目标 边；冲突时先声明者优先，环边丢弃。
func (u unionFind) addEdge(s, t uint32, ruleKey string) {
	if s == t {
		return // 自边无意义（目标自身实例本就归入合一行）
	}
	if _, dup := u.parent[s]; dup {
		logWarn("merge[%s] 源 %d 已被先前规则合并，本条忽略（声明顺序优先）", ruleKey, s)
		return
	}
	if u.reaches(t, s) {
		logWarn("merge[%s] 会形成循环吸收（%d ⇄ %d 链），丢弃该边", ruleKey, s, t)
		return
	}
	u.parent[s] = t
}

// find 返回 x 的组代表 id（链式解析，64 步上限防环）。
func (u unionFind) find(x uint32) uint32 {
	r := x
	for i := 0; i < 64; i++ {
		p, ok := u.parent[r]
		if !ok || p == r {
			break
		}
		r = p
	}
	return r
}

// grouped x 是否属于某个合并组（x 是合并源、合并目标或自合并 id）。
func (u unionFind) grouped(x uint32) bool {
	return u.reps[u.find(x)]
}

// readAliasFile 读并查集/改名配置；文件不存在 → 空配置（静默，属正常）。
func readAliasFile(path string) (aliasConfig, error) {
	cfg := aliasConfig{}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Merge == nil {
		cfg.Merge = json.RawMessage(`{}`)
	}
	return cfg, nil
}

// loadAlias 读入配置并返回（供 agg 热载）。
func loadAlias(path string) (aliasConfig, aliasNames, int) {
	cfg, err := readAliasFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			logWarn("别名配置解析失败（沿用旧配置）: %v", err)
		}
		return cfg, emptyAliasNames(), 0
	}
	an, n := parseAliasNames(cfg.Names)
	return cfg, an, n
}

func emptyAliasNames() aliasNames {
	return aliasNames{Ally: map[string]string{}, Enemy: map[string]string{}}
}
