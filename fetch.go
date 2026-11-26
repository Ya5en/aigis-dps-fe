// fetch.go —— 每次启动从 Worker 独立拉取 NameText 资源并建立本地缓存。
// 链路（与 aigisReader/perp/download.py + unatb.py 同构，纯 Go 实现）：
//   1. GET Workers /url → {"value": 资源地址}
//   2. GET 地址 → XOR 0xDA 解密 → index.txt（资源索引）
//   3. 索引中找 NameText.atb 条目 → GET（前 12 字节自定义头 + LZ4 frame）
//   4. ALTB/ALRD 解析：id = 行号+1（CardID 同编号空间），message = 全名
//   5. 写 data/unit_names.json 缓存 → 桥既有 mtime 热载自动生效
// 拉取/解析失败时沿用磁盘缓存，不影响启动。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pierrec/lz4/v4"
)

const (
	workerURL  = "https://aigis.m-152-rider.workers.dev/url"
	assetsBase = "https://assets.millennium-war.net"
)

var nameClient = &http.Client{Timeout: 30 * time.Second}

func httpGet(url string) ([]byte, error) {
	resp, err := nameClient.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// namesMeta 缓存元数据：记录两级资源 URL 的 SHA-256，逐级检查、任一命中即短路。
// 索引 URL 是版本化的：不变 ⇒ 整套资源未更新（连 index 都不用下）；
// 变了才下载 index 并继续看 atb URL，仍不变则只跳过 atb 下载（理论上的兜底层）。
type namesMeta struct {
	IndexHash string `json:"indexHash"` // index.txt（资源索引）URL 哈希
	AtbHash   string `json:"atbHash"`   // NameText.atb URL 哈希
	Updated   int64  `json:"updated"`   // 上次全量更新 Unix 秒
}

func metaPath(namesPath string) string {
	return filepath.Join(filepath.Dir(namesPath), "names_meta.json")
}

func loadNamesMeta(namesPath string) namesMeta {
	var m namesMeta
	b, err := os.ReadFile(metaPath(namesPath))
	if err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func saveNamesMeta(namesPath string, m namesMeta) {
	m.Updated = time.Now().Unix()
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.WriteFile(metaPath(namesPath), b, 0o644)
}

func urlHash(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:])
}

// fetchAndCacheNames 在线更新名字表缓存。
// 逐级检查短路：index URL 未变 → 直接用缓存；index 变而 atb URL 未变 → 只补 meta；
// 都变（或缓存/meta 缺失）→ 全量重下重建。
func fetchAndCacheNames(namesPath string) {
	// 磁盘缓存不存在时必须全量拉取
	fullFetch := false
	if _, err := os.Stat(namesPath); err != nil {
		fullFetch = true
	}

	// 1. Workers API 取资源地址（轻量，每次必做）
	v, err := httpGet(workerURL)
	if err != nil {
		logWarn("在线拉取名字表失败（沿用缓存 %s）: %v", namesPath, err)
		return
	}
	var loc struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(v, &loc); err != nil || loc.Value == "" {
		logWarn("Worker /url 响应无效（沿用缓存）: %v", err)
		return
	}
	ih := urlHash(loc.Value)

	meta := loadNamesMeta(namesPath)
	if !fullFetch && ih == meta.IndexHash {
		logInfo("名字表：索引 URL 未变化，跳过下载（使用缓存）")
		return
	}

	// 2. 下载 index（XOR 0xDA 解密）
	idx, err := httpGet(loc.Value)
	if err != nil {
		logWarn("索引下载失败（沿用缓存）: %v", err)
		return
	}
	for i := range idx {
		idx[i] ^= 0xDA
	}

	// 3. 找 NameText.atb 条目（行格式：路径,文件名,...）
	atbURL := ""
	for _, line := range strings.Split(string(idx), "\n") {
		if !strings.Contains(line, ",NameText.atb") {
			continue
		}
		parts := strings.Split(strings.TrimSpace(line), ",")
		if len(parts) >= 2 {
			atbURL = assetsBase + "/" + strings.TrimSpace(parts[0]) + "/" + strings.TrimSpace(parts[1])
			break
		}
	}
	if atbURL == "" {
		logWarn("index.txt 中未找到 NameText.atb 条目（沿用缓存）")
		return
	}
	ah := urlHash(atbURL)
	if !fullFetch && ah == meta.AtbHash {
		saveNamesMeta(namesPath, namesMeta{IndexHash: ih, AtbHash: ah})
		logInfo("名字表：atb URL 未变化，跳过下载（使用缓存）")
		return
	}

	// 4. 下载 atb（跳过前 12 字节自定义头，剩余为 LZ4 frame）
	atb, err := httpGet(atbURL)
	if err != nil {
		logWarn("NameText.atb 下载失败（沿用缓存）: %v", err)
		return
	}
	if len(atb) <= 12 {
		logWarn("NameText.atb 内容过短（%d B），沿用缓存", len(atb))
		return
	}
	names, n, err := parseNameText(atb[12:])
	if err != nil {
		logWarn("名字表解析失败（沿用缓存）: %v", err)
		return
	}
	j, err := json.Marshal(names)
	if err != nil {
		logWarn("名字表序列化失败: %v", err)
		return
	}
	if err := os.WriteFile(namesPath, j, 0o644); err != nil {
		logWarn("名字表缓存写入失败: %v", err)
		return
	}
	saveNamesMeta(namesPath, namesMeta{IndexHash: ih, AtbHash: ah})
	logInfo("名字表已在线更新：%d 条 → %s", n, namesPath)
}

