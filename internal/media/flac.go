package media

import (
	"encoding/binary"
	"errors"
	"io"
	"time"
)

// flacDuration reads the total sample count and sample rate from the
// STREAMINFO block, which the FLAC spec requires to be the first metadata
// block.
func flacDuration(r io.ReaderAt) (time.Duration, error) {
	b := make([]byte, 4+4+34) // "fLaC", block header, STREAMINFO
	if _, err := r.ReadAt(b, skipID3v2(r)); err != nil {
		return 0, err
	}
	if string(b[:4]) != "fLaC" {
		return 0, errors.New("not a FLAC stream")
	}
	if b[4]&0x7f != 0 {
		return 0, errors.New("FLAC stream does not start with STREAMINFO")
	}
	// Bytes 10-17 of STREAMINFO: sample rate (20 bits), channels (3),
	// bits per sample (5), total samples (36).
	x := binary.BigEndian.Uint64(b[8+10:])
	rate := x >> 44
	total := x & (1<<36 - 1)
	if rate == 0 || total == 0 {
		return 0, errors.New("FLAC stream has unknown length")
	}
	return seconds(float64(total) / float64(rate)), nil
}
