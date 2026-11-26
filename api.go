// api.go —— HTTP/SSE 服务（127.0.0.1 only）+ 名字映射热载。
// SSE 推渲染就绪快照帧：ver 不变不发，无事件 5s 心跳。
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"
)

// ==================== 名字映射 ====================

func readNamesFile(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}

func statFile(path string) (os.FileInfo, error) { return os.Stat(path) }

// humanSize 日志用的人类可读字节数。
func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1fMiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKiB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%dB", n)
	}
}

// ==================== SSE hub ====================

type hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

func newHub() *hub { return &hub{subs: map[chan []byte]struct{}{}} }

func (h *hub) add() chan []byte {
	ch := make(chan []byte, 8)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) remove(ch chan []byte) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// broadcast 非阻塞投递；慢订阅者丢帧（快照是全量状态，丢了下帧自然补齐）。
func (h *hub) broadcast(b []byte) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default:
		}
	}
	h.mu.Unlock()
}

// ==================== 服务 ====================

type server struct {
	agg        *agg
	hub        *hub
	webFS      http.FileSystem
	dots       *dotStore
	subs       *subManager
	aliasPath  string
	shmOk      atomic.Bool
	lastSeq    atomic.Uint32 // 最近一次看到的 seq_total
	lastChg    atomic.Int64  // seq_total 最近变化时刻（UnixNano）
	curReader  atomic.Pointer[shmReader]
	replaying  atomic.Bool   // 历史回放进行中（UI 加载过渡用）
	replayLeft atomic.Uint32 // 回放剩余条数（进度展示）
	imported   sync.Map      // 已导入映像的 SHA-256 → 防重复导入
	picking    atomic.Bool   // 文件选择框占用中（对话框是模态的，需防重复弹出）
}

func (s *server) statusState() map[string]any {
	epoch, seqTotal, oldest := uint32(0), uint32(0), uint32(0)
	if r := s.curReader.Load(); r != nil {
		epoch, seqTotal, oldest = r.meta()
	}
	shm := "none"
	if s.shmOk.Load() {
		shm = "ok"
	}
	stalled := false
	if s.shmOk.Load() && seqTotal != 0 &&
		time.Now().UnixNano()-s.lastChg.Load() > 30*time.Second.Nanoseconds() {
		stalled = true // 数据静止：疑似 writer 停走 / 退出（视图冻结）
	}
	return map[string]any{
		"shm":        shm,
		"stalled":    stalled,
		"epoch":      epoch,
		"seqTotal":   seqTotal,
		"oldestSeq":  oldest,
		"replaying":  s.replaying.Load(),
		"replayLeft": s.replayLeft.Load(),
		"sessions":   s.agg.metas(),
	}
}

// importSink 导入重放的聚合路由：事件只进本源导入会话，与实时会话完全隔离。
// 复用 shmReader.poll 的全套协议逻辑（槽表校验/ABA/去 pad/gf 回退分段），自己只管会话切换。
// batch = 本源导入批次号（会话 sid = (batch<<32)|idx，与实时/其他批次互不混用）。
type importSink struct {
	agg   *agg
	batch uint64
	idx   uint32 // 批内战斗序号（OnSessionStart 递增）
	ses   *session
}

func (is *importSink) OnSessionStart(epoch uint32) {
	is.idx++
	is.ses = is.agg.newImportedSession(epoch, is.batch, is.idx)
}

func (is *importSink) OnClock(gf uint32) {} // 导入侧无实时时钟源，战斗时钟在 applyImported 内按事件 gf 推进

func (is *importSink) OnEvent(e ev) {
	if is.ses != nil {
		is.agg.applyImported(is.ses, e)
	}
}

