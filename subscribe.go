// subscribe.go —— 规则订阅层：从远程 URL 拉取社区维护的别名规则。
//
// 分层模型：合并结果 = 订阅层（多 URL 依序）← 覆盖 ← 本地 aliases.json（最高优先级）。
// 订阅只做「新增/补漏」，本地同名键永远赢；合并规则按「本地声明在前、订阅在后」
// 拼接，天然复用并查集「先声明者优先」的冲突裁决。
//
// 配置 data/subscriptions.json（mtime 变化即生效）：
//
//	{
//	  "enabled": true,
//	  "intervalMin": 30,          // 自动刷新间隔（分钟）
//	  "urls": ["https://cdn.jsdelivr.net/gh/<user>/<repo>@main/aliases.json"]
//	}
//
// 拉取策略：ETag/If-None-Match 条件请求，304 短路；单文件上限 1MB；
// schema 校验（names/merge 须为对象）+ 净化（去控制字符、单名 ≤64 rune）。
// 订阅层合并产物落盘 data/subcache/sub_merged.json（agg 重载时叠加），
// 状态落盘 data/subcache/meta.json（API /api/subs 展示）。
// 任一环节失败 → 沿用上次成功缓存，不影响主功能。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	subMaxBytes    = 1 << 20 // 单个订阅文件上限 1MB
	subNameMaxRune = 64      // 订阅提供的单个名称上限
)

// subConfig 订阅配置（data/subscriptions.json）。
type subConfig struct {
	Enabled    bool     `json:"enabled"`
	IntervalMin int     `json:"intervalMin"`
	Urls       []string `json:"urls"`
}

// subURLStatus 单条订阅的最新拉取状态。
type subURLStatus struct {
	URL     string `json:"url"`
	Status  string `json:"status"` // ok / notmodified / error / pending / removed
	Err     string `json:"err,omitempty"`
	Entries int    `json:"entries"` // names 条数（merge 条数另计）
	Merges  int    `json:"merges"`
	LastOK  int64  `json:"lastOk"` // 最近一次成功生效的 Unix 秒（含 304）
}

// subStatus 整体状态快照（/api/subs 响应）。
type subStatus struct {
	Enabled     bool           `json:"enabled"`
	IntervalMin int            `json:"intervalMin"`
	Urls        []subURLStatus `json:"urls"`
	LastRefresh int64          `json:"lastRefresh"`
	SubNames    int            `json:"subNames"` // 合并产物中 names 条数
	SubMerges   int            `json:"subMerges"`
}

// subMeta 持久化的拉取元数据（ETag + 状态）。
type subMeta struct {
	ETags       map[string]string `json:"etags"`       // URL → ETag
	LastOK      map[string]int64  `json:"lastOk"`      // URL → 最近成功 Unix 秒
	Statuses    map[string]string `json:"statuses"`    // URL → ok/notmodified/error
	Errs        map[string]string `json:"errs"`        // URL → 最近错误信息
	Entries     map[string]int    `json:"entries"`     // URL → names 条数
	Merges      map[string]int    `json:"merges"`      // URL → merge 条数
	LastRefresh int64             `json:"lastRefresh"` // 最近一轮刷新
}

// subManager 订阅管理器（单 goroutine 持有；refresh 加锁防重入）。
type subManager struct {
	mu       sync.Mutex
	cfgPath  string
	cacheDir string
	client   *http.Client
	cfgMod   time.Time
	cfg      subConfig
	meta     subMeta
	onChange func() // 合并产物更新后的回调（触发 agg.reloadAliases）
}

func newSubManager(cfgPath, cacheDir string, onChange func()) *subManager {
	_ = os.MkdirAll(cacheDir, 0o755) // 缓存目录缺失会导致合并产物写盘失败
	m := &subManager{
		cfgPath:  cfgPath,
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 15 * time.Second},
		meta: subMeta{
			ETags: map[string]string{}, LastOK: map[string]int64{},
			Statuses: map[string]string{}, Errs: map[string]string{},
			Entries: map[string]int{}, Merges: map[string]int{},
		},
		onChange: onChange,
	}
	m.loadConfig()
	m.loadMeta()
	return m
}

