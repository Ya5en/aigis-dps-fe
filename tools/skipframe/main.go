// skipframe —— 实验：klauspost/compress zstd 对可跳过帧（skippable frame）的行为。
// 1) 手工构造 [skippable帧][zstd帧] 流，klauspost 解码器能否透明跳过并还原；
// 2) 编码器 API 是否提供写可跳过帧的入口；
// 3) 只有可跳过帧、无 zstd 数据帧时解码器的反应。
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"

	kzstd "github.com/klauspost/compress/zstd"
)

const skipMagic = 0x184D2A50 // 0x50 2A 4D 18（0x184D2A50..0x184D2A5F 均合法）

// makeSkipFrame 手工编码一个可跳过帧：magic + LE32 长度 + 负载。
func makeSkipFrame(payload []byte) []byte {
	var b bytes.Buffer
	var magic [4]byte
	binary.LittleEndian.PutUint32(magic[:], skipMagic)
	b.Write(magic[:])
	var sz [4]byte
	binary.LittleEndian.PutUint32(sz[:], uint32(len(payload)))
	b.Write(sz[:])
	b.Write(payload)
	return b.Bytes()
}

func main() {
	raw := bytes.Repeat([]byte("hello-zstd-"), 100000) // ~1.1MB 明文

	enc, _ := kzstd.NewWriter(nil)
	defer enc.Close()
	var zf bytes.Buffer
	enc.Reset(&zf)
	enc.Write(raw)
	enc.Close()

	// 1) [skippable][zstd帧]
	stream := append(makeSkipFrame([]byte(`{"ver":1,"tool":"dpsmeter"}`)), zf.Bytes()...)
	// 真正走解码器：每次实验独立建 Reader 并绑定输入
	run := func(name string, data []byte) {
		d, err := kzstd.NewReader(bytes.NewReader(data))
		if err != nil {
			fmt.Println(name, "newreader:", err)
			return
		}
		defer d.Close()
		out, rerr := io.ReadAll(d)
		ok := rerr == nil && bytes.Equal(out, raw)
		fmt.Printf("%s: len(out)=%d err=%v roundtrip=%v\n", name, len(out), rerr, ok)
	}
	run("1) skippable+zstd帧", stream)

	// 2) 编码器 API：无公开的可跳过帧写入口（仅 WithEncoderPad 防对齐扰动用的随机 skippable）
	fmt.Println("2) Encoder 公开 API 无写 skippable 入口；源码 frameenc.go 有内部 skippableFrame()（WithEncoderPad 随机填充用）")

	// 3) 纯 skippable 流（无 zstd 数据帧）
	run("3) 纯skippable流", makeSkipFrame([]byte("meta-only")))
}
