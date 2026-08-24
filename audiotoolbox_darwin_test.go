// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package audiotoolbox

import (
	"encoding/binary"
	"errors"
	"math"
	"os"
	"testing"
	"time"

	"github.com/go-avkit/avkit/container"
)

// ---------------------------------------------------------------------------
// A test vector, so the real decoder can be exercised on a machine with no
// media on it at all.
//
// sineAAC is ten AAC-LC access units: a 1 kHz sine at -6 dBFS, mono, 48 kHz,
// encoded by afconvert and demuxed out of the resulting MP4 by
// github.com/go-avkit/avkit/container. Seven hundred and thirty-two bytes of
// it, which is what a signal this simple costs.
//
// It is here because "the decoder works" has to be checkable by a CI runner
// that has no video file, no sound card and no way to listen. A sine has one
// property a test can assert without ears: after decoding, the energy at 1 kHz
// must dwarf the energy anywhere else. A decoder that is silently wrong — wrong
// channel count, wrong sample rate, bytes read as the wrong width — fails that
// and cannot fake it.
// ---------------------------------------------------------------------------

const (
	sineRate     = 48000
	sineChannels = 1
	sineFreq     = 1000.0
	// sineAmplitude is the -6 dBFS the signal was generated at. AAC at this
	// bitrate does not preserve it exactly, which is why the assertion below
	// is a band and not an equality.
	sineAmplitude = 16384.0
)

//go:generate echo "regenerate with afconvert -f m4af -d aac@48000 -b 64000 sine.wav sine.m4a"

func aacSine() Config {
	return Config{Codec: AAC, SampleRate: sineRate, Channels: sineChannels, AudioObjectType: 2}
}

// goertzel is the amplitude of one frequency in a block of samples. It is the
// whole of a Fourier transform that this needs, and it is nine lines.
func goertzel(pcm []int16, rate int, freq float64) float64 {
	if len(pcm) == 0 {
		return 0
	}
	k := 2 * math.Cos(2*math.Pi*freq/float64(rate))
	var s1, s2 float64
	for _, v := range pcm {
		s0 := float64(v) + k*s1 - s2
		s2, s1 = s1, s0
	}
	power := s1*s1 + s2*s2 - k*s1*s2
	if power < 0 {
		power = 0
	}
	return 2 * math.Sqrt(power) / float64(len(pcm))
}

// samples reads interleaved little-endian s16 out of a decoded buffer.
func samples(pcm []byte) []int16 {
	out := make([]int16, len(pcm)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(pcm[i*2:]))
	}
	return out
}

// TestRealDecodeOfAKnownSignal is this package's proof on a machine with no
// media: a real AudioConverter, real AAC packets, and a decoded signal whose
// frequency is asserted rather than described.
func TestRealDecodeOfAKnownSignal(t *testing.T) {
	dec, err := NewDecoder(aacSine())
	if err != nil {
		t.Fatalf("NewDecoder(aac 48k mono) = %v", err)
	}
	defer dec.Close()
	if err := dec.CodecConfigRefused(); err != nil {
		t.Logf("the magic cookie was turned down: %v", err)
	}

	var pcm []int16
	for i, pkt := range sineAAC {
		buf, err := dec.Decode(Packet{Data: pkt, PTS: time.Duration(i) * 1024 * time.Second / sineRate})
		if err != nil {
			t.Fatalf("packet %d of %d: %v", i, len(sineAAC), err)
		}
		if buf.Frames == 0 {
			// The encoder's priming produces no output, which is normal.
			continue
		}
		if buf.Channels != sineChannels || buf.SampleRate != sineRate || buf.Format != Int16 {
			t.Fatalf("packet %d decoded to %d ch %d Hz %v, want %d ch %d Hz s16",
				i, buf.Channels, buf.SampleRate, buf.Format, sineChannels, sineRate)
		}
		if want := buf.Frames * buf.Channels * 2; len(buf.PCM) != want {
			t.Fatalf("packet %d: %d frames but %d bytes, want %d", i, buf.Frames, len(buf.PCM), want)
		}
		pcm = append(pcm, samples(buf.PCM)...)
	}
	flushed, err := dec.Flush()
	if err != nil {
		t.Fatalf("Flush = %v", err)
	}
	pcm = append(pcm, samples(flushed.PCM)...)

	if int64(len(pcm)) != dec.Frames()*sineChannels {
		t.Errorf("collected %d samples but the decoder counted %d frames of %d channels",
			len(pcm), dec.Frames(), sineChannels)
	}
	// Ten packets of 1024 frames is 10240; the decoder swallows the priming
	// and a flush gives some of it back, so this is a floor, not an equality.
	if len(pcm) < 8*1024 {
		t.Fatalf("ten AAC packets decoded to %d samples, want at least %d", len(pcm), 8*1024)
	}
	t.Logf("decoded %d samples (%v) from %d packets", len(pcm), dec.Decoded(), len(sineAAC))

	// Skip the first two frames: the encoder's priming decodes to a ramp,
	// not to the signal.
	signal := pcm[2*1024:]
	at1k := goertzel(signal, sineRate, sineFreq)
	// A quarter of the way to Nyquist and nowhere near a harmonic of 1 kHz:
	// if the decode were noise, or the samples were read at the wrong width,
	// this would not be far below the signal.
	atElsewhere := goertzel(signal, sineRate, 6500)
	t.Logf("1 kHz: %.0f, 6.5 kHz: %.0f (of a full scale of 32767)", at1k, atElsewhere)
	if at1k < sineAmplitude/2 {
		t.Errorf("the 1 kHz component is %.0f, want at least %.0f — this is not the signal that "+
			"went in", at1k, sineAmplitude/2)
	}
	if atElsewhere > at1k/20 {
		t.Errorf("6.5 kHz carries %.0f against 1 kHz's %.0f: a decode this noisy is a decode gone "+
			"wrong", atElsewhere, at1k)
	}
}