// loadConfig 读配置（mtime 变化才重读）。文件不存在/解析失败 = 停用。
func (m *subManager) loadConfig() {
	st, err := os.Stat(m.cfgPath)
	if err != nil {
		m.cfg = subConfig{}
		return
	}
	if st.ModTime().Equal(m.cfgMod) {
		return
	}
	m.cfgMod = st.ModTime()
	m.cfg = subConfig{IntervalMin: 30}
	b, err := os.ReadFile(m.cfgPath)
	if err == nil {
		if err := json.Unmarshal(b, &m.cfg); err != nil {
			log.Printf("[!] 订阅配置解析失败（停用订阅）: %v", err)
			m.cfg = subConfig{}
		}
	}
	// 规范化
	if m.cfg.IntervalMin < 1 {
		m.cfg.IntervalMin = 30
	}
	urls := m.cfg.Urls[:0]
	for _, u := range m.cfg.Urls {
		u = strings.TrimSpace(u)
		pu, err := url.Parse(u)
		if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			log.Printf("[!] 订阅 URL 非法（忽略）: %q", u)
			continue
		}
		urls = append(urls, u)
	}
	m.cfg.Urls = urls
}

func (m *subManager) metaPath() string  { return filepath.Join(m.cacheDir, "meta.json") }
func (m *subManager) mergedPath() string { return filepath.Join(m.cacheDir, "sub_merged.json") }

func (m *subManager) loadMeta() {
	b, err := os.ReadFile(m.metaPath())
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &m.meta)
	if m.meta.ETags == nil {
		m.meta = subMeta{
			ETags: map[string]string{}, LastOK: map[string]int64{},
			Statuses: map[string]string{}, Errs: map[string]string{},
			Entries: map[string]int{}, Merges: map[string]int{},
		}
	}
}

func (m *subManager) saveMeta() {
	b, _ := json.MarshalIndent(m.meta, "", "  ")
	_ = os.WriteFile(m.metaPath(), b, 0o644)
}

// status 当前状态快照（API 用）。
func (m *subManager) status() subStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := subStatus{
		Enabled: m.cfg.Enabled, IntervalMin: m.cfg.IntervalMin,
		LastRefresh: m.meta.LastRefresh,
	}
	var sn, sm int
	if b, err := os.ReadFile(m.mergedPath()); err == nil {
		var merged struct {
			Names map[string]string     `json:"names"`
			Merge map[string]json.RawMessage `json:"merge"`
		}
		if json.Unmarshal(b, &merged) == nil {
			sn, sm = len(merged.Names), len(merged.Merge)
		}
	}
	st.SubNames, st.SubMerges = sn, sm
	for _, u := range m.cfg.Urls {
		s := subURLStatus{
			URL: u, Status: m.meta.Statuses[u], Err: m.meta.Errs[u],
			Entries: m.meta.Entries[u], Merges: m.meta.Merges[u], LastOK: m.meta.LastOK[u],
		}
		if s.Status == "" {
			s.Status = "pending"
		}
		st.Urls = append(st.Urls, s)
	}
	return st
}

// refreshNow 外部（API/循环）触发的刷新入口。
func (m *subManager) refreshNow() {
	if err := m.refresh(); err != nil {
		log.Printf("[!] 订阅刷新失败: %v", err)
	}
}

