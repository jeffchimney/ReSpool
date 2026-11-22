package media

import (
	"bytes"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The fixtures in testdata were generated with ffmpeg; see testdata/README.md.

func near(t *testing.T, what string, got, want, tolerance time.Duration) {
	t.Helper()
	if math.Abs(float64(got-want)) > float64(tolerance) {
		t.Errorf("%s = %v, want %v (±%v)", what, got, want, tolerance)
	}
}

func TestProbeTags(t *testing.T) {
	info, err := Probe("testdata/tagged.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "Chapter One" || info.Album != "Test Book" || info.Artist != "Test Author" || info.AlbumArtist != "Test Author" {
		t.Errorf("tags = %q / %q / %q / %q", info.Title, info.Album, info.Artist, info.AlbumArtist)
	}
	if info.Track != 1 {
		t.Errorf("track = %d, want 1", info.Track)
	}
	if !info.HasPicture || info.PictureMIME != "image/jpeg" {
		t.Errorf("picture = %v %q, want embedded JPEG", info.HasPicture, info.PictureMIME)
	}
	near(t, "duration", info.Duration, 3*time.Second, 60*time.Millisecond)
}

func TestProbeDuration(t *testing.T) {
	tests := []struct {
		file      string
		want, tol time.Duration
	}{
		{"vbr.mp3", 4 * time.Second, 60 * time.Millisecond},                 // Xing header
		{"cbr-noxing.mp3", 5068 * time.Millisecond, 100 * time.Millisecond}, // bitrate estimate
		{"chapters.m4b", 6 * time.Second, 30 * time.Millisecond},
		{"qt-chapters.m4b", 6 * time.Second, 30 * time.Millisecond},
		{"tone.flac", 2 * time.Second, time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			info, err := Probe(filepath.Join("testdata", tt.file))
			if err != nil {
				t.Fatal(err)
			}
			near(t, "duration", info.Duration, tt.want, tt.tol)
		})
	}
}

func TestMP4Chapters(t *testing.T) {
	want := []Chapter{{0, "Opening"}, {2 * time.Second, "The Middle"}, {4500 * time.Millisecond, "Ending"}}
	// chapters.m4b has a Nero chpl box and a QuickTime chapter track;
	// qt-chapters.m4b only has the track.
	for _, file := range []string{"chapters.m4b", "qt-chapters.m4b"} {
		t.Run(file, func(t *testing.T) {
			info, err := Probe(filepath.Join("testdata", file))
			if err != nil {
				t.Fatal(err)
			}
			if len(info.Chapters) != len(want) {
				t.Fatalf("got %d chapters %+v, want %d", len(info.Chapters), info.Chapters, len(want))
			}
			for i, c := range info.Chapters {
				if c.Title != want[i].Title {
					t.Errorf("chapter %d title = %q, want %q", i, c.Title, want[i].Title)
				}
				near(t, "chapter start", c.Start, want[i].Start, 30*time.Millisecond)
			}
			if info.Album != "Chaptered Book" || info.Artist != "Ada Author" {
				t.Errorf("tags = %q / %q", info.Album, info.Artist)
			}
			if info.Description != "A tiny book used in tests." {
				t.Errorf("description = %q", info.Description)
			}
		})
	}
}

func TestNeroChaptersOnly(t *testing.T) {
	f, err := os.Open("testdata/chapters.m4b")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	st, _ := f.Stat()
	moov, err := readTopLevelBox(f, st.Size(), "moov")
	if err != nil {
		t.Fatal(err)
	}
	if got := neroChapters(path(moov, "udta", "chpl")); len(got) != 3 || got[1].Title != "The Middle" {
		t.Errorf("nero chapters = %+v", got)
	}
}

func TestSyntheticCBR(t *testing.T) {
	// 100 MPEG-1 Layer III frames at 128 kbit/s, 44.1 kHz: 417 bytes each.
	var b bytes.Buffer
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFB, 0x90, 0x40})
	for range 100 {
		b.Write(frame)
	}
	d, err := mp3Duration(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	near(t, "duration", d, 2606*time.Millisecond, 10*time.Millisecond)
}

func TestNotAudio(t *testing.T) {
	junk := bytes.Repeat([]byte("not audio "), 1000)
	if _, err := mp3Duration(bytes.NewReader(junk), int64(len(junk))); err == nil {
		t.Error("mp3Duration accepted junk")
	}
	if _, err := flacDuration(bytes.NewReader(junk)); err == nil {
		t.Error("flacDuration accepted junk")
	}
	if _, err := probeMP4(bytes.NewReader(junk), int64(len(junk))); err == nil {
		t.Error("probeMP4 accepted junk")
	}
}

func TestPicture(t *testing.T) {
	data, mime, err := Picture("testdata/tagged.mp3")
	if err != nil {
		t.Fatal(err)
	}
	if mime != "image/jpeg" || !bytes.HasPrefix(data, []byte{0xFF, 0xD8}) {
		t.Errorf("picture = %q, %x…", mime, data[:4])
	}
}

func TestIsAudio(t *testing.T) {
	for name, want := range map[string]bool{
		"a.mp3": true, "B.M4B": true, "c.flac": true, "cover.jpg": false, "notes.txt": false, "noext": false,
	} {
		if got := IsAudio(name); got != want {
			t.Errorf("IsAudio(%q) = %v", name, got)
		}
	}
}

func TestSniff(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		return b[:min(len(b), 512)]
	}
	for _, tt := range []struct {
		ext  string
		head []byte
		want bool
	}{
		{".mp3", read("tagged.mp3"), true},
		{".mp3", read("cbr-noxing.mp3"), true},
		{".M4B", read("chapters.m4b"), true},
		{".flac", read("tone.flac"), true},
		{".m4b", read("tagged.mp3"), false},
		{".mp3", []byte("<html><script>"), false},
		{".wav", []byte("RIFF\x00\x00\x00\x00WAVEfmt "), true},
		{".ogg", []byte("OggS\x00"), true},
		{".png", []byte("\x89PNG\r\n\x1a\n"), true},
		{".jpg", []byte("GIF89a"), false},
		{".exe", []byte("MZ"), false},
	} {
		if got := Sniff(tt.ext, tt.head); got != tt.want {
			t.Errorf("Sniff(%q, %q…) = %v, want %v", tt.ext, tt.head[:min(len(tt.head), 6)], got, tt.want)
		}
	}
}