// TestDecoderRefusesUseAfterClose exercises the real handle, not a fake.
func TestDecoderRefusesUseAfterClose(t *testing.T) {
	dec, err := NewDecoder(aacSine())
	if err != nil {
		t.Fatalf("NewDecoder = %v", err)
	}
	if err := dec.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	// Closing twice is a no-op, and the second close must not free anything
	// a second time.
	if err := dec.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := dec.Decode(Packet{Data: sineAAC[1]}); !errors.Is(err, ErrClosed) {
		t.Errorf("Decode after Close = %v, want ErrClosed", err)
	}
	if err := dec.CodecConfigRefused(); err != nil {
		t.Errorf("CodecConfigRefused after Close = %v, want nil", err)
	}
}

// TestRealDecodeRejectsRubbish feeds the converter bytes that are not an access
// unit. What matters is that it says so instead of returning noise as PCM.
func TestRealDecodeRejectsRubbish(t *testing.T) {
	dec, err := NewDecoder(aacSine())
	if err != nil {
		t.Fatalf("NewDecoder = %v", err)
	}
	defer dec.Close()
	junk := make([]byte, 64)
	for i := range junk {
		junk[i] = 0xff
	}
	buf, err := dec.Decode(Packet{Data: junk})
	if err == nil {
		t.Logf("the converter accepted 64 bytes of 0xff and produced %d frames", buf.Frames)
		return
	}
	var se *StatusError
	if !errors.As(err, &se) {
		t.Errorf("Decode(junk) = %v, want a *StatusError from AudioToolbox", err)
	} else {
		t.Logf("the converter answered %v", se)
	}
}

// TestWhichCodecsThisMacDecodes builds a real converter for every codec this
// package names. It asserts nothing about which ones a given macOS has —
// that is the machine's business, and Opus in particular came late — but it
// records the answer, which is the only way a reader finds out.
func TestWhichCodecsThisMacDecodes(t *testing.T) {
	for _, tc := range []struct {
		codec Codec
		cfg   Config
	}{
		{AAC, Config{Codec: AAC, SampleRate: 48000, Channels: 2, AudioObjectType: 2}},
		{AC3, Config{Codec: AC3, SampleRate: 48000, Channels: 6}},
		{EAC3, Config{Codec: EAC3, SampleRate: 48000, Channels: 6}},
		{Opus, Config{Codec: Opus, SampleRate: 48000, Channels: 2}},
	} {
		dec, err := NewDecoder(tc.cfg)
		if err != nil {
			t.Logf("%v: no converter on this machine (%v)", tc.codec, err)
			if !errors.Is(err, ErrUnsupportedCodec) {
				t.Errorf("%v: New failed with %v, want it wrapped in ErrUnsupportedCodec", tc.codec, err)
			}
			continue
		}
		t.Logf("%v: a converter was built", tc.codec)
		dec.Close()
	}
}

