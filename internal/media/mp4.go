package media

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"time"
	"unicode/utf16"
)

const (
	maxMoovSize = 128 << 20
	maxChapters = 10000
)

type mp4Info struct {
	duration    time.Duration
	chapters    []Chapter
	description string
}

// probeMP4 reads the duration, chapters and long description of an
// MP4/M4A/M4B file. Chapters come from a Nero "chpl" box when present,
// otherwise from a QuickTime chapter text track.
func probeMP4(r io.ReaderAt, size int64) (mp4Info, error) {
	var mi mp4Info
	moov, err := readTopLevelBox(r, size, "moov")
	if err != nil {
		return mi, err
	}
	mi.duration = mvhdDuration(child(moov, "mvhd"))
	mi.chapters = neroChapters(path(moov, "udta", "chpl"))
	if len(mi.chapters) == 0 {
		mi.chapters = quicktimeChapters(r, moov)
	}
	if len(mi.chapters) < 2 {
		mi.chapters = nil // a single chapter adds nothing
	}
	for i := range mi.chapters {
		if mi.chapters[i].Title == "" {
			mi.chapters[i].Title = fmt.Sprintf("Chapter %d", i+1)
		}
	}
	mi.description = mp4Description(moov)
	return mi, nil
}

func readTopLevelBox(r io.ReaderAt, size int64, want string) ([]byte, error) {
	var off int64
	hdr := make([]byte, 16)
	for off+8 <= size {
		if _, err := r.ReadAt(hdr[:8], off); err != nil {
			return nil, err
		}
		boxSize := int64(binary.BigEndian.Uint32(hdr))
		typ := string(hdr[4:8])
		hdrLen := int64(8)
		switch boxSize {
		case 1:
			if _, err := r.ReadAt(hdr[8:16], off+8); err != nil {
				return nil, err
			}
			boxSize = int64(binary.BigEndian.Uint64(hdr[8:]))
			hdrLen = 16
		case 0:
			boxSize = size - off
		}
		if boxSize < hdrLen || boxSize > size-off {
			return nil, fmt.Errorf("malformed %q box at offset %d", typ, off)
		}
		if typ == want {
			n := boxSize - hdrLen
			if n > maxMoovSize {
				return nil, fmt.Errorf("%s box too large (%d bytes)", want, n)
			}
			b := make([]byte, n)
			if _, err := r.ReadAt(b, off+hdrLen); err != nil {
				return nil, err
			}
			return b, nil
		}
		off += boxSize
	}
	return nil, fmt.Errorf("no %s box", want)
}

type box struct {
	typ  string
	data []byte
}

// boxes splits b into the consecutive boxes it contains.
func boxes(b []byte) []box {
	var out []box
	for len(b) >= 8 {
		size := uint64(binary.BigEndian.Uint32(b))
		typ := string(b[4:8])
		hdr := uint64(8)
		switch size {
		case 1:
			if len(b) < 16 {
				return out
			}
			size = binary.BigEndian.Uint64(b[8:])
			hdr = 16
		case 0:
			size = uint64(len(b))
		}
		if size < hdr || size > uint64(len(b)) {
			return out
		}
		out = append(out, box{typ, b[hdr:size]})
		b = b[size:]
	}
	return out
}

func child(b []byte, typ string) []byte {
	if typ == "meta" {
		return metaChildren(b)
	}
	for _, bx := range boxes(b) {
		if bx.typ == typ {
			return bx.data
		}
	}
	return nil
}

// metaChildren finds a "meta" box in b and returns its children. In ISO
// files meta is a full box (4 bytes of version/flags before its children);
// in QuickTime files it isn't.
func metaChildren(b []byte) []byte {
	for _, bx := range boxes(b) {
		if bx.typ != "meta" {
			continue
		}
		if len(bx.data) >= 8 && string(bx.data[4:8]) == "hdlr" {
			return bx.data
		}
		if len(bx.data) >= 4 {
			return bx.data[4:]
		}
	}
	return nil
}

