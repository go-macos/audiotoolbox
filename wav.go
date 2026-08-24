// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

package audiotoolbox

import (
	"encoding/binary"
	"fmt"
	"io"
)

// WAV is a RIFF/WAVE file being written.
//
// It is here because a decoder nobody can check is a decoder nobody should
// trust. "It plays" is not a result — the reader of a report cannot hear it,
// and neither can a CI runner. A WAV file is: it opens in anything, its size
// can be divided by the frame size and compared against the track duration,
// and its samples can be looked at.
//
// The writer needs an [io.WriteSeeker] because RIFF states the size of the file
// and of the data chunk in their headers, before either is known. [WAV.Close]
// goes back and fills them in; a file closed without it is a file every reader
// refuses.
type WAV struct {
	w      io.WriteSeeker
	cfg    PlayerConfig
	frames int64
	closed bool
}

// wavHeaderSize is the bytes before the samples: 12 for the RIFF header, 24 for
// a PCM fmt chunk, 8 for the data chunk header.
const wavHeaderSize = 44

// WAV format tags. There are more, and these are the two a decoder produces.
const (
	wavFormatPCM   = 1
	wavFormatFloat = 3
)

// NewWAV starts a WAV file for the PCM cfg describes. The header is written
// with placeholder sizes, which [WAV.Close] corrects.
func NewWAV(w io.WriteSeeker, cfg PlayerConfig) (*WAV, error) {
	resolved, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	f := &WAV{w: w, cfg: resolved}
	if err := f.writeHeader(); err != nil {
		return nil, err
	}
	return f, nil
}

// header builds the 44 bytes that precede the samples, for a file holding the
// stated number of frames.
func (f *WAV) header() []byte {
	var (
		bps      = f.cfg.Format.Size() * 8
		blockAln = f.cfg.BytesPerFrame()
		data     = f.frames * int64(blockAln)
		tag      = uint16(wavFormatPCM)
	)
	if f.cfg.Format == Float32 {
		tag = wavFormatFloat
	}
	h := make([]byte, 0, wavHeaderSize)
	le := binary.LittleEndian
	put32 := func(v uint32) { h = le.AppendUint32(h, v) }
	put16 := func(v uint16) { h = le.AppendUint16(h, v) }

	h = append(h, "RIFF"...)
	// The RIFF size is everything after this field: the header minus the
	// eight bytes of "RIFF" and the size itself, plus the samples. A file too
	// big for the field is clamped rather than wrapped — a wrapped size makes
	// a reader stop early and say nothing.
	put32(clampU32(int64(wavHeaderSize-8) + data))
	h = append(h, "WAVEfmt "...)
	put32(16) // the size of a PCM fmt chunk
	put16(tag)
	put16(uint16(f.cfg.Channels))
	put32(uint32(f.cfg.SampleRate))
	put32(uint32(f.cfg.SampleRate * blockAln)) // bytes per second
	put16(uint16(blockAln))
	put16(uint16(bps))
	h = append(h, "data"...)
	put32(clampU32(data))
	return h
}

// clampU32 saturates a size at the largest a RIFF field holds, which is the
// honest thing to write for a file past four gigabytes: too small stops a
// reader early, wrapped sends it somewhere else entirely.
func clampU32(n int64) uint32 {
	const max = int64(^uint32(0))
	if n < 0 {
		return 0
	}
	if n > max {
		return uint32(max)
	}
	return uint32(n)
}

func (f *WAV) writeHeader() error {
	_, err := f.w.Write(f.header())
	return err
}

// Write appends interleaved PCM. A partial frame is refused, for the reason
// [Player.Write] gives.
func (f *WAV) Write(pcm []byte) (int, error) {
	if f.closed {
		return 0, ErrClosed
	}
	if len(pcm) == 0 {
		return 0, nil
	}
	bpf := f.cfg.BytesPerFrame()
	if len(pcm)%bpf != 0 {
		return 0, fmt.Errorf("%w: %d bytes is not a whole number of %d-byte frames",
			ErrPacket, len(pcm), bpf)
	}
	n, err := f.w.Write(pcm)
	f.frames += int64(n / bpf)
	return n, err
}

// Frames is how many sample frames have been written.
func (f *WAV) Frames() int64 { return f.frames }

// Close rewrites the header with the sizes now known. It does NOT close the
// underlying writer, which the caller opened and should close. Calling it twice
// is a no-op.
func (f *WAV) Close() error {
	if f.closed {
		return nil
	}
	f.closed = true
	if _, err := f.w.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if _, err := f.w.Write(f.header()); err != nil {
		return err
	}
	_, err := f.w.Seek(0, io.SeekEnd)
	return err
}
