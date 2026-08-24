// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

// Package audiotoolbox decodes compressed audio and plays PCM on macOS through
// AudioToolbox, with no cgo.
//
// It exists because of a measured hole, and it is the audio half of the one
// [github.com/go-macos/videotoolbox] fills for pictures. AVFoundation plays an
// MP4 end to end and will not demux Matroska at all, so a player that wants
// sound out of an MKV has to demux the file itself and hand the coded packets
// to a decoder directly. [github.com/go-avkit/avkit/container] does the
// demuxing — it already reports audio tracks, their codec, channel count,
// sample rate and coded packets, for MP4 and for Matroska alike. This package
// is the other half: AudioConverter to turn those packets into PCM, and
// AudioQueue to put the PCM on the system output.
//
// Everything goes through github.com/ebitengine/purego, so a consumer still
// builds with CGO_ENABLED=0.
//
// # Two halves, used together or apart
//
// A [Decoder] turns one coded [Packet] into a [Buffer] of interleaved PCM. A
// [Player] takes interleaved PCM and plays it. Neither knows about the other:
// a caller that only wants samples never opens an output device, and a caller
// with PCM of its own never builds a decoder.
//
//	dec, _ := audiotoolbox.NewDecoder(cfg)
//	p, _ := audiotoolbox.NewPlayer(audiotoolbox.PlayerConfigFor(cfg))
//	p.Start()
//	for _, pkt := range packets {
//	        buf, _ := dec.Decode(audiotoolbox.Packet{Data: pkt.Data})
//	        p.Write(buf.PCM)
//	}
//	p.Drain()
//
// # The format is stated, never sniffed
//
// A [Config] says what the packets hold: which codec, at what sample rate, with
// how many channels. It is taken as stated, the way
// [github.com/go-macos/videotoolbox] takes its bitstream form as stated, and for
// the same reason: a demuxer already knows, and a decoder that guesses is a
// decoder that is occasionally, silently wrong. container.TrackConfig hands
// back every field this needs.
//
// AAC in an MP4 or a Matroska file carries no in-band configuration — no ADTS
// header, nothing before the first packet — so the decoder is set up from an
// AudioSpecificConfig. When the demuxer states one, in Config.CodecConfig, it
// is used as it stands; when it does not, one is built from the sample rate,
// the channel count and the AAC profile the demuxer did state. See
// [Config.MagicCookie].
//
// # Buffers alias, and the reason is one memcpy
//
// [Decoder.Decode] returns PCM that lives in a scratch buffer the decoder owns,
// and the next call overwrites it. That is deliberate: a player copies the
// samples into its own queue anyway, and allocating a fresh slice per packet —
// forty-seven of them a second at 48 kHz — buys nothing. [Buffer.Clone] copies
// for a caller that wants to keep one.
//
// # The clock
//
// [Player.Played] is how many seconds of audio have left the device. It is not
// a count of what was written: a player that synchronises video against bytes
// handed to the output is a player that drifts, because the output consumes
// them at its own rate. Audio is the master clock of every serious player — the
// ear hears a drift the eye does not — so this reads the AudioQueue's own
// timeline rather than guessing at it.
//
// [Player.Drain] is what waits for the last of it. AudioQueueStop does not:
// asked to stop gently it returns at once and finishes in its own time, so a
// drain that trusted it would cut the tail off. This one waits for the queue to
// hand every buffer back, which is when the device has finished with them.
package audiotoolbox

import (
	"errors"
	"fmt"
	"time"
)

// Errors reported by the package. They are stable and may be tested with
// errors.Is.
var (
	// ErrUnsupported is returned by every entry point on non-darwin platforms.
	ErrUnsupported = errors.New("audiotoolbox: unsupported on this platform (darwin only)")
	// ErrClosed is returned when a Decoder or a Player is used after Close.
	ErrClosed = errors.New("audiotoolbox: closed")
	// ErrUnsupportedCodec is returned for a codec this package does not
	// describe to AudioToolbox.
	ErrUnsupportedCodec = errors.New("audiotoolbox: unsupported codec")
	// ErrConfig is returned for a configuration that does not describe a
	// stream: no sample rate, no channels, an impossible sample format.
	ErrConfig = errors.New("audiotoolbox: invalid configuration")
	// ErrPacket is returned when a packet cannot be submitted as given.
	ErrPacket = errors.New("audiotoolbox: invalid packet")
)

// ---------------------------------------------------------------------------
// Codecs.
// ---------------------------------------------------------------------------

// Codec names the compressed form a decoder is set up for.
type Codec uint8

