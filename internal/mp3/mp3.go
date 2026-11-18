// Package mp3 splits an MPEG audio byte stream into frames without decoding
// it. Icegator forwards frames untouched; it only needs to know where they
// begin and end, and how long each one lasts.
package mp3

import (
	"bufio"
	"errors"
	"io"
	"time"
)

// Header is a decoded MPEG audio frame header.
type Header struct {
	Version    Version
	Layer      int // 1, 2 or 3
	Bitrate    int // bits per second; 0 never occurs (free format is rejected)
	SampleRate int // Hz
	Channels   int // 1 or 2
	Padding    bool
	Protected  bool // a 16-bit CRC follows the header
}

// Version is the MPEG audio version.
type Version int

const (
	MPEG1 Version = iota
	MPEG2
	MPEG25
)

func (v Version) String() string {
	switch v {
	case MPEG1:
		return "MPEG-1"
	case MPEG2:
		return "MPEG-2"
	default:
		return "MPEG-2.5"
	}
}

// HeaderSize is the size of a frame header in bytes.
const HeaderSize = 4

var errNoHeader = errors.New("mp3: not a frame header")

// kbps, indexed [column][bitrate index]. The columns are MPEG-1 Layers 1,
// 2 and 3, then MPEG-2 Layer 1, then MPEG-2 Layers 2 and 3 together.
var bitrates = [5][16]int{
	{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0},
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},
	{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},
	{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},
	{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},
}

var sampleRates = [3][3]int{
	{44100, 48000, 32000},
	{22050, 24000, 16000},
	{11025, 12000, 8000},
}

// ParseHeader decodes the first four bytes of b as a frame header.
func ParseHeader(b []byte) (Header, error) {
	var h Header
	if len(b) < HeaderSize || b[0] != 0xFF || b[1]&0xE0 != 0xE0 {
		return h, errNoHeader
	}
	switch (b[1] >> 3) & 3 {
	case 0:
		h.Version = MPEG25
	case 2:
		h.Version = MPEG2
	case 3:
		h.Version = MPEG1
	default:
		return h, errNoHeader
	}
	switch (b[1] >> 1) & 3 {
	case 1:
		h.Layer = 3
	case 2:
		h.Layer = 2
	case 3:
		h.Layer = 1
	default:
		return h, errNoHeader
	}
	h.Protected = b[1]&1 == 0

	var col int
	if h.Version == MPEG1 {
		col = h.Layer - 1
	} else if h.Layer == 1 {
		col = 3
	} else {
		col = 4
	}
	h.Bitrate = bitrates[col][b[2]>>4] * 1000
	if h.Bitrate == 0 {
		return h, errNoHeader // free format or invalid
	}
	sr := (b[2] >> 2) & 3
	if sr == 3 {
		return h, errNoHeader
	}
	h.SampleRate = sampleRates[h.Version][sr]
	h.Padding = b[2]&2 != 0
	if b[3]>>6 == 3 {
		h.Channels = 1
	} else {
		h.Channels = 2
	}
	return h, nil
}

// Samples is the number of PCM samples per channel the frame decodes to.
func (h Header) Samples() int {
	switch {
	case h.Layer == 1:
		return 384
	case h.Layer == 2:
		return 1152
	case h.Version == MPEG1:
		return 1152
	default:
		return 576
	}
}

// Size is the frame's total length in bytes, header included.
func (h Header) Size() int {
	pad := 0
	if h.Padding {
		pad = 1
	}
	if h.Layer == 1 {
		return (12*h.Bitrate/h.SampleRate + pad) * 4
	}
	return h.Samples()/8*h.Bitrate/h.SampleRate + pad
}

// Duration is how long the frame plays for.
func (h Header) Duration() time.Duration {
	return time.Duration(h.Samples()) * time.Second / time.Duration(h.SampleRate)
}

// compatible reports whether two headers could belong to the same stream.
// Bitrate and padding vary frame to frame; these fields do not.
func (h Header) compatible(o Header) bool {
	return h.Version == o.Version && h.Layer == o.Layer && h.SampleRate == o.SampleRate
}

// Frame is one complete MPEG audio frame.
type Frame struct {
	Header Header
	// Data is the whole frame, header included. It is only valid until the
	// next call to Reader.Next.
	Data []byte
}

// Reader finds frames in a byte stream. It skips ID3v2 tags and any other
// bytes between frames, and resynchronises after corruption.
type Reader struct {
	br      *bufio.Reader
	buf     []byte
	last    Header
	synced  bool
	skipped int64
}

// maxFrame is larger than any valid frame (MPEG-1 Layer 2 at 384k/32k is 1729).
const maxFrame = 2048

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 16*1024), buf: make([]byte, maxFrame)}
}

// Skipped is the number of non-frame bytes discarded so far.
func (r *Reader) Skipped() int64 { return r.skipped }

// Next returns the next frame. The error is io.EOF at a clean end of stream,
// or whatever the underlying reader returned.
func (r *Reader) Next() (Frame, error) {
	for {
		if err := r.skipID3(); err != nil {
			return Frame{}, err
		}
		head, err := r.br.Peek(HeaderSize)
		if err != nil {
			return Frame{}, eof(err)
		}
		h, err := ParseHeader(head)
		if err != nil || (r.synced && !h.compatible(r.last)) || !r.confirm(h) {
			r.br.Discard(1)
			r.skipped++
			continue
		}
		data := r.buf[:h.Size()]
		if _, err := io.ReadFull(r.br, data); err != nil {
			return Frame{}, eof(err)
		}
		r.last, r.synced = h, true
		return Frame{Header: h, Data: data}, nil
	}
}

// confirm guards against false sync words inside audio data or tags. Unless
// we are already locked on, the bytes right after the candidate frame must
// be another compatible header. At end of stream there is nothing to check
// against, so the candidate is accepted.
func (r *Reader) confirm(h Header) bool {
	size := h.Size()
	if size < HeaderSize {
		return false
	}
	if r.synced {
		// Locked on and the header matches the stream; a glitch here
		// costs one frame, and the next call resyncs.
		return true
	}
	b, err := r.br.Peek(size + HeaderSize)
	if err != nil {
		return len(b) >= size
	}
	next, err := ParseHeader(b[size:])
	return err == nil && next.compatible(h)
}

// skipID3 discards an ID3v2 tag if one starts at the current position.
func (r *Reader) skipID3() error {
	b, err := r.br.Peek(10)
	if err != nil || b[0] != 'I' || b[1] != 'D' || b[2] != '3' {
		return nil // too short to be a tag, or not one; Next reports any error
	}
	if b[6]|b[7]|b[8]|b[9] >= 0x80 {
		return nil
	}
	n := 10 + (int(b[6])<<21 | int(b[7])<<14 | int(b[8])<<7 | int(b[9]))
	if b[5]&0x10 != 0 {
		n += 10 // footer
	}
	d, err := r.br.Discard(n)
	r.skipped += int64(d)
	return eof(err)
}

func eof(err error) error {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return io.EOF
	}
	return err
}