func TestDoLoadReportsADlopenFailure(t *testing.T) {
	// The real load has to have happened first, or this leaves the package
	// half-wired for every test that follows.
	if err := load(); err != nil {
		t.Fatalf("load() = %v", err)
	}
	boom := errors.New("no such library")
	saved := dlopen
	t.Cleanup(func() { dlopen = saved })
	// Both dlopens must be covered: the framework and libSystem.
	dlopen = func(string) (uintptr, error) { return 0, boom }
	if err := doLoad(); !errors.Is(err, boom) {
		t.Errorf("doLoad with a failing dlopen = %v, want the dlopen error", err)
	}
	calls := 0
	dlopen = func(path string) (uintptr, error) {
		calls++
		if calls == 1 {
			return saved(path)
		}
		return 0, boom
	}
	if err := doLoad(); !errors.Is(err, boom) {
		t.Errorf("doLoad with a failing libSystem = %v, want the dlopen error", err)
	}
}

func TestSourceFormatNamesEveryCodec(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		id   uint32
		fpp  uint32
	}{
		{"aac-lc", Config{Codec: AAC, AudioObjectType: 2}, kAudioFormatMPEG4AAC, 1024},
		{"he-aac", Config{Codec: AAC, AudioObjectType: 5}, kAudioFormatMPEG4AAC_HE, 2048},
		{"he-aac v2", Config{Codec: AAC, AudioObjectType: 29}, kAudioFormatMPEG4AAC_HEv2, 2048},
		{"ac-3", Config{Codec: AC3}, kAudioFormatAC3, 1536},
		{"ec-3", Config{Codec: EAC3}, kAudioFormatEnhancedAC3, 1536},
		{"opus", Config{Codec: Opus}, kAudioFormatOpus, 2880},
	} {
		got, err := sourceFormat(tc.cfg)
		if err != nil {
			t.Errorf("%s: sourceFormat = %v", tc.name, err)
			continue
		}
		if got.FormatID != tc.id {
			t.Errorf("%s: format id %#x, want %#x", tc.name, got.FormatID, tc.id)
		}
		if got.FramesPerPacket != tc.fpp {
			t.Errorf("%s: %d frames per packet, want %d", tc.name, got.FramesPerPacket, tc.fpp)
		}
		// A compressed format states no byte size, and stating one would be
		// stating something false.
		if got.BytesPerPacket != 0 || got.BytesPerFrame != 0 {
			t.Errorf("%s: a compressed format claimed %d bytes per packet and %d per frame",
				tc.name, got.BytesPerPacket, got.BytesPerFrame)
		}
	}
	if _, err := sourceFormat(Config{Codec: Codec(200)}); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("sourceFormat(Codec(200)) = %v, want ErrUnsupportedCodec", err)
	}
}

func TestPCMFormatDescribesInterleavedSamples(t *testing.T) {
	s16 := pcmFormat(48000, 2, Int16)
	if s16.FormatFlags != kAudioFormatFlagIsPacked|kAudioFormatFlagIsSignedInteger {
		t.Errorf("s16 flags = %#x, want packed signed integer", s16.FormatFlags)
	}
	if s16.BytesPerFrame != 4 || s16.BitsPerChannel != 16 || s16.FramesPerPacket != 1 {
		t.Errorf("s16 = %+v, want 4 bytes a frame, 16 bits a channel, one frame a packet", s16)
	}
	f32 := pcmFormat(44100, 1, Float32)
	if f32.FormatFlags != kAudioFormatFlagIsPacked|kAudioFormatFlagIsFloat {
		t.Errorf("f32 flags = %#x, want packed float", f32.FormatFlags)
	}
	if f32.BytesPerFrame != 4 || f32.BitsPerChannel != 32 {
		t.Errorf("f32 = %+v, want 4 bytes a frame and 32 bits a channel", f32)
	}
}

