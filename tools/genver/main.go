// tools/genver: 由 VERSION（语义化版本，单一真源）刷新 versioninfo.json。
//
//	go run ./tools/genver           # 写入 versioninfo.json
//	go run ./tools/genver -check     # 只校验（不一致时非零退出，供 CI 用）
//
// VERSION 允许 `1.2.3` 或带预发布后缀 `1.2.3-rc.1`；Windows 四段版本号取前三段、Build 固定 0，
// 完整语义化字符串写入 StringFileInfo 的 FileVersion/ProductVersion。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	versionFile = "VERSION"
	infoFile    = "versioninfo.json"
)

var semverRe = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)(-[0-9A-Za-z.-]+)?$`)

type numVer struct {
	Major int `json:"Major"`
	Minor int `json:"Minor"`
	Patch int `json:"Patch"`
	Build int `json:"Build"`
}

type fixedFileInfo struct {
	FileVersion    numVer `json:"FileVersion"`
	ProductVersion numVer `json:"ProductVersion"`
	FileFlagsMask  string `json:"FileFlagsMask"`
	FileFlags      string `json:"FileFlags"`
	FileOS         string `json:"FileOS"`
	FileType       string `json:"FileType"`
	FileSubType    string `json:"FileSubType"`
}

type translation struct {
	LangID    string `json:"LangID"`
	CharsetID string `json:"CharsetID"`
}

type varFileInfo struct {
	Translation translation `json:"Translation"`
}

type versionInfo struct {
	FixedFileInfo  fixedFileInfo     `json:"FixedFileInfo"`
	StringFileInfo map[string]string `json:"StringFileInfo"`
	VarFileInfo    varFileInfo       `json:"VarFileInfo"`
	IconPath       string            `json:"IconPath"`
	ManifestPath   string            `json:"ManifestPath"`
}

func main() {
	check := flag.Bool("check", false, "只校验 versioninfo.json 是否与 VERSION 一致")
	flag.Parse()

	raw, err := os.ReadFile(versionFile)
	if err != nil {
		fail("%v", err)
	}
	ver := strings.TrimSpace(string(raw))
	m := semverRe.FindStringSubmatch(ver)
	if m == nil {
		fail("VERSION 内容不是合法语义化版本（期望 X.Y.Z 或 X.Y.Z-pre）：%q", ver)
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	patch, _ := strconv.Atoi(m[3])
	num := numVer{Major: major, Minor: minor, Patch: patch, Build: 0}

	body, err := os.ReadFile(infoFile)
	if err != nil {
		fail("%v", err)
	}
	var vi versionInfo
	if err := json.Unmarshal(body, &vi); err != nil {
		fail("versioninfo.json 解析失败: %v", err)
	}
	if vi.StringFileInfo == nil {
		vi.StringFileInfo = map[string]string{}
	}

	same := vi.FixedFileInfo.FileVersion == num && vi.FixedFileInfo.ProductVersion == num &&
		vi.StringFileInfo["FileVersion"] == ver && vi.StringFileInfo["ProductVersion"] == ver
	if *check {
		if !same {
			fmt.Fprintf(os.Stderr, "版本不一致：VERSION=%s，versioninfo.json(FileVersion=%s, fixed=%v)\n",
				ver, vi.StringFileInfo["FileVersion"], vi.FixedFileInfo.FileVersion)
			os.Exit(1)
		}
		fmt.Printf("版本一致：%s\n", ver)
		return
	}

	vi.FixedFileInfo.FileVersion = num
	vi.FixedFileInfo.ProductVersion = num
	vi.StringFileInfo["FileVersion"] = ver
	vi.StringFileInfo["ProductVersion"] = ver
	out, err := json.MarshalIndent(vi, "", "    ")
	if err != nil {
		fail("%v", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(infoFile, out, 0o644); err != nil {
		fail("%v", err)
	}
	if same {
		fmt.Printf("versioninfo.json 已是 %s，无变化\n", ver)
	} else {
		fmt.Printf("versioninfo.json ← %s（FixedFileInfo %d.%d.%d.0）\n", ver, major, minor, patch)
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
