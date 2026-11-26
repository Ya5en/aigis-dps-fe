// main.go —— dpsmeter：aigis-dps 前端桥（SHM 只读消费 → 聚合 → 本地 HTTP/SSE）。
// 默认形态：overlay 小窗（无边框置顶、点击穿透、托盘控制）。
// 构建：go build -ldflags "-H windowsgui" -o dpsmeter.exe .
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// version 由构建注入（build.bat: -X main.version=<VERSION 内容>），未注入时为 dev。
// 语义化版本的真源是同目录 VERSION 文件。
var version = "dev"

func setupLog() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(filepath.Dir(exe), "dpsmeter.log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(f, os.Stderr))
}

func parseSize(s string) (int, int) {
	parts := strings.SplitN(s, "x", 2)
	w, _ := strconv.Atoi(parts[0])
	h := 420
	if len(parts) == 2 {
		h, _ = strconv.Atoi(parts[1])
	}
	if w < 120 || w > 4096 {
		w = 520
	}
	if h < 60 || h > 4096 {
		h = 420
	}
	return w, h
}

// ensureDataFiles 单文件分发兜底：数据目录与规则文件缺失时自动重建。
// 数据目录先建（fetch.go 写名字表缓存、编辑器保存规则都依赖它存在）；
// aliases.json 缺失时写入空模板（读侧本就静默容错，此处只为让编辑器
// 与配置文件开箱可见）。点阵缓存 dotcache 由 dot.go 写入前自建，不在此管。
func ensureDataFiles(namesPath, aliasPath string) {
	dir := filepath.Dir(aliasPath)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		logError("数据目录创建失败（%s）: %v", dir, err)
		return
	}
	if _, err := os.Stat(aliasPath); os.IsNotExist(err) {
		tpl := []byte("{\n  \"names\": {},\n  \"merge\": {}\n}\n")
		if err := os.WriteFile(aliasPath, tpl, 0o644); err != nil {
			logWarn("规则文件模板创建失败: %v", err)
		} else {
			logInfo("已创建规则文件模板：%s", aliasPath)
		}
	}
}

func main() {
	var (
		addr     = flag.String("addr", "127.0.0.1:9832", "HTTP 监听地址（仅本地）")
		data     = flag.String("data", "data", "数据目录（unit_names.json 所在）")
		pollMs   = flag.Int("poll", 1, "SHM 轮询间隔（毫秒）")
		headless = flag.Bool("headless", false, "仅启动本地服务，不创建窗口（浏览器访问面板）")
		size     = flag.String("size", "520x420", "overlay 窗口尺寸 WxH")
		showVer  = flag.Bool("version", false, "打印版本号后退出")
		logLvl   = flag.String("log-level", "warning", "日志级别：info / warning / error（默认 warning，只输出警告与错误）")
		verbose  = flag.Bool("v", false, "显示 INFO 日志（等价 -log-level info）")
	)
	flag.Parse()
	if *showVer {
		fmt.Println("dpsmeter " + version)
		return
	}
	lv, ok := parseLogLevel(*logLvl)
	if !ok {
		fmt.Fprintf(os.Stderr, "无效的 -log-level: %q（可选 info / warning / error）\n", *logLvl)
		os.Exit(2)
	}
	if *verbose {
		lv = levelInfo
	}
	minLogLevel = lv
	setupLog()

	namesPath := resolveDataFile(*data, "unit_names.json")
	aliasPath := resolveDataFile(*data, "aliases.json")
	ensureDataFiles(namesPath, aliasPath)
	go fetchAndCacheNames(namesPath) // 在线拉最新名字表 → 写缓存 → mtime 热载
	s := &server{
		agg:       newAgg(namesPath, aliasPath),
		hub:       newHub(),
		webFS:     mustWebFS(),
		dots:      newDotStore(filepath.Join(filepath.Dir(aliasPath), "dotcache")),
		subs:      newSubManager(filepath.Join(filepath.Dir(aliasPath), "subscriptions.json"),
			filepath.Join(filepath.Dir(aliasPath), "subcache"), nil),
		aliasPath: aliasPath,
	}
	logInfo("dpsmeter v%s 启动：http://%s（log-level=%s, names=%s, alias=%s）",
		version, *addr, minLogLevel, namesPath, aliasPath)
	go s.dots.refresh() // 在线拉最新 index → 抽取 EnemyDot 编号表（fetch.go 同构）
	go subLoop(s.subs)  // 订阅循环：启动刷一轮 + 配置 mtime/间隔触发的定时刷新

	go s.readerLoop(*pollMs)
	go s.publishLoop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- http.ListenAndServe(*addr, s.routes()) }()

	if *headless {
		if err := <-serveErr; err != nil {
			logFatal("HTTP 服务失败: %v", err)
		}
		for { // 纯服务形态：常驻直到进程被结束
			time.Sleep(time.Hour)
		}
	}

	// 默认 overlay 形态
	time.Sleep(300 * time.Millisecond) // 等服务起
	select {
	case err := <-serveErr:
		logFatal("HTTP 服务失败: %v", err)
	default:
	}
	w, h := parseSize(*size)
	runOverlay(*addr, w, h) // 阻塞于消息泵；窗口关闭即退出
}
