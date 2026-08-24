// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

package audiotoolbox

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

// memFile is an in-memory io.WriteSeeker that can be told to fail on a chosen
// call, so every error branch of the writer is reachable without a disk that
// misbehaves on demand.
type memFile struct {
	buf []byte
	pos int64

	writes    int
	failWrite int // fail the nth Write, counting from 1; 0 never fails
	seeks     int
	failSeek  int // likewise for Seek
}

var errDisk = errors.New("the disk said no")

func (f *memFile) Write(p []byte) (int, error) {
	f.writes++
	if f.writes == f.failWrite {
		return 0, errDisk
	}
	if need := f.pos + int64(len(p)); need > int64(len(f.buf)) {
		f.buf = append(f.buf, make([]byte, need-int64(len(f.buf)))...)
	}
	copy(f.buf[f.pos:], p)
	f.pos += int64(len(p))
	return len(p), nil
}

func (f *memFile) Seek(off int64, whence int) (int64, error) {
	f.seeks++
	if f.seeks == f.failSeek {
		return 0, errDisk
	}
	switch whence {
	case io.SeekStart:
		f.pos = off
	case io.SeekCurrent:
		f.pos += off
	case io.SeekEnd:
		f.pos = int64(len(f.buf)) + off
	}
	return f.pos, nil
}

// stereo48k is the output a decoder of the control file produces.
func stereo48k() PlayerConfig {
	return PlayerConfig{SampleRate: 48000, Channels: 2, Format: Int16}
}

func TestWAVRefusesAConfigurationThatDescribesNoOutput(t *testing.T) {
	if _, err := NewWAV(&memFile{}, PlayerConfig{}); !errors.Is(err, ErrConfig) {
		t.Errorf("NewWAV(no sample rate) = %v, want ErrConfig", err)
	}
}

func TestWAVReportsAHeaderItCouldNotWrite(t *testing.T) {
	if _, err := NewWAV(&memFile{failWrite: 1}, stereo48k()); !errors.Is(err, errDisk) {
		t.Errorf("NewWAV(failing writer) = %v, want the disk error", err)
	}
}

