// shm.go —— aigis-dps SHM 只读接入（协议移植自 recv/main.go，已与 DLL 写入协议对账）。
// 契约见 D:\prog\aigis-dps\docs\SHM_PROTOCOL.md（布局 v2，magic "DPS2"）。
// 红线：本侧只读，绝不写共享区。
package main

import (
	"encoding/binary"
	"fmt"
	"syscall"
	"unsafe"
)

// 协议常量：与 src/mfplay_proxy.c / SHM_PROTOCOL.md 严格一致。
const (
	shmName   = `Local\aigis_dps`
	shmTotal  = 0x1000000 // 16MB
	offHead   = 0x100000
	offBody   = 0x300000
	bodySize  = shmTotal - offBody
	headSlots = 131072
	magic     = 0x44505332 // "DPS2"
	hdrVer    = 2

	mOffEpoch    = 0x04
	mOffSeqTotal = 0x40
	mOffOldest   = 0x48
	mOffLastGf   = 0x30 // 最近一次读到的 GameFrame（战斗开始清零、暂停停走）

	slotSize = 16 // body_off / span / seq / reserved
)

var (
	k32          = syscall.NewLazyDLL("kernel32.dll")
	procOpenFile = k32.NewProc("OpenFileMappingW")
	procMapView  = k32.NewProc("MapViewOfFile")
	procUnmap    = k32.NewProc("UnmapViewOfFile")
	procClose    = k32.NewProc("CloseHandle")
)

// mapRead 以 PAGE_READONLY 映射整个 section（dwMaximumSize 传 0）。
func mapRead() ([]byte, uintptr, error) {
	name, _ := syscall.UTF16PtrFromString(shmName)
	h, _, e := procOpenFile.Call(0x0004 /*FILE_MAP_READ*/, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return nil, 0, fmt.Errorf("OpenFileMappingW 失败（writer 未启动？）: %v", e)
	}
	base, _, e := procMapView.Call(h, 0x0004 /*FILE_MAP_READ*/, 0, 0, 0)
	if base == 0 {
		procClose.Call(h)
		return nil, 0, fmt.Errorf("MapViewOfFile: %v", e)
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(base)), shmTotal), base, nil
}

func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// ev 一条伤害事件（payload JSON 字段）。
type ev struct {
	Gf uint32 // GameFrame
	D  uint32 // 伤害值
	Ty uint32 // 飘字颜色类型
	Ai uint32 // 攻击者 CardID
	Ag uint32 // 攻击者实例 gen
	Vi uint32 // 受击者身份 id（同 vi = 同一只怪；v10.5+ 恒有，旧 DLL 缺省 0）
	Vg uint32 // 受击者实体句柄 gen（每个 vg = 一个独立实体；v10.4+ 恒有，旧 DLL 缺省 0）
}

// Sink 聚合器回调接口。
type Sink interface {
	OnSessionStart(epoch uint32) // 新会话（首次接入 / epoch 变化 / gf 单调性破坏）
	OnClock(gf uint32)           // 战斗时钟推进（meta.last_gf，用于显示时间）
	OnEvent(e ev)                // 消费一条事件
	OnLost(n uint64, ranges int) // 丢失归因
}

// shmReader 私有状态只有 exp / lastEpoch / lastGf，符合协议 §8。
type shmReader struct {
	v, head, body []byte
	exp           uint32
	lastEpoch     uint32
	lastGf        uint32 // 单调性哨兵：last_gf 回退 = 新战斗
	sink          Sink

	// 历史回放（串行追赶）：replayTarget = attach 时刻的 seq_total 快照，
	// 游标追到该值即回放结束（之后的消费全是实时增量）。0 = 非回放态。
	replayTarget uint32
	// 事件级 gf 单调性哨兵：回放跨多场战斗时 meta.last_gf 不回退（writer
	// 只反映实时），分段改由事件序列本身检测——e.Gf 回退 = 战斗边界。
	lastEvGf  uint32
	haveEvGf  bool
}