func (is *importSink) OnLost(n uint64, ranges int) {
	if is.ses != nil {
		is.agg.applyLost(is.ses, n, ranges)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.statusState())
	})

	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		sid := uint64(0)
		fmt.Sscanf(r.URL.Query().Get("sid"), "%d", &sid)
		snap, ok := s.agg.snapshotOf(sid)
		if !ok {
			writeJSON(w, 404, map[string]string{"err": "no session"})
			return
		}
		writeJSON(w, 200, snap)
	})

	// 重置：清空所有 DPS 统计（含历史），开新空会话；不动 SHM 与游标
	mux.HandleFunc("POST /api/reset", func(w http.ResponseWriter, r *http.Request) {
		epoch := uint32(0)
		if rd := s.curReader.Load(); rd != nil {
			epoch, _, _ = rd.meta()
		}
		s.agg.reset(epoch)
		logInfo("统计已重置（epoch=%d）", epoch)
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	mux.HandleFunc("GET /api/names", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.agg.currentNames())
	})

	// 当前别名配置（改名覆盖 + 合并组），便于核对配置是否生效
	mux.HandleFunc("GET /api/alias", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.agg.currentAlias())
	})

	// ===== 规则编辑器 =====

	// 读原始配置文件（不存在 → 空模板）
	mux.HandleFunc("GET /api/aliasfile", func(w http.ResponseWriter, r *http.Request) {
		b, err := os.ReadFile(s.aliasPath)
		if err != nil {
			writeJSON(w, 200, map[string]json.RawMessage{
				"names": json.RawMessage(`{}`), "merge": json.RawMessage(`{}`),
			})
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(b)
	})

	// 保存配置（校验 JSON 后写回；桥侧 mtime 热载自动生效）
	mux.HandleFunc("POST /api/aliasfile", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		if err != nil {
			writeJSON(w, 400, map[string]string{"err": "读取失败"})
			return
		}
		var v map[string]json.RawMessage
		if err := json.Unmarshal(b, &v); err != nil {
			writeJSON(w, 400, map[string]string{"err": "JSON 校验失败: " + err.Error()})
			return
		}
		if _, ok := v["names"]; !ok {
			writeJSON(w, 400, map[string]string{"err": "缺少 names 段"})
			return
		}
		if _, ok := v["merge"]; !ok {
			writeJSON(w, 400, map[string]string{"err": "缺少 merge 段"})
			return
		}
		if err := os.WriteFile(s.aliasPath, b, 0o644); err != nil {
			writeJSON(w, 500, map[string]string{"err": "写入失败: " + err.Error()})
			return
		}
		logInfo("别名配置已由编辑器保存（%s）", s.aliasPath)
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// ===== 规则订阅 =====

	// 订阅状态（URL 列表 + 逐条拉取状态 + 合并产物计数）
	mux.HandleFunc("GET /api/subs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, s.subs.status())
	})

	// 保存订阅配置（校验 JSON 后写回；管理器 mtime 检测自动生效并立即刷新）
	mux.HandleFunc("POST /api/subsfile", func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		if err != nil {
			writeJSON(w, 400, map[string]string{"err": "读取失败"})
			return
		}
		var cfg struct {
			Enabled     bool     `json:"enabled"`
			IntervalMin int      `json:"intervalMin"`
			Urls        []string `json:"urls"`
		}
		if err := json.Unmarshal(b, &cfg); err != nil {
			writeJSON(w, 400, map[string]string{"err": "JSON 校验失败: " + err.Error()})
			return
		}
		for _, u := range cfg.Urls {
			pu, err := url.Parse(strings.TrimSpace(u))
			if err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
				writeJSON(w, 400, map[string]string{"err": "URL 非法（须为 http/https）: " + u})
				return
			}
		}
		if err := os.WriteFile(s.subs.cfgPath, b, 0o644); err != nil {
			writeJSON(w, 500, map[string]string{"err": "写入失败: " + err.Error()})
			return
		}
		logInfo("订阅配置已保存（%d 个 URL，启用=%v）", len(cfg.Urls), cfg.Enabled)
		go s.subs.refreshNow() // 立即刷一轮；agg 经 sub_merged.json mtime 自动重载
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// 手动立即刷新订阅
	mux.HandleFunc("POST /api/subs/refresh", func(w http.ResponseWriter, r *http.Request) {
		go s.subs.refreshNow()
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// 系统浏览器打开规则编辑器页
	mux.HandleFunc("POST /api/openeditor", func(w http.ResponseWriter, r *http.Request) {
		url := "http://" + r.Host + "/editor.html"
		if err := exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start(); err != nil {
			writeJSON(w, 500, map[string]string{"err": err.Error()})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	})

	// ===== 历史战斗数据导入导出 =====

	// 导出：SHM 原样 dump——16MB 全量映像（头 + 槽表 + 事件环，一字节不改）一次拷出
	// 只读映射，经 zstd 压缩落盘 data/exports/，随后打开资源管理器定位。
	// zstd SpeedDefault：基准实测（真实 16MB 映像）61KB / 压缩 1.9GB/s / 解压 1.9GB/s，
	// 比率约为 lz4 的 1/2.8 且解压更快；纯 Go 实现，单 exe 内嵌无额外运行时。
	// 文件名只保留映像里提取不到的信息：应用归属 + 导出时刻。
	// epoch/seqTotal/oldest 等元数据映像头部自带（epoch@0x04 / seqTotal@0x40 /
	// oldest@0x48），写入文件名属冗余；.ashm = aigis shared memory dump。
	mux.HandleFunc("POST /api/export", func(w http.ResponseWriter, r *http.Request) {
		rd := s.curReader.Load()
		if rd == nil || !s.shmOk.Load() {
			writeJSON(w, 409, map[string]string{"err": "SHM 未接入，无可导出数据"})
			return
		}
		epoch, seqTotal, oldest := rd.meta() // 拷贝时刻的元数据（响应留档；映像头部同样自带，导入侧自行解析）
		img := make([]byte, shmTotal)
		copy(img, rd.v) // 一次性整体拷出；writer 并发写下的瞬时撕裂风险与 poll 同级，可接受

		dir := filepath.Join(filepath.Dir(s.aliasPath), "exports")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			writeJSON(w, 500, map[string]string{"err": "创建导出目录失败: " + err.Error()})
			return
		}
		name := fmt.Sprintf("aigis-dps_%s.ashm", time.Now().Format("20060102-150405"))
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			writeJSON(w, 500, map[string]string{"err": "创建导出文件失败: " + err.Error()})
			return
		}
		zw, _ := zstd.NewWriter(f, zstd.WithEncoderLevel(zstd.SpeedDefault))
		_, cerr := io.Copy(zw, bytes.NewReader(img))
		zerr := zw.Close()
		st, serr := f.Stat() // Stat 必须在 Close 之前（closed file 上 Stat 返回 nil）
		ferr := f.Close()
		if cerr != nil || zerr != nil || ferr != nil || serr != nil {
			_ = os.Remove(p) // 残缺文件不留盘
			writeJSON(w, 500, map[string]string{"err": "压缩写入失败"})
			return
		}
		logInfo("SHM 已导出：%s（epoch=%d seq=%d oldest=%d，%s）", p, epoch, seqTotal, oldest, humanSize(st.Size()))
		_ = exec.Command("rundll32", "url.dll,FileProtocolHandler", dir).Start() // 资源管理器定位导出目录
		writeJSON(w, 200, map[string]any{
			"ok": true, "path": p, "bytes": st.Size(),
			"epoch": epoch, "seqTotal": seqTotal, "oldest": oldest,
		})
	})

	// 导入：读回 .ashm（zstd 压缩的 SHM 16MB 全量映像），在映像副本上
	// 按 SHM 协议重放事件进聚合器——产生的会话标记 src=import（场次下拉「导入」分组），
	// 不触碰实时会话，更不写共享区。
	// 文件来源：?name= 直接指定 exports 内文件名（程序化调用/测试）；
	// 缺省从 Go 侧弹出系统文件选择框（comdlg32，overlay 形态下 JS 文件选择不可靠）。
	// 同一映像按 SHA-256 去重，重复导入拒绝。
	// 可扩展点（暂未实现）：zstd 流首可放置 skippable frame（magic 0x184D2A50..5F，
	// 12B 头 + 负载）承载 JSON 元数据（导出工具版本 / 内容校验和等）——klauspost 解码器
	// 透明跳过、标准 zstd CLI 兼容（tools/skipframe 实验已验证），将来需要时在此帧写入。
	mux.HandleFunc("POST /api/import", func(w http.ResponseWriter, r *http.Request) {
		dir := filepath.Join(filepath.Dir(s.aliasPath), "exports")
		p := ""
		if name := filepath.Base(r.URL.Query().Get("name")); name != "" && name != "." {
			if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && !fi.IsDir() {
				p = filepath.Join(dir, name)
			}
		}
		if p == "" { // 缺省：系统文件选择框（模态，防重复弹出）
			if !s.picking.CompareAndSwap(false, true) {
				writeJSON(w, 409, map[string]string{"err": "已有文件选择框等待确认"})
				return
			}
			sel, err := pickOpenFile(hwndMain, "选择战斗数据文件（.ashm）", dir,
				"aigis-dps 战斗数据", "*.ashm", "所有文件", "*.*")
			s.picking.Store(false)
			if err != nil {
				logWarn("文件选择框失败: %v", err)
				writeJSON(w, 500, map[string]string{"err": err.Error()})
				return
			}
			if sel == "" { // 用户取消：不是错误，前端静默
				logInfo("导入已取消（用户关闭文件选择框）")
				writeJSON(w, 200, map[string]any{"ok": false, "canceled": true})
				return
			}
			logInfo("导入文件选择: %s", sel)
			p = sel
		}

		cb, err := os.ReadFile(p)
		if err != nil {
			writeJSON(w, 500, map[string]string{"err": "读取失败: " + err.Error()})
			return
		}
		zr, err := zstd.NewReader(bytes.NewReader(cb))
		if err != nil {
			writeJSON(w, 400, map[string]string{"err": "zstd 解压器创建失败: " + err.Error()})
			return
		}
		defer zr.Close()
		img, err := io.ReadAll(io.LimitReader(zr, shmTotal+1))
		if err != nil {
			writeJSON(w, 400, map[string]string{"err": "解压失败: " + err.Error()})
			return
		}
		if len(img) != shmTotal {
			writeJSON(w, 400, map[string]string{"err": fmt.Sprintf("映像大小不符（%d B，期望 %d B）", len(img), shmTotal)})
			return
		}
		if le32(img, 0) != magic {
			writeJSON(w, 400, map[string]string{"err": "magic 不符（非 DPS2 映像）"})
			return
		}

		sum := sha256.Sum256(img)
		if _, dup := s.imported.LoadOrStore(string(sum[:]), struct{}{}); dup {
			writeJSON(w, 409, map[string]string{"err": "该映像已导入过（内容 SHA-256 相同）"})
			return
		}

		epoch, seqTotal, oldest := le32(img, mOffEpoch), le32(img, mOffSeqTotal), le32(img, mOffOldest)
		is := &importSink{agg: s.agg, batch: s.agg.nextImportBatch()}
		is.OnSessionStart(epoch) // 首个导入会话（后续战斗由事件级 gf 回退分段检出）
		rd := &shmReader{
			v: img, head: img[offHead:], body: img[offBody:], sink: is,
			lastEpoch: epoch,                 // 映像 epoch 即本底，meta 对比不触发伪开场
			lastGf:    le32(img, mOffLastGf), // 同上：对齐 meta.last_gf，避免伪 OnClock/伪重置
			exp:       oldest,                // 从环内最旧完整记录起重放（与 attach 回放一致）
		}
		n := 0
		for {
			c := rd.poll()
			if c == 0 {
				break // 映像冻结：追到 seqTotal 即停
			}
			n += c
		}
		logInfo("已导入 %s（批次 %d，epoch=%d seq=%d→%d：%d 条事件，%d 场）", filepath.Base(p), is.batch, epoch, oldest, seqTotal, n, is.idx)
		writeJSON(w, 200, map[string]any{
			"ok": true, "file": filepath.Base(p), "events": n, "battles": is.idx,
			"batch": is.batch, "epoch": epoch, "seqTotal": seqTotal, "oldest": oldest,
		})
	})

	// 退出程序：有窗体走 WM_CLOSE 优雅关闭（托盘清理等 defer 生效），无窗体直接退出
	mux.HandleFunc("POST /api/exit", func(w http.ResponseWriter, r *http.Request) {
		logInfo("收到退出请求")
		writeJSON(w, 200, map[string]any{"ok": true})
		go func() {
			time.Sleep(150 * time.Millisecond) // 让响应先落到客户端
			if hwndMain != 0 {
				procPostMsg.Call(hwndMain, wmClose, 0, 0)
				return
			}
			os.Exit(0)
		}()
	})

	// 敌方点阵：编号表 / vi→dot / 图集 PNG
	s.dots.registerDotRoutes(mux)

	// SSE：/events
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no flusher", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		fmt.Fprint(w, ": connected\n\n")
		fl.Flush()

		// 接入即推当前快照（重连恢复路径）
		if snap, ok := s.agg.snapshotOf(0); ok {
			b, _ := json.Marshal(snap)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}

		ch := s.hub.add()
		defer s.hub.remove(ch)
		hb := time.NewTicker(5 * time.Second)
		defer hb.Stop()
		for {
			select {
			case b := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			case <-hb.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})

	// 嵌入的 UI 静态资源
	mux.Handle("/", http.FileServer(s.webFS))
	return logRequest(mux)
}

func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" { // SSE 长连接不刷日志
			logInfo("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}

// ==================== 快照发布循环 ====================

// publishLoop 每 200ms 检查 ver；有订阅者且 ver 变化才推帧。
// sid 变化（新会话/重置）时 ver 归零，须强制推一帧。
func (s *server) publishLoop() {
	var lastSentSid uint64
	var lastSentVer uint64
	tick := time.NewTicker(200 * time.Millisecond)
	for range tick.C {
		s.agg.reloadNames() // mtime 热载（内部有变化检测，廉价）
		s.agg.reloadAliases()
		if s.hub.count() == 0 {
			continue
		}
		snap, ok := s.agg.snapshotOf(0)
		if !ok {
			continue
		}
		if snap.Sid == lastSentSid && snap.Ver == lastSentVer {
			continue
		}
		b, err := json.Marshal(snap)
		if err != nil {
			continue
		}
		lastSentSid = snap.Sid
		lastSentVer = snap.Ver
		s.hub.broadcast(b)
	}
}

// ==================== reader 循环 ====================

// readerLoop 接入重试 + 1ms 轮询。视图冻结（writer 退出）后内容静止，
// poll 自然消费 0 条，由 statusState 的 stalled 判定兜住 UI 展示。
func (s *server) readerLoop(pollMs int) {
	for {
		r, err := tryAttach(s.agg)
		if err != nil {
			s.shmOk.Store(false)
			time.Sleep(500 * time.Millisecond)
			continue
		}
		s.curReader.Store(r)
		s.shmOk.Store(true)
		// 启动策略（串行追赶）：环内存量非空 → 从最旧完整记录起回放重建战斗记录；
		// 否则对齐到当前 seq_total 从此刻起统计。
		// 注意 oldest_seq 初始值即 0（seq 从 0 起，未覆写时保持 0），不能排除 0。
		_, total, oldest := r.meta()
		if total > oldest {
			r.startReplay(total)
			s.replaying.Store(true)
			s.replayLeft.Store(total - oldest)
			logInfo("已接入 %s（epoch=%d），回放存量 %d 条（seq %d→%d）重建战斗记录",
				shmName, r.lastEpoch, total-oldest, oldest, total)
		} else {
			r.skipBacklog()
			logInfo("已接入 %s（epoch=%d），无存量可回放，从 seq=%d 起统计",
				shmName, r.lastEpoch, total)
		}
		s.agg.OnSessionStart(r.lastEpoch)
		s.lastSeq.Store(0)

		for {
			r.poll()
			// 回放进度上报（reader 内部追平/中止后 replayTarget 归零）
			if r.replayTarget != 0 {
				s.replaying.Store(true)
				s.replayLeft.Store(r.replayTarget - r.exp)
			} else if s.replaying.Swap(false) {
				logInfo("历史回放结束")
			}
			_, total, _ := r.meta()
			if total != s.lastSeq.Load() {
				s.lastSeq.Store(total)
				s.lastChg.Store(time.Now().UnixNano())
			}
			time.Sleep(time.Duration(pollMs) * time.Millisecond)
		}
	}
}

// resolveDataFile data 目录内文件定位：优先相对 cwd，其次 exe 同级。
func resolveDataFile(dir, file string) string {
	if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
		return filepath.Join(dir, file)
	}
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), dir, file)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(dir, file) // 缺省路径（文件可后续创建）
}
