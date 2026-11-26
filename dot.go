// dot.go —— 敌方点阵预览：vi → EnemyDot<vi>.aar → ALTX 图集 PNG。
//
// 数据链（用户实测定案：vi 与点阵编号同空间，直接对应）：
//   dot = vi，编号表不在构建期固化——与 NameText 同构：启动后台从 Worker 拉
//   最新 index（XOR 0xDA 解密）→ 抽取 EnemyDot*.aar 行 → {编号: [hash1, hash2]}
//   → 落盘 data/dotcache/dotcdn.json；index URL 哈希未变则短路沿用缓存。
//   aar (ALAR 容器) 内含 Stand/Walk/Attack/Damage.aod + EnemyDotNNNN.atx
//   atx = ALL4 头 12B + LZ4 frame → ALTX：base=u16@8，宽高 @base+16/+20，
//         调色板 @base+u16@(base+26)（256×RGBA），像素索引 @base+u16@(base+28)（w×h）
//
// 产物缓存于 data/dotcache/（aar 原包 + 解码 PNG + 元数据 JSON），离线可用。
// 前端通过 /api/dotindex 拿完整编号表（编辑器 autocomplete 用，localStorage 缓存）。
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pierrec/lz4/v4"
)

const cdnBase = "https://assets.millennium-war.net/"

// ==================== dotStore ====================

type dotStore struct {
	mu       sync.Mutex
	cdn      map[string][2]string // dot 编号 → [dirhash, filehash]
	cacheDir string               // dotcache 目录
	client   *http.Client
}

func newDotStore(cacheDir string) *dotStore {
	// 缓存目录在启动时就建好：编号表/meta/aar/png 都写这里，
	// 首次运行（data/dotcache 尚不存在）也不能让写盘失败。
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		logWarn("创建 dot 缓存目录失败: %v", err)
	}
	d := &dotStore{
		cdn:      map[string][2]string{},
		cacheDir: cacheDir,
		client:   &http.Client{Timeout: 60 * time.Second},
	}
	d.loadCdnCache() // 上次成功拉取的磁盘缓存（如有）立即可用
	return d
}

// cdnCachePath 编号表缓存路径（dotcdn_meta.json 同目录）。
func (d *dotStore) cdnCachePath() string {
	return filepath.Join(d.cacheDir, "dotcdn.json")
}

// loadCdnCache 从磁盘缓存装载编号表（启动时同步调用；失败不致命，等在线刷新）。
func (d *dotStore) loadCdnCache() {
	b, err := os.ReadFile(d.cdnCachePath())
	if err != nil {
		return // 无缓存：等 refresh() 首次拉取
	}
	var cd map[string][2]string
	if err := json.Unmarshal(b, &cd); err != nil || len(cd) == 0 {
		logWarn("dotcdn 缓存损坏（%s）: %v，等待在线刷新", d.cdnCachePath(), err)
		return
	}
	d.mu.Lock()
	d.cdn = cd
	d.mu.Unlock()
	logInfo("dot 编号表已从缓存装载：%d 条", len(cd))
}

