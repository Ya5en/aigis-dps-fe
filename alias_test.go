package main

import (
	"encoding/json"
	"testing"
)

// allyUF 测试辅助：raw → 我方侧并查集（v 条目不参与我方侧）。
func allyUF(raw json.RawMessage) unionFind {
	a, _ := splitUnionFind(raw)
	return a
}

func TestParseAliasNamesPrefixed(t *testing.T) {
	raw := json.RawMessage(`{
		"a590":  "我方改名",
		"v8001": "敌人名",
		"a3001": "未来新单位",
		"x100":  "非法前缀",
		"a":     "无id",
		"v70x":  "id非数字",
		"a7002": ""
	}`)
	an, n := parseAliasNames(raw)
	if an.Ally["590"] != "我方改名" || an.Ally["3001"] != "未来新单位" {
		t.Fatal(`"a" 前缀应落入 Ally`)
	}
	if an.Enemy["8001"] != "敌人名" {
		t.Fatal(`"v" 前缀应落入 Enemy`)
	}
	if len(an.Ally) != 2 || len(an.Enemy) != 1 {
		t.Fatalf("Ally 应 2 条 / Enemy 应 1 条，实得 %d/%d", len(an.Ally), len(an.Enemy))
	}
	if n != 3 {
		t.Fatalf("有效条目应 3，实得 %d", n)
	}
}

func TestParseAliasNamesLeadingZero(t *testing.T) {
	an, _ := parseAliasNames(json.RawMessage(`{"a0590":"补零规范化"}`))
	if _, ok := an.Ally["590"]; !ok {
		t.Fatal("前导零应规范化为十进制")
	}
}

func TestParseAliasNamesEmpty(t *testing.T) {
	an, n := parseAliasNames(json.RawMessage(`{}`))
	if n != 0 || len(an.Ally)+len(an.Enemy) != 0 {
		t.Fatal("空配置应得空结果")
	}
	an2, _ := parseAliasNames(nil)
	if len(an2.Ally) != 0 {
		t.Fatal("nil 应得空 Ally")
	}
}

func TestUnionFindTargetAbsorbs(t *testing.T) {
	// "590": ["1469"]：把 1469 并入 590，590 自身实例也并入合一行
	u := allyUF(json.RawMessage(`{"590": ["1469"]}`))
	if !u.grouped(590) || !u.grouped(1469) {
		t.Fatal("目标与源都应 grouped")
	}
	if u.find(1469) != 590 {
		t.Fatal("源 find 应指向目标 590")
	}
}

func TestUnionFindSelfMergeOnly(t *testing.T) {
	// "590": []：空源列表 = 仅同 id 自合并
	u := allyUF(json.RawMessage(`{"590": []}`))
	if !u.grouped(590) {
		t.Fatal("自合并目标应 grouped")
	}
	if u.grouped(777) {
		t.Fatal("无关 id 不应受影响")
	}
}

func TestUnionFindSingleSourceString(t *testing.T) {
	// 简写 "590": "1469" 等价于 ["1469"]
	u := allyUF(json.RawMessage(`{"590": "1469"}`))
	if u.find(1469) != 590 || !u.grouped(590) {
		t.Fatal("单源字符串简写应生效")
	}
}

func TestUnionFindChain(t *testing.T) {
	// {"300":["100"], "200":["300"]}：100/300 都归到 200
	u := allyUF(json.RawMessage(`{"300": ["100"], "200": ["300"]}`))
	if r := u.find(100); r != 200 {
		t.Fatalf("链式 find(100)=%d 期望 200", r)
	}
	if !u.grouped(300) || !u.grouped(100) || !u.grouped(200) {
		t.Fatal("链中间与终点都应 grouped")
	}
}

func TestUnionFindCycleRejected(t *testing.T) {
	// 循环吸收 {"590":["1469"], "1469":["590"]}：第二条的边构成环 → 丢弃
	// 结果确定性：1469 归 590（先声明者优先），不依赖 map 迭代序
	u := allyUF(json.RawMessage(`{"590": ["1469"], "1469": ["590"]}`))
	if u.find(1469) != 590 {
		t.Fatalf("环边应被丢弃，find(1469)=%d 期望 590", u.find(1469))
	}
	if !u.grouped(590) || !u.grouped(1469) {
		t.Fatal("先声明的合并关系应保持")
	}
	// 自指 {"700": ["700"]}：自边忽略，但目标声明仍生效（等价于 "700": [] 自合并）
	u2 := allyUF(json.RawMessage(`{"700": ["700"]}`))
	if !u2.grouped(700) {
		t.Fatal("自指目标应视为自合并")
	}
	if len(u2.parent) != 0 {
		t.Fatal("自边不应产生归属映射")
	}
}

func TestUnionFindDuplicateSourceFirstWins(t *testing.T) {
	// 相同源多条规则：先声明者优先（1469 归 590 而非 600）
	u := allyUF(json.RawMessage(`{"590": ["1469"], "600": ["1469"]}`))
	if u.find(1469) != 590 {
		t.Fatalf("先声明者优先，find(1469)=%d 期望 590", u.find(1469))
	}
	if !u.grouped(600) {
		t.Fatal("600 作为目标仍自合并")
	}
	if _, ok := u.parent[1469]; !ok {
		t.Fatal("1469 应有一条归属边")
	}
}