// refresh 一轮完整刷新：配置 → 逐 URL 条件拉取 → 合并落盘 → 回调。
// 返回 error 仅表示整体流程异常（单 URL 失败记入状态不中断）。
func (m *subManager) refresh() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.loadConfig()

	if !m.cfg.Enabled || len(m.cfg.Urls) == 0 {
		// 停用/清空 → 撤掉合并产物，agg 下次重载自动回到纯本地
		if _, err := os.Stat(m.mergedPath()); err == nil {
			_ = os.Remove(m.mergedPath())
			log.Printf("[*] 订阅已停用，移除合并产物（%s）", m.mergedPath())
			m.saveMeta()
			if m.onChange != nil {
				m.onChange()
			}
		}
		return nil
	}

	changed := false
	for _, u := range m.cfg.Urls {
		if st := m.fetchOne(u); st {
			changed = true
		}
	}

	// 合并订阅层（URL 顺序 = 优先级，先者优先）并落盘
	if err := m.writeMerged(m.cfg.Urls); err != nil {
		return err
	}
	m.meta.LastRefresh = time.Now().Unix()
	m.saveMeta()
	if changed && m.onChange != nil {
		m.onChange()
	}
	return nil
}

// fetchOne 拉取单个订阅（条件请求 + 上限 + 校验净化）。
// 返回是否产生了需要重新合并的变化（ok 或失败状态翻转）。
func (m *subManager) fetchOne(u string) (changed bool) {
	prevStatus := m.meta.Statuses[u]
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		m.setErr(u, "构造请求失败: "+err.Error())
		return prevStatus != "error"
	}
	if etag := m.meta.ETags[u]; etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		m.setErr(u, err.Error())
		return prevStatus != "error"
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		m.meta.Statuses[u] = "notmodified"
		m.meta.LastOK[u] = time.Now().Unix()
		m.meta.Errs[u] = ""
		return false
	case resp.StatusCode != http.StatusOK:
		m.setErr(u, fmt.Sprintf("HTTP %d", resp.StatusCode))
		return prevStatus != "error"
	}

	b, err := ioReadLimited(resp.Body, subMaxBytes)
	if err != nil {
		m.setErr(u, err.Error())
		return prevStatus != "error"
	}

	_, _, nN, nM, err := parseSubRules(b)
	if err != nil {
		m.setErr(u, "校验失败: "+err.Error())
		return prevStatus != "error"
	}

	// 成功：记录 ETag 与计数（names/merge 原文随合并阶段重读，此处仅记录状态）
	if etag := resp.Header.Get("ETag"); etag != "" {
		m.meta.ETags[u] = etag
	} else {
		delete(m.meta.ETags, u)
	}
	m.meta.Statuses[u] = "ok"
	m.meta.Errs[u] = ""
	m.meta.LastOK[u] = time.Now().Unix()
	m.meta.Entries[u], m.meta.Merges[u] = nN, nM
	// 原文缓存（固定名覆盖写）：合并阶段从这里重读，离线可用
	_ = os.WriteFile(m.rawPath(u), b, 0o644)
	log.Printf("[*] 订阅已更新：%s（命名 %d / 合并 %d）", u, nN, nM)
	return true
}

func (m *subManager) setErr(u, msg string) {
	m.meta.Statuses[u] = "error"
	m.meta.Errs[u] = msg
	log.Printf("[!] 订阅拉取失败 %s: %s（沿用缓存）", u, msg)
}

// writeMerged 按 URL 顺序合并订阅层并落盘。命名：先者优先；合并规则：按序拼接。
func (m *subManager) writeMerged(urls []string) error {
	names := map[string]string{}
	var mergeRules []mergeRule
	seen := map[string]bool{}
	for _, u := range urls {
		// 该 URL 最近一次成功下载的原文 = 状态 ok/notmodified 时对应缓存
		raw, ok := m.latestRaw(u)
		if !ok {
			continue // 该 URL 从未成功：跳过
		}
		subNames, subMerge, _, _, err := parseSubRules(raw)
		if err != nil {
			continue
		}
		for k, v := range subNames {
			if !seen[k] {
				names[k] = v
			}
		}
		mergeRules = append(mergeRules, subMerge...)
	}
	// 组装 {"names":..., "merge":...}（merge 须保序，手工序列化）
	var sb strings.Builder
	sb.WriteString(`{"names":`)
	nb, _ := json.Marshal(names)
	sb.Write(nb)
	sb.WriteString(`,"merge":{`)
	for i, r := range mergeRules {
		if i > 0 {
			sb.WriteByte(',')
		}
		kb, _ := json.Marshal(r.Key)
		sb.Write(kb)
		sb.WriteByte(':')
		sb.Write(r.Val)
	}
	sb.WriteString(`}}`)
	return os.WriteFile(m.mergedPath(), []byte(sb.String()), 0o644)
}

