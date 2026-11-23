// bench —— 压缩算法基准：对真实 SHM 全量映像（16MB）对比各 Go 库的
// 压缩率 / 压缩速度 / 解压速度（导入重放路径同样吃解压速度，一并测）。
// 用法：go run ./tools/bench <raw.bin>；各算法跑 3 轮取中位数，解压后全量比对。
package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"sort"
	"time"

	kzstd "github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

type result struct {
	name   string
	dep    string
	size   int
	cms    float64 // 压缩中位数（ms）
	dms    float64 // 解压中位数（ms）
	failed bool
}

func median(xs []float64) float64 {
	sort.Float64s(xs)
	return xs[len(xs)/2]
}

// run 单算法基准：3 轮压缩+解压+全量比对，取中位数。
// 读端统一 io.Reader（lz4 Reader 无 Close；zstd/gzip 的 Close 对完整读非必需）。
func run(name, dep string, raw []byte,
	mkw func(io.Writer) (io.WriteCloser, error),
	mkr func(io.Reader) (io.Reader, error)) result {

	r := result{name: name, dep: dep}
	var cms, dms []float64
	for i := 0; i < 3; i++ {
		var buf bytes.Buffer
		t0 := time.Now()
		w, err := mkw(&buf)
		if err != nil {
			r.failed = true
			break
		}
		n, werr := w.Write(raw)
		cerr := w.Close()
		if werr != nil || cerr != nil || n != len(raw) {
			r.failed = true
			break
		}
		cms = append(cms, time.Since(t0).Seconds()*1000)

		t0 = time.Now()
		rd, err := mkr(bytes.NewReader(buf.Bytes()))
		if err != nil {
			r.failed = true
			break
		}
		out, rerr := io.ReadAll(rd)
		dms = append(dms, time.Since(t0).Seconds()*1000)
		if rerr != nil || !bytes.Equal(out, raw) {
			r.failed = true
			break
		}
		r.size = buf.Len()
	}
	if len(cms) > 0 {
		r.cms, r.dms = median(cms), median(dms)
	}
	return r
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: bench <raw.bin>")
		os.Exit(1)
	}
	raw, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	fmt.Printf("样本：%s（%d B）\n\n", os.Args[1], len(raw))

	mb := float64(len(raw)) / (1 << 20)
	var rs []result
	add := func(r result) { rs = append(rs, r) }

	// lz4（项目现有直接依赖）
	add(run("lz4 Fast(level0)", "pierrec/lz4/v4（现有依赖）", raw,
		func(w io.Writer) (io.WriteCloser, error) {
			zw := lz4.NewWriter(w)
			return zw, zw.Apply(lz4.CompressionLevelOption(lz4.Fast))
		},
		func(r io.Reader) (io.Reader, error) { return lz4.NewReader(r), nil }))
	add(run("lz4 CCompatFast", "pierrec/lz4/v4（现有依赖）", raw,
		func(w io.Writer) (io.WriteCloser, error) {
			zw := lz4.NewWriter(w)
			return zw, zw.Apply(lz4.CompressionLevelOption(lz4.CCompatFast))
		},
		func(r io.Reader) (io.Reader, error) { return lz4.NewReader(r), nil }))

	// zstd（纯 Go，本次新增依赖）
	for _, lv := range []struct {
		n string
		l kzstd.EncoderLevel
	}{
		{"zstd Fastest", kzstd.SpeedFastest},
		{"zstd Default", kzstd.SpeedDefault},
		{"zstd Best", kzstd.SpeedBestCompression},
	} {
		lvl := lv.l
		add(run(lv.n, "klauspost/compress（新增纯Go依赖）", raw,
			func(w io.Writer) (io.WriteCloser, error) {
				return kzstd.NewWriter(w, kzstd.WithEncoderLevel(lvl))
			},
			func(r io.Reader) (io.Reader, error) {
				return kzstd.NewReader(r)
			}))
	}

	// gzip（标准库，零依赖）
	add(run("gzip BestSpeed", "标准库", raw,
		func(w io.Writer) (io.WriteCloser, error) {
			return gzip.NewWriterLevel(w, gzip.BestSpeed)
		},
		func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }))
	add(run("gzip Default", "标准库", raw,
		func(w io.Writer) (io.WriteCloser, error) { return gzip.NewWriter(w), nil },
		func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }))
	add(run("gzip BestCompression", "标准库", raw,
		func(w io.Writer) (io.WriteCloser, error) {
			return gzip.NewWriterLevel(w, gzip.BestCompression)
		},
		func(r io.Reader) (io.Reader, error) { return gzip.NewReader(r) }))

	fmt.Printf("%-22s %-30s %11s %9s %9s %8s\n", "算法", "依赖", "大小", "压缩ms", "解压ms", "比率")
	for _, r := range rs {
		if r.failed {
			fmt.Printf("%-22s %-30s %11s\n", r.name, r.dep, "FAILED")
			continue
		}
		fmt.Printf("%-22s %-30s %10dB %9.1f %9.1f %7.2f%%\n",
			r.name, r.dep, r.size, r.cms, r.dms, float64(r.size)/mb)
	}
	fmt.Println("\n参考：raw =", int(mb), "MiB；压缩/解压速度可由 大小/时间 换算（MiB/s）")
}