// The codecs this package describes to AudioToolbox. Every one of them is a
// codec [github.com/go-avkit/avkit/container] reports for a demuxed audio
// track, which is the whole point: what the demuxer produces is what this
// takes.
const (
	// AAC is MPEG-4 Advanced Audio Coding, the "mp4a" of an MP4 sample entry
	// and the A_AAC of a Matroska track. It is what the overwhelming
	// majority of MP4 files carry.
	AAC Codec = iota + 1
	// AC3 is Dolby Digital, the "ac-3" of an MP4 sample entry and the A_AC3
	// of a Matroska track. It is what a great many films in MKV carry, in
	// 5.1.
	AC3
	// EAC3 is Dolby Digital Plus, "ec-3".
	EAC3
	// Opus is the IETF codec, "Opus" in an MP4 sample entry and A_OPUS in
	// Matroska. macOS decodes it, but only on a recent enough system; a
	// session that cannot be built says so rather than falling back.
	Opus
)

// String names the codec.
func (c Codec) String() string {
	switch c {
	case AAC:
		return "aac"
	case AC3:
		return "ac-3"
	case EAC3:
		return "ec-3"
	case Opus:
		return "opus"
	default:
		return fmt.Sprintf("Codec(%d)", uint8(c))
	}
}

// CodecFor maps the codec names container.TrackConfig reports onto a [Codec].
// It reports false for anything else, which is how a caller finds out that a
// track it demuxed is not one this package decodes before it builds a decoder.
//
// The names are the ones avkit normalises to: "mp4a" for AAC whether it came
// from an MP4 sample entry or from A_AAC/MPEG4/LC, "ac-3" for AC-3 whether from
// "ac-3" or A_AC3/BSID9, "ec-3", and "Opus" — with the capital, which is what
// the MP4 sample entry spells.
func CodecFor(name string) (Codec, bool) {
	switch name {
	case "mp4a":
		return AAC, true
	case "ac-3":
		return AC3, true
	case "ec-3":
		return EAC3, true
	case "Opus", "opus":
		return Opus, true
	}
	return 0, false
}

// ---------------------------------------------------------------------------
// Sample formats.
// ---------------------------------------------------------------------------

// SampleFormat is the layout of one PCM sample.
type SampleFormat uint8

const (
	// Int16 is signed 16-bit, little-endian, interleaved. It is the zero
	// value because it is what an output device wants, what a WAV file
	// holds, and what a reader can look at with any tool it already has.
	Int16 SampleFormat = iota
	// Float32 is 32-bit float, native-endian, interleaved, nominally in
	// [-1, 1]. It is what a mixer or a resampler would rather have.
	Float32
)

// String names the sample format.
func (f SampleFormat) String() string {
	switch f {
	case Int16:
		return "s16"
	case Float32:
		return "f32"
	default:
		return fmt.Sprintf("SampleFormat(%d)", uint8(f))
	}
}

// Size is how many bytes one sample of one channel takes.
func (f SampleFormat) Size() int {
	switch f {
	case Int16:
		return 2
	case Float32:
		return 4
	default:
		return 0
	}
}

// maxChannels is the largest channel count this package will describe. It is
// not AudioToolbox's limit; it is a sanity bound, so a demuxer that reports
// rubbish is refused here rather than inside a framework.
const maxChannels = 64

// ---------------------------------------------------------------------------
// Configuration.
// ---------------------------------------------------------------------------

// Config describes the track a decoder is built for, the way a demuxer states
// it.
//
// container.TrackConfig hands back every field but Codec, which [CodecFor]
// maps from its Codec string.
type Config struct {
	// Codec is the compressed form in the packets. It is taken as stated and
	// never inferred.
	Codec Codec
	// SampleRate is the coded sample rate in Hz, as the container states it.
	SampleRate int
	// Channels is the coded channel count.
	Channels int
	// AudioObjectType is the AAC profile from the track's AudioSpecificConfig:
	// 2 for AAC-LC, 5 for HE-AAC, 29 for HE-AAC v2. Zero means AAC-LC, which
	// is what a Matroska track that states no configuration at all is.
	// Ignored by every other codec.
	AudioObjectType byte
	// CodecConfig is the codec's own configuration record, as the demuxer
	// read it: an AudioSpecificConfig for AAC, an OpusHead for Opus. When it
	// is empty for AAC, one is built — see [Config.MagicCookie].
	CodecConfig []byte
	// OutputChannels is how many channels to decode into. Zero means
	// Channels, which is the usual case; setting 2 for a 5.1 track asks
	// AudioToolbox to downmix, which it will do for AC-3 and refuse for
	// codecs whose decoder has no matrix.
	OutputChannels int
	// Output is the PCM sample format to decode into. The zero value is
	// [Int16].
	Output SampleFormat
}

