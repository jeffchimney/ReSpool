package media

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// Bitrates in kbit/s, indexed by [MPEG-1 / MPEG-2 & 2.5][layer I, II, III][index].
var mp3Bitrates = [2][3][16]int{
	{
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	},
	{
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
	},
}

var mp3SampleRates = map[int][3]int{
	1:  {44100, 48000, 32000},
	2:  {22050, 24000, 16000},
	25: {11025, 12000, 8000},
}

type mp3Frame struct {
	version    int // 1, 2 or 25 (MPEG-2.5)
	layer      int // 1, 2 or 3
	bitrate    int // bit/s
	sampleRate int
	padding    int
	mono       bool
}

func parseMP3Header(b []byte) (mp3Frame, bool) {
	var f mp3Frame
	if len(b) < 4 || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return f, false
	}
	verBits := (b[1] >> 3) & 0x03
	layerBits := (b[1] >> 1) & 0x03
	brIdx := b[2] >> 4
	srIdx := (b[2] >> 2) & 0x03
	// Reserved values, and "free format" bitrate (0), which we can't size.
	if verBits == 1 || layerBits == 0 || brIdx == 0 || brIdx == 15 || srIdx == 3 {
		return f, false
	}
	switch verBits {
	case 0:
		f.version = 25
	case 2:
		f.version = 2
	case 3:
		f.version = 1
	}
	f.layer = 4 - int(layerBits)
	table := 0
	if f.version != 1 {
		table = 1
	}
	f.bitrate = mp3Bitrates[table][f.layer-1][brIdx] * 1000
	f.sampleRate = mp3SampleRates[f.version][srIdx]
	f.padding = int((b[2] >> 1) & 0x01)
	f.mono = b[3]>>6 == 3
	return f, true
}

func (f mp3Frame) samplesPerFrame() int {
	switch {
	case f.layer == 1:
		return 384
	case f.layer == 2 || f.version == 1:
		return 1152
	default:
		return 576
	}
}

func (f mp3Frame) size() int {
	switch {
	case f.layer == 1:
		return (12*f.bitrate/f.sampleRate + f.padding) * 4
	case f.layer == 3 && f.version != 1:
		return 72*f.bitrate/f.sampleRate + f.padding
	default:
		return 144*f.bitrate/f.sampleRate + f.padding
	}
}

func (f mp3Frame) sideInfoSize() int {
	switch {
	case f.version == 1 && f.mono:
		return 17
	case f.version == 1:
		return 32
	case f.mono:
		return 9
	default:
		return 17
	}
}

func (f mp3Frame) durationOf(frames uint32, trim int) time.Duration {
	samples := float64(frames) * float64(f.samplesPerFrame())
	if float64(trim) < samples {
		samples -= float64(trim)
	}
	return seconds(samples / float64(f.sampleRate))
}

// mp3Duration finds the first audio frame and reads the duration from its
// Xing/Info or VBRI header, falling back to a constant-bitrate estimate.
func mp3Duration(r io.ReaderAt, size int64) (time.Duration, error) {
	start := skipID3v2(r)

	buf := make([]byte, 256<<10)
	n, err := r.ReadAt(buf, start)
	if n == 0 && err != nil {
		return 0, err
	}
	buf = buf[:n]

	for i := 0; i+4 <= len(buf); i++ {
		f, ok := parseMP3Header(buf[i:])
		if !ok {
			continue
		}
		// A lone 0xFFE pattern is common in other data; insist that the
		// next frame header lines up too.
		if next := i + f.size(); next+4 <= len(buf) {
			if _, ok := parseMP3Header(buf[next:]); !ok {
				continue
			}
		}
		if d, ok := vbrHeaderDuration(buf[i:], f); ok {
			return d, nil
		}
		audio := size - (start + int64(i))
		if hasID3v1(r, size) {
			audio -= 128
		}
		return seconds(float64(audio) * 8 / float64(f.bitrate)), nil
	}
	return 0, errors.New("no MPEG audio frame found")
}

func vbrHeaderDuration(frame []byte, f mp3Frame) (time.Duration, bool) {
	if f.layer == 3 {
		off := 4 + f.sideInfoSize()
		if len(frame) >= off+12 {
			if id := string(frame[off : off+4]); id == "Xing" || id == "Info" {
				flags := binary.BigEndian.Uint32(frame[off+4:])
				if flags&0x1 != 0 {
					if frames := binary.BigEndian.Uint32(frame[off+8:]); frames > 0 {
						return f.durationOf(frames, encoderTrim(frame, off, flags)), true
					}
				}
			}
		}
	}
	// The VBRI header (Fraunhofer encoders) always sits 32 bytes after the
	// frame header.
	const vbri = 4 + 32
	if len(frame) >= vbri+18 && string(frame[vbri:vbri+4]) == "VBRI" {
		if frames := binary.BigEndian.Uint32(frame[vbri+14:]); frames > 0 {
			return f.durationOf(frames, 0), true
		}
	}
	return 0, false
}

// encoderTrim reads the encoder delay and padding (in samples) from the LAME
// extension that follows a Xing header. LAME and ffmpeg both write it.
func encoderTrim(frame []byte, xing int, flags uint32) int {
	p := xing + 8
	if flags&0x1 != 0 {
		p += 4 // frame count
	}
	if flags&0x2 != 0 {
		p += 4 // byte count
	}
	if flags&0x4 != 0 {
		p += 100 // seek table
	}
	if flags&0x8 != 0 {
		p += 4 // quality
	}
	if len(frame) < p+24 {
		return 0
	}
	for _, c := range frame[p : p+4] { // encoder name, e.g. "LAME" or "Lavc"
		if c < 'A' || c > 'z' {
			return 0
		}
	}
	v := int(frame[p+21])<<16 | int(frame[p+22])<<8 | int(frame[p+23])
	return v>>12 + v&0xfff
}

func hasID3v1(r io.ReaderAt, size int64) bool {
	if size < 128 {
		return false
	}
	b := make([]byte, 3)
	_, err := r.ReadAt(b, size-128)
	return err == nil && string(b) == "TAG"
}