// TestWAVHeaderIsTheOneEveryReaderExpects checks the 44 bytes field by field,
// because a WAV a reader refuses is a proof that proves nothing.
func TestWAVHeaderIsTheOneEveryReaderExpects(t *testing.T) {
	f := &memFile{}
	w, err := NewWAV(f, stereo48k())
	if err != nil {
		t.Fatalf("NewWAV: %v", err)
	}
	// Ten frames of stereo s16 is forty bytes.
	pcm := make([]byte, 40)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	n, err := w.Write(pcm)
	if err != nil || n != len(pcm) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(pcm))
	}
	if got := w.Frames(); got != 10 {
		t.Errorf("Frames = %d, want 10", got)
	}
	// An empty write is not an error and moves nothing.
	if n, err := w.Write(nil); n != 0 || err != nil {
		t.Errorf("Write(nil) = %d, %v; want 0, nil", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Closing twice is a no-op rather than a second header.
	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	b := f.buf
	if len(b) != wavHeaderSize+40 {
		t.Fatalf("the file is %d bytes, want %d", len(b), wavHeaderSize+40)
	}
	le := binary.LittleEndian
	for _, tc := range []struct {
		name       string
		got, want  uint32
		sixteenBit bool
	}{
		{name: "RIFF size", got: le.Uint32(b[4:]), want: uint32(wavHeaderSize - 8 + 40)},
		{name: "fmt chunk size", got: le.Uint32(b[16:]), want: 16},
		{name: "format tag", got: uint32(le.Uint16(b[20:])), want: wavFormatPCM},
		{name: "channels", got: uint32(le.Uint16(b[22:])), want: 2},
		{name: "sample rate", got: le.Uint32(b[24:]), want: 48000},
		{name: "bytes per second", got: le.Uint32(b[28:]), want: 48000 * 4},
		{name: "block align", got: uint32(le.Uint16(b[32:])), want: 4},
		{name: "bits per sample", got: uint32(le.Uint16(b[34:])), want: 16},
		{name: "data size", got: le.Uint32(b[40:]), want: 40},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
	for _, tc := range []struct {
		off  int
		want string
	}{
		{0, "RIFF"}, {8, "WAVE"}, {12, "fmt "}, {36, "data"},
	} {
		if got := string(b[tc.off : tc.off+4]); got != tc.want {
			t.Errorf("bytes at %d = %q, want %q", tc.off, got, tc.want)
		}
	}
	if !bytes.Equal(b[wavHeaderSize:], pcm) {
		t.Error("the samples are not the ones written")
	}
}

// TestWAVFloatGetsTheFloatTag guards the one field that depends on the sample
// format: a float WAV tagged as integer PCM plays as noise.
func TestWAVFloatGetsTheFloatTag(t *testing.T) {
	f := &memFile{}
	w, err := NewWAV(f, PlayerConfig{SampleRate: 44100, Channels: 1, Format: Float32})
	if err != nil {
		t.Fatalf("NewWAV: %v", err)
	}
	if _, err := w.Write(make([]byte, 4)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	le := binary.LittleEndian
	if got := le.Uint16(f.buf[20:]); got != wavFormatFloat {
		t.Errorf("format tag = %d, want %d", got, wavFormatFloat)
	}
	if got := le.Uint16(f.buf[34:]); got != 32 {
		t.Errorf("bits per sample = %d, want 32", got)
	}
	if got := le.Uint16(f.buf[32:]); got != 4 {
		t.Errorf("block align = %d, want 4", got)
	}
}

func TestWAVRefusesAPartialFrameAndAWriteAfterClose(t *testing.T) {
	f := &memFile{}
	w, err := NewWAV(f, stereo48k())
	if err != nil {
		t.Fatalf("NewWAV: %v", err)
	}
	// Three bytes is not a whole number of four-byte frames, and queueing it
	// would shift every channel after it by one.
	if _, err := w.Write([]byte{1, 2, 3}); !errors.Is(err, ErrPacket) {
		t.Errorf("Write(partial frame) = %v, want ErrPacket", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := w.Write(make([]byte, 4)); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
}

func TestWAVReportsEveryFailureClosingIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		file *memFile
	}{
		// The header write is the first, so the rewrite is the second.
		{"the rewind fails", &memFile{failSeek: 1}},
		{"the rewritten header fails", &memFile{failWrite: 2}},
		{"the seek back to the end fails", &memFile{failSeek: 2}},
	} {
		w, err := NewWAV(tc.file, stereo48k())
		if err != nil {
			t.Fatalf("%s: NewWAV: %v", tc.name, err)
		}
		if err := w.Close(); !errors.Is(err, errDisk) {
			t.Errorf("%s: Close = %v, want the disk error", tc.name, err)
		}
	}
}

// TestClampU32 is direct because neither bound is reachable through a header:
// a frame count is never negative, and four gigabytes of samples is not a test
// anyone should run.
func TestClampU32(t *testing.T) {
	const max = int64(^uint32(0))
	for _, tc := range []struct {
		in   int64
		want uint32
	}{
		{-1, 0},
		{0, 0},
		{44, 44},
		{max, uint32(max)},
		{max + 1, uint32(max)},
		{1 << 40, uint32(max)},
	} {
		if got := clampU32(tc.in); got != tc.want {
			t.Errorf("clampU32(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestWAVSizeIsTheDurationArithmetic is the check a reader of a report can
// repeat by hand: a WAV of a given duration is exactly this many bytes.
func TestWAVSizeIsTheDurationArithmetic(t *testing.T) {
	f := &memFile{}
	cfg := stereo48k()
	w, err := NewWAV(f, cfg)
	if err != nil {
		t.Fatalf("NewWAV: %v", err)
	}
	// Three seconds of 48 kHz stereo s16.
	const seconds = 3
	frames := cfg.SampleRate * seconds
	if _, err := w.Write(make([]byte, frames*cfg.BytesPerFrame())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	want := wavHeaderSize + seconds*cfg.SampleRate*cfg.Channels*2
	if len(f.buf) != want {
		t.Errorf("a %d-second file is %d bytes, want %d (44 + duration x rate x channels x 2)",
			seconds, len(f.buf), want)
	}
}