func tryAttach(sink Sink) (*shmReader, error) {
	v, _, err := mapRead()
	if err != nil {
		return nil, err
	}
	if le32(v, 0) != magic {
		return nil, fmt.Errorf("magic 不符: got 0x%08X want 0x%08X（旧布局？）", le32(v, 0), magic)
	}
	r := &shmReader{v: v, head: v[offHead:], body: v[offBody:], sink: sink}
	r.lastEpoch = le32(v, mOffEpoch)
	return r, nil
}

// meta 当前元数据快照（供 /api/state）。
func (r *shmReader) meta() (epoch, seqTotal, oldest uint32) {
	return le32(r.v, mOffEpoch), le32(r.v, mOffSeqTotal), le32(r.v, mOffOldest)
}

// curGf 当前战斗时钟（meta.last_gf，战斗开始清零、暂停停走）。
func (r *shmReader) curGf() uint32 { return le32(r.v, mOffLastGf) }

// skipBacklog 启动策略（无历史可回放时）：游标直接对齐到当前 seq_total。
func (r *shmReader) skipBacklog() {
	_, total, _ := r.meta()
	if total > 0 {
		r.exp = total
	}
}

// startReplay 历史回放：游标对齐到环内最旧完整记录，追到 attach 快照为止。
func (r *shmReader) startReplay(target uint32) {
	r.exp = le32(r.v, mOffOldest)
	r.replayTarget = target
}

// poll 单轮消费。返回本轮消费条数。
// 关键时序与 recv/main.go 逐语句一致：先 oldest 再 total；slot.seq 判发布；
// body stamp 判 ABA；payload 复制出只读映射后再解析。
func (r *shmReader) poll() int {
	v := r.v
	if le32(v, 0) != magic {
		return 0 // writer 已退出 / 视图冻结
	}
	if e := le32(v, mOffEpoch); e != r.lastEpoch {
		r.lastEpoch = e
		r.exp = 0
		r.replayTarget = 0 // writer 重启作废全部历史
		r.haveEvGf = false
		r.lastGf = le32(v, mOffLastGf) // writer init 后 gf 从 0 走，对齐哨兵防误判
		r.sink.OnSessionStart(e)
	}

	// 战斗时钟 + 单调性检测：last_gf 战斗内单调不减，回退 = 新战斗开打
	//（同 epoch 下战斗重开也能检出，事件无需先到）。
	if gf := le32(v, mOffLastGf); gf != r.lastGf {
		reset := gf < r.lastGf
		r.lastGf = gf
		if reset {
			r.haveEvGf = false // 实时侧边界已由 meta 检出，事件级哨兵重新起算
			if r.replayTarget != 0 {
				// 回放期间实时侧开新战斗：剩余历史让位于实时，立即追平，
				// 避免旧战斗事件污染刚开的新会话（损失几条已发布的新战斗事件，可接受）
				r.exp = le32(v, mOffSeqTotal)
				r.replayTarget = 0
				return 0
			}
			r.sink.OnSessionStart(r.lastEpoch)
		}
		r.sink.OnClock(gf) // 回退后也同步时钟（新会话 clock 从 gf/60 起步）
	}

	oldest := le32(v, mOffOldest)
	total := le32(v, mOffSeqTotal)
	if total == 0 {
		return 0 // writer 尚未发布任何记录
	}

	if int32(oldest-r.exp) > 0 { // 覆盖发生：[exp, oldest) 已被覆写
		r.sink.OnLost(uint64(oldest-r.exp), 1) // 无符号减法，回绕安全
		r.exp = oldest
	}

	n := 0
	for r.exp != total {
		slot := r.head[uint64(r.exp)%headSlots*slotSize:]
		if le32(slot, 8) != r.exp {
			break // 未发布/竞态：下轮重试（正常等待态）
		}
		bodyOff := le32(slot, 0)
		span := le32(slot, 4)
		if span < 8 || span > 4096 || bodyOff+span > bodySize {
			break // 异常槽：未写完/竞态，下轮重试
		}
		if le32(r.body, int(bodyOff)) != r.exp {
			r.sink.OnLost(1, 1) // ABA：旧指针，视同丢失
			oldest = le32(v, mOffOldest)
			r.exp = oldest
			break
		}

		raw := r.body[bodyOff+4 : bodyOff+span]
		end := len(raw)
		for end > 0 && raw[end-1] == 0 {
			end-- // 去尾部 0 pad
		}
		// 关键：复制到独立缓冲，绝不在只读映射上 append/写
		line := append(make([]byte, 0, end), raw[:end]...)

		var e ev
		if err := jsonUnmarshalEv(line, &e); err == nil {
			// 事件级战斗分段：回放期间 meta.last_gf 不回退（writer 只反映实时），
			// 跨场边界由事件 gf 序列自身的回退检出（战斗内 gf 单调不减）。
			if r.haveEvGf && e.Gf < r.lastEvGf {
				r.sink.OnSessionStart(r.lastEpoch)
			}
			r.lastEvGf = e.Gf
			r.haveEvGf = true
			r.sink.OnEvent(e)
		}
		r.exp++
		n++
	}
	if r.replayTarget != 0 && r.exp == r.replayTarget {
		r.replayTarget = 0 // 追平 attach 快照，回放结束
	}
	return n
}

