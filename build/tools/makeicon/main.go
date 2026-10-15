// Command makeicon 生成应用图标 build/icon.ico。
//
// 为什么要用程序生成而不是直接提交一个二进制图标：
//   - 图标可以被审阅、被调整（改颜色/尺寸只需改几行代码）；
//   - 不引入来源不明的二进制资源；
//   - 打包脚本可以随时重新生成，产物可复现。
//
// 输出为 ICO 容器内嵌 PNG（Vista 及以上原生支持）。
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// sizes 是 ICO 中包含的尺寸。多尺寸让 Windows 在任务栏、资源管理器、
// Alt-Tab 等不同场景下都能取到合适的位图，而不是靠系统缩放导致模糊。
var sizes = []int{16, 32, 48, 64, 128, 256}

func main() {
	out := "build/icon.ico"
	if len(os.Args) > 1 {
		out = os.Args[1]
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fail(err)
	}
	if err := writeICO(out, sizes); err != nil {
		fail(err)
	}
	fmt.Printf("已生成 %s（尺寸 %v）\n", out, sizes)
}

func fail(err error) {
	fmt.Fprintf(os.Stderr, "makeicon: %v\n", err)
	os.Exit(1)
}

// writeICO 按 ICO 规范写出多尺寸图标。
//
// 文件结构（全部小端）：
//
//	ICONDIR      reserved=0, type=1, count=N
//	ICONDIRENTRY × N  每项 16 字节，含尺寸、大小与数据偏移
//	图像数据 × N      PNG 字节流
func writeICO(path string, sizes []int) error {
	images := make([][]byte, len(sizes))
	for i, size := range sizes {
		encoded, err := encodePNG(draw(size))
		if err != nil {
			return err
		}
		images[i] = encoded
	}

	var buf bytes.Buffer
	// ICONDIR
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(len(sizes)))

	offset := 6 + 16*len(sizes)
	for i, size := range sizes {
		// ICONDIRENTRY：宽高为 0 表示 256
		dim := byte(size)
		if size >= 256 {
			dim = 0
		}
		_ = binary.Write(&buf, binary.LittleEndian, dim)  // width
		_ = binary.Write(&buf, binary.LittleEndian, dim)  // height
		_ = binary.Write(&buf, binary.LittleEndian, uint8(0)) // color count
		_ = binary.Write(&buf, binary.LittleEndian, uint8(0)) // reserved
		_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // planes
		_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bit count
		_ = binary.Write(&buf, binary.LittleEndian, uint32(len(images[i])))
		_ = binary.Write(&buf, binary.LittleEndian, uint32(offset))
		offset += len(images[i])
	}
	for _, data := range images {
		buf.Write(data)
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// draw 绘制图标：圆角蓝色方块 + 白色下载箭头。
func draw(size int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	scale := float64(size) / 256.0

	bg := color.RGBA{R: 0x25, G: 0x63, B: 0xEB, A: 0xFF}
	radius := 48.0 * scale

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			if insideRoundedRect(float64(x)+0.5, float64(y)+0.5, float64(size), radius) {
				img.Set(x, y, bg)
			}
		}
	}

	white := color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	// 竖杆
	shaftX0, shaftX1 := 112.0*scale, 144.0*scale
	shaftY0, shaftY1 := 48.0*scale, 150.0*scale
	// 箭头三角
	triTopY := 132.0*scale
	triLeftX, triRightX := 70.0*scale, 186.0*scale
	triBottomY := 196.0*scale
	// 底座横线
	baseY0, baseY1 := 204.0*scale, 220.0*scale
	baseX0, baseX1 := 64.0*scale, 192.0*scale

	fillRect(img, shaftX0, shaftY0, shaftX1, shaftY1, white)
	fillTriangle(img, triLeftX, triTopY, triRightX, triTopY, (triLeftX+triRightX)/2, triBottomY, white)
	fillRect(img, baseX0, baseY0, baseX1, baseY1, white)
	return img
}

func insideRoundedRect(x, y, size, radius float64) bool {
	if x < 0 || y < 0 || x > size || y > size {
		return false
	}
	// 四个角的圆心
	corners := [][2]float64{
		{radius, radius},
		{size - radius, radius},
		{radius, size - radius},
		{size - radius, size - radius},
	}
	insideX := x >= radius && x <= size-radius
	insideY := y >= radius && y <= size-radius
	if insideX || insideY {
		return x >= 0 && x <= size && y >= 0 && y <= size
	}
	for _, c := range corners {
		dx, dy := x-c[0], y-c[1]
		if dx*dx+dy*dy <= radius*radius {
			return true
		}
	}
	return false
}

func fillRect(img *image.RGBA, x0, y0, x1, y1 float64, c color.RGBA) {
	b := img.Bounds()
	for y := int(y0); y < int(y1) && y < b.Max.Y; y++ {
		if y < 0 {
			continue
		}
		for x := int(x0); x < int(x1) && x < b.Max.X; x++ {
			if x < 0 {
				continue
			}
			img.Set(x, y, c)
		}
	}
}

// fillTriangle 用重心坐标判断像素是否落在三角形内部。
func fillTriangle(img *image.RGBA, x0, y0, x1, y1, x2, y2 float64, c color.RGBA) {
	b := img.Bounds()
	minX := int(min3(x0, x1, x2))
	maxX := int(max3(x0, x1, x2)) + 1
	minY := int(min3(y0, y1, y2))
	maxY := int(max3(y0, y1, y2)) + 1

	denom := (y1-y2)*(x0-x2) + (x2-x1)*(y0-y2)
	if denom == 0 {
		return
	}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			if x < 0 || y < 0 || x >= b.Max.X || y >= b.Max.Y {
				continue
			}
			px, py := float64(x)+0.5, float64(y)+0.5
			a := ((y1-y2)*(px-x2) + (x2-x1)*(py-y2)) / denom
			bw := ((y2-y0)*(px-x2) + (x0-x2)*(py-y2)) / denom
			cw := 1 - a - bw
			if a >= 0 && bw >= 0 && cw >= 0 {
				img.Set(x, y, c)
			}
		}
	}
}

func min3(a, b, c float64) float64 { return min2(min2(a, b), c) }
func max3(a, b, c float64) float64 { return max2(max2(a, b), c) }
func min2(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}
func max2(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
