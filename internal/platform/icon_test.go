package platform

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"testing"
)

func TestAppIcon(t *testing.T) {
	for _, mac := range []bool{true, false} {
		for _, size := range []int{16, 64, 256, 1024} {
			img, err := png.Decode(bytes.NewReader(AppIcon(size, mac)))
			if err != nil {
				t.Fatal(err)
			}
			if b := img.Bounds(); b.Dx() != size || b.Dy() != size {
				t.Fatalf("尺寸 %d 的图标实际是 %v", size, b)
			}
			if size < 64 {
				continue // 太小时边缘都是抗锯齿的过渡色，不查颜色
			}
			at := func(fx, fy float64) color.NRGBA {
				return color.NRGBAModel.Convert(img.At(int(fx*float64(size)), int(fy*float64(size)))).(color.NRGBA)
			}
			if c := at(0, 0); c.A != 0 {
				t.Errorf("mac=%v size=%d：左上角应该透明，实际 %v", mac, size, c)
			}
			if c := at(0.3, 0.5); c != blue {
				t.Errorf("mac=%v size=%d：气泡里面应该是蓝底，实际 %v", mac, size, c)
			}
			if c := at(0.5, 0.5); c != white {
				t.Errorf("mac=%v size=%d：正中间应该是白色的播放三角，实际 %v", mac, size, c)
			}
		}
	}
}

// Windows 托盘图标是一个 ICO，里面每个尺寸一张 PNG；图形和程序图标一样，暂停时底色变灰、三角换成竖线。
func TestTrayWindowsIcon(t *testing.T) {
	gray := color.NRGBA{0x8b, 0x8f, 0x98, 255}
	for _, paused := range []bool{false, true} {
		ico := TrayWindowsIcon(paused)
		if binary.LittleEndian.Uint16(ico[2:]) != 1 || int(binary.LittleEndian.Uint16(ico[4:])) != len(windowsTraySizes) {
			t.Fatalf("paused=%v：ICO 文件头不对", paused)
		}
		var largest image.Image
		for i, size := range windowsTraySizes {
			d := ico[6+16*i:]
			n, off := binary.LittleEndian.Uint32(d[8:]), binary.LittleEndian.Uint32(d[12:])
			img, err := png.Decode(bytes.NewReader(ico[off : off+n]))
			if err != nil {
				t.Fatalf("paused=%v：第 %d 张图读不出来：%v", paused, i, err)
			}
			if int(d[0]) != size || img.Bounds().Dx() != size || img.Bounds().Dy() != size {
				t.Fatalf("paused=%v：第 %d 张图应该是 %d 像素，目录里写的 %d，实际 %v", paused, i, size, d[0], img.Bounds())
			}
			largest = img
		}
		size := float64(largest.Bounds().Dx())
		at := func(fx, fy float64) color.NRGBA {
			return color.NRGBAModel.Convert(largest.At(int(fx*size), int(fy*size))).(color.NRGBA)
		}
		bg, mark := blue, at(0.5, 0.43) // 运行时正中是播放三角
		if paused {
			bg, mark = gray, at(0.4, 0.43) // 暂停时是左边那条竖线
			if c := at(0.5, 0.43); c != gray {
				t.Errorf("暂停时两条竖线中间应该是灰底，实际 %v", c)
			}
		}
		if c := at(0, 0); c.A != 0 {
			t.Errorf("paused=%v：左上角应该透明，实际 %v", paused, c)
		}
		if c := at(0.3, 0.43); c != bg {
			t.Errorf("paused=%v：气泡里面应该是底色 %v，实际 %v", paused, bg, c)
		}
		if mark != white {
			t.Errorf("paused=%v：气泡中间应该有白色的图形，实际 %v", paused, mark)
		}
	}
}
