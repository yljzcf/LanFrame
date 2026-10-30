package platform

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// 托盘图标和程序图标都在代码里画出来：一个对话气泡，运行时里面是播放三角，暂停服务时是两条竖线。
// 托盘在 Mac 上用黑色模板图（系统按深浅色菜单栏自动着色）；Windows 的任务栏可能是深色也可能是浅色，
// 所以和程序图标一样画在蓝底（暂停时灰底）圆角方块上，包成 ICO。
// 程序图标由 tools/appicon 在打包时生成，网页的 favicon 也用它。

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

// within 把一个 0~1 坐标的图形缩放到 (x0, y0)~(x1, y1) 这块区域里。
func within(f shapeFunc, x0, y0, x1, y1 float64) shapeFunc {
	return func(x, y float64) bool { return f((x-x0)/(x1-x0), (y-y0)/(y1-y0)) }
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

// polygon 是一圈点围成的多边形。
func polygon(pts [][2]float64) shapeFunc {
	return func(x, y float64) bool {
		in := false
		for i, j := 0, len(pts)-1; i < len(pts); j, i = i, i+1 {
			a, b := pts[i], pts[j]
			if (a[1] > y) != (b[1] > y) && x < (b[0]-a[0])*(y-a[1])/(b[1]-a[1])+a[0] {
				in = !in
			}
		}
		return in
	}
}

// sdRoundBox 是点到圆角矩形边缘的距离，在里面是负数。按它画出的轮廓粗细均匀。
func sdRoundBox(x, y, x0, y0, x1, y1, r float64) float64 {
	qx := math.Abs(x-(x0+x1)/2) - (x1-x0)/2 + r
	qy := math.Abs(y-(y0+y1)/2) - (y1-y0)/2 + r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

// 对话气泡的主体（圆角矩形）和线宽
const bubbleX0, bubbleY0, bubbleX1, bubbleY1, bubbleR, bubbleStroke = 0.06, 0.08, 0.94, 0.76, 0.28, 0.08

// bubble 是对话气泡的白色轮廓：主体是圆角矩形，尾巴从左下的圆角斜着伸出去。
// 外轮廓用一圈点围出来，拐角用二次贝塞尔曲线做圆滑过渡；空心部分是主体往里缩一个线宽，尾巴是实心的。
func bubble() shapeFunc {
	x0, y0, x1, y1, r := bubbleX0, bubbleY0, bubbleX1, bubbleY1, bubbleR
	var pts [][2]float64
	arcPt := func(cx, cy, deg float64) [2]float64 {
		t := deg * math.Pi / 180
		return [2]float64{cx + r*math.Cos(t), cy + r*math.Sin(t)}
	}
	arc := func(cx, cy, from, to float64) {
		for i := 0; i <= 16; i++ {
			pts = append(pts, arcPt(cx, cy, from+(to-from)*float64(i)/16))
		}
	}
	curve := func(p0, c, p1 [2]float64) {
		for i := 0; i <= 12; i++ {
			t := float64(i) / 12
			u := 1 - t
			pts = append(pts, [2]float64{u*u*p0[0] + 2*u*t*c[0] + t*t*p1[0], u*u*p0[1] + 2*u*t*c[1] + t*t*p1[1]})
		}
	}
	toward := func(from, to [2]float64, d float64) [2]float64 {
		dx, dy := to[0]-from[0], to[1]-from[1]
		l := math.Hypot(dx, dy)
		return [2]float64{from[0] + dx/l*d, from[1] + dy/l*d}
	}
	tip, join := [2]float64{0.13, 0.96}, [2]float64{0.44, y1} // 尾巴尖，尾巴右边从底边伸出的位置
	cx, cy := x0+r, y1-r                                      // 左下圆角的圆心
	left := arcPt(cx, cy, 112)                                // 尾巴左边接到左下圆角上的位置
	arc(x0+r, y0+r, 180, 270)
	arc(x1-r, y0+r, 270, 360)
	arc(x1-r, y1-r, 0, 90)
	curve([2]float64{join[0] + 0.05, y1}, join, toward(join, tip, 0.05))
	curve(toward(tip, join, 0.03), tip, toward(tip, left, 0.03))
	curve(toward(left, tip, 0.04), left, arcPt(cx, cy, 122))
	arc(cx, cy, 122, 180)
	outer := polygon(pts)
	return func(x, y float64) bool {
		// 先排除空心部分和外面，多边形判断比较慢
		if x < x0 || x > x1 || y < y0 || y > tip[1] || sdRoundBox(x, y, x0, y0, x1, y1, r) <= -bubbleStroke {
			return false
		}
		return outer(x, y)
	}
}

// logo 是对话气泡加上里面的播放三角（或暂停竖线），三角挪到气泡主体的正中。
func logo(paused bool) shapeFunc {
	mid := (bubbleY0 + bubbleY1) / 2
	return union(bubble(), within(glyph(paused), 0, mid-0.5, 1, mid+0.5))
}

var (
	blue  = color.NRGBA{0x0a, 0x84, 0xff, 255}
	white = color.NRGBA{255, 255, 255, 255}
)

// TrayTemplateIcon 是 Mac 菜单栏用的黑色模板图。
func TrayTemplateIcon(paused bool) []byte {
	return render(36, []layer{{logo(paused), color.NRGBA{0, 0, 0, 255}}})
}

// badge 是圆角方块底色上一个白色的对话气泡，里面是播放三角（暂停时是两条竖线）。
// m 是方块四周的留白，r 是方块的圆角，pad 是气泡和方块边缘的距离，都按图标边长的比例算。
func badge(bg color.NRGBA, paused bool, m, r, pad float64) []layer {
	return []layer{
		{roundRect(m, m, 1-m, 1-m, r), bg},
		{within(logo(paused), m+pad, m+pad, 1-m-pad, 1-m-pad), white},
	}
}

// AppIcon 是程序图标：蓝底圆角方块里一个白色的对话气泡和播放三角，和托盘图标是同一套图形。
// mac 为真时按 Apple 的图标网格四周留白（主体占 824/1024），否则只留很窄的边，给 Windows 和网页用。
func AppIcon(size int, mac bool) []byte {
	if mac {
		return render(size, badge(blue, false, 0.1, 0.18, 0.1))
	}
	return render(size, badge(blue, false, 0.04, 0.2, 0.08))
}

// windowsTraySizes 是 Windows 托盘图标里放的几种尺寸：通知区域在 100%～200% 缩放下显示 16～32 像素，
// 而托盘库（systray）按"大图标"的尺寸加载（没声明支持高分屏的程序固定是 32），系统再缩小显示。
var windowsTraySizes = []int{16, 20, 24, 32, 40, 48, 64}

// trayWindowsIcons 缓存运行和暂停两种图标，每张都要画七个尺寸，不用每次刷新托盘都重画。
var trayWindowsIcons = map[bool]func() []byte{
	false: sync.OnceValue(func() []byte { return trayWindowsICO(false) }),
	true:  sync.OnceValue(func() []byte { return trayWindowsICO(true) }),
}

// TrayWindowsIcon 是 Windows 通知区域用的图标（ICO 格式）：和程序图标一样的蓝底白色对话气泡，
// 暂停服务时底色变灰、三角换成两条竖线。托盘图标很小，气泡比程序图标留的边更少，尽量画大。
func TrayWindowsIcon(paused bool) []byte { return trayWindowsIcons[paused]() }

func trayWindowsICO(paused bool) []byte {
	bg := blue
	if paused {
		bg = color.NRGBA{0x8b, 0x8f, 0x98, 255}
	}
	pngs := make([][]byte, len(windowsTraySizes))
	for i, size := range windowsTraySizes {
		pngs[i] = render(size, badge(bg, paused, 0.02, 0.2, 0.05))
	}
	return pngsToICO(windowsTraySizes, pngs)
}

// pngsToICO 把几张不同尺寸的 PNG 包成一个 ICO 文件，Windows 加载时挑最合适的一张。
func pngsToICO(sizes []int, pngs [][]byte) []byte {
	var buf bytes.Buffer
	binary.Write(&buf, binary.LittleEndian, [3]uint16{0, 1, uint16(len(pngs))}) // 保留、类型=图标、数量
	offset := 6 + 16*len(pngs)                                                  // 文件头 6 字节，每张图的目录项 16 字节
	for i, data := range pngs {
		buf.Write([]byte{byte(sizes[i]), byte(sizes[i]), 0, 0})   // 宽、高（256 记成 0）、调色板、保留
		binary.Write(&buf, binary.LittleEndian, [2]uint16{1, 32}) // 色彩平面、位深
		binary.Write(&buf, binary.LittleEndian, [2]uint32{uint32(len(data)), uint32(offset)})
		offset += len(data)
	}
	for _, data := range pngs {
		buf.Write(data)
	}
	return buf.Bytes()
}
