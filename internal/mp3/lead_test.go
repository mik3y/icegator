package mp3

import (
	"bytes"
	"io"
	"testing"

	gomp3 "github.com/hajimehoshi/go-mp3"
)

func allFrames(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var out [][]byte
	r := NewReader(bytes.NewReader(data))
	for {
		f, err := r.Next()
		if err != nil {
			return out
		}
		out = append(out, append([]byte(nil), f.Data...))
	}
}

// decode returns the PCM a real decoder produces for a sequence of frames.
func decode(t *testing.T, frames [][]byte) []byte {
	t.Helper()
	dec, err := gomp3.NewDecoder(bytes.NewReader(bytes.Join(frames, nil)))
	if err != nil {
		t.Fatal(err)
	}
	pcm, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	return pcm
}

const pcmPerFrame = 1152 * 4 // 16-bit stereo

func TestCarrierIsSilentAndSameSize(t *testing.T) {
	frames := allFrames(t, fixture(t))
	carrier, ok := Carrier(frames[10], 0)
	if !ok || len(carrier) != len(frames[10]) {
		t.Fatalf("ok=%v len=%d want %d", ok, len(carrier), len(frames[10]))
	}
	h, _ := ParseHeader(carrier)
	if !bytes.Equal(carrier[h.dataOffset():], frames[10][h.dataOffset():]) {
		t.Fatal("the carrier does not hold the frame's original data bytes")
	}
	if n, _ := MainDataBegin(carrier); n != 0 {
		t.Fatalf("the carrier borrows %d bytes", n)
	}
	for _, sample := range decode(t, [][]byte{carrier, carrier, carrier}) {
		if sample != 0 {
			t.Fatal("the carrier does not decode as silence")
		}
	}
}

// A stream cut in mid-flow, with its lead in front, has to decode to the
// same audio as the uncut stream does from that point on.
func TestLeadMakesAMidStreamStartDecodeExactly(t *testing.T) {
	frames := allFrames(t, fixture(t))
	whole := decode(t, frames)

	// Start somewhere the first frame borrows as much as it can.
	start := 0
	for i := 20; i < len(frames)-20; i++ {
		if n, _ := MainDataBegin(frames[i]); n > 400 {
			start = i
			break
		}
	}
	if start == 0 {
		t.Fatal("the fixture has no frame that borrows heavily")
	}

	lead := Lead(frames[start-MaxLead:start], frames[start])
	if len(lead) < 2 {
		t.Fatalf("lead is %d frames; a frame that borrows over 400 bytes needs at least 2 at this bitrate", len(lead))
	}
	cut := decode(t, append(append([][]byte{}, lead...), frames[start:]...))

	// In the cut stream the lead comes first, then the frames from start.
	// The frame at start itself overlaps with the silence before it, so
	// the comparison begins one frame later.
	from := (len(lead) + 1) * pcmPerFrame
	want := whole[(start+1)*pcmPerFrame:]
	got := cut[from:]
	n := min(len(want), len(got))
	if n < 10*pcmPerFrame {
		t.Fatalf("only %d bytes of audio to compare", n)
	}
	if !bytes.Equal(got[:n], want[:n]) {
		t.Fatal("audio after the join differs from the original")
	}

	// The same cut without a lead does not decode to the same audio, which
	// is what shows that the comparison above can tell the difference.
	bare := decode(t, frames[start:])
	bareFrom := 1 * pcmPerFrame
	m := min(len(bare)-bareFrom, n)
	if m > 0 && bytes.Equal(bare[bareFrom:bareFrom+m], want[:m]) {
		t.Fatal("a bare cut decoded identically, so this fixture does not exercise the reservoir")
	}
}

func TestLeadIsEmptyWhenNothingIsBorrowed(t *testing.T) {
	frames := allFrames(t, fixture(t))
	if lead := Lead(nil, frames[0]); len(lead) != 0 {
		t.Fatalf("the first frame of a stream got a lead of %d", len(lead))
	}
	// With no history to draw on, there is nothing to send.
	if lead := Lead(nil, frames[30]); len(lead) != 0 {
		t.Fatalf("lead of %d from no preceding frames", len(lead))
	}
}

func TestCarrierKeepsACRCValid(t *testing.T) {
	// A CRC-protected frame built by hand: protection bit clear, zeroed
	// side information, and the CRC a real encoder would write for that.
	frame := make([]byte, 417)
	copy(frame, []byte{0xFF, 0xFA, 0x90, 0x64})
	crc := crc16(crc16(0xFFFF, frame[2:4]), frame[6:6+32])
	frame[4], frame[5] = byte(crc>>8), byte(crc)
	// Give it non-zero side information, as a real frame would have.
	frame[6], frame[10] = 0x55, 0xAA

	carrier, ok := Carrier(frame, 0)
	if !ok {
		t.Fatal("rejected")
	}
	if carrier[4] != byte(crc>>8) || carrier[5] != byte(crc) {
		t.Fatalf("crc %02x%02x, want %04x", carrier[4], carrier[5], crc)
	}
}
