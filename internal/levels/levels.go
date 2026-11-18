// Package levels meters a DJ's stream. It decodes a copy of the audio (the
// frames that go to air are never touched) and publishes VU-style readings
// plus a coarse verdict on whether the stream is too quiet or too hot.
package levels

import (
	"io"
	"math"

	gomp3 "github.com/hajimehoshi/go-mp3"

	"github.com/splatboard/icegator/internal/session"
)

// Thresholds classify loudness (RMS over the last few seconds, dBFS).
type Thresholds struct {
	Silent float64 // below this the stream is effectively silence; default -60
	Low    float64 // below this it is too quiet; default -24
	High   float64 // above this it is too hot; default -5
}

func (t Thresholds) withDefaults() Thresholds {
	if t.Silent == 0 {
		t.Silent = -60
	}
	if t.Low == 0 {
		t.Low = -24
	}
	if t.High == 0 {
		t.High = -5
	}
	return t
}

func (t Thresholds) verdict(loudness float64) string {
	switch {
	case loudness < t.Silent:
		return "silent"
	case loudness < t.Low:
		return "low"
	case loudness > t.High:
		return "high"
	default:
		return "good"
	}
}

const (
	floorDB = -120.0
	// The decoder always produces 16-bit stereo.
	bytesPerSample = 4
	// readingsPerSecond is how often a reading is published. Each one
	// averages the audio since the last, so fewer readings make a calmer
	// meter. Four a second moves about as a mechanical VU meter does.
	readingsPerSecond = 4
	// loudnessWindows readings span the three seconds Loudness covers.
	loudnessWindows = 3 * readingsPerSecond
)

func db(meanSquare float64) float64 {
	if meanSquare <= 1e-12 {
		return floorDB
	}
	return math.Max(floorDB, 10*math.Log10(meanSquare))
}

// Meter runs until the session ends, storing readings in s.Level four
// times a second. Start it in its own goroutine before the session's Run.
func Meter(s *session.Session, t Thresholds) {
	t = t.withDefaults()
	sub := s.Subscribe(256)
	defer sub.Close()

	pr, pw := io.Pipe()
	go func() {
		for frame := range sub.Frames {
			if _, err := pw.Write(frame); err != nil {
				return
			}
		}
		pw.Close()
	}()
	defer pr.Close()

	dec, err := gomp3.NewDecoder(pr)
	if err != nil {
		return
	}
	window := dec.SampleRate() / readingsPerSecond // samples per reading
	if window <= 0 {
		return
	}
	buf := make([]byte, window*bytesPerSample)
	var recent [loudnessWindows]float64 // mean square of both channels per window
	n := 0
	for {
		if _, err := io.ReadFull(dec, buf); err != nil {
			return
		}
		var sum, peak [2]float64
		for i := 0; i+3 < len(buf); i += bytesPerSample {
			for ch := 0; ch < 2; ch++ {
				v := float64(int16(uint16(buf[i+2*ch])|uint16(buf[i+2*ch+1])<<8)) / 32768
				sum[ch] += v * v
				if a := math.Abs(v); a > peak[ch] {
					peak[ch] = a
				}
			}
		}
		w := float64(window)
		recent[n%loudnessWindows] = (sum[0] + sum[1]) / (2 * w)
		n++
		var total float64
		count := min(n, loudnessWindows)
		for _, ms := range recent[:count] {
			total += ms
		}
		loud := db(total / float64(count))
		s.Level.Store(&session.Level{
			RMS:      [2]float64{round(db(sum[0] / w)), round(db(sum[1] / w))},
			Peak:     [2]float64{round(db(peak[0] * peak[0])), round(db(peak[1] * peak[1]))},
			Loudness: round(loud),
			Verdict:  t.verdict(loud),
		})
	}
}

func round(v float64) float64 { return math.Round(v*10) / 10 }