// framesPerPacket is how many frames one coded packet of this codec holds.
//
// AudioToolbox needs it stated in the source format description: unlike a WAV
// file, a compressed packet does not say how long it is, and the converter uses
// this to size its output. The numbers are the codecs' own fixed frame sizes —
// 1024 for AAC-LC, 2048 for the spectral-band-replication profiles whose
// decoder runs at twice the coded rate, 1536 for AC-3 and its enhanced form.
//
// Opus is the exception: its packets are variable, 2.5 ms to 60 ms, and the
// decoder is told the largest one so the output buffer is always big enough.
func (c Config) framesPerPacket() int {
	switch c.Codec {
	case AAC:
		switch c.AudioObjectType {
		case 5, 29:
			return 2048
		default:
			return 1024
		}
	case AC3, EAC3:
		return 1536
	case Opus:
		// 60 ms at 48 kHz, which is Opus's longest frame.
		return 2880
	default:
		return 0
	}
}

// outputChannels resolves the channel count to decode into.
func (c Config) outputChannels() int {
	if c.OutputChannels > 0 {
		return c.OutputChannels
	}
	return c.Channels
}

// validate resolves a configuration into the form a decoder is built from, or
// says why it describes nothing.
func (c Config) validate() (Config, error) {
	switch c.Codec {
	case AAC, AC3, EAC3, Opus:
	default:
		return Config{}, fmt.Errorf("%w: %v", ErrUnsupportedCodec, c.Codec)
	}
	if c.SampleRate <= 0 {
		return Config{}, fmt.Errorf("%w: sample rate %d, and a decoder cannot be built without one",
			ErrConfig, c.SampleRate)
	}
	if c.Channels <= 0 || c.Channels > maxChannels {
		return Config{}, fmt.Errorf("%w: %d channels, and this describes 1 to %d",
			ErrConfig, c.Channels, maxChannels)
	}
	out := c.outputChannels()
	if out > maxChannels {
		return Config{}, fmt.Errorf("%w: %d output channels, and this describes 1 to %d",
			ErrConfig, out, maxChannels)
	}
	if c.Output.Size() == 0 {
		return Config{}, fmt.Errorf("%w: %v is not a PCM sample format", ErrConfig, c.Output)
	}
	c.OutputChannels = out
	return c, nil
}

// ---------------------------------------------------------------------------
// The AAC magic cookie.
//
// AAC in an MP4 or a Matroska file is raw: no ADTS header, nothing in the
// packets that says what profile or what channel configuration they are. The
// decoder is set up from an AudioSpecificConfig instead, which AudioToolbox
// takes as a converter's "magic cookie".
// ---------------------------------------------------------------------------

// aacSampleRates is the AudioSpecificConfig sampling frequency table, ISO/IEC
// 14496-3 Table 1.18. The index of a rate in this table is what the four-bit
// field holds; 15 is the escape that spells the rate out in 24 bits.
var aacSampleRates = []int{96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050, 16000, 12000, 11025, 8000, 7350}

// aacSampleRateIndex is a rate's index in the table, or 15 for one that is not
// in it — which is the escape value, and means the rate follows in full.
func aacSampleRateIndex(rate int) int {
	for i, r := range aacSampleRates {
		if r == rate {
			return i
		}
	}
	return 15
}

// aacChannelConfiguration maps a channel count onto the AudioSpecificConfig's
// four-bit channelConfiguration, ISO/IEC 14496-3 Table 1.19. It is not the
// count: 7.1 is eight channels and configuration 7, and there is no
// configuration for seven. Zero means "the configuration is in the bitstream",
// which is what an unusual count has to fall back to.
func aacChannelConfiguration(channels int) int {
	switch {
	case channels >= 1 && channels <= 6:
		return channels
	case channels == 8:
		return 7
	default:
		return 0
	}
}

// MagicCookie is the codec configuration AudioToolbox is set up from, or nil
// for a codec that needs none.
//
// For AAC it is an MPEG-4 ES_Descriptor — an "esds" — wrapping the track's
// AudioSpecificConfig. The wrapping is not decoration, it is measured: offered
// the bare AudioSpecificConfig, AudioConverterSetProperty answers
// kAudioCodecBadPropertySizeError ('!dat') and the configuration is not
// applied. Wrapped, the same two bytes are taken.
//
// For every other codec it is [Config.CodecConfig] as the demuxer read it, or
// nil when there is none. AC-3 and Enhanced AC-3 carry everything in their
// packets — the sync frame states the rate, the bitrate and the channel mode —
// so a cookie would describe what the decoder is about to read anyway.
func (c Config) MagicCookie() []byte {
	if c.Codec != AAC {
		return c.CodecConfig
	}
	return esdsCookie(c.AudioSpecificConfig())
}