func path(b []byte, types ...string) []byte {
	for _, t := range types {
		if b = child(b, t); b == nil {
			return nil
		}
	}
	return b
}

func be32(b []byte) uint32 { return binary.BigEndian.Uint32(b) }

func mvhdDuration(b []byte) time.Duration {
	var scale, dur uint64
	switch {
	case len(b) >= 32 && b[0] == 1:
		scale, dur = uint64(be32(b[20:])), binary.BigEndian.Uint64(b[24:])
	case len(b) >= 20 && b[0] == 0:
		scale, dur = uint64(be32(b[12:])), uint64(be32(b[16:]))
		if dur == math.MaxUint32 {
			return 0 // "unknown"
		}
	default:
		return 0
	}
	if scale == 0 {
		return 0
	}
	return seconds(float64(dur) / float64(scale))
}

// neroChapters parses a Nero "chpl" box: version/flags, 4 more bytes when
// version > 0, an 8-bit count, then (start in 100ns units, length-prefixed
// title) per chapter.
func neroChapters(b []byte) []Chapter {
	if len(b) < 5 {
		return nil
	}
	p := 4
	if b[0] != 0 {
		p += 4
	}
	if len(b) < p+1 {
		return nil
	}
	n := int(b[p])
	p++
	var out []Chapter
	for i := 0; i < n && len(b) >= p+9; i++ {
		start := binary.BigEndian.Uint64(b[p:])
		l := int(b[p+8])
		p += 9
		if len(b) < p+l {
			break
		}
		out = append(out, Chapter{Start: time.Duration(start) * 100, Title: clean(string(b[p : p+l]))})
		p += l
	}
	return out
}

// quicktimeChapters reads chapter titles from the text track that the audio
// track references through a "tref/chap" box.
func quicktimeChapters(r io.ReaderAt, moov []byte) []Chapter {
	traks := map[uint32][]byte{}
	var chapID uint32
	for _, bx := range boxes(moov) {
		if bx.typ != "trak" {
			continue
		}
		traks[tkhdID(child(bx.data, "tkhd"))] = bx.data
		if chap := path(bx.data, "tref", "chap"); chapID == 0 && len(chap) >= 4 {
			chapID = be32(chap)
		}
	}
	trak := traks[chapID]
	if chapID == 0 || trak == nil {
		return nil
	}
	scale := mdhdTimescale(path(trak, "mdia", "mdhd"))
	stbl := path(trak, "mdia", "minf", "stbl")
	if scale == 0 || stbl == nil {
		return nil
	}
	deltas := sttsDeltas(child(stbl, "stts"))
	sizes := stszSizes(child(stbl, "stsz"))
	offsets := sampleOffsets(child(stbl, "stsc"), chunkOffsets(stbl), sizes)

	var out []Chapter
	var t uint64
	for i := 0; i < len(offsets) && i < len(sizes); i++ {
		out = append(out, Chapter{
			Start: seconds(float64(t) / float64(scale)),
			Title: readTextSample(r, offsets[i], sizes[i]),
		})
		if i < len(deltas) {
			t += uint64(deltas[i])
		}
	}
	return out
}

func tkhdID(b []byte) uint32 {
	switch {
	case len(b) >= 24 && b[0] == 1:
		return be32(b[20:])
	case len(b) >= 16 && b[0] == 0:
		return be32(b[12:])
	}
	return 0
}

func mdhdTimescale(b []byte) uint32 {
	switch {
	case len(b) >= 24 && b[0] == 1:
		return be32(b[20:])
	case len(b) >= 16 && b[0] == 0:
		return be32(b[12:])
	}
	return 0
}

func sttsDeltas(b []byte) []uint32 {
	if len(b) < 8 {
		return nil
	}
	var out []uint32
	n := int(be32(b[4:]))
	for i := 0; i < n && 16+i*8 <= len(b); i++ {
		count, delta := be32(b[8+i*8:]), be32(b[12+i*8:])
		for j := uint32(0); j < count && len(out) < maxChapters; j++ {
			out = append(out, delta)
		}
	}
	return out
}

