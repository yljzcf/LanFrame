package platform

import (
	"bytes"
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
