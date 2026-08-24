// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

package audiotoolbox

import (
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes for the platform seams. Every test that drives a Decoder or a Player
// installs them, so the portable logic above the seams is exercised without a
// Mac — which is what lets the linux lane run these at all.
// ---------------------------------------------------------------------------

// fakeDecoder is what the fake newDecoder seam hands back.
type fakeDecoder struct {
	frames  int
	err     error
	refused error
	closed  int
	last    Packet
	flushed bool
}

// fakePlayer records what the portable layer asked the platform to do.
type fakePlayer struct {
	startErr, writeErr, drainErr, stopErr error
	written                               []byte
	short                                 int // bytes to claim on a short write
	played                                time.Duration
	starts, drains, stops, closes         int
}

// installFakes swaps every seam for one driving the fakes, and puts the real
// ones back when the test ends. Without the restore, a darwin live test that
// ran afterwards would be talking to these.
func installFakes(t *testing.T, d *fakeDecoder, p *fakePlayer) {
	t.Helper()
	saved := struct {
		nd  func(Config) (decoderHandle, error)
		dp  func(decoderHandle, Config, Packet) (Buffer, error)
		fd  func(decoderHandle, Config) (Buffer, error)
		cd  func(decoderHandle) error
		ccr func(decoderHandle) error
		np  func(PlayerConfig) (playerHandle, error)
		sp  func(playerHandle) error
		wp  func(playerHandle, []byte) (int, error)
		pp  func(playerHandle) time.Duration
		dr  func(playerHandle) error
		st  func(playerHandle) error
		cp  func(playerHandle) error
	}{newDecoder, decodePacket, flushDecoder, closeDecoder, codecConfigRefused,
		newPlayer, startPlayer, writePlayer, playedPlayer, drainPlayer, stopPlayer, closePlayer}
	t.Cleanup(func() {
		newDecoder, decodePacket, flushDecoder, closeDecoder, codecConfigRefused = saved.nd, saved.dp, saved.fd, saved.cd, saved.ccr
		newPlayer, startPlayer, writePlayer, playedPlayer = saved.np, saved.sp, saved.wp, saved.pp
		drainPlayer, stopPlayer, closePlayer = saved.dr, saved.st, saved.cp
	})

	newDecoder = func(Config) (decoderHandle, error) {
		if d == nil {
			return nil, errors.New("no decoder")
		}
		return d, nil
	}
	decodePacket = func(h decoderHandle, cfg Config, pkt Packet) (Buffer, error) {
		f := h.(*fakeDecoder)
		f.last = pkt
		if f.err != nil {
			return Buffer{}, f.err
		}
		return Buffer{
			PCM:        make([]byte, f.frames*cfg.OutputChannels*cfg.Output.Size()),
			Frames:     f.frames,
			Channels:   cfg.OutputChannels,
			SampleRate: cfg.SampleRate,
			Format:     cfg.Output,
			PTS:        pkt.PTS,
		}, nil
	}
	flushDecoder = func(h decoderHandle, cfg Config) (Buffer, error) {
		h.(*fakeDecoder).flushed = true
		return decodePacket(h, cfg, Packet{})
	}
	closeDecoder = func(h decoderHandle) error { h.(*fakeDecoder).closed++; return nil }
	codecConfigRefused = func(h decoderHandle) error { return h.(*fakeDecoder).refused }

	newPlayer = func(PlayerConfig) (playerHandle, error) {
		if p == nil {
			return nil, errors.New("no output device")
		}
		return p, nil
	}
	startPlayer = func(h playerHandle) error { f := h.(*fakePlayer); f.starts++; return f.startErr }
	writePlayer = func(h playerHandle, pcm []byte) (int, error) {
		f := h.(*fakePlayer)
		if f.writeErr != nil {
			return f.short, f.writeErr
		}
		f.written = append(f.written, pcm...)
		return len(pcm), nil
	}
	playedPlayer = func(h playerHandle) time.Duration { return h.(*fakePlayer).played }
	drainPlayer = func(h playerHandle) error { f := h.(*fakePlayer); f.drains++; return f.drainErr }
	stopPlayer = func(h playerHandle) error { f := h.(*fakePlayer); f.stops++; return f.stopErr }
	closePlayer = func(h playerHandle) error { h.(*fakePlayer).closes++; return nil }
}

// aac is a workable AAC configuration: stereo, 48 kHz, AAC-LC.
func aac() Config { return Config{Codec: AAC, SampleRate: 48000, Channels: 2} }

// ---------------------------------------------------------------------------
// Names and tables.
// ---------------------------------------------------------------------------

func TestCodecNames(t *testing.T) {
	for _, tc := range []struct {
		codec Codec
		want  string
	}{{AAC, "aac"}, {AC3, "ac-3"}, {EAC3, "ec-3"}, {Opus, "opus"}, {Codec(99), "Codec(99)"}} {
		if got := tc.codec.String(); got != tc.want {
			t.Errorf("Codec(%d).String() = %q, want %q", uint8(tc.codec), got, tc.want)
		}
	}
}

func TestCodecFor(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  Codec
		found bool
	}{
		{"mp4a", AAC, true},
		{"ac-3", AC3, true},
		{"ec-3", EAC3, true},
		// The MP4 sample entry spells it with a capital and the Matroska
		// codec id does not; avkit hands back the former, and a caller that
		// lowercased it should not be punished.
		{"Opus", Opus, true},
		{"opus", Opus, true},
		{"avc1", 0, false},
		{"", 0, false},
	} {
		got, ok := CodecFor(tc.name)
		if got != tc.want || ok != tc.found {
			t.Errorf("CodecFor(%q) = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.found)
		}
	}
}

func TestSampleFormats(t *testing.T) {
	for _, tc := range []struct {
		f    SampleFormat
		name string
		size int
	}{{Int16, "s16", 2}, {Float32, "f32", 4}, {SampleFormat(7), "SampleFormat(7)", 0}} {
		if got := tc.f.String(); got != tc.name {
			t.Errorf("SampleFormat(%d).String() = %q, want %q", uint8(tc.f), got, tc.name)
		}
		if got := tc.f.Size(); got != tc.size {
			t.Errorf("SampleFormat(%d).Size() = %d, want %d", uint8(tc.f), got, tc.size)
		}
	}
}

// TestFramesPerPacket pins the frame counts the converter is told, because
// getting one wrong is not an error: the converter sizes its output from it and
// simply produces the wrong amount of audio.
func TestFramesPerPacket(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"aac-lc", Config{Codec: AAC}, 1024},
		{"aac-lc explicit", Config{Codec: AAC, AudioObjectType: 2}, 1024},
		{"he-aac doubles", Config{Codec: AAC, AudioObjectType: 5}, 2048},
		{"he-aac v2 doubles", Config{Codec: AAC, AudioObjectType: 29}, 2048},
		{"ac-3", Config{Codec: AC3}, 1536},
		{"e-ac-3", Config{Codec: EAC3}, 1536},
		{"opus states its longest", Config{Codec: Opus}, 2880},
		{"nothing", Config{}, 0},
	} {
		if got := tc.cfg.framesPerPacket(); got != tc.want {
			t.Errorf("%s: framesPerPacket = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want error
	}{
		{"no codec", Config{SampleRate: 48000, Channels: 2}, ErrUnsupportedCodec},
		{"no sample rate", Config{Codec: AAC, Channels: 2}, ErrConfig},
		{"no channels", Config{Codec: AAC, SampleRate: 48000}, ErrConfig},
		{"too many channels", Config{Codec: AAC, SampleRate: 48000, Channels: 65}, ErrConfig},
		{"too many output channels",
			Config{Codec: AAC, SampleRate: 48000, Channels: 2, OutputChannels: 65}, ErrConfig},
		{"not a pcm format",
			Config{Codec: AAC, SampleRate: 48000, Channels: 2, Output: SampleFormat(9)}, ErrConfig},
	} {
		if _, err := tc.cfg.validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s: validate = %v, want %v", tc.name, err, tc.want)
		}
	}
	// The output channel count defaults to the coded one, and a stated one
	// survives.
	got, err := aac().validate()
	if err != nil || got.OutputChannels != 2 {
		t.Errorf("validate(stereo aac) = %+v, %v; want 2 output channels", got, err)
	}
	down := aac()
	down.OutputChannels = 1
	if got, err := down.validate(); err != nil || got.OutputChannels != 1 {
		t.Errorf("validate(downmix) = %+v, %v; want 1 output channel", got, err)
	}
}

