package engine

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

// Bitmap 是定长的位图，用于记录「等长分块」的完成情况。
//
// 为什么用位图而不是「区间列表」：大文件下载过程中完成区间会高度碎片化，
// 区间列表的长度会随碎片数增长，而位图的空间是确定且紧凑的
// （1 个 1MB 分块只占 1 bit，100GB 文件约 12.8KB）。
//
// Bitmap 的零值不可用，必须通过 NewBitmap 创建或经 UnmarshalJSON 还原。
type Bitmap struct {
	n    uint64
	bits []byte
}

// ErrBitmapOverflow 表示访问了下标越界的位。
var ErrBitmapOverflow = errors.New("bitmap: 位下标越界")

// NewBitmap 创建可容纳 n 位的全零位图。
func NewBitmap(n uint64) *Bitmap {
	return &Bitmap{n: n, bits: make([]byte, (n+7)/8)}
}

// BitmapFromBytes 用给定的原始位数据构造位图，n 为有效位数。
func BitmapFromBytes(n uint64, raw []byte) (*Bitmap, error) {
	if uint64(len(raw))*8 < n {
		return nil, fmt.Errorf("bitmap: 原始数据 %d 字节不足以表示 %d 位", len(raw), n)
	}
	cp := make([]byte, (n+7)/8)
	copy(cp, raw)
	return &Bitmap{n: n, bits: cp}, nil
}

// Len 返回位数。
func (b *Bitmap) Len() uint64 {
	if b == nil {
		return 0
	}
	return b.n
}

// Set 置位第 i 位。越界返回 ErrBitmapOverflow。
func (b *Bitmap) Set(i uint64) error {
	if i >= b.n {
		return ErrBitmapOverflow
	}
	b.bits[i/8] |= 1 << (i % 8)
	return nil
}

// Clear 清零第 i 位。越界返回 ErrBitmapOverflow。
func (b *Bitmap) Clear(i uint64) error {
	if i >= b.n {
		return ErrBitmapOverflow
	}
	b.bits[i/8] &^= 1 << (i % 8)
	return nil
}

// Get 返回第 i 位是否置位；越界返回 false。
func (b *Bitmap) Get(i uint64) bool {
	if b == nil || i >= b.n {
		return false
	}
	return b.bits[i/8]&(1<<(i%8)) != 0
}

// SetRange 置位 [start, end) 区间，返回实际置位的新增数量。
// 越界部分会被裁剪，不返回错误，便于按分块粒度批量标记。
func (b *Bitmap) SetRange(start, end uint64) uint64 {
	if b == nil || start >= end {
		return 0
	}
	if end > b.n {
		end = b.n
	}
	var added uint64
	for i := start; i < end; i++ {
		if !b.Get(i) {
			_ = b.Set(i)
			added++
		}
	}
	return added
}

// Count 返回已置位的位数。
func (b *Bitmap) Count() uint64 {
	if b == nil {
		return 0
	}
	var n uint64
	for _, by := range b.bits {
		n += uint64(popcount8(by))
	}
	return n
}

// Complete 表示是否所有位都已置位。
func (b *Bitmap) Complete() bool {
	return b != nil && b.n > 0 && b.Count() == b.n
}

// FirstClear 返回第一个未置位的下标；全部完成时返回 (0, false)。
func (b *Bitmap) FirstClear() (uint64, bool) {
	if b == nil {
		return 0, false
	}
	for i := uint64(0); i < b.n; i++ {
		if !b.Get(i) {
			return i, true
		}
	}
	return 0, false
}

// Ranges 返回已置位的连续区间列表 [start, end)，用于「按缺口续下」。
func (b *Bitmap) Ranges() [][2]uint64 {
	if b == nil || b.n == 0 {
		return nil
	}
	var out [][2]uint64
	var i uint64
	for i < b.n {
		if !b.Get(i) {
			i++
			continue
		}
		start := i
		for i < b.n && b.Get(i) {
			i++
		}
		out = append(out, [2]uint64{start, i})
	}
	return out
}

// Gaps 返回未置位的连续区间列表，即「待下载缺口」。
func (b *Bitmap) Gaps() [][2]uint64 {
	if b == nil || b.n == 0 {
		return nil
	}
	var out [][2]uint64
	var i uint64
	for i < b.n {
		if b.Get(i) {
			i++
			continue
		}
		start := i
		for i < b.n && !b.Get(i) {
			i++
		}
		out = append(out, [2]uint64{start, i})
	}
	return out
}

// Clone 返回深拷贝。
func (b *Bitmap) Clone() *Bitmap {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b.bits))
	copy(cp, b.bits)
	return &Bitmap{n: b.n, bits: cp}
}

// Bytes 返回底层位的拷贝，调用方不能修改位图内部状态。
func (b *Bitmap) Bytes() []byte {
	if b == nil {
		return nil
	}
	cp := make([]byte, len(b.bits))
	copy(cp, b.bits)
	return cp
}

// jsonBitmap 是 Bitmap 的可序列化表示。位数据用 base64 编码（[]byte 的 JSON 默认行为）。
// 带上 n 使位图自描述，反序列化后无需外部再补齐长度——这是「元数据损坏可检测」的基础：
// 位数与字节数不匹配（被截断/被篡改）会直接报错，而不是静默算出错误进度。
type jsonBitmap struct {
	N    uint64 `json:"n"`
	Bits []byte `json:"bits,omitempty"`
}

// MarshalJSON 实现 json.Marshaler。
func (b *Bitmap) MarshalJSON() ([]byte, error) {
	if b == nil {
		return []byte("null"), nil
	}
	return json.Marshal(jsonBitmap{N: b.n, Bits: b.bits})
}

// UnmarshalJSON 实现 json.Unmarshaler。
func (b *Bitmap) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		return nil
	}
	var jb jsonBitmap
	if err := json.Unmarshal(data, &jb); err != nil {
		return fmt.Errorf("bitmap: JSON 解析失败: %w", err)
	}
	want := (jb.N + 7) / 8
	if uint64(len(jb.Bits)) != want {
		return fmt.Errorf("bitmap: 位数据长度 %d 字节与位数 %d 不匹配（期望 %d 字节）",
			len(jb.Bits), jb.N, want)
	}
	b.n = jb.N
	b.bits = jb.Bits
	return nil
}

// String 返回便于日志输出的紧凑描述，包含进度百分比。
func (b *Bitmap) String() string {
	if b == nil {
		return "bitmap(nil)"
	}
	if b.n == 0 {
		return "bitmap(0/0)"
	}
	c := b.Count()
	return fmt.Sprintf("bitmap(%d/%d, %.2f%%)", c, b.n, float64(c)*100/float64(b.n))
}

// EncodeBase64 返回位数据的 base64 字符串（调试与 CSV 导出用）。
func (b *Bitmap) EncodeBase64() string {
	if b == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b.bits)
}

func popcount8(x byte) int {
	n := 0
	for x != 0 {
		x &= x - 1
		n++
	}
	return n
}