// rawPath URL 对应的原文缓存路径（固定名，每次成功覆盖写）。
func (m *subManager) rawPath(u string) string {
	sum := sha256.Sum256([]byte(u))
	return filepath.Join(m.cacheDir, "raw_"+hex.EncodeToString(sum[:8])+".json")
}

// latestRaw URL 最近一次成功下载的原文（无缓存 = 该 URL 从未成功）。
func (m *subManager) latestRaw(u string) ([]byte, bool) {
	b, err := os.ReadFile(m.rawPath(u))
	return b, err == nil
}

// parseSubRules 校验订阅文件并净化：返回 (净化后 names, 保序 merge 原文, names 数, merge 数, err)。
func parseSubRules(b []byte) (map[string]string, []mergeRule, int, int, error) {
	var v struct {
		Names json.RawMessage `json:"names"`
		Merge json.RawMessage `json:"merge"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, nil, 0, 0, fmt.Errorf("JSON 解析: %w", err)
	}
	if v.Names == nil && v.Merge == nil {
		return nil, nil, 0, 0, fmt.Errorf("缺少 names/merge 段")
	}
	names := map[string]string{}
	if v.Names != nil {
		var raw map[string]string
		if err := json.Unmarshal(v.Names, &raw); err != nil {
			return nil, nil, 0, 0, fmt.Errorf("names 段须为对象: %w", err)
		}
		for k, n := range raw {
			n = sanitizeSubName(n)
			if n == "" {
				continue
			}
			names[k] = n
		}
	}
	var rules []mergeRule
	if v.Merge != nil {
		rules = parseMergeOrdered(v.Merge)
	}
	return names, rules, len(names), len(rules), nil
}

// sanitizeSubName 净化订阅提供的名称：去控制字符，截断到上限。
func sanitizeSubName(s string) string {
	var sb strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		sb.WriteRune(r)
		if sb.Len() >= subNameMaxRune*4 { // rune 上限的宽松字节保护
			break
		}
	}
	runes := []rune(sb.String())
	if len(runes) > subNameMaxRune {
		runes = runes[:subNameMaxRune]
	}
	return strings.TrimSpace(string(runes))
}

// ioReadLimited 读取至多 max 字节；超出即报错（防止超大响应）。
func ioReadLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("响应超过 %d 字节上限", max)
	}
	return b, nil
}

// subLoop 订阅后台循环：每分钟醒来，配置 mtime 变化或到达间隔才刷新。
func subLoop(m *subManager) {
	// 启动先刷一轮（网络失败不影响主功能）
	m.refreshNow()
	var lastRefresh time.Time
	t := time.NewTicker(time.Minute)
	for range t.C {
		m.mu.Lock()
		cfgMod, cfg := m.cfgMod, m.cfg
		m.mu.Unlock()
		interval := time.Duration(cfg.IntervalMin) * time.Minute
		need := cfg.Enabled && len(cfg.Urls) > 0 &&
			(!lastRefresh.Equal(cfgMod) || time.Since(m.lastRefreshTime()) >= interval)
		if cfgMod != lastRefresh {
			// 配置变了：立即刷一轮（含 URL 增删/启停）
			m.refreshNow()
			lastRefresh = cfgMod
			continue
		}
		if need {
			m.refreshNow()
		}
	}
}

// lastRefreshTime 最近刷新时间（线程安全读取）。
func (m *subManager) lastRefreshTime() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return time.Unix(m.meta.LastRefresh, 0)
}
