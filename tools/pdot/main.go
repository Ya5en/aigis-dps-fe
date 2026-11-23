// tools/pdot: 把单个 PlayerDot/EnemyDot aar（已解 gzip）里的图集落成 PNG。
// 只做「解包 + 图集」，不解析 ATX 分组与 AOD 动画。
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"

	"github.com/pierrec/lz4/v4"
)

type aarEntry struct {
	Name string
	Data []byte
}

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
	out := make([]aarEntry, 0, count)
	for _, off := range offs {
		if off+18 > len(b) {
			return nil, errors.New("entry offset out of range")
		}
		start := binary.LittleEndian.Uint32(b[off+4:])
		size := binary.LittleEndian.Uint32(b[off+8:])
		np := off + 18
		if np > len(b) {
			return nil, errors.New("name offset out of range")
		}
		end := bytes.IndexByte(b[np:], 0)
		if end < 0 {
			end = len(b) - np
		}
		name := string(b[np : np+end])
		if int64(start)+int64(size) > int64(len(b)) {
			return nil, fmt.Errorf("entry %q data out of range", name)
		}
		out = append(out, aarEntry{Name: name, Data: b[start : start+size]})
	}
	return out, nil
}

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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: pdot <x.aar|decompressed.bin> [out.png]")
		os.Exit(2)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if len(raw) > 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, _ := os.Open(os.Args[1])
		_ = zr
		fmt.Fprintln(os.Stderr, "输入仍是 gzip，请先 gunzip")
		os.Exit(1)
	}
	entries, err := unpackAAR(raw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "unpack:", err)
		os.Exit(1)
	}
	fmt.Printf("条目 %d 个：\n", len(entries))
	var atx []byte
	atxName := ""
	for _, e := range entries {
		fmt.Printf("  %-24s %8d B  magic=%q\n", e.Name, len(e.Data), string(e.Data[:min(4, len(e.Data))]))
		l := len(e.Name)
		if l > 4 && e.Name[l-4:] == ".atx" && atx == nil {
			atx, atxName = e.Data, e.Name
		}
	}
	if atx == nil {
		fmt.Fprintln(os.Stderr, "包内无 .atx")
		os.Exit(1)
	}
	inner, err := atxInner(atx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "atxInner:", err)
		os.Exit(1)
	}
	fmt.Printf("ALTX inner: %d B（%q）\n", len(inner), string(inner[:4]))
	img, err := decodeALTX(inner)
	if err != nil {
		fmt.Fprintln(os.Stderr, "decodeALTX:", err)
		os.Exit(1)
	}
	b := img.Bounds()
	out := os.Args[2]
	if out == "" {
		out = filepath.Join(filepath.Dir(os.Args[1]), atxName+".png")
	}
	f, err := os.Create(out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	f.Close()
	st, _ := os.Stat(out)
	fmt.Printf("已写出 %s（%dx%d，%d B）\n", out, b.Dx(), b.Dy(), st.Size())
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