// AudioSpecificConfig is the two-or-so bytes of ISO/IEC 14496-3 configuration
// an AAC decoder is set up from.
//
// [Config.CodecConfig] is used as it stands when the demuxer stated one: it
// came out of the file and is more authoritative than anything derived. When
// there is none — which is what a Matroska track with no CodecPrivate gives,
// and what every MP4 track avkit reads gives, because avkit reports the AAC
// profile rather than the record it came from — one is built from the profile,
// the sample rate and the channel count the demuxer DID state.
//
// It is exported because a caller that wants to know what its decoder was set
// up from should be able to look, and because it is the one piece of this
// package worth checking against a hex dump.
func (c Config) AudioSpecificConfig() []byte {
	if len(c.CodecConfig) > 0 {
		return c.CodecConfig
	}
	objectType := int(c.AudioObjectType)
	if objectType == 0 {
		// A track that states no profile is AAC-LC. That is not a guess:
		// object type 0 is "NULL" in the table and no encoder emits it, so
		// the field being zero means the demuxer had nothing to read.
		objectType = 2
	}
	var w bitWriter
	if objectType < 31 {
		w.write(uint32(objectType), 5)
	} else {
		// The five-bit field escapes at 31, and the real type follows as
		// six bits biased by 32.
		w.write(31, 5)
		w.write(uint32(objectType-32), 6)
	}
	index := aacSampleRateIndex(c.SampleRate)
	w.write(uint32(index), 4)
	if index == 15 {
		w.write(uint32(c.SampleRate), 24)
	}
	w.write(uint32(aacChannelConfiguration(c.Channels)), 4)
	// GASpecificConfig: frameLengthFlag 0 (1024-sample frames),
	// dependsOnCoreCoder 0, extensionFlag 0.
	w.write(0, 3)
	return w.bytes()
}

// MPEG-4 descriptor tags, ISO/IEC 14496-1 Table 1.
const (
	tagESDescriptor            = 0x03
	tagDecoderConfigDescriptor = 0x04
	tagDecoderSpecificInfo     = 0x05
	tagSLConfigDescriptor      = 0x06
)

// esdsCookie wraps an AudioSpecificConfig in the ES_Descriptor AudioToolbox
// takes as an AAC converter's magic cookie.
//
// Everything the descriptor can say and this does not know — the elementary
// stream id, the buffer size, the bitrates — is written as zero, because that
// is what an esds in an MP4 file written by anything says too: the decoder
// reads the object type, the stream type and the specific info, and ignores the
// rest. The two constants that are not zero are 0x40, "MPEG-4 audio", and 0x15,
// which is stream type 5 (audio) with the upstream flag clear and the reserved
// bit set, as the field requires.
//
// It returns nil for an empty config: a descriptor wrapping nothing describes
// nothing.
func esdsCookie(asc []byte) []byte {
	if len(asc) == 0 {
		return nil
	}
	dsi := descriptor(tagDecoderSpecificInfo, asc)
	config := descriptor(tagDecoderConfigDescriptor, append([]byte{
		0x40,    // objectTypeIndication: MPEG-4 audio
		0x15,    // streamType 5 (audio), upStream 0, reserved 1
		0, 0, 0, // bufferSizeDB
		0, 0, 0, 0, // maxBitrate
		0, 0, 0, 0, // avgBitrate
	}, dsi...))
	// SLConfigDescriptor: predefined 2, "reserved for use in MP4 files",
	// which is what every esds in an MP4 carries.
	sl := descriptor(tagSLConfigDescriptor, []byte{0x02})

	body := []byte{0, 0, 0} // ES_ID 0, and no stream dependence, URL or OCR
	body = append(body, config...)
	body = append(body, sl...)
	return descriptor(tagESDescriptor, body)
}

// descriptor is one MPEG-4 descriptor: a tag, a length, and the body.
//
// The length is written in the expandable form ISO/IEC 14496-1 defines — seven
// bits a byte, the top bit set on every byte but the last. A single byte is
// enough for anything here and would be simpler, but an AudioSpecificConfig
// with a program config element runs past 127 bytes, and a length that wrapped
// would describe a different descriptor entirely.
func descriptor(tag byte, body []byte) []byte {
	out := []byte{tag}
	n := len(body)
	var size []byte
	for {
		b := byte(n & 0x7f)
		n >>= 7
		size = append([]byte{b}, size...)
		if n == 0 {
			break
		}
	}
	for i := 0; i < len(size)-1; i++ {
		size[i] |= 0x80
	}
	out = append(out, size...)
	return append(out, body...)
}

// bitWriter writes big-endian bit fields, which is how every MPEG-4 descriptor
// is laid out.
type bitWriter struct {
	out  []byte
	acc  uint64
	bits uint
}

// write appends the low n bits of v, most significant first.
func (w *bitWriter) write(v uint32, n uint) {
	w.acc = w.acc<<n | uint64(v)&(1<<n-1)
	w.bits += n
	for w.bits >= 8 {
		w.bits -= 8
		w.out = append(w.out, byte(w.acc>>w.bits))
	}
}