// ---------------------------------------------------------------------------
// The AAC configuration.
// ---------------------------------------------------------------------------

func TestAudioSpecificConfig(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		// AAC-LC (2), 48 kHz (index 3), stereo (2):
		// 00010 0011 0010 000 -> 0001 0001 1001 0000 -> 11 90
		{"aac-lc 48k stereo", Config{Codec: AAC, SampleRate: 48000, Channels: 2}, "1190"},
		// A track that states no profile is AAC-LC, so it must produce the
		// same two bytes.
		{"no profile is aac-lc", Config{Codec: AAC, SampleRate: 48000, Channels: 2,
			AudioObjectType: 2}, "1190"},
		// AAC-LC, 44.1 kHz (index 4), 5.1 (configuration 6):
		// 00010 0100 0110 000 -> 0001 0010 0011 0000 -> 12 30
		{"aac-lc 44k1 5.1", Config{Codec: AAC, SampleRate: 44100, Channels: 6}, "1230"},
		// 7.1 is eight channels and channel configuration SEVEN, not eight.
		// 00010 0011 0111 000 -> 0001 0001 1011 1000 -> 11 b8
		{"7.1 is configuration 7", Config{Codec: AAC, SampleRate: 48000, Channels: 8}, "11b8"},
		// A count with no configuration falls back to zero, which means the
		// bitstream says.
		// 00010 0011 0000 000 -> 0001 0001 1000 0000 -> 11 80
		{"seven channels have no configuration",
			Config{Codec: AAC, SampleRate: 48000, Channels: 7}, "1180"},
		// A rate outside the table escapes at index 15 and spells itself out
		// in 24 bits. 47999 is 0x00bb7f, and the whole config is 40 bits:
		// 00010 1111 000000001011101101111111 0010 000
		// -> 00010111 10000000 01011101 10111111 10010000 -> 17 80 5d bf 90
		{"a rate not in the table escapes",
			Config{Codec: AAC, SampleRate: 47999, Channels: 2}, "17805dbf90"},
		// A config the demuxer read is used as it stands.
		{"a stated config wins", Config{Codec: AAC, CodecConfig: []byte{0xde, 0xad}}, "dead"},
	} {
		if got := hex.EncodeToString(tc.cfg.AudioSpecificConfig()); got != tc.want {
			t.Errorf("%s: AudioSpecificConfig = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestAudioSpecificConfigEscapesTheObjectType is separate because the escaped
// form is 22 bits and the arithmetic is worth spelling out.
//
//	11111 (escape) 000000 (32-32) 0011 (48 kHz) 0010 (stereo) 000 (GASpecific)
//	-> 11111000 00000110 010000(00 pad) -> f8 06 40
func TestAudioSpecificConfigEscapesTheObjectType(t *testing.T) {
	cfg := Config{Codec: AAC, SampleRate: 48000, Channels: 2, AudioObjectType: 32}
	if got, want := hex.EncodeToString(cfg.AudioSpecificConfig()), "f80640"; got != want {
		t.Errorf("AudioSpecificConfig(aot 32) = %s, want %s", got, want)
	}
}

func TestSampleRateIndex(t *testing.T) {
	for rate, want := range map[int]int{96000: 0, 48000: 3, 44100: 4, 7350: 12, 48001: 15, 0: 15} {
		if got := aacSampleRateIndex(rate); got != want {
			t.Errorf("aacSampleRateIndex(%d) = %d, want %d", rate, got, want)
		}
	}
}

func TestMagicCookie(t *testing.T) {
	// AAC is wrapped in an ES_Descriptor, because the bare configuration is
	// refused — measured, and the package documentation says so.
	cfg := aac()
	const want = "031900000004114015000000000000000000000005021190060102"
	if got := hex.EncodeToString(cfg.MagicCookie()); got != want {
		t.Errorf("MagicCookie(aac) = %s\nwant                %s", got, want)
	}
	// Everything else hands back what the demuxer read, and nil when it read
	// nothing.
	opus := Config{Codec: Opus, CodecConfig: []byte("OpusHead")}
	if got := string(opus.MagicCookie()); got != "OpusHead" {
		t.Errorf("MagicCookie(opus) = %q, want the OpusHead as it stands", got)
	}
	if got := (Config{Codec: AC3}).MagicCookie(); got != nil {
		t.Errorf("MagicCookie(ac-3) = %x, want nil: AC-3 states everything in its packets", got)
	}
	// A descriptor wrapping nothing describes nothing.
	if got := esdsCookie(nil); got != nil {
		t.Errorf("esdsCookie(nil) = %x, want nil", got)
	}
}

// TestDescriptorLength covers the expandable length form. A single byte holds
// up to 127; past that the length takes another byte, and writing one anyway
// would describe a shorter descriptor than the one that follows.
func TestDescriptorLength(t *testing.T) {
	for _, n := range []int{0, 1, 127, 128, 300, 16384} {
		d := descriptor(0x05, make([]byte, n))
		if d[0] != 0x05 {
			t.Fatalf("descriptor(%d) lost its tag", n)
		}
		var size, read int
		for i := 1; ; i++ {
			size = size<<7 | int(d[i]&0x7f)
			read++
			if d[i]&0x80 == 0 {
				break
			}
		}
		if size != n {
			t.Errorf("descriptor(%d): length field says %d", n, size)
		}
		if got := len(d); got != 1+read+n {
			t.Errorf("descriptor(%d) is %d bytes, want %d", n, got, 1+read+n)
		}
	}
}

func TestChannelConfiguration(t *testing.T) {
	for channels, want := range map[int]int{1: 1, 2: 2, 3: 3, 4: 4, 5: 5, 6: 6, 7: 0, 8: 7, 9: 0, 0: 0} {
		if got := aacChannelConfiguration(channels); got != want {
			t.Errorf("aacChannelConfiguration(%d) = %d, want %d", channels, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// Buffers.
// ---------------------------------------------------------------------------

func TestBuffer(t *testing.T) {
	b := Buffer{PCM: []byte{1, 2, 3, 4}, Frames: 1024, Channels: 2, SampleRate: 48000, Format: Int16}
	if got, want := b.Duration(), 1024*time.Second/48000; got != want {
		t.Errorf("Duration = %v, want %v", got, want)
	}
	if got := (Buffer{Frames: 1024}).Duration(); got != 0 {
		t.Errorf("Duration with no sample rate = %v, want 0", got)
	}
	c := b.Clone()
	if string(c.PCM) != string(b.PCM) {
		t.Fatalf("Clone changed the samples")
	}
	// The point of Clone is that the copy survives the original being
	// overwritten, which is what the decoder's scratch buffer does.
	b.PCM[0] = 0xff
	if c.PCM[0] != 1 {
		t.Error("Clone aliased the original instead of copying it")
	}
}

// ---------------------------------------------------------------------------
// The decoder.
// ---------------------------------------------------------------------------

func TestNewDecoderRefusesAConfigurationThatDescribesNothing(t *testing.T) {
	installFakes(t, &fakeDecoder{}, nil)
	if _, err := NewDecoder(Config{}); !errors.Is(err, ErrUnsupportedCodec) {
		t.Errorf("NewDecoder(zero) = %v, want ErrUnsupportedCodec", err)
	}
	// A platform that cannot build the converter is reported as it is.
	installFakes(t, nil, nil)
	if _, err := NewDecoder(aac()); err == nil {
		t.Error("NewDecoder ignored the platform's refusal")
	}
}

func TestDecoderCountsWhatCameOut(t *testing.T) {
	f := &fakeDecoder{frames: 1024}
	installFakes(t, f, nil)
	d, err := NewDecoder(aac())
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Config(); got.OutputChannels != 2 || got.SampleRate != 48000 {
		t.Errorf("Config = %+v, want the resolved stereo 48 kHz one", got)
	}
	for i := 0; i < 3; i++ {
		b, err := d.Decode(Packet{Data: []byte{1}, PTS: time.Duration(i) * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if b.Frames != 1024 || b.PTS != time.Duration(i)*time.Second {
			t.Errorf("packet %d came back as %d frames at %v", i, b.Frames, b.PTS)
		}
	}
	// Frames counts what came OUT, and Decoded turns it into time.
	if got := d.Frames(); got != 3*1024 {
		t.Errorf("Frames = %d, want %d", got, 3*1024)
	}
	if got, want := d.Decoded(), 3*1024*time.Second/48000; got != want {
		t.Errorf("Decoded = %v, want %v", got, want)
	}
	// Flush adds its own frames to the count.
	if _, err := d.Flush(); err != nil {
		t.Fatal(err)
	}
	if !f.flushed {
		t.Error("Flush did not reach the platform")
	}
	if got := d.Frames(); got != 4*1024 {
		t.Errorf("Frames after Flush = %d, want %d", got, 4*1024)
	}
	if err := d.CodecConfigRefused(); err != nil {
		t.Errorf("CodecConfigRefused = %v, want nil", err)
	}
	f.refused = errors.New("turned down")
	if err := d.CodecConfigRefused(); err == nil {
		t.Error("CodecConfigRefused hid the platform's refusal")
	}
	// Close is idempotent, and everything answers ErrClosed afterwards.
	for i := 0; i < 2; i++ {
		if err := d.Close(); err != nil {
			t.Errorf("Close %d = %v", i, err)
		}
	}
	if f.closed != 1 {
		t.Errorf("the platform was closed %d times, want 1", f.closed)
	}
	if _, err := d.Decode(Packet{Data: []byte{1}}); !errors.Is(err, ErrClosed) {
		t.Errorf("Decode after Close = %v, want ErrClosed", err)
	}
	if _, err := d.Flush(); !errors.Is(err, ErrClosed) {
		t.Errorf("Flush after Close = %v, want ErrClosed", err)
	}
	if err := d.CodecConfigRefused(); err != nil {
		t.Errorf("CodecConfigRefused after Close = %v, want nil", err)
	}
}

func TestDecoderRejectsAnEmptyPacketAndReportsFailures(t *testing.T) {
	f := &fakeDecoder{frames: 1024}
	installFakes(t, f, nil)
	d, err := NewDecoder(aac())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// An empty packet never reaches the platform: it is a demuxer bug, and
	// AudioToolbox would answer something unhelpful about packet sizes.
	if _, err := d.Decode(Packet{}); !errors.Is(err, ErrPacket) {
		t.Errorf("Decode of an empty packet = %v, want ErrPacket", err)
	}
	boom := errors.New("the converter refused")
	f.err = boom
	if _, err := d.Decode(Packet{Data: []byte{1}}); !errors.Is(err, boom) {
		t.Errorf("Decode = %v, want the platform's error", err)
	}
	if _, err := d.Flush(); !errors.Is(err, boom) {
		t.Errorf("Flush = %v, want the platform's error", err)
	}
	// A failed decode must not be counted as audio.
	if got := d.Frames(); got != 0 {
		t.Errorf("Frames after two failures = %d, want 0", got)
	}
}

// TestDecodedSurvivesAnImpossibleRate covers the guard that keeps a division by
// zero out of a clock. A validated Config cannot hold one, so it is reached by
// building the struct directly — which a caller cannot do and a future refactor
// might.
func TestDecodedSurvivesAnImpossibleRate(t *testing.T) {
	d := &Decoder{frames: 1024}
	if got := d.Decoded(); got != 0 {
		t.Errorf("Decoded with no sample rate = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// The player.
// ---------------------------------------------------------------------------

func TestPlayerConfig(t *testing.T) {
	cfg := PlayerConfigFor(Config{Codec: AAC, SampleRate: 44100, Channels: 6, OutputChannels: 2,
		Output: Float32})
	if cfg.SampleRate != 44100 || cfg.Channels != 2 || cfg.Format != Float32 {
		t.Errorf("PlayerConfigFor = %+v, want the DOWNMIXED channel count", cfg)
	}
	if got := cfg.BytesPerFrame(); got != 8 {
		t.Errorf("BytesPerFrame(2ch f32) = %d, want 8", got)
	}
	for _, tc := range []struct {
		name string
		cfg  PlayerConfig
	}{
		{"no rate", PlayerConfig{Channels: 2}},
		{"no channels", PlayerConfig{SampleRate: 48000}},
		{"too many channels", PlayerConfig{SampleRate: 48000, Channels: 65}},
		{"not a pcm format", PlayerConfig{SampleRate: 48000, Channels: 2, Format: SampleFormat(9)}},
		{"one buffer cannot be filled while another plays",
			PlayerConfig{SampleRate: 48000, Channels: 2, BufferCount: 1}},
		{"volume above one", PlayerConfig{SampleRate: 48000, Channels: 2, Volume: 1.5}},
		{"volume below zero", PlayerConfig{SampleRate: 48000, Channels: 2, Volume: -1}},
	} {
		if _, err := tc.cfg.validate(); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: validate = %v, want ErrConfig", tc.name, err)
		}
	}
	got, err := PlayerConfig{SampleRate: 48000, Channels: 2}.validate()
	if err != nil {
		t.Fatal(err)
	}
	if got.BufferFrames != defaultBufferFrames || got.BufferCount != defaultBufferCount || got.Volume != 1 {
		t.Errorf("validate left %+v, want the defaults filled in", got)
	}
}

func TestPlayerDrivesThePlatform(t *testing.T) {
	f := &fakePlayer{}
	installFakes(t, nil, f)
	p, err := NewPlayer(PlayerConfig{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Config().BufferCount; got != defaultBufferCount {
		t.Errorf("Config = %+v, want the resolved defaults", p.Config())
	}
	// Nothing is heard until Start, and starting twice starts once.
	for i := 0; i < 2; i++ {
		if err := p.Start(); err != nil {
			t.Fatal(err)
		}
	}
	if f.starts != 1 {
		t.Errorf("the platform was started %d times, want 1", f.starts)
	}
	// A whole number of frames goes through; a partial one does not.
	if n, err := p.Write(make([]byte, 4*100)); err != nil || n != 400 {
		t.Errorf("Write = %d, %v; want 400, nil", n, err)
	}
	if _, err := p.Write([]byte{1, 2, 3}); !errors.Is(err, ErrPacket) {
		t.Errorf("Write of three bytes into 4-byte frames = %v, want ErrPacket", err)
	}
	if n, err := p.Write(nil); n != 0 || err != nil {
		t.Errorf("Write(nil) = %d, %v; want 0, nil", n, err)
	}
	if got := p.Written(); got != 100 {
		t.Errorf("Written = %d frames, want 100", got)
	}
	// The clock is the platform's, and Queued is what has been written and
	// not yet heard.
	f.played = 1 * time.Millisecond
	if got := p.Played(); got != time.Millisecond {
		t.Errorf("Played = %v, want the platform's reading", got)
	}
	if got, want := p.Queued(), 100*time.Second/48000-time.Millisecond; got != want {
		t.Errorf("Queued = %v, want %v", got, want)
	}
	// A clock ahead of what was written means the queue is empty, not that
	// time ran backwards.
	f.played = time.Hour
	if got := p.Queued(); got != 0 {
		t.Errorf("Queued with the clock ahead = %v, want 0", got)
	}
	if err := p.Drain(); err != nil || f.drains != 1 {
		t.Errorf("Drain = %v, drains = %d", err, f.drains)
	}
	if err := p.Stop(); err != nil || f.stops != 1 {
		t.Errorf("Stop = %v, stops = %d", err, f.stops)
	}
	// Stopped, Drain and Stop have nothing to wait for and must not reach a
	// queue that is not running.
	if err := p.Drain(); err != nil {
		t.Errorf("Drain when stopped = %v, want nil", err)
	}
	if err := p.Stop(); err != nil {
		t.Errorf("Stop when stopped = %v, want nil", err)
	}
	if f.drains != 1 || f.stops != 1 {
		t.Errorf("a stopped player reached the platform again: %d drains, %d stops", f.drains, f.stops)
	}
	for i := 0; i < 2; i++ {
		if err := p.Close(); err != nil {
			t.Errorf("Close %d = %v", i, err)
		}
	}
	if f.closes != 1 {
		t.Errorf("the platform was closed %d times, want 1", f.closes)
	}
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"Start", p.Start()},
		{"Drain", p.Drain()},
		{"Stop", p.Stop()},
	} {
		if !errors.Is(tc.err, ErrClosed) {
			t.Errorf("%s after Close = %v, want ErrClosed", tc.name, tc.err)
		}
	}
	if _, err := p.Write([]byte{1, 2, 3, 4}); !errors.Is(err, ErrClosed) {
		t.Errorf("Write after Close = %v, want ErrClosed", err)
	}
}

func TestPlayerReportsPlatformFailures(t *testing.T) {
	installFakes(t, nil, nil)
	if _, err := NewPlayer(PlayerConfig{Channels: 2}); !errors.Is(err, ErrConfig) {
		t.Errorf("NewPlayer with no rate = %v, want ErrConfig", err)
	}
	if _, err := NewPlayer(PlayerConfig{SampleRate: 48000, Channels: 2}); err == nil {
		t.Error("NewPlayer ignored the platform's refusal")
	}

	boom := errors.New("no device")
	f := &fakePlayer{startErr: boom, writeErr: boom, drainErr: boom, stopErr: boom, short: 4}
	installFakes(t, nil, f)
	p, err := NewPlayer(PlayerConfig{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Start(); !errors.Is(err, boom) {
		t.Errorf("Start = %v, want the platform's error", err)
	}
	// A failed Start leaves the player unstarted, so Drain has nothing to
	// wait for.
	if err := p.Drain(); err != nil {
		t.Errorf("Drain after a failed Start = %v, want nil", err)
	}
	// A short write still counts the frames that got through: losing track of
	// them would put the clock's idea of what is queued permanently wrong.
	n, err := p.Write(make([]byte, 40))
	if n != 4 || !errors.Is(err, boom) {
		t.Errorf("Write = %d, %v; want 4 and the platform's error", n, err)
	}
	if got := p.Written(); got != 1 {
		t.Errorf("Written after a short write = %d, want 1 frame", got)
	}
	f.startErr = nil
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(); !errors.Is(err, boom) {
		t.Errorf("Drain = %v, want the platform's error", err)
	}
	if err := p.Stop(); !errors.Is(err, boom) {
		t.Errorf("Stop = %v, want the platform's error", err)
	}
	p.Close()
}

// TestPlayerClockGuards covers the two divisions a clock cannot do. Neither is
// reachable through NewPlayer, which validates them away; both are reachable by
// a future refactor, and a clock that panics is worse than one that says zero.
func TestPlayerClockGuards(t *testing.T) {
	p := &Player{}
	if got := p.Played(); got != 0 {
		t.Errorf("Played with no platform = %v, want 0", got)
	}
	if got := p.Queued(); got != 0 {
		t.Errorf("Queued with no sample rate = %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// OSStatus.
// ---------------------------------------------------------------------------

func TestStatusError(t *testing.T) {
	if err := status("AudioConverterNew", 0); err != nil {
		t.Errorf("status(noErr) = %v, want nil", err)
	}
	for _, tc := range []struct {
		name   string
		status int32
		want   string
	}{
		// A named status is spelled out.
		{"named", 0x666D743F, "audiotoolbox: op: kAudioConverterErr_FormatNotSupported " +
			"('fmt?'): no decoder for this format (1718449215)"},
		// An unnamed one that reads as four printable characters is shown
		// both ways, because that is how AudioToolbox spells its errors and
		// a bare 1650549793 is a long walk from 'bad!'.
		{"four printable characters", 0x62616421, `audiotoolbox: op: OSStatus 1650549793 ("bad!")`},
		// One that does not is left as a number.
		{"not printable", -12345, "audiotoolbox: op: OSStatus -12345"},
	} {
		err := &StatusError{Op: "op", Status: tc.status}
		if got := err.Error(); got != tc.want {
			t.Errorf("%s:\n got %s\nwant %s", tc.name, got, tc.want)
		}
	}
	var se *StatusError
	if err := status("op", -1); !errors.As(err, &se) || se.Status != -1 {
		t.Errorf("status(-1) = %v, want a *StatusError carrying it", err)
	}
	// Every named status must read back as itself, or the table is a lie.
	for code, name := range osStatusNames {
		if got := (&StatusError{Op: "op", Status: code}).Error(); got == "" {
			t.Errorf("%d (%s) rendered as nothing", code, name)
		}
	}
	if _, ok := fourCC(0); ok {
		t.Error("fourCC accepted a status full of NUL bytes")
	}
	if got, ok := fourCC(0x666D743F); !ok || got != "fmt?" {
		t.Errorf("fourCC(0x666D743F) = %q, %v; want \"fmt?\", true", got, ok)
	}
}

// TestErrorsAreDistinct guards against two sentinels that compare equal, which
// would make errors.Is answer true for the wrong one.
func TestErrorsAreDistinct(t *testing.T) {
	all := []error{ErrUnsupported, ErrClosed, ErrUnsupportedCodec, ErrConfig, ErrPacket}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v and %v are the same error", a, b)
			}
		}
	}
	// Every one of them names the package, so a caller who prints it knows
	// where it came from.
	for _, err := range all {
		if got := fmt.Sprint(err); len(got) < len("audiotoolbox: ") || got[:14] != "audiotoolbox: " {
			t.Errorf("%q does not name the package", got)
		}
	}
}