func TestUnionFindInvalidEntries(t *testing.T) {
	// 非法目标/源/值类型各自忽略，不影响其余条目
	u := allyUF(json.RawMessage(`{
		"0":   ["100"],
		"abc": ["101"],
		"590": 123,
		"592": ["x", "0"]
	}`))
	if u.grouped(100) || u.grouped(101) {
		t.Fatal("非法目标条目的源不应进组")
	}
	if !u.grouped(590) || !u.grouped(592) {
		t.Fatal("合法目标即使源非法也应自合并")
	}
	if len(u.parent) != 0 {
		t.Fatalf("不应产生任何源映射，实得 %d", len(u.parent))
	}
}

func TestUnionFindUnrelated(t *testing.T) {
	u := allyUF(json.RawMessage(`{"590": ["1469"]}`))
	if u.grouped(777) {
		t.Fatal("无关 id 不应 grouped")
	}
}

func TestSplitUnionFindSides(t *testing.T) {
	// a/v 前缀分流：我方按 ai、敌方按 vi，规则一致、id 空间互不串扰
	ua, uv := splitUnionFind(json.RawMessage(`{
		"a590":  ["1469"],
		"v4755": "4756",
		"v8001": [],
		"590":   ["3001"]
	}`))
	if ua.find(1469) != 590 || ua.find(3001) != 590 {
		t.Fatalf("我方侧 find 失败：1469→%d 3001→%d", ua.find(1469), ua.find(3001))
	}
	if !ua.grouped(590) || !ua.grouped(1469) || !ua.grouped(3001) {
		t.Fatal("我方侧目标与源都应 grouped")
	}
	if uv.find(4756) != 4755 {
		t.Fatalf("敌方侧链应归到 4755，实得 %d", uv.find(4756))
	}
	if !uv.grouped(4755) || !uv.grouped(4756) || !uv.grouped(8001) {
		t.Fatal("敌方侧目标与源都应 grouped")
	}
	if ua.grouped(4756) || uv.grouped(1469) {
		t.Fatal("两侧 id 空间不应互通（同数字分属不同阵营互不影响）")
	}
}

func TestSplitUnionFindInvalidPrefix(t *testing.T) {
	// 非法前缀/非数字目标两侧都忽略；错阵营源前缀视为非法
	ua, uv := splitUnionFind(json.RawMessage(`{
		"x100":  ["200"],
		"a":     ["5"],
		"v70x":  ["9"],
		"a590":  ["v4756"],
		"v8001": ["a590"]
	}`))
	if ua.grouped(100) {
		t.Fatal("非法目标条目的源不应进我方组")
	}
	if !ua.grouped(590) {
		t.Fatal("合法目标即使源非法也应自合并")
	}
	if uv.grouped(70) {
		t.Fatal("非法敌方目标不应成组")
	}
	if !uv.grouped(8001) {
		t.Fatal("合法敌方目标即使源非法也应自合并")
	}
	if len(ua.parent)+len(uv.parent) != 0 {
		t.Fatalf("错阵营源不应产生归属映射，实得 %d+%d", len(ua.parent), len(uv.parent))
	}
}

func TestSplitUnionFindEnemyRules(t *testing.T) {
	// 敌方侧同样适用：链式、先声明优先、环丢弃
	ua, uv := splitUnionFind(json.RawMessage(`{
		"v4755": ["4756"],
		"v6000": ["4756"],
		"v300":  ["100"],
		"v200":  ["300"],
		"v4757": ["4755"],
		"v4755": ["4757"]
	}`))
	if uv.find(4756) != 4757 {
		t.Fatalf("先声明优先 + 链式：find(4756)=%d 期望 4757", uv.find(4756))
	}
	if uv.find(100) != 200 {
		t.Fatalf("敌方链式：find(100)=%d 期望 200", uv.find(100))
	}
	if uv.find(4755) != 4757 || uv.find(4757) != 4757 {
		t.Fatalf("环边应丢弃（先声明的 4755→4757 保留）：find(4755)=%d find(4757)=%d",
			uv.find(4755), uv.find(4757))
	}
	if ua.grouped(4755) {
		t.Fatal("敌方规则不应泄漏到我方侧")
	}
}

func TestParseMergeKey(t *testing.T) {
	if id, enemy, ok := parseMergeKey("a590"); !ok || enemy || id != 590 {
		t.Fatal(`"a590" 应解析为我方 590`)
	}
	if id, enemy, ok := parseMergeKey("v4755"); !ok || !enemy || id != 4755 {
		t.Fatal(`"v4755" 应解析为敌方 4755`)
	}
	if id, enemy, ok := parseMergeKey("590"); !ok || enemy || id != 590 {
		t.Fatal(`无前缀 "590" 应解析为我方 590`)
	}
	for _, k := range []string{"x100", "a", "v", "a70x", "v0", "0", ""} {
		if _, _, ok := parseMergeKey(k); ok {
			t.Fatalf("%q 应判非法", k)
		}
	}
}
