package main

import (
	"bytes"
	"encoding/binary"
	"image/color"
	"image/png"
	"sort"

	"lanreview/internal/platform"
)

// Windows 的 .exe 图标是编进程序里的"资源"。这里直接写出一个只含图标资源的 COFF 目标文件（.syso），
// 不用额外的工具。格式见 PE/COFF 规范的".rsrc 段"一节：三层目录（类型 → 编号 → 语言），
// 最底层的条目指向数据；数据的地址要等链接时才知道，所以每个数据条目带一个重定位。

const (
	rtIcon      = 3
	rtGroupIcon = 14
	langEnUS    = 0x0409
)

// 资源管理器、任务栏、Alt+Tab 会从里面挑最合适的尺寸。
var windowsIconSizes = []int{16, 24, 32, 48, 64, 128, 256}

type resource struct {
	typ, id uint32
	data    []byte
}

func windowsIconSyso() []byte {
	var res []resource
	var group bytes.Buffer // 图标组：列出每张图的尺寸和编号
	writeLE(&group, uint16(0), uint16(1), uint16(len(windowsIconSizes)))
	for i, s := range windowsIconSizes {
		data := iconImage(s)
		id := uint16(i + 1)
		w := uint8(s) // 256 记成 0
		writeLE(&group, w, w, uint8(0), uint8(0), uint16(1), uint16(32), uint32(len(data)), id)
		res = append(res, resource{rtIcon, uint32(id), data})
	}
	res = append(res, resource{rtGroupIcon, 1, group.Bytes()})
	return coffObject(resourceSection(res))
}

// iconImage 是图标资源里的一张图：256 的用 PNG，小尺寸用传统的位图格式，兼容性最好。
func iconImage(size int) []byte {
	pngData := platform.AppIcon(size, false)
	if size >= 256 {
		return pngData
	}
	img, err := png.Decode(bytes.NewReader(pngData))
	if err != nil {
		panic(err)
	}
	var b bytes.Buffer
	// BITMAPINFOHEADER，高度要写两倍（下面还跟着一张 AND 掩码）
	writeLE(&b, uint32(40), int32(size), int32(size*2), uint16(1), uint16(32), uint32(0), uint32(0), int32(0), int32(0), uint32(0), uint32(0))
	stride := (size + 31) / 32 * 4
	mask := make([]byte, stride*size)
	// 位图从最下面一行开始存
	for row, y := 0, size-1; y >= 0; row, y = row+1, y-1 {
		for x := 0; x < size; x++ {
			c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
			b.Write([]byte{c.B, c.G, c.R, c.A})
			if c.A == 0 {
				mask[row*stride+x/8] |= 0x80 >> (x % 8)
			}
		}
	}
	b.Write(mask)
	return b.Bytes()
}

// resourceSection 排好三层目录和数据，返回段的内容和需要重定位的位置（数据条目里的地址）。
func resourceSection(res []resource) ([]byte, []uint32) {
	sort.Slice(res, func(i, j int) bool {
		if res[i].typ != res[j].typ {
			return res[i].typ < res[j].typ
		}
		return res[i].id < res[j].id
	})
	var types []uint32
	for i, r := range res {
		if i == 0 || r.typ != res[i-1].typ {
			types = append(types, r.typ)
		}
	}
	// 先算好每一块的位置：根目录、各类型的目录、各资源的语言目录、数据条目、数据
	off := 16 + 8*len(types)
	typeDir := map[uint32]int{}
	for _, t := range types {
		typeDir[t] = off
		n := 0
		for _, r := range res {
			if r.typ == t {
				n++
			}
		}
		off += 16 + 8*n
	}
	langDir, entry, data := make([]int, len(res)), make([]int, len(res)), make([]int, len(res))
	for i := range res {
		langDir[i] = off
		off += 16 + 8
	}
	for i := range res {
		entry[i] = off
		off += 16
	}
	for i, r := range res {
		off = (off + 7) &^ 7
		data[i] = off
		off += len(r.data)
	}

	buf := make([]byte, off)
	put := func(at int, v uint32) { binary.LittleEndian.PutUint32(buf[at:], v) }
	const subdir = 0x80000000
	// 目录头 16 字节，只用到最后的"编号条目数"；后面跟着一个个 8 字节的条目：编号、指向的位置
	dir := func(at int, ids, targets []uint32) {
		binary.LittleEndian.PutUint16(buf[at+14:], uint16(len(ids)))
		for k := range ids {
			put(at+16+8*k, ids[k])
			put(at+20+8*k, targets[k])
		}
	}
	var typeTargets []uint32
	for _, t := range types {
		typeTargets = append(typeTargets, subdir|uint32(typeDir[t]))
		var ids, targets []uint32
		for i, r := range res {
			if r.typ == t {
				ids = append(ids, r.id)
				targets = append(targets, subdir|uint32(langDir[i]))
			}
		}
		dir(typeDir[t], ids, targets)
	}
	dir(0, types, typeTargets)
	var relocs []uint32
	for i, r := range res {
		dir(langDir[i], []uint32{langEnUS}, []uint32{uint32(entry[i])})
		put(entry[i], uint32(data[i])) // 段内偏移，链接器会加上段的地址
		put(entry[i]+4, uint32(len(r.data)))
		relocs = append(relocs, uint32(entry[i]))
		copy(buf[data[i]:], r.data)
	}
	return buf, relocs
}

// coffObject 把资源段包成 amd64 的 COFF 目标文件：文件头、一个 .rsrc 段、重定位、只有段符号的符号表。
func coffObject(sect []byte, relocs []uint32) []byte {
	const fileHeader, sectionHeader = 20, 40
	rawOff := fileHeader + sectionHeader
	relocOff := rawOff + len(sect)
	symOff := relocOff + 10*len(relocs)
	var b bytes.Buffer
	writeLE(&b, uint16(0x8664), uint16(1), uint32(0), uint32(symOff), uint32(1), uint16(0), uint16(0))
	b.WriteString(".rsrc\x00\x00\x00")
	// 初始化过的只读数据
	writeLE(&b, uint32(0), uint32(0), uint32(len(sect)), uint32(rawOff), uint32(relocOff), uint32(0), uint16(len(relocs)), uint16(0), uint32(0x40000040))
	b.Write(sect)
	for _, at := range relocs {
		writeLE(&b, at, uint32(0), uint16(3)) // IMAGE_REL_AMD64_ADDR32NB，相对 0 号符号（.rsrc 段本身）
	}
	b.WriteString(".rsrc\x00\x00\x00")
	writeLE(&b, uint32(0), int16(1), uint16(0), uint8(3), uint8(0)) // 段 1 的静态符号
	writeLE(&b, uint32(4))                                          // 空的字符串表
	return b.Bytes()
}

func writeLE(b *bytes.Buffer, vs ...any) {
	for _, v := range vs {
		binary.Write(b, binary.LittleEndian, v)
	}
}
