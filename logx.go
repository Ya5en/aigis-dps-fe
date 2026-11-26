// logx.go —— 分级日志：INFO / WARNING / ERROR。
//
// 默认只输出 WARNING 与 ERROR（日志文件与 stderr 同步）；需要操作细节时用
// 启动参数 -log-level info（或 -v）放开 INFO。级别只影响是否打印，调用处
// 的语义不变：INFO 是正常里程碑，WARNING 是「降级但还能跑」，ERROR 是
// 「功能不可用 / 进程将退出」。
package main

import (
	"log"
	"strings"
)

type logLevel int

const (
	levelInfo logLevel = iota
	levelWarn
	levelError
)

// minLogLevel 当前阈值（默认 WARNING）。启动时由 -log-level / -v 设定。
var minLogLevel = levelWarn

// parseLogLevel 解析启动参数；未知值返回 false 由调用方报错退出。
func parseLogLevel(s string) (logLevel, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return levelInfo, true
	case "warn", "warning":
		return levelWarn, true
	case "error":
		return levelError, true
	}
	return levelWarn, false
}

func (l logLevel) String() string {
	switch l {
	case levelInfo:
		return "info"
	case levelError:
		return "error"
	}
	return "warning"
}

// logInfo 正常里程碑：启动、装载、保存、导入导出、在线刷新成功等。
func logInfo(format string, a ...any) {
	if minLogLevel > levelInfo {
		return
	}
	log.Printf("[INFO] "+format, a...)
}

// logWarn 可恢复的降级：沿用缓存、配置条目被忽略、缓存写盘失败等。
func logWarn(format string, a ...any) {
	if minLogLevel > levelWarn {
		return
	}
	log.Printf("[WARN] "+format, a...)
}

// logError 不可用状态：服务起不来、关键目录建不了、窗口初始化失败等。
func logError(format string, a ...any) {
	log.Printf("[ERROR] "+format, a...)
}

// logFatal 记录 ERROR 后退出（原 log.Fatalf 的替代）。
func logFatal(format string, a ...any) {
	log.Fatalf("[ERROR] "+format, a...)
}