// refresh 从 Worker 拉最新 index 抽取 EnemyDot 编号表（fetchAndCacheNames 同构）。
// 逐级短路：index URL 哈希未变 → 沿用现有缓存；变了才重下重抽。
func (d *dotStore) refresh() {
	metaPath := filepath.Join(d.cacheDir, "dotcdn_meta.json")

	// 1. Workers API 取 index 地址
	v, err := httpGet(workerURL)
	if err != nil {
		logWarn("dot 编号表在线刷新失败（沿用缓存）: %v", err)
		return
	}
	var loc struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(v, &loc); err != nil || loc.Value == "" {
		logWarn("dot 编号表：Worker /url 响应无效（沿用缓存）: %v", err)
		return
	}
	ih := urlHash(loc.Value)

	// 2. index URL 未变 → 短路
	var meta struct {
		IndexHash string `json:"indexHash"`
	}
	if mb, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(mb, &meta)
	}
	if meta.IndexHash == ih && len(d.cdn) > 0 {
		logInfo("dot 编号表：索引 URL 未变化，跳过（使用缓存）")
		return
	}

	// 3. 下载 index（XOR 0xDA 解密）
	idx, err := httpGet(loc.Value)
	if err != nil {
		logWarn("dot 编号表：索引下载失败（沿用缓存）: %v", err)
		return
	}
	for i := range idx {
		idx[i] ^= 0xDA
	}

	// 4. 抽取 EnemyDot*.aar 行 → {编号: [hash1, hash2]}
	cd := map[string][2]string{}
	for _, line := range strings.Split(string(idx), "\n") {
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) < 5 || !strings.HasPrefix(parts[4], "EnemyDot") ||
			!strings.HasSuffix(parts[4], ".aar") {
			continue
		}
		num := strings.TrimSuffix(strings.TrimPrefix(parts[4], "EnemyDot"), ".aar")
		if num == "" {
			continue
		}
		for _, c := range num {
			if c < '0' || c > '9' {
				num = "" // 非纯数字（如 EnemyDotStand.aar 之类）不收
				break
			}
		}
		if num == "" {
			continue
		}
		cd[strconv.FormatInt(parseInt64(num), 10)] = [2]string{parts[0], parts[1]}
	}
	if len(cd) == 0 {
		logWarn("dot 编号表：index 中未抽到任何 EnemyDot 条目（沿用缓存）")
		return
	}

	// 5. 落盘 + 记 meta + 热替换内存表
	j, err := json.Marshal(cd)
	if err != nil {
		logWarn("dot 编号表序列化失败: %v", err)
		return
	}
	if err := os.WriteFile(d.cdnCachePath(), j, 0o644); err != nil {
		logWarn("dot 编号表缓存写入失败: %v", err)
		return
	}
	mb, _ := json.Marshal(struct {
		IndexHash string `json:"indexHash"`
	}{IndexHash: ih})
	_ = os.WriteFile(metaPath, mb, 0o644)

	d.mu.Lock()
	d.cdn = cd
	n := len(cd)
	d.mu.Unlock()
	logInfo("dot 编号表已在线更新：%d 条 → %s", n, d.cdnCachePath())
}

// parseInt64 十进制字符串 → int64（调用方已保证纯数字）。
func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// viToDot vi → dot 编号（同空间直映）；编号不在 CDN 索引 → 不存在。
func (d *dotStore) viToDot(vi int64) (dot int64, ok bool) {
	return vi, d.hasDot(vi)
}

// hasDot dot 编号是否存在于 CDN 索引。
func (d *dotStore) hasDot(dot int64) bool {
	_, ok := d.cdn[strconv.FormatInt(dot, 10)]
	return ok
}

