package engine

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestBitmapBasicOps(t *testing.T) {
	b := NewBitmap(10)
	if b.Len() != 10 {
		t.Fatalf("Len() = %d, 期望 10", b.Len())
	}
	if b.Count() != 0 {
		t.Fatalf("初始 Count() = %d, 期望 0", b.Count())
	}
	for _, i := range []uint64{0, 3, 9} {
		if err := b.Set(i); err != nil {
			t.Fatalf("Set(%d) 失败: %v", i, err)
		}
	}
	if b.Count() != 3 {
		t.Fatalf("Count() = %d, 期望 3", b.Count())
	}
	if !b.Get(3) || b.Get(4) {
		t.Fatalf("Get 结果不符合预期")
	}
	if _, ok := b.FirstClear(); !ok {
		t.Fatalf("未完成时 FirstClear 应返回 true")
	}
	if err := b.Set(10); !errors.Is(err, ErrBitmapOverflow) {
		t.Fatalf("越界 Set 应返回 ErrBitmapOverflow, 实际 %v", err)
	}
	if b.Get(10) {
		t.Fatalf("越界 Get 应返回 false")
	}
	if err := b.Clear(3); err != nil {
		t.Fatalf("Clear 失败: %v", err)
	}
	if b.Count() != 2 {
		t.Fatalf("Clear 后 Count() = %d, 期望 2", b.Count())
	}
}

func TestBitmapRangesAndGaps(t *testing.T) {
	b := NewBitmap(16)
	b.SetRange(2, 5)  // [2,5)
	b.SetRange(9, 12) // [9,12)

	got := b.Ranges()
	want := [][2]uint64{{2, 5}, {9, 12}}
	if len(got) != len(want) {
		t.Fatalf("Ranges() = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Ranges() = %v, 期望 %v", got, want)
		}
	}

	gaps := b.Gaps()
	if len(gaps) != 3 || gaps[0] != [2]uint64{0, 2} || gaps[1] != [2]uint64{5, 9} || gaps[2] != [2]uint64{12, 16} {
		t.Fatalf("Gaps() = %v, 与预期不符", gaps)
	}

	if added := b.SetRange(2, 5); added != 0 {
		t.Fatalf("重复 SetRange 应新增 0 位，实际 %d", added)
	}
	// 越界区间应被裁剪而不是报错。
	if added := b.SetRange(14, 100); added != 2 {
		t.Fatalf("越界 SetRange 裁剪后应新增 2 位，实际 %d", added)
	}
}

func TestBitmapCompleteAndFirstClear(t *testing.T) {
	b := NewBitmap(8)
	b.SetRange(0, 7)
	if b.Complete() {
		t.Fatalf("缺少 1 位时不应判定完成")
	}
	if idx, ok := b.FirstClear(); !ok || idx != 7 {
		t.Fatalf("FirstClear() = (%d,%v), 期望 (7,true)", idx, ok)
	}
	b.Set(7)
	if !b.Complete() {
		t.Fatalf("全部置位后应判定完成")
	}
	if _, ok := b.FirstClear(); ok {
		t.Fatalf("全部置位时 FirstClear 应返回 false")
	}

	empty := NewBitmap(0)
	if empty.Complete() {
		t.Fatalf("零长度位图不应判定为完成")
	}
}

func TestBitmapJSONRoundTrip(t *testing.T) {
	b := NewBitmap(100)
	b.SetRange(0, 40)
	b.SetRange(60, 61)
	b.Set(99)

	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("Marshal 失败: %v", err)
	}

	var back Bitmap
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal 失败: %v", err)
	}
	if back.Len() != b.Len() {
		t.Fatalf("Len() = %d, 期望 %d", back.Len(), b.Len())
	}
	if back.Count() != b.Count() {
		t.Fatalf("Count() = %d, 期望 %d", back.Count(), b.Count())
	}
	for i := uint64(0); i < b.Len(); i++ {
		if back.Get(i) != b.Get(i) {
			t.Fatalf("第 %d 位往返后不一致", i)
		}
	}

	// nil 位图应序列化为 null 并可还原。
	rawNil, err := json.Marshal((*Bitmap)(nil))
	if err != nil {
		t.Fatalf("Marshal(nil) 失败: %v", err)
	}
	if string(rawNil) != "null" {
		t.Fatalf("Marshal(nil) = %s, 期望 null", rawNil)
	}
}

// 位图自描述长度是「元数据损坏可检测」的关键：被截断或不自洽的数据必须报错，
// 而不是被静默接受后算出错误的下载进度。
func TestBitmapJSONRejectsCorruptData(t *testing.T) {
	cases := map[string]string{
		"字节数不足": `{"n":100,"bits":"AAAA"}`,
		"字节数过多": `{"n":8,"bits":"AAAAAAAAAAAA"}`,
		"缺少位数":  `{"bits":"AAAA"}`,
		"缺省位数据但位数非零": `{"n":64}`,
		"非法类型":   `{"n":"many","bits":"AA"}`,
	}
	for name, raw := range cases {
		var b Bitmap
		if err := json.Unmarshal([]byte(raw), &b); err == nil {
			t.Errorf("%s: 期望解析失败，实际成功得到 %s", name, b.String())
		}
	}

	// 合法边界：位数不是 8 的整数倍时，byte 数向上取整（此处 9 位 → 2 字节）。
	var b Bitmap
	if err := json.Unmarshal([]byte(`{"n":9,"bits":"AAA="}`), &b); err != nil {
		t.Fatalf("合法数据解析失败: %v", err)
	}
	if b.Len() != 9 {
		t.Fatalf("Len() = %d, 期望 9", b.Len())
	}
}

func TestBitmapCloneIsDeep(t *testing.T) {
	src := NewBitmap(16)
	src.Set(1)
	cp := src.Clone()
	if err := cp.Set(2); err != nil {
		t.Fatalf("Set 失败: %v", err)
	}
	if src.Get(2) {
		t.Fatalf("Clone 不是深拷贝：修改副本影响了原对象")
	}
}