// bytes is what was written, with the last byte padded with zero bits — which
// is what an AudioSpecificConfig does at its end anyway.
func (w *bitWriter) bytes() []byte {
	if w.bits > 0 {
		w.out = append(w.out, byte(w.acc<<(8-w.bits)))
		w.bits = 0
	}
	return w.out
}

// ---------------------------------------------------------------------------
// Packets and buffers.
// ---------------------------------------------------------------------------

// Packet is one coded audio packet handed to the decoder — one AAC access unit,
// one AC-3 sync frame, one Opus packet. It is what container.Reader's Samples
// hands back for an audio track, one element at a time.
type Packet struct {
	// Data is the coded packet, as the demuxer produced it.
	Data []byte
	// PTS is when this packet's audio starts, relative to the start of the
	// track. It travels through the decoder and comes back on the [Buffer];
	// nothing here depends on it.
	PTS time.Duration
}

// Buffer is the PCM one packet decoded into.
//
// PCM is NOT copied out of the decoder: it aliases a scratch buffer the
// [Decoder] owns, and the next call to [Decoder.Decode] overwrites it. Use
// [Buffer.Clone] to keep one. A [Player.Write] consumes it before returning, so
// the common path never needs a copy.
type Buffer struct {
	// PCM is the decoded samples, interleaved, Frames*Channels*Format.Size()
	// bytes of them.
	PCM []byte
	// Frames is how many sample frames PCM holds — samples per channel, not
	// samples.
	Frames int
	// Channels is how many channels are interleaved in PCM.
	Channels int
	// SampleRate is the rate PCM is at, in Hz.
	SampleRate int
	// Format is the layout of one sample.
	Format SampleFormat
	// PTS is the presentation timestamp of the packet this came from.
	PTS time.Duration
}

// Duration is how long this buffer's audio lasts.
func (b Buffer) Duration() time.Duration {
	if b.SampleRate <= 0 {
		return 0
	}
	return time.Duration(b.Frames) * time.Second / time.Duration(b.SampleRate)
}

// Clone copies the samples out of the decoder's scratch buffer, so the result
// outlives the next [Decoder.Decode].
func (b Buffer) Clone() Buffer {
	out := b
	out.PCM = append([]byte(nil), b.PCM...)
	return out
}

// ---------------------------------------------------------------------------
// The decoder.
// ---------------------------------------------------------------------------

// Decoder is one AudioConverter: a track's decoder, fed a coded packet at a
// time.
//
// It is NOT safe for concurrent use.
type Decoder struct {
	cfg    Config
	closed bool
	frames int64

	h decoderHandle
}

// NewDecoder builds a decoder for a track.
func NewDecoder(cfg Config) (*Decoder, error) {
	resolved, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	h, err := newDecoder(resolved)
	if err != nil {
		return nil, err
	}
	return &Decoder{cfg: resolved, h: h}, nil
}

// Config returns the configuration the decoder was built from, with the
// defaults resolved.
func (d *Decoder) Config() Config { return d.cfg }

// Frames is how many sample frames the decoder has produced since it was built.
// It is the honest count of what came out, which is not the count of what went
// in: an AAC decoder swallows the encoder's priming packets and emits nothing
// for them.
func (d *Decoder) Frames() int64 { return d.frames }

// Decoded is how long the audio produced so far lasts.
func (d *Decoder) Decoded() time.Duration {
	if d.cfg.SampleRate <= 0 {
		return 0
	}
	return time.Duration(d.frames) * time.Second / time.Duration(d.cfg.SampleRate)
}

// Decode turns one coded packet into PCM.
//
// The buffer it returns may hold no frames at all, and that is not an error: an
// AAC decoder emits nothing for the encoder delay at the start of a track, and
// a converter is entitled to hold a packet back. The samples alias the
// decoder's scratch buffer and the next call overwrites them.
func (d *Decoder) Decode(p Packet) (Buffer, error) {
	if d.closed {
		return Buffer{}, ErrClosed
	}
	if len(p.Data) == 0 {
		return Buffer{}, fmt.Errorf("%w: the packet carries no bytes", ErrPacket)
	}
	b, err := decodePacket(d.h, d.cfg, p)
	if err != nil {
		return Buffer{}, err
	}
	d.frames += int64(b.Frames)
	return b, nil
}

// Flush returns whatever the decoder was still holding. A caller that has
// submitted its last packet must call it, or lose the tail of the track.
func (d *Decoder) Flush() (Buffer, error) {
	if d.closed {
		return Buffer{}, ErrClosed
	}
	b, err := flushDecoder(d.h, d.cfg)
	if err != nil {
		return Buffer{}, err
	}
	d.frames += int64(b.Frames)
	return b, nil
}

