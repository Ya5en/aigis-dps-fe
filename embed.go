// embed.go —— 前端静态资源嵌入（web/dist 由 vite build 产出）。
// go:embed 要求目录存在：仓库内保留 web/dist/index.html 占位文件，
// vite build 会整目录重写。
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
)

//go:embed all:web/dist
var distFS embed.FS

func mustWebFS() http.FileSystem {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		log.Fatalf("[!] embed web/dist 失败: %v", err)
	}
	return http.FS(sub)
}
