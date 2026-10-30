package media

import (
	"encoding/binary"
	"errors"
	"os"
)

// 只读 MP4/MOV 里视频轨的帧表，拿到准确的帧率、帧数和编码。

type mp4Info struct {
	fps      float64
	frames   int
	duration float64
	vfr      bool
	codec    string
	width    int
	height   int
}

func probeMP4(path string) (mp4Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return mp4Info{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return mp4Info{}, err
	}
	off, n, err := findTopBox(f, st.Size(), "moov")
	if err != nil {
		return mp4Info{}, err
	}
	if n > 64<<20 {
		return mp4Info{}, errors.New("moov 太大")
	}
	moov := make([]byte, n)
	if _, err := f.ReadAt(moov, off); err != nil {
		return mp4Info{}, err
	}
	var info mp4Info
	found := false
	eachBox(moov, func(typ string, p []byte) bool {
		if typ == "trak" {
			if v, ok := parseTrak(p); ok {
				info, found = v, true
				return false
			}
		}
		return true
	})
	if !found {
		return mp4Info{}, errors.New("没有视频轨")
	}
	return info, nil
}

func findTopBox(f *os.File, size int64, want string) (int64, int64, error) {
	var pos int64
	hdr := make([]byte, 16)
	for pos+8 <= size {
		if _, err := f.ReadAt(hdr[:8], pos); err != nil {
			return 0, 0, err
		}
		bs := int64(binary.BigEndian.Uint32(hdr))
		typ := string(hdr[4:8])
		h := int64(8)
		switch bs {
		case 1:
			if _, err := f.ReadAt(hdr[8:16], pos+8); err != nil {
				return 0, 0, err
			}
			bs, h = int64(binary.BigEndian.Uint64(hdr[8:16])), 16
		case 0:
			bs = size - pos
		}
		if bs < h {
			return 0, 0, errors.New("文件结构损坏")
		}
		if typ == want {
			return pos + h, bs - h, nil
		}
		pos += bs
	}
	return 0, 0, errors.New("找不到 " + want)
}

func eachBox(b []byte, fn func(typ string, payload []byte) bool) {
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := uint64(8)
		switch size {
		case 1:
			if len(b) < 16 {
				return
			}
			size, hdr = binary.BigEndian.Uint64(b[8:16]), 16
		case 0:
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return
		}
		if !fn(typ, b[hdr:size]) {
			return
		}
		b = b[size:]
	}
}

func childBox(b []byte, want string) []byte {
	var out []byte
	eachBox(b, func(typ string, p []byte) bool {
		if typ == want {
			out = p
			return false
		}
		return true
	})
	return out
}

func parseTrak(trak []byte) (mp4Info, bool) {
	var info mp4Info
	mdia := childBox(trak, "mdia")
	hdlr := childBox(mdia, "hdlr")
	if len(hdlr) < 12 || string(hdlr[8:12]) != "vide" {
		return info, false
	}
	var timescale uint32
	if mdhd := childBox(mdia, "mdhd"); len(mdhd) >= 24 {
		if mdhd[0] == 1 {
			timescale = binary.BigEndian.Uint32(mdhd[20:24])
		} else {
			timescale = binary.BigEndian.Uint32(mdhd[12:16])
		}
	}
	stbl := childBox(childBox(mdia, "minf"), "stbl")
	// stsd 里第一个样本描述：编码 fourcc 和画面宽高
	if stsd := childBox(stbl, "stsd"); len(stsd) >= 8+36 {
		e := stsd[8:]
		info.codec = string(e[4:8])
		info.width = int(binary.BigEndian.Uint16(e[32:34]))
		info.height = int(binary.BigEndian.Uint16(e[34:36]))
	}
	stts := childBox(stbl, "stts")
	if len(stts) < 8 || timescale == 0 {
		return info, true
	}
	n := int(binary.BigEndian.Uint32(stts[4:8]))
	type entry struct{ count, delta uint32 }
	var entries []entry
	for i := 0; i < n && 8+i*8+8 <= len(stts); i++ {
		p := stts[8+i*8:]
		entries = append(entries, entry{binary.BigEndian.Uint32(p), binary.BigEndian.Uint32(p[4:])})
	}
	var total uint64
	byDelta := map[uint32]int{}
	for _, e := range entries {
		info.frames += int(e.count)
		total += uint64(e.count) * uint64(e.delta)
		byDelta[e.delta] += int(e.count)
	}
	var main uint32
	for d, c := range byDelta {
		if c > byDelta[main] {
			main = d
		}
	}
	if main > 0 {
		info.fps = float64(timescale) / float64(main)
	}
	info.duration = float64(total) / float64(timescale)
	// 最后一帧时长常常不一样，不算可变帧率
	other := info.frames - byDelta[main]
	if len(entries) > 0 && entries[len(entries)-1].delta != main && entries[len(entries)-1].count == 1 {
		other--
	}
	info.vfr = other > 2 && other*100 > info.frames
	return info, true
}