// CodecConfigRefused reports what AudioToolbox answered when the codec
// configuration was offered to it, or nil when it was taken — and nil, too, for
// a codec that has none to offer.
//
// A refusal is not fatal and is not treated as one: measured on a plain AAC MP4,
// a decoder whose cookie was refused still decoded every packet bit for bit
// against afconvert. It is reported because the alternative is a caller who
// believes the configuration took effect when it did not, and finds out from a
// 5.1 track that comes back as noise.
func (d *Decoder) CodecConfigRefused() error {
	if d.closed || d.h == nil {
		return nil
	}
	return codecConfigRefused(d.h)
}

// Close tears the decoder down. It is safe to call more than once.
func (d *Decoder) Close() error {
	if d.closed {
		return nil
	}
	d.closed = true
	return closeDecoder(d.h)
}

// ---------------------------------------------------------------------------
// The player.
// ---------------------------------------------------------------------------

// defaultBufferFrames and defaultBufferCount size the output queue. Three
// buffers of 4096 frames is about a quarter of a second at 48 kHz: long enough
// that a decode that stalls briefly is not heard, short enough that a seek is
// not answered a second later.
const (
	defaultBufferFrames = 4096
	defaultBufferCount  = 3
)

// PlayerConfig describes the output.
type PlayerConfig struct {
	// SampleRate is the rate of the PCM that will be written, in Hz.
	SampleRate int
	// Channels is how many channels are interleaved in it.
	Channels int
	// Format is the layout of one sample. The zero value is [Int16].
	Format SampleFormat
	// BufferFrames is how many frames one output buffer holds; zero means
	// 4096.
	BufferFrames int
	// BufferCount is how many of them the queue cycles through; zero means 3.
	// Two is the minimum that lets one play while another is filled.
	BufferCount int
	// Volume is the output gain, 0 to 1. Zero means 1 — silence is asked for
	// by not playing, not by a zero field nobody filled in.
	Volume float64
}

// PlayerConfigFor is the output that matches what a [Decoder] built from cfg
// produces, which is what a player feeding one decoder wants.
func PlayerConfigFor(cfg Config) PlayerConfig {
	return PlayerConfig{
		SampleRate: cfg.SampleRate,
		Channels:   cfg.outputChannels(),
		Format:     cfg.Output,
	}
}

// BytesPerFrame is how many bytes one sample frame takes.
func (c PlayerConfig) BytesPerFrame() int { return c.Channels * c.Format.Size() }

// validate resolves the defaults, or says why the configuration describes no
// output.
func (c PlayerConfig) validate() (PlayerConfig, error) {
	if c.SampleRate <= 0 {
		return PlayerConfig{}, fmt.Errorf("%w: sample rate %d", ErrConfig, c.SampleRate)
	}
	if c.Channels <= 0 || c.Channels > maxChannels {
		return PlayerConfig{}, fmt.Errorf("%w: %d channels, and this plays 1 to %d",
			ErrConfig, c.Channels, maxChannels)
	}
	if c.Format.Size() == 0 {
		return PlayerConfig{}, fmt.Errorf("%w: %v is not a PCM sample format", ErrConfig, c.Format)
	}
	if c.BufferFrames <= 0 {
		c.BufferFrames = defaultBufferFrames
	}
	if c.BufferCount <= 0 {
		c.BufferCount = defaultBufferCount
	}
	if c.BufferCount < 2 {
		return PlayerConfig{}, fmt.Errorf("%w: %d output buffers, and one cannot be filled while "+
			"another plays with fewer than 2", ErrConfig, c.BufferCount)
	}
	if c.Volume == 0 {
		c.Volume = 1
	}
	if c.Volume < 0 || c.Volume > 1 {
		return PlayerConfig{}, fmt.Errorf("%w: volume %v, and the range is 0 to 1", ErrConfig, c.Volume)
	}
	return c, nil
}

// Player plays interleaved PCM on the system output.
//
// [Player.Write] is the only entry point that blocks: it waits for a queue
// buffer to come free, which is what paces a decode loop to real time without
// the caller sleeping on a guess.
//
// It is NOT safe for concurrent use, except that [Player.Played] may be read
// from another goroutine — which is the point of a clock.
type Player struct {
	cfg     PlayerConfig
	closed  bool
	started bool
	written int64 // frames handed to the queue

	h playerHandle
}

// NewPlayer opens the system output. Nothing is heard until [Player.Start].
func NewPlayer(cfg PlayerConfig) (*Player, error) {
	resolved, err := cfg.validate()
	if err != nil {
		return nil, err
	}
	h, err := newPlayer(resolved)
	if err != nil {
		return nil, err
	}
	return &Player{cfg: resolved, h: h}, nil
}

