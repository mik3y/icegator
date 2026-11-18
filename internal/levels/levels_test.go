package levels

import (
	"bytes"
	"os"
	"runtime"
	"testing"

	"github.com/splatboard/icegator/internal/session"
)

func TestMeterReadsTone(t *testing.T) {
	// The fixture is a 440 Hz tone at 0.12 of full scale under a tremolo
	// of 0.6 +/- 0.4. Peaks reach 20*log10(0.12) = -18.4 dBFS and the RMS
	// works out to 0.12*sqrt((0.36+0.08)/2) = 0.056, or -25 dBFS. That is
	// just under the default "low" threshold of -24.
	data, err := os.ReadFile("../../testdata/tone-2s.mp3")
	if err != nil {
		t.Fatal(err)
	}
	s := session.New(session.Identity{Username: "dj"}, "", "", nil)
	done := make(chan struct{})
	go func() { Meter(s, Thresholds{}); close(done) }()
	// Subscribe happens inside Meter; give it the stream once it has.
	for s.Subscribers() == 0 {
		runtime.Gosched()
	}
	s.Run(bytes.NewReader(data))
	<-done

	l := s.Level.Load()
	if l == nil {
		t.Fatal("no reading")
	}
	if l.Loudness < -26 || l.Loudness > -24 {
		t.Errorf("loudness %.1f dBFS, want about -25", l.Loudness)
	}
	if l.Verdict != "low" {
		t.Errorf("verdict %q", l.Verdict)
	}
	if d := l.RMS[0] - l.RMS[1]; d > 0.5 || d < -0.5 {
		t.Errorf("channels differ: %v", l.RMS)
	}
	for _, p := range l.Peak {
		if p > -15 || p < -30 {
			t.Errorf("peak %.1f dBFS out of range", p)
		}
	}
}

func TestVerdicts(t *testing.T) {
	th := Thresholds{}.withDefaults()
	for loud, want := range map[float64]string{-90: "silent", -35: "low", -14: "good", -2: "high"} {
		if got := th.verdict(loud); got != want {
			t.Errorf("%.0f dBFS: %q, want %q", loud, got, want)
		}
	}
}