// parseNameText 解析 NameText.atb（ALTB/gdtx + ALRD 结构）。
// 头部固定布局（小端）：magic1[4] ver[2] count[2] table_begin[2] index_begin[2]
// unknown2[4] data_size[4] data_begin[4] magic2[4] → ALRD: magic[4] ukn[2] rd_count[2] ukn[2]
// idx 数组位于 index_begin，count*rd_count 个 u32；第 i 行：col0=message、col1=realName。
func parseNameText(atb []byte) (map[string]string, int, error) {
	zr := lz4.NewReader(bytes.NewReader(atb))
	d, err := io.ReadAll(zr)
	if err != nil {
		return nil, 0, fmt.Errorf("LZ4 解压: %w", err)
	}
	if len(d) < 38 || string(d[0:4]) != "ALTB" {
		return nil, 0, fmt.Errorf("ALTB 头无效（len=%d magic=%q）", len(d), d[0:4])
	}
	count := binary.LittleEndian.Uint16(d[6:8])
	idxBegin := binary.LittleEndian.Uint16(d[10:12])
	dataBegin := binary.LittleEndian.Uint32(d[20:24])
	if string(d[28:32]) != "ALRD" {
		return nil, 0, fmt.Errorf("ALRD 头无效: %q", d[28:32])
	}
	rdCount := binary.LittleEndian.Uint16(d[34:36])
	if count == 0 || rdCount < 2 {
		return nil, 0, fmt.Errorf("count=%d rd_count=%d", count, rdCount)
	}

	cstr := func(off uint32) string {
		if off >= uint32(len(d)) {
			return ""
		}
		end := bytes.IndexByte(d[off:], 0)
		if end < 0 {
			end = len(d) - int(off)
		}
		return string(d[off : uint32(off)+uint32(end)])
	}

	names := make(map[string]string, count)
	base := uint32(idxBegin)
	for i := uint16(0); i < count; i++ {
		msgOff := dataBegin + binary.LittleEndian.Uint32(d[base+uint32(i)*4*uint32(rdCount):])
		rnOff := dataBegin + binary.LittleEndian.Uint32(d[base+uint32(i)*4*uint32(rdCount)+4:])
		// 全名优先（message），短名兜底（realName）
		name := cstr(msgOff)
		if name == "" {
			name = cstr(rnOff)
		}
		if name == "" {
			continue
		}
		names[fmt.Sprintf("%04d", i+1)] = name
	}
	return names, len(names), nil
}
