package main

import "testing"

func TestJSONUnmarshalEvFormats(t *testing.T) {
	cases := []struct {
		name string
		line string
		want ev
	}{
		{
			name: "旧格式（无 ex，vg 在 vi 前）",
			line: `{"gf":172,"ai":357,"ag":9453,"vg":9436,"vi":2250,"d":883,"ty":1}`,
			want: ev{Gf: 172, Ai: 357, Ag: 9453, Vg: 9436, Vi: 2250, D: 883, Ty: 1},
		},
		{
			name: "新格式（ex 空对象在末尾）",
			line: `{"gf":347,"ai":2187,"ag":702,"vi":3389,"vg":752,"d":1316,"ty":0,"ex":{}}`,
			want: ev{Gf: 347, Ai: 2187, Ag: 702, Vi: 3389, Vg: 752, D: 1316, Ty: 0},
		},
		{
			name: "新格式（ex 带具名字段与嵌套同名键，不得污染业务字段）",
			line: `{"gf":9,"ai":8,"ag":7,"vi":6,"vg":5,"d":4,"ty":3,"ex":{"why":16,"src":2,"ai":999,"nested":{"d":777,"vi":666},"note":"a,b}"}}`,
			want: ev{Gf: 9, Ai: 8, Ag: 7, Vi: 6, Vg: 5, D: 4, Ty: 3},
		},
		{
			name: "ex 在中间且含数组",
			line: `{"gf":2,"ex":{"tags":[1,2,{"ai":55}]},"ai":3,"d":4}`,
			want: ev{Gf: 2, Ai: 3, D: 4},
		},
		{
			name: "未知非数字值不得误判",
			line: `{"gf":11,"extra":"x,y","flag":null,"ai":12,"d":13}`,
			want: ev{Gf: 11, Ai: 12, D: 13},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got ev
			if err := jsonUnmarshalEv([]byte(c.line), &got); err != nil {
				t.Fatalf("解析失败: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}
}