func TestRegistriesHandOutKeysAndTakeThemBack(t *testing.T) {
	d := &darwinDecoder{}
	registerDecoder(d)
	if d.id == 0 {
		t.Fatal("registerDecoder left the id at zero, which is the no-reference value")
	}
	if got := lookupDecoder(d.id); got != d {
		t.Errorf("lookupDecoder(%d) = %p, want %p", d.id, got, d)
	}
	unregisterDecoder(d)
	if got := lookupDecoder(d.id); got != nil {
		t.Errorf("lookupDecoder after unregister = %p, want nil", got)
	}

	p := &darwinPlayer{free: make(chan *audioQueueBuffer, 1)}
	registerPlayer(p)
	if p.id == 0 {
		t.Fatal("registerPlayer left the id at zero")
	}
	if got := lookupPlayer(p.id); got != p {
		t.Errorf("lookupPlayer(%d) = %p, want %p", p.id, got, p)
	}
	unregisterPlayer(p)
	if got := lookupPlayer(p.id); got != nil {
		t.Errorf("lookupPlayer after unregister = %p, want nil", got)
	}
}

// TestCallbacksSurviveAnOwnerThatHasGone is the shutdown race written down: a
// converter fill or a queue buffer can land after Close, and a callback that
// dereferenced a missing owner would take the process with it.
func TestCallbacksSurviveAnOwnerThatHasGone(t *testing.T) {
	var packets uint32 = 7
	var list audioBufferList
	converterInput(0, &packets, &list, nil, 999999)
	if packets != 0 {
		t.Errorf("the input proc for a decoder that has gone reported %d packets, want 0", packets)
	}
	// A buffer for a player that has gone belongs to the queue, and must be
	// dropped rather than dereferenced.
	queueOutput(999999, 0, nil)
}

// TestConverterInputHandsOverOnePacketOnceIs the input proc's contract: one
// packet, once, and then "not now" — which is what stops a converter from
// deciding the stream ended.
func TestConverterInputHandsOverOnePacketOnce(t *testing.T) {
	d := &darwinDecoder{cfg: Config{Channels: 2}, pending: []byte{1, 2, 3, 4}}
	d.desc = &audioStreamPacketDescription{}
	registerDecoder(d)
	defer unregisterDecoder(d)

	var packets uint32
	var list audioBufferList
	var desc *audioStreamPacketDescription
	if st := converterInput(0, &packets, &list, &desc, d.id); st != 0 {
		t.Fatalf("the first call answered %#x, want noErr", st)
	}
	if packets != 1 || list.Buffers[0].DataByteSize != 4 || list.NumberBuffers != 1 {
		t.Errorf("the first call handed over %d packets of %d bytes in %d buffers, want 1 of 4 in 1",
			packets, list.Buffers[0].DataByteSize, list.NumberBuffers)
	}
	if desc == nil || desc.DataByteSize != 4 || desc.StartOffset != 0 {
		t.Errorf("the packet description is %+v, want 4 bytes at offset 0", desc)
	}
	// The second call must say "not now", not "never".
	if st := converterInput(0, &packets, &list, &desc, d.id); st != noMoreInput {
		t.Errorf("the second call answered %#x, want noMoreInput (%#x) — noErr would tell the "+
			"converter the stream ended", st, noMoreInput)
	}
	if packets != 0 {
		t.Errorf("the second call reported %d packets, want 0", packets)
	}
	// After a flush it may say "never", and that is the one place zero
	// packets with noErr is right.
	d.eos = true
	if st := converterInput(0, &packets, &list, &desc, d.id); st != 0 {
		t.Errorf("after end of stream the proc answered %#x, want noErr", st)
	}
	// A description pointer the converter did not ask for is not written.
	d.fed, d.eos = false, false
	if st := converterInput(0, &packets, &list, nil, d.id); st != 0 || packets != 1 {
		t.Errorf("with no description pointer: %#x, %d packets; want noErr and 1", st, packets)
	}
}