// jsonUnmarshalEv 解析 payload 中的 u32 业务字段。
// 手写迷你解析（无反射分配）：扫描 "key":数字 对即可；非数字值（对象/数组/
// 字符串/null）整段跳过，不报错——v10.53+ 载荷末尾新增 "ex":{...} 调试对象，
// 老写法遇到 '{' 会 errBadJSON 把整条事件丢弃（新 writer 条条带 ex = 全丢）。
func jsonUnmarshalEv(b []byte, out *ev) error {
	for i := 0; i < len(b); {
		if b[i] != '"' {
			i++
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			j++
		}
		if j >= len(b) {
			return errBadJSON
		}
		key := string(b[i+1 : j])
		k := j + 1
		for k < len(b) && (b[k] == ':' || b[k] == ' ') {
			k++
		}
		if k >= len(b) {
			break
		}
		// ex 等嵌套容器：整段跳过（含内部同名键，避免污染业务字段）
		if b[k] == '{' || b[k] == '[' {
			i = skipJSONContainer(b, k)
			continue
		}
		start := k
		for k < len(b) && b[k] >= '0' && b[k] <= '9' {
			k++
		}
		if k == start { // 非数字值：字符串/null/bool 等，跳到下个分隔符
			for k < len(b) && b[k] != ',' && b[k] != '}' {
				k++
			}
			i = k
			continue
		}
		num := uint32(0)
		for _, c := range b[start:k] {
			num = num*10 + uint32(c-'0')
		}
		switch key {
		case "gf":
			out.Gf = num
		case "d":
			out.D = num
		case "ty":
			out.Ty = num
		case "ai":
			out.Ai = num
		case "ag":
			out.Ag = num
		case "vi":
			out.Vi = num
		case "vg":
			out.Vg = num
		}
		i = k
	}
	return nil
}

// skipJSONContainer 从 '{' / '[' 起跳到配套闭合符之后；字符串内的括号不算层。
// 未闭合时返回末尾——后续已无字段可读，调用方循环自然结束。
func skipJSONContainer(b []byte, i int) int {
	depth, inStr, esc := 0, false, false
	for ; i < len(b); i++ {
		c := b[i]
		if inStr {
			if esc {
				esc = false
			} else if c == '\\' {
				esc = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return i
}

type jsonErr string

func (e jsonErr) Error() string { return string(e) }

const errBadJSON = jsonErr("bad payload json")