// dotIDs 全部可用编号（升序）。
func (d *dotStore) dotIDs() []int64 {
	ids := make([]int64, 0, len(d.cdn))
	for k := range d.cdn {
		n, _ := strconv.ParseInt(k, 10, 64)
		ids = append(ids, n)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// ==================== ALAR 解包 ====================

type aarEntry struct {
	Name string
	Data []byte
}

// unpackAAR 解 ALAR 容器（0x4103/0x4503 = 1B 偏移单位，0x4903 = 4B）。
func unpackAAR(b []byte) ([]aarEntry, error) {
	if len(b) < 18 || string(b[:4]) != "ALAR" {
		return nil, errors.New("not ALAR container")
	}
	ver := binary.LittleEndian.Uint16(b[4:6])
	count := int(binary.LittleEndian.Uint16(b[6:8]))
	var bs int
	switch ver {
	case 0x4503, 0x4103:
		bs = 1
	case 0x4903:
		bs = 4
	default:
		return nil, fmt.Errorf("unsupported ALAR version 0x%04x", ver)
	}
	offs := make([]int, count)
	for i := range offs {
		offs[i] = int(binary.LittleEndian.Uint16(b[18+i*2:])) * bs
	}
	var out []aarEntry
	for _, off := range offs {
		if off+18 > len(b) {
			return nil, errors.New("entry offset out of range")
		}
		start := binary.LittleEndian.Uint32(b[off+4:])
		size := binary.LittleEndian.Uint32(b[off+8:])
		np := off + 18 // fid(4)+start(4)+size(4)+unknown(6)
		if np > len(b) {
			return nil, errors.New("name offset out of range")
		}
		end := bytes.IndexByte(b[np:], 0)
		if end < 0 {
			end = len(b) - np
		}
		name := string(b[np : np+end])
		if int64(start)+int64(size) > int64(len(b)) {
			return nil, errors.New("data out of range")
		}
		out = append(out, aarEntry{Name: name, Data: b[start : start+size]})
	}
	return out, nil
}

// ==================== ALTX 解码 ====================

// atxInner 解 ALL4 包装：校验 magic 后 LZ4 解压出 ALTX inner。
func atxInner(atx []byte) ([]byte, error) {
	if len(atx) < 12 || string(atx[:4]) != "ALL4" {
		return nil, errors.New("not ALL4 wrapper")
	}
	inner, err := io.ReadAll(lz4.NewReader(bytes.NewReader(atx[12:])))
	if err != nil {
		return nil, fmt.Errorf("lz4 decompress: %w", err)
	}
	return inner, nil
}

// decodeALTX inner（已解压的 ALTX）→ 全量图集（调色板索引 → RGBA）。
func decodeALTX(inner []byte) (image.Image, error) {
	if len(inner) < 8 || string(inner[:4]) != "ALTX" {
		return nil, errors.New("not ALTX image")
	}
	base := int(binary.LittleEndian.Uint16(inner[8:]))
	if base+32 > len(inner) || string(inner[base:base+4]) != "ALIG" {
		return nil, errors.New("not ALIG block")
	}
	w := int(binary.LittleEndian.Uint16(inner[base+16:]))
	h := int(binary.LittleEndian.Uint16(inner[base+20:]))
	ppal := base + int(binary.LittleEndian.Uint16(inner[base+26:]))
	prgb := base + int(binary.LittleEndian.Uint16(inner[base+28:]))
	if w <= 0 || h <= 0 || ppal+1024 > len(inner) || prgb+w*h > len(inner) {
		return nil, errors.New("ALIG header out of range")
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		p := ppal + int(inner[prgb+i])*4
		img.Pix[i*4+0] = inner[p+0]
		img.Pix[i*4+1] = inner[p+1]
		img.Pix[i*4+2] = inner[p+2]
		img.Pix[i*4+3] = inner[p+3]
	}
	return img, nil
}

// ==================== 取包与渲染 ====================

// loadAAR dot 包字节：本地 tools/EnemyDotNNNN.aar → dotcache 缓存 → CDN。
func (d *dotStore) loadAAR(dot int64) ([]byte, error) {
	key := "d" + strconv.FormatInt(dot, 10)
	cacheAar := filepath.Join(d.cacheDir, key+".aar")
	if b, err := os.ReadFile(cacheAar); err == nil {
		return b, nil
	}
	if b, err := os.ReadFile(resolveDataFile("tools", "EnemyDot"+fmt.Sprintf("%04d", dot)+".aar")); err == nil {
		return b, nil
	}
	hashes, ok := d.cdn[strconv.FormatInt(dot, 10)]
	if !ok {
		return nil, errors.New("dot 不在 CDN 索引中")
	}
	url := cdnBase + hashes[0] + "/" + hashes[1]
	resp, err := d.client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("CDN 拉取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDN 返回 %d", resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if len(b) > 2 && b[0] == 0x1f && b[1] == 0x8b { // CDN 偶发 gzip 外壳
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		if b, err = io.ReadAll(zr); err != nil {
			return nil, err
		}
	}
	if string(b[:4]) != "ALAR" {
		return nil, errors.New("CDN 内容非 ALAR 包")
	}
	_ = os.MkdirAll(d.cacheDir, 0o755)
	if err := os.WriteFile(cacheAar, b, 0o644); err != nil {
		logWarn("aar 缓存写入失败: %v", err)
	}
	return b, nil
}

// atxOf 从包条目中取 .atx 数据（不存在 → nil）。
func atxOf(entries []aarEntry) []byte {
	for _, e := range entries {
		if l := len(e.Name); l > 4 && e.Name[l-4:] == ".atx" {
			return e.Data
		}
	}
	return nil
}

// loadInner dot → (包条目, 已解压 ALTX inner)。
func (d *dotStore) loadInner(dot int64) ([]aarEntry, []byte, error) {
	aar, err := d.loadAAR(dot)
	if err != nil {
		return nil, nil, err
	}
	entries, err := unpackAAR(aar)
	if err != nil {
		return nil, nil, err
	}
	atx := atxOf(entries)
	if atx == nil {
		return nil, nil, errors.New("包内无 atx 图集")
	}
	inner, err := atxInner(atx)
	if err != nil {
		return nil, nil, err
	}
	return entries, inner, nil
}

// renderPNG dot → PNG 字节（磁盘缓存命中则直接返回）。
func (d *dotStore) renderPNG(dot int64) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := "d" + strconv.FormatInt(dot, 10)
	cachePng := filepath.Join(d.cacheDir, key+".png")
	if b, err := os.ReadFile(cachePng); err == nil {
		return b, nil
	}
	_, inner, err := d.loadInner(dot)
	if err != nil {
		return nil, err
	}
	img, err := decodeALTX(inner)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	_ = os.MkdirAll(d.cacheDir, 0o755)
	if err := os.WriteFile(cachePng, buf.Bytes(), 0o644); err != nil {
		logWarn("png 缓存写入失败: %v", err)
	}
	return buf.Bytes(), nil
}

// ==================== ATX 帧表 / AOD 时间轴（逆向定案，见 aigis-re/aod_parse.py） ====================

type dotFrame struct {
	X  int `json:"x"`
	Y  int `json:"y"`
	W  int `json:"w"`
	H  int `json:"h"`
	AX int `json:"ax"` // 锚点：绘制左上角 = 单位位置 - (ax, ay)
	AY int `json:"ay"`
}

type dotGroup struct {
	ID     uint32     `json:"id"`
	Frames []dotFrame `json:"frames"`
}

type dotMotion struct {
	Tex   uint32   `json:"tex"`
	Total int      `json:"total"` // 总 tick
	FPS   int      `json:"fps"`
	Loop  int      `json:"loop"`
	Seq   [][2]int `json:"seq"` // 关键帧 (tick, frameIndex)
}

type dotMotionEntry struct {
	Name   string    `json:"name"`
	Motion dotMotion `json:"motion"`
}

type dotMeta struct {
	W       int              `json:"w"`
	H       int              `json:"h"`
	Groups  []dotGroup       `json:"groups"`
	Motions []dotMotionEntry `json:"motions"`
}

// parseATXGroups 解 ALTX 帧表：组 × (帧矩形 x/y/w/h + 锚点 ax/ay)，返回 (组, 图集宽, 高)。
func parseATXGroups(inner []byte) ([]dotGroup, int, int, error) {
	if len(inner) < 0x14 || string(inner[:4]) != "ALTX" {
		return nil, 0, 0, errors.New("not ALTX")
	}
	n := int(binary.LittleEndian.Uint16(inner[6:]))
	base := int(binary.LittleEndian.Uint16(inner[8:]))
	if base+32 > len(inner) || string(inner[base:base+4]) != "ALIG" {
		return nil, 0, 0, errors.New("not ALIG block")
	}
	w := int(binary.LittleEndian.Uint16(inner[base+16:]))
	h := int(binary.LittleEndian.Uint16(inner[base+20:]))
	off := 0x14
	groups := make([]dotGroup, 0, n)
	for i := 0; i < n && off+8 <= len(inner); i++ {
		tid := binary.LittleEndian.Uint32(inner[off:])
		nf := int(binary.LittleEndian.Uint16(inner[off+4:]))
		p := off + 8
		if p+nf*12 > len(inner) {
			return nil, 0, 0, errors.New("frame table out of range")
		}
		g := dotGroup{ID: tid, Frames: make([]dotFrame, nf)}
		for j := 0; j < nf; j++ {
			g.Frames[j].X = int(binary.LittleEndian.Uint16(inner[p:]))
			g.Frames[j].Y = int(binary.LittleEndian.Uint16(inner[p+2:]))
			g.Frames[j].W = int(binary.LittleEndian.Uint16(inner[p+4:]))
			g.Frames[j].H = int(binary.LittleEndian.Uint16(inner[p+6:]))
			p += 8
		}
		for j := 0; j < nf; j++ {
			g.Frames[j].AX = int(binary.LittleEndian.Uint16(inner[p:]))
			g.Frames[j].AY = int(binary.LittleEndian.Uint16(inner[p+2:]))
			p += 4
		}
		groups = append(groups, g)
		off = p
	}
	return groups, w, h, nil
}

// parseAOD 解 ALOD 动作：texId@0x34，total@0x60，fps@0x62，loop@0x63，
// 关键帧序列自 0x6C 起 (tick, frame) 对，tick=0xFFFF 终止。
func parseAOD(d []byte) (dotMotion, error) {
	if len(d) < 0x6C || string(d[:4]) != "ALOD" {
		return dotMotion{}, errors.New("not ALOD")
	}
	m := dotMotion{
		Tex:   binary.LittleEndian.Uint32(d[0x34:]),
		Total: int(binary.LittleEndian.Uint16(d[0x60:])),
		FPS:   int(d[0x62]),
		Loop:  int(d[0x63]),
	}
	for p := 0x6C; p+4 <= len(d); p += 4 {
		tick := int(binary.LittleEndian.Uint16(d[p:]))
		if tick == 0xFFFF {
			break
		}
		m.Seq = append(m.Seq, [2]int{tick, int(binary.LittleEndian.Uint16(d[p+2:]))})
	}
	return m, nil
}

// meta dot → 帧表+动作元数据（磁盘缓存 dotcache/dNNNN.json）。
func (d *dotStore) meta(dot int64) (*dotMeta, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	key := "d" + strconv.FormatInt(dot, 10)
	cacheMeta := filepath.Join(d.cacheDir, key+".json")
	if b, err := os.ReadFile(cacheMeta); err == nil {
		var m dotMeta
		if json.Unmarshal(b, &m) == nil && len(m.Groups) > 0 {
			return &m, nil
		}
	}
	entries, inner, err := d.loadInner(dot)
	if err != nil {
		return nil, err
	}
	groups, w, h, err := parseATXGroups(inner)
	if err != nil {
		return nil, err
	}
	m := &dotMeta{W: w, H: h, Groups: groups}
	for _, e := range entries {
		l := len(e.Name)
		if l > 4 && e.Name[l-4:] == ".aod" {
			mo, err := parseAOD(e.Data)
			if err != nil {
				continue
			}
			m.Motions = append(m.Motions, dotMotionEntry{
				Name:   e.Name[:l-4],
				Motion: mo,
			})
		}
	}
	b, _ := json.Marshal(m)
	_ = os.MkdirAll(d.cacheDir, 0o755)
	if err := os.WriteFile(cacheMeta, b, 0o644); err != nil {
		logWarn("meta 缓存写入失败: %v", err)
	}
	return m, nil
}

// ==================== HTTP 端点 ====================

// registerDotRoutes 编辑器相关端点（挂到现有 mux 上）。
func (d *dotStore) registerDotRoutes(mux *http.ServeMux) {
	// 完整编号表（前端 localStorage 缓存）
	mux.HandleFunc("GET /api/dotindex", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ids": d.dotIDs()})
	})

	// vi → dot 解析
	mux.HandleFunc("GET /api/vdot", func(w http.ResponseWriter, r *http.Request) {
		vi, err := strconv.ParseInt(r.URL.Query().Get("v"), 10, 64)
		if err != nil || vi <= 0 {
			writeJSON(w, 400, map[string]string{"err": "参数 v 须为正整数"})
			return
		}
		dot, ok := d.viToDot(vi)
		if !ok {
			writeJSON(w, 200, map[string]any{"vi": vi, "dot": 0, "found": false})
			return
		}
		writeJSON(w, 200, map[string]any{"vi": vi, "dot": dot, "found": true})
	})

	// dot 图集 PNG：?d=编号 或 ?v=vi（后者先查映射）
	mux.HandleFunc("GET /api/dot.png", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var dot int64
		if s := q.Get("d"); s != "" {
			dot, _ = strconv.ParseInt(s, 10, 64)
		} else if s := q.Get("v"); s != "" {
			vi, _ := strconv.ParseInt(s, 10, 64)
			dot, _ = d.viToDot(vi)
		}
		if dot <= 0 {
			http.Error(w, "参数缺失", 400)
			return
		}
		b, err := d.renderPNG(dot)
		if err != nil {
			writeJSON(w, 404, map[string]string{"err": err.Error()})
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	})

	// dot 帧表+动作时间轴：?d=编号
	mux.HandleFunc("GET /api/dotmeta", func(w http.ResponseWriter, r *http.Request) {
		dot, _ := strconv.ParseInt(r.URL.Query().Get("d"), 10, 64)
		if dot <= 0 {
			writeJSON(w, 400, map[string]string{"err": "参数 d 须为正整数"})
			return
		}
		m, err := d.meta(dot)
		if err != nil {
			writeJSON(w, 404, map[string]string{"err": err.Error()})
			return
		}
		writeJSON(w, 200, m)
	})
}
