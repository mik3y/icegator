package mp3

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../../testdata/tone-2s.mp3")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func readAll(t *testing.T, r *Reader) (frames int, bytes int, dur time.Duration) {
	t.Helper()
	for {
		f, err := r.Next()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		if len(f.Data) != f.Header.Size() {
			t.Fatalf("frame %d: len %d, header says %d", frames, len(f.Data), f.Header.Size())
		}
		frames++
		bytes += len(f.Data)
		dur += f.Header.Duration()
	}
}

func TestParseHeader(t *testing.T) {
	// MPEG-1 Layer 3, 128 kbps, 44.1 kHz, no padding, joint stereo.
	h, err := ParseHeader([]byte{0xFF, 0xFB, 0x90, 0x64})
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != MPEG1 || h.Layer != 3 || h.Bitrate != 128000 || h.SampleRate != 44100 || h.Channels != 2 {
		t.Fatalf("got %+v", h)
	}
	if h.Size() != 417 || h.Samples() != 1152 {
		t.Fatalf("size %d samples %d", h.Size(), h.Samples())
	}
	for _, bad := range [][]byte{
		{0xFF, 0xFB, 0xF0, 0x64}, // bitrate index 15
		{0xFF, 0xFB, 0x00, 0x64}, // free format
		{0xFF, 0xFB, 0x9C, 0x64}, // reserved sample rate
		{0xFF, 0xE9, 0x90, 0x64}, // reserved version
		{0x00, 0xFB, 0x90, 0x64},
	} {
		if _, err := ParseHeader(bad); err == nil {
			t.Errorf("%x: accepted", bad)
		}
	}
}

func TestReaderCleanStream(t *testing.T) {
	data := fixture(t)
	r := NewReader(bytes.NewReader(data))
	frames, n, dur := readAll(t, r)
	if n+int(r.Skipped()) != len(data) {
		t.Fatalf("frames %d bytes + %d skipped != %d", n, r.Skipped(), len(data))
	}
	if frames < 70 || dur < 1900*time.Millisecond || dur > 2200*time.Millisecond {
		t.Fatalf("%d frames, %v", frames, dur)
	}
}

func TestReaderSkipsID3AndGarbage(t *testing.T) {
	data := fixture(t)
	want, _, _ := readAll(t, NewReader(bytes.NewReader(data)))

	// A tag whose body is full of sync-like bytes, then junk, then audio.
	tag := append([]byte{'I', 'D', '3', 4, 0, 0, 0, 0, 2, 0}, bytes.Repeat([]byte{0xFF, 0xFB}, 128)...)
	junk := []byte{0xFF, 0xFB, 0x90, 0x64, 1, 2, 3, 0xFF, 0xFF}
	in := append(append(append([]byte{}, tag...), junk...), data...)

	r := NewReader(bytes.NewReader(in))
	got, _, _ := readAll(t, r)
	if got != want {
		t.Fatalf("got %d frames, want %d (skipped %d)", got, want, r.Skipped())
	}
}

func TestReaderResyncsAfterCorruption(t *testing.T) {
	data := fixture(t)
	want, _, _ := readAll(t, NewReader(bytes.NewReader(data)))

	// Drop 100 bytes from the middle. At most a couple of frames are lost.
	mid := len(data) / 2
	in := append(append([]byte{}, data[:mid]...), data[mid+100:]...)
	got, _, _ := readAll(t, NewReader(bytes.NewReader(in)))
	if got < want-3 || got > want {
		t.Fatalf("got %d frames, want about %d", got, want)
	}
}

func TestMainDataBegin(t *testing.T) {
	// The first frame of any stream has nothing before it to borrow from.
	// An encoder starts borrowing soon after, so later frames do.
	r := NewReader(bytes.NewReader(fixture(t)))
	first, borrowing, clean := true, 0, 0
	for {
		f, err := r.Next()
		if err != nil {
			break
		}
		n, ok := MainDataBegin(f.Data)
		if !ok || n < 0 || n > 511 {
			t.Fatalf("frame: n=%d ok=%v", n, ok)
		}
		if first && n != 0 {
			t.Fatalf("first frame borrows %d bytes", n)
		}
		first = false
		if n == 0 {
			clean++
		} else {
			borrowing++
		}
	}
	if borrowing == 0 || clean == 0 {
		t.Fatalf("%d frames borrow, %d do not; the fixture should have both", borrowing, clean)
	}
	if _, ok := MainDataBegin([]byte{0xFF, 0xFB, 0x90}); ok {
		t.Fatal("accepted a truncated frame")
	}
}

func TestJoinable(t *testing.T) {
	stereo44, _ := ParseHeader([]byte{0xFF, 0xFB, 0x90, 0x64})
	stereo44at192, _ := ParseHeader([]byte{0xFF, 0xFB, 0xB0, 0x64})
	mono44, _ := ParseHeader([]byte{0xFF, 0xFB, 0x90, 0xC4})
	stereo48, _ := ParseHeader([]byte{0xFF, 0xFB, 0x94, 0x64})
	if !stereo44.Joinable(stereo44at192) {
		t.Error("a change of bitrate alone should be joinable")
	}
	if stereo44.Joinable(mono44) || stereo44.Joinable(stereo48) {
		t.Error("a change of channels or sample rate should not be joinable")
	}
}
