package platform

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
)

// 托盘图标在运行时画出来：一个圆角屏幕框，运行时里面是播放三角，暂停服务时是两条竖线。
// Mac 用黑色模板图（系统按深浅色菜单栏自动着色），Windows 用彩色图包成 ICO。

type shapeFunc func(x, y float64) bool // 坐标是 0~1

func roundRect(x0, y0, x1, y1, r float64) shapeFunc {
	return func(x, y float64) bool {
		if x < x0 || x > x1 || y < y0 || y > y1 {
			return false
		}
		cx := math.Max(x0+r, math.Min(x, x1-r))
		cy := math.Max(y0+r, math.Min(y, y1-r))
		return math.Hypot(x-cx, y-cy) <= r
	}
}

func triangle(ax, ay, bx, by, cx, cy float64) shapeFunc {
	side := func(px, py, qx, qy, x, y float64) float64 { return (qx-px)*(y-py) - (qy-py)*(x-px) }
	return func(x, y float64) bool {
		d1, d2, d3 := side(ax, ay, bx, by, x, y), side(bx, by, cx, cy, x, y), side(cx, cy, ax, ay, x, y)
		return !((d1 < 0 || d2 < 0 || d3 < 0) && (d1 > 0 || d2 > 0 || d3 > 0))
	}
}

func union(fs ...shapeFunc) shapeFunc {
	return func(x, y float64) bool {
		for _, f := range fs {
			if f(x, y) {
				return true
			}
		}
		return false
	}
}

func minus(a, b shapeFunc) shapeFunc {
	return func(x, y float64) bool { return a(x, y) && !b(x, y) }
}

type layer struct {
	shape shapeFunc
	col   color.NRGBA
}

// render 用 4×4 超采样画出带抗锯齿的图。
func render(size int, layers []layer) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, a float64
			for _, l := range layers {
				hit := 0
				for sy := 0; sy < ss; sy++ {
					for sx := 0; sx < ss; sx++ {
						if l.shape((float64(px)+(float64(sx)+0.5)/ss)/float64(size), (float64(py)+(float64(sy)+0.5)/ss)/float64(size)) {
							hit++
						}
					}
				}
				cov := float64(hit) / ss / ss * float64(l.col.A) / 255
				// 简单的"源在上"混合
				r = float64(l.col.R)*cov + r*(1-cov)
				g = float64(l.col.G)*cov + g*(1-cov)
				b = float64(l.col.B)*cov + b*(1-cov)
				a = cov + a*(1-cov)
			}
			if a > 0 {
				img.SetNRGBA(px, py, color.NRGBA{uint8(r / a), uint8(g / a), uint8(b / a), uint8(a * 255)})
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func glyph(paused bool) shapeFunc {
	if paused {
		return union(roundRect(0.34, 0.32, 0.45, 0.68, 0.02), roundRect(0.55, 0.32, 0.66, 0.68, 0.02))
	}
	return triangle(0.38, 0.30, 0.38, 0.70, 0.70, 0.50)
}

// trayTemplateIcon 是 Mac 菜单栏用的黑色模板图。
func TrayTemplateIcon(paused bool) []byte {
	frame := minus(roundRect(0.06, 0.14, 0.94, 0.86, 0.14), roundRect(0.14, 0.22, 0.86, 0.78, 0.07))
	black := color.NRGBA{0, 0, 0, 255}
	return render(36, []layer{{frame, black}, {glyph(paused), black}})
}

// trayWindowsIcon 是 Windows 通知区域用的彩色图标（ICO 格式，里面包一张 PNG）。
func TrayWindowsIcon(paused bool) []byte {
	bg := color.NRGBA{0x0a, 0x84, 0xff, 255}
	if paused {
		bg = color.NRGBA{0x8b, 0x8f, 0x98, 255}
	}
	pngData := render(32, []layer{
		{roundRect(0.04, 0.04, 0.96, 0.96, 0.2), bg},
		{glyph(paused), color.NRGBA{255, 255, 255, 255}},
	})
	return pngToICO(pngData, 32)
}

func pngToICO(pngData []byte, size int) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, 1}) // 保留、类型=图标、数量
	buf.Write([]byte{byte(size), byte(size), 0, 0})
	binary.Write(&buf, binary.LittleEndian, [2]uint16{1, 32}) // 色彩平面、位深
	binary.Write(&buf, binary.LittleEndian, [2]uint32{uint32(len(pngData)), 22})
	buf.Write(pngData)
	return buf.Bytes()
}