func stszSizes(b []byte) []uint32 {
	if len(b) < 12 {
		return nil
	}
	fixed := be32(b[4:])
	count := min(int(be32(b[8:])), maxChapters)
	out := make([]uint32, 0, count)
	for i := 0; i < count; i++ {
		if fixed != 0 {
			out = append(out, fixed)
			continue
		}
		if 16+i*4 > len(b) {
			break
		}
		out = append(out, be32(b[12+i*4:]))
	}
	return out
}

func chunkOffsets(stbl []byte) []uint64 {
	var out []uint64
	if b := child(stbl, "stco"); len(b) >= 8 {
		n := int(be32(b[4:]))
		for i := 0; i < n && 12+i*4 <= len(b) && len(out) < maxChapters; i++ {
			out = append(out, uint64(be32(b[8+i*4:])))
		}
	} else if b := child(stbl, "co64"); len(b) >= 8 {
		n := int(be32(b[4:]))
		for i := 0; i < n && 16+i*8 <= len(b) && len(out) < maxChapters; i++ {
			out = append(out, binary.BigEndian.Uint64(b[8+i*8:]))
		}
	}
	return out
}

// sampleOffsets turns the sample-to-chunk table into a file offset for
// every sample.
func sampleOffsets(stsc []byte, chunks []uint64, sizes []uint32) []uint64 {
	if len(stsc) < 8 {
		return nil
	}
	type entry struct{ first, perChunk uint32 }
	var entries []entry
	n := int(be32(stsc[4:]))
	for i := 0; i < n && 20+i*12 <= len(stsc); i++ {
		entries = append(entries, entry{be32(stsc[8+i*12:]), be32(stsc[12+i*12:])})
	}
	if len(entries) == 0 {
		return nil
	}
	var out []uint64
	sample, e := 0, 0
	for c := 0; c < len(chunks) && sample < len(sizes); c++ {
		for e+1 < len(entries) && uint32(c+1) >= entries[e+1].first {
			e++
		}
		off := chunks[c]
		for k := uint32(0); k < entries[e].perChunk && sample < len(sizes); k++ {
			out = append(out, off)
			off += uint64(sizes[sample])
			sample++
		}
	}
	return out
}

// readTextSample decodes a QuickTime text sample: a 16-bit length followed
// by UTF-8 text, or UTF-16 text with a byte order mark.
func readTextSample(r io.ReaderAt, off uint64, size uint32) string {
	if size < 2 || off > math.MaxInt64 {
		return ""
	}
	b := make([]byte, min(size, 1024))
	n, _ := r.ReadAt(b, int64(off))
	b = b[:n]
	if len(b) < 2 {
		return ""
	}
	text := b[2:]
	if l := int(binary.BigEndian.Uint16(b)); l < len(text) {
		text = text[:l]
	}
	if len(text) >= 2 && (text[0] == 0xFE && text[1] == 0xFF || text[0] == 0xFF && text[1] == 0xFE) {
		bigEndian := text[0] == 0xFE
		text = text[2:]
		u := make([]uint16, len(text)/2)
		for i := range u {
			if bigEndian {
				u[i] = uint16(text[2*i])<<8 | uint16(text[2*i+1])
			} else {
				u[i] = uint16(text[2*i+1])<<8 | uint16(text[2*i])
			}
		}
		return clean(string(utf16.Decode(u)))
	}
	return clean(string(text))
}

// mp4Description returns the iTunes long description ("ldes") or
// description ("desc") tag.
func mp4Description(moov []byte) string {
	ilst := path(moov, "udta", "meta", "ilst")
	for _, key := range []string{"ldes", "desc"} {
		if data := path(ilst, key, "data"); len(data) > 8 {
			return clean(string(data[8:]))
		}
	}
	return ""
}