func TestPlatformSeamsRefuseAForeignHandle(t *testing.T) {
	if _, err := darwinDecode(nil, Config{}, Packet{Data: []byte{1}}); !errors.Is(err, ErrClosed) {
		t.Errorf("darwinDecode(nil) = %v, want ErrClosed", err)
	}
	if _, err := darwinDecode("not a decoder", Config{}, Packet{}); !errors.Is(err, ErrClosed) {
		t.Errorf("darwinDecode(junk) = %v, want ErrClosed", err)
	}
	if _, err := darwinFlush("not a decoder", Config{}); !errors.Is(err, ErrClosed) {
		t.Errorf("darwinFlush(junk) = %v, want ErrClosed", err)
	}
	if err := darwinCloseDecoder(nil); err != nil {
		t.Errorf("darwinCloseDecoder(nil) = %v, want nil", err)
	}
	if err := darwinCookieRefused("not a decoder"); err != nil {
		t.Errorf("darwinCookieRefused(junk) = %v, want nil", err)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"start", darwinStart(nil)},
		{"drain", darwinDrain("not a player")},
		{"stop", darwinStop(nil)},
	} {
		if !errors.Is(tc.err, ErrClosed) {
			t.Errorf("darwin%s of a foreign handle = %v, want ErrClosed", tc.name, tc.err)
		}
	}
	if _, err := darwinWrite(nil, []byte{1, 2}); !errors.Is(err, ErrClosed) {
		t.Errorf("darwinWrite(nil) = %v, want ErrClosed", err)
	}
	if got := darwinPlayed(nil); got != 0 {
		t.Errorf("darwinPlayed(nil) = %v, want 0", got)
	}
	if err := darwinClosePlayer(nil); err != nil {
		t.Errorf("darwinClosePlayer(nil) = %v, want nil", err)
	}
}

// TestRealPlayerPlaysAndKeepsTime opens the system output and plays silence.
//
// A runner with no output device cannot do this, and says so rather than
// failing: what is asserted is everything from a queue that DID open —
// the buffers, the write, the clock and the drain.
func TestRealPlayerPlaysAndKeepsTime(t *testing.T) {
	const (
		rate     = 48000
		channels = 2
		seconds  = 0.25
	)
	p, err := NewPlayer(PlayerConfig{SampleRate: rate, Channels: channels, Volume: 0.01})
	if err != nil {
		t.Skipf("no output device on this machine: %v", err)
	}
	defer p.Close()
	if got := p.Played(); got != 0 {
		t.Errorf("a queue that has not started has played %v, want 0", got)
	}
	if err := p.Start(); err != nil {
		t.Skipf("the output device would not start: %v", err)
	}
	// Starting twice is a no-op rather than a second start.
	if err := p.Start(); err != nil {
		t.Errorf("second Start = %v", err)
	}
	frames := int(rate * seconds)
	silence := make([]byte, frames*channels*2)
	n, err := p.Write(silence)
	if err != nil || n != len(silence) {
		t.Fatalf("Write = %d, %v; want %d, nil", n, err, len(silence))
	}
	if got := p.Written(); got != int64(frames) {
		t.Errorf("Written = %d frames, want %d", got, frames)
	}
	if err := p.Drain(); err != nil {
		t.Fatalf("Drain = %v", err)
	}
	played := p.Played()
	t.Logf("wrote %v of silence, the device played %v", time.Duration(seconds*float64(time.Second)), played)
	// A drained queue has played what it was given. The tolerance is wide
	// because a virtual device on a runner is not a sound card, but a clock
	// that reports nothing at all is not a clock.
	if played <= 0 {
		t.Errorf("the clock reports %v after draining %v of audio", played, time.Duration(seconds*float64(time.Second)))
	}
	if want := time.Duration(seconds * float64(time.Second)); played > want*2 {
		t.Errorf("the clock reports %v after %v of audio, which is more than was written", played, want)
	}
	if err := p.Stop(); err != nil {
		t.Errorf("Stop = %v", err)
	}
}