// Config returns the configuration the player was built from, with the defaults
// resolved.
func (p *Player) Config() PlayerConfig { return p.cfg }

// Start begins playback. Writing before it is allowed and usual: filling the
// queue first is what stops the first buffers from being played half empty.
func (p *Player) Start() error {
	if p.closed {
		return ErrClosed
	}
	if p.started {
		return nil
	}
	if err := startPlayer(p.h); err != nil {
		return err
	}
	p.started = true
	return nil
}

// Write hands interleaved PCM to the output, blocking until it has all been
// queued. It returns the number of bytes written, which is len(pcm) unless it
// failed part way.
//
// A partial frame is refused rather than queued: half a sample frame shifts
// every channel that follows it by one, and the noise that makes is not
// obviously a bug.
func (p *Player) Write(pcm []byte) (int, error) {
	if p.closed {
		return 0, ErrClosed
	}
	if len(pcm) == 0 {
		return 0, nil
	}
	if bpf := p.cfg.BytesPerFrame(); len(pcm)%bpf != 0 {
		return 0, fmt.Errorf("%w: %d bytes is not a whole number of %d-byte frames",
			ErrPacket, len(pcm), bpf)
	}
	n, err := writePlayer(p.h, pcm)
	p.written += int64(n / p.cfg.BytesPerFrame())
	return n, err
}

// Played is how much audio has left the device since [Player.Start].
//
// It is the clock a player synchronises video against. It is read from the
// output's own timeline, not counted from what was written: the two differ by
// whatever is still sitting in the queue, which is a quarter of a second by
// default and the whole of the drift a naive player accumulates.
//
// One honest caveat, measured: the timeline is the DEVICE's, and a device that
// runs out of audio does not stop — it plays silence and the clock goes on
// advancing. So a player that stops writing sees Played run past what it wrote.
// That is the right behaviour for a master clock, which must not stall, and
// [Player.Queued] is what says whether there is anything left to hear.
func (p *Player) Played() time.Duration {
	if p.h == nil {
		return 0
	}
	return playedPlayer(p.h)
}

// Queued is how much audio has been written but not yet played — the latency
// between a decode and the sound.
func (p *Player) Queued() time.Duration {
	if p.cfg.SampleRate <= 0 {
		return 0
	}
	written := time.Duration(p.written) * time.Second / time.Duration(p.cfg.SampleRate)
	if q := written - p.Played(); q > 0 {
		return q
	}
	return 0
}

// Written is how many sample frames have been handed to the output.
func (p *Player) Written() int64 { return p.written }

// Drain waits for everything written to be played. A caller that has written
// its last buffer must call it, or Close cuts the tail off.
func (p *Player) Drain() error {
	if p.closed {
		return ErrClosed
	}
	if !p.started {
		// Nothing was ever played, so there is nothing to wait for. Waiting
		// anyway would block for ever on a queue that was never started.
		return nil
	}
	return drainPlayer(p.h)
}

// Stop stops playback at once, dropping whatever is still queued.
func (p *Player) Stop() error {
	if p.closed {
		return ErrClosed
	}
	if !p.started {
		return nil
	}
	p.started = false
	return stopPlayer(p.h)
}

// Close stops the output and gives it back. It is safe to call more than once,
// and it does NOT drain: call [Player.Drain] first to hear the end.
func (p *Player) Close() error {
	if p.closed {
		return nil
	}
	p.closed = true
	p.started = false
	return closePlayer(p.h)
}

// ---------------------------------------------------------------------------
// Platform seams. The darwin build assigns the real AudioToolbox
// implementations in an init(); every other platform assigns unsupported stubs.
// Keeping the portable logic above them lets this file be exercised without a
// Mac, and lets a test drive Decoder and Player through fakes.
// ---------------------------------------------------------------------------

// decoderHandle and playerHandle are the platform's state, opaque here.
type (
	decoderHandle any
	playerHandle  any
)

var (
	// newDecoder builds an AudioConverter for a validated configuration.
	newDecoder func(cfg Config) (decoderHandle, error)
	// decodePacket converts one coded packet into PCM.
	decodePacket func(h decoderHandle, cfg Config, p Packet) (Buffer, error)
	// flushDecoder drains whatever the converter still holds.
	flushDecoder func(h decoderHandle, cfg Config) (Buffer, error)
	// closeDecoder tears the converter down.
	closeDecoder func(h decoderHandle) error
	// codecConfigRefused reports whether the magic cookie was turned down.
	codecConfigRefused func(h decoderHandle) error

	// newPlayer opens an AudioQueue on the system output.
	newPlayer func(cfg PlayerConfig) (playerHandle, error)
	// startPlayer begins playback.
	startPlayer func(h playerHandle) error
	// writePlayer queues PCM, blocking until it is all taken.
	writePlayer func(h playerHandle, pcm []byte) (int, error)
	// playedPlayer reads the output's timeline.
	playedPlayer func(h playerHandle) time.Duration
	// drainPlayer waits for the queue to empty.
	drainPlayer func(h playerHandle) error
	// stopPlayer stops at once.
	stopPlayer func(h playerHandle) error
	// closePlayer disposes of the queue.
	closePlayer func(h playerHandle) error
)

