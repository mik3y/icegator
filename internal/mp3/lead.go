package mp3

// An MP3 frame does not have to hold all of its own audio data. An encoder
// may start a frame's data in space left over at the end of earlier frames
// (the "bit reservoir"), and the frame records how far back its data begins.
// A stream that starts on such a frame is missing the start of that frame's
// data, and a decoder will drop the frame or play noise.
//
// The cure used here is the one MP3 cutting tools use. The frames just
// before the starting point are sent as well, altered so that they decode as
// silence but still carry their original data bytes. The first real frame
// then finds the bytes it reaches back for exactly where it expects them.
// No audio is decoded or re-encoded. The cost is a few hundredths of a
// second of silence ahead of the first frame.

// sideInfoSize is the size of the block between a Layer III frame's header
// (and CRC, if any) and its audio data. It describes how to decode the data.
func (h Header) sideInfoSize() int {
	switch {
	case h.Version == MPEG1 && h.Channels == 2:
		return 32
	case h.Version == MPEG1, h.Channels == 2:
		return 17
	default:
		return 9
	}
}

// dataOffset is where a Layer III frame's own audio data area begins.
func (h Header) dataOffset() int {
	at := HeaderSize + h.sideInfoSize()
	if h.Protected {
		at += 2
	}
	return at
}

// Carrier returns a copy of a Layer III frame that decodes as silence while
// still carrying the frame's audio data bytes for later frames to borrow
// from. back is how many bytes of earlier carriers' data precede it in the
// stream. ok is false if the frame is not Layer III or is malformed.
//
// The carrier declares that its own (empty) data begins back bytes ago.
// That matters because decoders keep only as much old data as the current
// frame claims to reach back to. A carrier claiming to stand alone would
// make the decoder forget the carriers before it.
func Carrier(frame []byte, back int) (carrier []byte, ok bool) {
	h, err := ParseHeader(frame)
	if err != nil || h.Layer != 3 || len(frame) < h.dataOffset() || len(frame) != h.Size() {
		return nil, false
	}
	carrier = append([]byte(nil), frame...)
	side := HeaderSize
	if h.Protected {
		side += 2
	}
	// Zeroed side information says that each granule has no data to
	// decode, which is silence.
	clear(carrier[side : side+h.sideInfoSize()])
	if h.Version == MPEG1 {
		back = min(back, 511) // a 9-bit field
		carrier[side] = byte(back >> 1)
		carrier[side+1] = byte(back&1) << 7
	} else {
		carrier[side] = byte(min(back, 255)) // an 8-bit field
	}
	if h.Protected {
		// The CRC covers the last two header bytes and the side information.
		crc := crc16(0xFFFF, carrier[2:4])
		crc = crc16(crc, carrier[side:side+h.sideInfoSize()])
		carrier[4], carrier[5] = byte(crc>>8), byte(crc)
	}
	return carrier, true
}

// crc16 is the CRC MPEG audio uses, with polynomial 0x8005.
func crc16(crc uint16, data []byte) uint16 {
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x8005
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}

// Lead returns the frames to send ahead of first so that a stream starting
// there decodes cleanly. preceding holds the frames that came immediately
// before first in the original stream, oldest first. The result is empty
// if first borrows nothing, and is as much as can be provided if preceding
// does not reach back far enough.
func Lead(preceding [][]byte, first []byte) [][]byte {
	need, ok := MainDataBegin(first)
	if !ok || need == 0 {
		return nil
	}
	h, _ := ParseHeader(first)
	// Work out how far back to go, newest frame first.
	from := len(preceding)
	for from > 0 && need > 0 {
		ph, err := ParseHeader(preceding[from-1])
		if err != nil || ph.Layer != 3 || !h.Joinable(ph) || len(preceding[from-1]) != ph.Size() {
			break
		}
		from--
		need -= len(preceding[from]) - ph.dataOffset()
	}
	// Then build the carriers oldest first, each declaring what precedes it.
	var lead [][]byte
	back := 0
	for _, frame := range preceding[from:] {
		carrier, ok := Carrier(frame, back)
		if !ok {
			return nil
		}
		lead = append(lead, carrier)
		ph, _ := ParseHeader(frame)
		back += len(frame) - ph.dataOffset()
	}
	return lead
}

// IsCarrier reports whether a frame is one made by Carrier, which is to say
// a Layer III frame whose side information declares no audio.
func IsCarrier(frame []byte) bool {
	h, err := ParseHeader(frame)
	if err != nil || h.Layer != 3 || len(frame) < h.dataOffset() {
		return false
	}
	side := frame[h.dataOffset()-h.sideInfoSize() : h.dataOffset()]
	// Everything after the field saying where the data begins is zero.
	// That field is the first nine bits in MPEG-1 and the first eight
	// otherwise.
	if h.Version == MPEG1 {
		return side[1]&0x7F == 0 && allZero(side[2:])
	}
	return allZero(side[1:])
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// DataArea returns the part of a Layer III frame that holds audio data, or
// nil if it is not one.
func DataArea(frame []byte) []byte {
	h, err := ParseHeader(frame)
	if err != nil || h.Layer != 3 || len(frame) < h.dataOffset() {
		return nil
	}
	return frame[h.dataOffset():]
}

// MaxLead is the most frames Lead can need. A frame reaches back at most
// 511 bytes, and at the lowest bitrates a frame holds little more than 50.
const MaxLead = 12