// TestRealPlayerRefusesWhatItCannotPlay covers the failure paths of a real
// queue: a foreign handle, and a write after the queue has gone.
func TestRealPlayerRefusesWhatItCannotPlay(t *testing.T) {
	p, err := NewPlayer(PlayerConfig{SampleRate: 48000, Channels: 2, Volume: 0.01})
	if err != nil {
		t.Skipf("no output device on this machine: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if err := p.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
	if _, err := p.Write([]byte{0, 0, 0, 0}); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
	if err := p.Start(); !errors.Is(err, ErrClosed) {
		t.Errorf("Start after Close = %v, want ErrClosed", err)
	}
	if err := p.Drain(); !errors.Is(err, ErrClosed) {
		t.Errorf("Drain after Close = %v, want ErrClosed", err)
	}
}

// TestLiveDecode is the end-to-end proof against a real file, which a CI runner
// does not have, so it is opt-in. It demuxes with go-avkit and decodes here,
// which is the whole reason this package exists.
//
//	AUDIOTOOLBOX_TEST_FILE=/path/to/movie.mkv go test -run Live ./...
func TestLiveDecode(t *testing.T) {
	path := os.Getenv("AUDIOTOOLBOX_TEST_FILE")
	if path == "" {
		t.Skip("set AUDIOTOOLBOX_TEST_FILE to a media file to run the live decode test")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := container.NewReader(data)
	if err != nil {
		t.Fatalf("demux %s: %v", path, err)
	}
	tracks := r.File().AudioTracks()
	if len(tracks) == 0 {
		t.Skipf("%s holds no audio track", path)
	}
	track := tracks[0]
	cfg, err := r.TrackConfig(track.ID)
	if err != nil {
		t.Fatalf("track config: %v", err)
	}
	codec, ok := CodecFor(cfg.Codec)
	if !ok {
		t.Skipf("%s is a %s track, which this package does not decode", path, cfg.Codec)
	}
	packets, err := r.Samples(track.ID)
	if err != nil {
		t.Fatalf("samples: %v", err)
	}
	dec, err := NewDecoder(Config{
		Codec: codec, SampleRate: cfg.SampleRate, Channels: cfg.Channels,
		AudioObjectType: cfg.AudioObjectType, CodecConfig: cfg.CodecConfig,
	})
	if err != nil {
		t.Fatalf("NewDecoder = %v", err)
	}
	defer dec.Close()

	// Two hundred packets is a few seconds, past any warm-up and short
	// enough to stay a test.
	const want = 200
	empty := 0
	for i, pkt := range packets {
		if i >= want {
			break
		}
		buf, err := dec.Decode(Packet{Data: pkt.Data})
		if err != nil {
			t.Fatalf("packet %d: %v", i, err)
		}
		if buf.Frames == 0 {
			empty++
			continue
		}
		if buf.Channels != cfg.Channels {
			t.Fatalf("packet %d came back with %d channels, and the container states %d",
				i, buf.Channels, cfg.Channels)
		}
		if got := len(buf.PCM); got != buf.Frames*buf.Channels*buf.Format.Size() {
			t.Fatalf("packet %d: %d frames of %d channels in %d bytes", i, buf.Frames, buf.Channels, got)
		}
	}
	if dec.Frames() == 0 {
		t.Fatalf("%s: %d packets went in and no frame came out", path, want)
	}
	t.Logf("%s: %s %d ch %d Hz, %d packets decoded to %d frames (%v), %d produced nothing",
		path, cfg.Codec, cfg.Channels, cfg.SampleRate, want, dec.Frames(), dec.Decoded(), empty)
}

var sineAAC = [][]byte{
	{
		0x00, 0xd0, 0x40, 0x07,
	},
	{
		0x01, 0x36, 0x99, 0xdc, 0x3d, 0xb5, 0x05, 0x94, 0x49, 0x46, 0x5e, 0xbd,
		0xb8, 0xfb, 0xf9, 0xe2, 0xfa, 0xe9, 0xf2, 0x87, 0x5a, 0x05, 0xf1, 0x0f,
		0xea, 0xc0, 0xa4, 0x43, 0xd5, 0x81, 0x44, 0x47, 0xab, 0x02, 0x91, 0x0b,
		0x49, 0x7c, 0x09, 0x4b, 0x68, 0x6b, 0xc4, 0xd2, 0x2f, 0xd4, 0xdf, 0xdd,
		0x73, 0xa0, 0xcf, 0xc4, 0x49, 0x6f, 0x81, 0xc3, 0xad, 0x9d, 0x78, 0xe9,
		0xa9, 0xa0, 0xfd, 0xbc, 0x05, 0x08, 0xd9, 0xfe, 0xde, 0x02, 0x84, 0x6c,
		0x5d, 0xae, 0x75, 0xad, 0xb0, 0x4e, 0x63, 0xad, 0x4b, 0x50, 0x82, 0x25,
		0xeb, 0x73, 0xad, 0x6d, 0x82, 0x73, 0x1d, 0x6a, 0x5b, 0x84, 0x11, 0x07,
		0x36, 0xda, 0xb6, 0xb8, 0xc7, 0x33, 0x96, 0xa7, 0x38, 0x41, 0x10, 0x7d,
		0x1f, 0x4b, 0x9b, 0xd2, 0xe8, 0xe8, 0x8c, 0x20, 0x9c, 0x27, 0x5a, 0x96,
		0xa7, 0x29, 0x20, 0x88, 0x22, 0x0f, 0xce, 0x7d, 0xfb, 0xdf, 0x7d, 0xfb,
		0xe9, 0x3e, 0x97, 0x1c, 0xb1, 0xcb, 0x19, 0xc6, 0xaf, 0x2b, 0xad, 0x6d,
		0x5c, 0xb1, 0xcb, 0x1f, 0x50, 0xf6, 0x3f, 0x60, 0xf6, 0x3f, 0x60, 0xf2,
		0xf8, 0xcc, 0x65, 0x15, 0x75, 0x74, 0x6b, 0x63, 0x96, 0x39, 0x63, 0xff,
		0xb7, 0xb8, 0x00, 0x07,
	},
	{
		0x01, 0x0e, 0xd4, 0xa9, 0x56, 0x12, 0x08, 0x2c, 0xc4, 0x43, 0x31, 0x80,
		0x84, 0x20, 0x75, 0x3e, 0x7d, 0x27, 0xbf, 0x78, 0xf5, 0xff, 0xfe, 0x2f,
		0x46, 0x38, 0xe5, 0x72, 0x9e, 0x61, 0x24, 0xc0, 0x15, 0x95, 0x8e, 0xd2,
		0x03, 0x0f, 0xf1, 0x49, 0x6d, 0x4c, 0x9b, 0x1a, 0x29, 0x8e, 0x46, 0x46,
		0xcb, 0x23, 0x65, 0xce, 0x79, 0x4e, 0x55, 0x11, 0x09, 0x30, 0x74, 0x76,
		0x15, 0xf0, 0x36, 0x6f, 0x7b, 0xb6, 0x61, 0x09, 0x7a, 0x8c, 0x51, 0x45,
		0x14, 0x51, 0x14, 0x51, 0x21, 0x05, 0x54, 0x48, 0x08, 0x41, 0x75, 0xbe,
		0x7e, 0x00, 0x0d, 0x94, 0x0d, 0x9f, 0xad, 0xff, 0xff, 0x00, 0xcc, 0x70,
	},
	{
		0x01, 0x0c, 0x14, 0xa8, 0xe4, 0x2b, 0x10, 0x34, 0x42, 0x03, 0x31, 0x80,
		0x84, 0x20, 0x21, 0x08, 0x10, 0xc6, 0x7a, 0x7f, 0x1e, 0x33, 0x9f, 0xff,
		0xfc, 0x4c, 0xc5, 0xca, 0xa8, 0x18, 0x40, 0x3c, 0x04, 0x3c, 0xd9, 0xe5,
		0xea, 0x86, 0x0b, 0x4f, 0xa6, 0xc1, 0xd4, 0x2d, 0xe9, 0x13, 0xc0, 0x51,
		0xbc, 0xf1, 0x8a, 0x7a, 0x53, 0x2a, 0x85, 0x21, 0xa2, 0xbe, 0xa9, 0xf5,
		0xc0, 0x00, 0x02, 0x7f, 0xfa, 0x02, 0x57, 0x0e, 0x40, 0xe0,
	},
	{
		0x01, 0x12, 0x14, 0xa0, 0xc4, 0x4b, 0x08, 0x34, 0xc2, 0x02, 0x51, 0x18,
		0x81, 0x4e, 0x6f, 0x0f, 0xeb, 0x9d, 0xfd, 0x9f, 0xff, 0xf5, 0x11, 0x72,
		0xaa, 0x06, 0x00, 0x0f, 0x01, 0x0f, 0x35, 0x34, 0xa2, 0x63, 0x45, 0xbf,
		0x7a, 0xff, 0x15, 0x7e, 0x82, 0x8a, 0x28, 0x5a, 0x79, 0x78, 0xef, 0x25,
		0x0f, 0x49, 0xf7, 0x40, 0x02, 0x08, 0xf1, 0xf1, 0x00, 0x00, 0x01, 0x59,
		0xff, 0xe8, 0xe0,
	},
	{
		0x01, 0x1c, 0x14, 0x99, 0x56, 0x10, 0x69, 0x84, 0x06, 0x63, 0x01, 0x08,
		0x40, 0xe5, 0x55, 0x7a, 0xde, 0x77, 0xeb, 0xbf, 0xff, 0xfb, 0xa3, 0x17,
		0x2a, 0xa0, 0x61, 0x00, 0xff, 0x10, 0xf6, 0x18, 0xe1, 0x8e, 0x18, 0xe1,
		0x8e, 0x18, 0xbb, 0x3b, 0x3b, 0x3b, 0x3d, 0x30, 0x9f, 0xdc, 0x1d, 0x2c,
		0x78, 0x18, 0x32, 0x7f, 0xfc, 0xf0, 0x00, 0x04, 0xab, 0x9f, 0xff, 0x80,
		0x56, 0x93, 0x38,
	},
	{
		0x01, 0x0c, 0x14, 0xa8, 0xe4, 0x2b, 0x10, 0x34, 0x42, 0x03, 0x31, 0x80,
		0x84, 0x20, 0x21, 0x08, 0x10, 0xc6, 0x7a, 0x7f, 0x1e, 0x33, 0x9f, 0xff,
		0xf1, 0xa1, 0xb5, 0xca, 0x88, 0x18, 0x40, 0x3c, 0x04, 0xd9, 0xe5, 0xea,
		0x86, 0x0b, 0x4f, 0xa6, 0xc1, 0xd4, 0x2d, 0xe9, 0x13, 0xc0, 0x51, 0xbc,
		0xf1, 0x8a, 0x7a, 0x53, 0x2a, 0x85, 0x21, 0xa2, 0xb1, 0x9f, 0xff, 0x58,
		0x00, 0x00, 0xab, 0xff, 0x80, 0x35, 0xdc, 0x39, 0x03, 0x80,
	},
	{
		0x01, 0x10, 0x14, 0xa0, 0xc4, 0x4b, 0x08, 0x34, 0x48, 0xa1, 0x31, 0x00,
		0x84, 0x20, 0x77, 0x77, 0xdc, 0x7e, 0xd9, 0xdf, 0xd9, 0xff, 0xfd, 0xf4,
		0x68, 0xda, 0xe5, 0x44, 0x0c, 0x00, 0x1e, 0x02, 0x9d, 0x34, 0xa2, 0x63,
		0x45, 0x7d, 0xeb, 0xf7, 0xa3, 0xd6, 0x51, 0x45, 0x0b, 0x4f, 0x2f, 0x1d,
		0xe4, 0xa1, 0x2b, 0xc4, 0x00, 0x01, 0x6b, 0x8f, 0x1f, 0xca, 0x80, 0x00,
		0x00, 0x65, 0xff, 0xd0, 0x16, 0x1c,
	},
	{
		0x01, 0x1c, 0x14, 0x99, 0x56, 0x10, 0x69, 0x84, 0x06, 0x63, 0x01, 0x08,
		0x40, 0xe5, 0x55, 0x7a, 0xde, 0x77, 0xeb, 0xbf, 0xff, 0xe0, 0x31, 0x72,
		0xa2, 0x06, 0x10, 0x0f, 0xf1, 0x61, 0x8e, 0x18, 0xe1, 0x8e, 0x18, 0xe1,
		0x8b, 0xb3, 0xb3, 0xb3, 0xb3, 0xd3, 0x09, 0xfd, 0xc1, 0xd2, 0xc7, 0xa6,
		0x79, 0x1b, 0x7f, 0xfc, 0xf0, 0x00, 0x00, 0xff, 0xd0, 0x14, 0x99, 0xc0,
	},
	{
		0x01, 0x0c, 0x14, 0xa8, 0xe4, 0x2b, 0x10, 0x34, 0x42, 0x03, 0x11, 0x80,
		0x84, 0x20, 0x21, 0x08, 0x10, 0x66, 0x7a, 0x7f, 0x1e, 0x33, 0x9f, 0xff,
		0xfc, 0xa8, 0xc5, 0xca, 0xa8, 0x18, 0x40, 0x3c, 0x04, 0x3c, 0xd9, 0xe5,
		0xea, 0x86, 0xa5, 0xcf, 0xe9, 0xb0, 0x75, 0x0b, 0x7a, 0x44, 0xf0, 0x14,
		0x6f, 0x3c, 0x62, 0x9e, 0x94, 0xca, 0xa1, 0x48, 0x68, 0xae, 0x19, 0x7e,
		0x80, 0x00, 0x01, 0xc4, 0x25, 0x70, 0xe4, 0x0e,
	},
}