// ---------------------------------------------------------------------------
// OSStatus.
// ---------------------------------------------------------------------------

// StatusError carries an OSStatus from AudioToolbox, naming the call that
// returned it. AudioToolbox says everything through these codes, and half of
// them are four-character codes read as a signed integer — 1718449215 is
// 'fmt?', which is a great deal more informative.
type StatusError struct {
	// Op is the C function that failed.
	Op string
	// Status is the OSStatus it returned.
	Status int32
}

// osStatusNames are the AudioToolbox statuses worth spelling out. The
// four-character ones are given as the code they read as, because that is what
// a caller sees.
var osStatusNames = map[int32]string{
	0x666D743F: "kAudioConverterErr_FormatNotSupported ('fmt?'): no decoder for this format",
	0x6F703F3F: "kAudioConverterErr_OperationNotSupported ('op??')",
	0x70726F70: "kAudioConverterErr_PropertyNotSupported ('prop')",
	0x696E737A: "kAudioConverterErr_InvalidInputSize ('insz')",
	0x6F74737A: "kAudioConverterErr_InvalidOutputSize ('otsz')",
	0x77686174: "kAudioConverterErr_UnspecifiedError ('what')",
	0x2173697A: "kAudioConverterErr_BadPropertySizeError ('!siz')",
	0x21706B64: "kAudioConverterErr_RequiresPacketDescriptionsError ('!pkd'): the source format needs per-packet descriptions",
	0x21697372: "kAudioConverterErr_InputSampleRateOutOfRange ('!isr')",
	0x216F7372: "kAudioConverterErr_OutputSampleRateOutOfRange ('!osr')",
	0x21646174: "kAudioCodecBadPropertySizeError / no data ('!dat')",
	-50:        "paramErr: a parameter is wrong",
	-66687:     "kAudioQueueErr_InvalidBuffer",
	-66686:     "kAudioQueueErr_BufferEmpty",
	-66685:     "kAudioQueueErr_DisposalPending",
	-66684:     "kAudioQueueErr_InvalidProperty",
	-66683:     "kAudioQueueErr_InvalidPropertySize",
	-66682:     "kAudioQueueErr_InvalidParameter",
	-66681:     "kAudioQueueErr_CannotStart",
	-66680:     "kAudioQueueErr_InvalidDevice",
	-66679:     "kAudioQueueErr_BufferInQueue",
	-66678:     "kAudioQueueErr_InvalidRunState",
	-66677:     "kAudioQueueErr_InvalidQueueType",
	-66676:     "kAudioQueueErr_Permissions",
	-66675:     "kAudioQueueErr_InvalidPropertyValue",
	-66674:     "kAudioQueueErr_PrimeTimedOut",
	-66673:     "kAudioQueueErr_CodecNotFound",
	-66672:     "kAudioQueueErr_InvalidCodecAccess",
	-66671:     "kAudioQueueErr_QueueInvalidated",
	-66666:     "kAudioQueueErr_BufferEnqueuedTwice",
	-66665:     "kAudioQueueErr_CannotStartYet",
	-66632:     "kAudioQueueErr_EnqueueDuringReset",
	-66626:     "kAudioQueueErr_InvalidOfflineMode",
}

func (e *StatusError) Error() string {
	if name, ok := osStatusNames[e.Status]; ok {
		return fmt.Sprintf("audiotoolbox: %s: %s (%d)", e.Op, name, e.Status)
	}
	if code, ok := fourCC(e.Status); ok {
		return fmt.Sprintf("audiotoolbox: %s: OSStatus %d (%q)", e.Op, e.Status, code)
	}
	return fmt.Sprintf("audiotoolbox: %s: OSStatus %d", e.Op, e.Status)
}

// fourCC reads a status as the four printable characters it may be. Most of
// AudioToolbox's errors are, and a reader who is handed 1718449215 has to work
// that out by hand.
func fourCC(s int32) (string, bool) {
	b := [4]byte{byte(s >> 24), byte(s >> 16), byte(s >> 8), byte(s)}
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return "", false
		}
	}
	return string(b[:]), true
}

// status wraps an OSStatus, or reports nil for noErr.
func status(op string, s int32) error {
	if s == 0 {
		return nil
	}
	return &StatusError{Op: op, Status: s}
}
