// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin

package audiotoolbox

import (
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
)

// Framework paths. AudioToolbox carries both halves — AudioConverter and
// AudioQueue — and libSystem carries malloc, because the packets and the PCM
// buffers a converter reads and writes have to be memory the Go garbage
// collector is not entitled to move.
const (
	frameworkAudioToolbox = "/System/Library/Frameworks/AudioToolbox.framework/AudioToolbox"
	libSystem             = "/usr/lib/libSystem.B.dylib"
)

// Four-character format IDs, from CoreAudioBaseTypes.h.
const (
	kAudioFormatLinearPCM     = 0x6C70636D // 'lpcm'
	kAudioFormatMPEG4AAC      = 0x61616320 // 'aac '
	kAudioFormatAC3           = 0x61632D33 // 'ac-3'
	kAudioFormatEnhancedAC3   = 0x65632D33 // 'ec-3'
	kAudioFormatOpus          = 0x6F707573 // 'opus'
	kAudioFormatMPEG4AAC_HE   = 0x61616368 // 'aach'
	kAudioFormatMPEG4AAC_HEv2 = 0x61616370 // 'aacp'
)

// LinearPCM format flags.
const (
	kAudioFormatFlagIsFloat         = 1 << 0
	kAudioFormatFlagIsSignedInteger = 1 << 2
	kAudioFormatFlagIsPacked        = 1 << 3
)

// AudioConverter property IDs.
const (
	kAudioConverterDecompressionMagicCookie = 0x646D6763 // 'dmgc'
)

// AudioQueue parameter IDs.
const kAudioQueueParam_Volume = 1

// ---------------------------------------------------------------------------
// The C structures. Every one of them is passed BY POINTER, which is
// deliberate: purego.NewCallback cannot describe a struct passed by value as a
// callback argument — a 24-byte CMTime is passed by reference on arm64 and on
// the stack on amd64 — and neither of this package's two callbacks takes one.
// The layouts below are the 64-bit ones, padding written out where C inserts
// it, so a field never lands at the wrong offset.
// ---------------------------------------------------------------------------

// audioStreamBasicDescription is CoreAudio's ASBD: 40 bytes describing one
// stream format, compressed or not.
type audioStreamBasicDescription struct {
	SampleRate       float64
	FormatID         uint32
	FormatFlags      uint32
	BytesPerPacket   uint32
	FramesPerPacket  uint32
	BytesPerFrame    uint32
	ChannelsPerFrame uint32
	BitsPerChannel   uint32
	Reserved         uint32
}

// audioBuffer is one AudioBuffer: 16 bytes, four of them padding.
type audioBuffer struct {
	NumberChannels uint32
	DataByteSize   uint32
	Data           unsafe.Pointer
}

// audioBufferList is an AudioBufferList holding exactly one buffer, which is
// what interleaved PCM needs. Non-interleaved output would need one per
// channel, and this package does not ask for it.
type audioBufferList struct {
	NumberBuffers uint32
	_             uint32
	Buffers       [1]audioBuffer
}

// audioStreamPacketDescription describes one packet inside a buffer: 16 bytes.
type audioStreamPacketDescription struct {
	StartOffset            int64
	VariableFramesInPacket uint32
	DataByteSize           uint32
}

// smpteTime is the 24-byte SMPTETime inside an AudioTimeStamp. Nothing here
// reads it; it is written out so the fields after it land where C puts them.
type smpteTime struct {
	Subframes       int16
	SubframeDivisor int16
	Counter         uint32
	Type            uint32
	Flags           uint32
	Hours           int16
	Minutes         int16
	Seconds         int16
	Frames          int16
}

// audioTimeStamp is CoreAudio's AudioTimeStamp: 64 bytes, of which this reads
// SampleTime and nothing else.
type audioTimeStamp struct {
	SampleTime    float64
	HostTime      uint64
	RateScalar    float64
	WordClockTime uint64
	SMPTE         smpteTime
	Flags         uint32
	Reserved      uint32
}

// kAudioTimeStampSampleTimeValid asks AudioQueueGetCurrentTime for the field
// this package's clock is built on.
const kAudioTimeStampSampleTimeValid = 1 << 0

// audioQueueBuffer is an AudioQueueBuffer: 56 bytes, allocated by the queue and
// handed back to the callback. The const-qualified fields are still fields, and
// the padding C inserts after each UInt32 that precedes a pointer is written
// out.
type audioQueueBuffer struct {
	AudioDataBytesCapacity    uint32
	_                         uint32
	AudioData                 unsafe.Pointer
	AudioDataByteSize         uint32
	_                         uint32
	UserData                  unsafe.Pointer
	PacketDescriptionCapacity uint32
	_                         uint32
	PacketDescriptions        unsafe.Pointer
	PacketDescriptionCount    uint32
	_                         uint32
}

// ---------------------------------------------------------------------------
// The C entry points.
// ---------------------------------------------------------------------------

var (
	audioConverterNew func(source, destination *audioStreamBasicDescription, out *uintptr) int32
	audioConverterDispose,
	audioConverterReset func(converter uintptr) int32
	audioConverterSetProperty       func(converter uintptr, id uint32, size uint32, data unsafe.Pointer) int32
	audioConverterFillComplexBuffer func(converter, proc, userData uintptr, packets *uint32,
		data *audioBufferList, descriptions *audioStreamPacketDescription) int32

	audioQueueNewOutput func(format *audioStreamBasicDescription, callback, userData,
		runLoop, runLoopMode uintptr, flags uint32, out *uintptr) int32
	audioQueueAllocateBuffer func(queue uintptr, capacity uint32, out **audioQueueBuffer) int32
	audioQueueEnqueueBuffer  func(queue uintptr, buffer *audioQueueBuffer, packets uint32,
		descriptions *audioStreamPacketDescription) int32
	audioQueueStart        func(queue, startTime uintptr) int32
	audioQueueStop         func(queue uintptr, immediate bool) int32
	audioQueueDispose      func(queue uintptr, immediate bool) int32
	audioQueueSetParameter func(queue uintptr, id uint32, value float32) int32
	// The timeline and the discontinuity flag are both optional; this passes
	// nothing for either, and reads only the sample time.
	audioQueueGetCurrentTime func(queue, timeline uintptr, out *audioTimeStamp, discontinuity *bool) int32

	cMalloc func(uint64) unsafe.Pointer
	cFree   func(unsafe.Pointer)
)

// The two C function pointers this package ever creates. purego never frees a
// callback and allows a bounded number of them, so one per decoder or per
// player would be a leak with a hard ceiling: each carries an integer key and
// looks its owner up instead.
var (
	converterInputCallback uintptr
	queueOutputCallback    uintptr
)

var (
	loadOnce sync.Once
	loadErr  error
)

// load resolves the framework and its entry points once.
func load() error {
	loadOnce.Do(func() { loadErr = doLoad() })
	return loadErr
}

func doLoad() error {
	at, err := dlopen(frameworkAudioToolbox)
	if err != nil {
		return err
	}
	sys, err := dlopen(libSystem)
	if err != nil {
		return err
	}
	purego.RegisterLibFunc(&audioConverterNew, at, "AudioConverterNew")
	purego.RegisterLibFunc(&audioConverterDispose, at, "AudioConverterDispose")
	purego.RegisterLibFunc(&audioConverterReset, at, "AudioConverterReset")
	purego.RegisterLibFunc(&audioConverterSetProperty, at, "AudioConverterSetProperty")
	purego.RegisterLibFunc(&audioConverterFillComplexBuffer, at, "AudioConverterFillComplexBuffer")
	purego.RegisterLibFunc(&audioQueueNewOutput, at, "AudioQueueNewOutput")
	purego.RegisterLibFunc(&audioQueueAllocateBuffer, at, "AudioQueueAllocateBuffer")
	purego.RegisterLibFunc(&audioQueueEnqueueBuffer, at, "AudioQueueEnqueueBuffer")
	purego.RegisterLibFunc(&audioQueueStart, at, "AudioQueueStart")
	purego.RegisterLibFunc(&audioQueueStop, at, "AudioQueueStop")
	purego.RegisterLibFunc(&audioQueueDispose, at, "AudioQueueDispose")
	purego.RegisterLibFunc(&audioQueueSetParameter, at, "AudioQueueSetParameter")
	purego.RegisterLibFunc(&audioQueueGetCurrentTime, at, "AudioQueueGetCurrentTime")
	purego.RegisterLibFunc(&cMalloc, sys, "malloc")
	purego.RegisterLibFunc(&cFree, sys, "free")
	converterInputCallback = purego.NewCallback(converterInput)
	queueOutputCallback = purego.NewCallback(queueOutput)
	return nil
}

// dlopen is a seam so a test can force doLoad's failure path.
var dlopen = func(path string) (uintptr, error) {
	return purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
}

func init() {
	newDecoder = darwinNewDecoder
	decodePacket = darwinDecode
	flushDecoder = darwinFlush
	closeDecoder = darwinCloseDecoder
	codecConfigRefused = darwinCookieRefused

	newPlayer = darwinNewPlayer
	startPlayer = darwinStart
	writePlayer = darwinWrite
	playedPlayer = darwinPlayed
	drainPlayer = darwinDrain
	stopPlayer = darwinStop
	closePlayer = darwinClosePlayer
}

// ---------------------------------------------------------------------------
// The registries. A C callback cannot close over anything, so it carries an
// integer key and looks its owner up. An integer, not a Go pointer: handing C a
// pointer into the Go heap and expecting it back later is exactly what the cgo
// pointer rules forbid.
// ---------------------------------------------------------------------------

var registry struct {
	sync.Mutex
	next     uintptr
	decoders map[uintptr]*darwinDecoder
	players  map[uintptr]*darwinPlayer
}

func registerDecoder(d *darwinDecoder) {
	registry.Lock()
	defer registry.Unlock()
	if registry.decoders == nil {
		registry.decoders = map[uintptr]*darwinDecoder{}
	}
	registry.next++
	d.id = registry.next
	registry.decoders[d.id] = d
}

func unregisterDecoder(d *darwinDecoder) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.decoders, d.id)
}

func lookupDecoder(id uintptr) *darwinDecoder {
	registry.Lock()
	defer registry.Unlock()
	return registry.decoders[id]
}

func registerPlayer(p *darwinPlayer) {
	registry.Lock()
	defer registry.Unlock()
	if registry.players == nil {
		registry.players = map[uintptr]*darwinPlayer{}
	}
	registry.next++
	p.id = registry.next
	registry.players[p.id] = p
}

func unregisterPlayer(p *darwinPlayer) {
	registry.Lock()
	defer registry.Unlock()
	delete(registry.players, p.id)
}

func lookupPlayer(id uintptr) *darwinPlayer {
	registry.Lock()
	defer registry.Unlock()
	return registry.players[id]
}

// ---------------------------------------------------------------------------
// Format descriptions.
// ---------------------------------------------------------------------------

// sourceFormat is the ASBD describing the coded packets.
//
// Everything a compressed ASBD does NOT say is zero, and that is not laziness:
// a packet of AAC has no fixed byte size and no bytes per frame, so stating one
// would be stating something false. What it must say is the format, the rate,
// the channel count and how many frames a packet holds — the converter cannot
// size its own output without the last of these.
func sourceFormat(cfg Config) (audioStreamBasicDescription, error) {
	var id uint32
	switch cfg.Codec {
	case AAC:
		switch cfg.AudioObjectType {
		case 5:
			id = kAudioFormatMPEG4AAC_HE
		case 29:
			id = kAudioFormatMPEG4AAC_HEv2
		default:
			id = kAudioFormatMPEG4AAC
		}
	case AC3:
		id = kAudioFormatAC3
	case EAC3:
		id = kAudioFormatEnhancedAC3
	case Opus:
		id = kAudioFormatOpus
	default:
		return audioStreamBasicDescription{}, fmt.Errorf("%w: %v", ErrUnsupportedCodec, cfg.Codec)
	}
	return audioStreamBasicDescription{
		SampleRate:       float64(cfg.SampleRate),
		FormatID:         id,
		FramesPerPacket:  uint32(cfg.framesPerPacket()),
		ChannelsPerFrame: uint32(cfg.Channels),
	}, nil
}

// pcmFormat is the ASBD describing interleaved PCM, which is what both the
// converter writes and the queue plays.
func pcmFormat(rate, channels int, f SampleFormat) audioStreamBasicDescription {
	size := f.Size()
	flags := uint32(kAudioFormatFlagIsPacked)
	if f == Float32 {
		flags |= kAudioFormatFlagIsFloat
	} else {
		flags |= kAudioFormatFlagIsSignedInteger
	}
	// Endianness is not stated, and the absence is the statement: CoreAudio
	// reads a format with no big-endian flag as native-endian, which on every
	// Mac this runs on is little-endian.
	return audioStreamBasicDescription{
		SampleRate:       float64(rate),
		FormatID:         kAudioFormatLinearPCM,
		FormatFlags:      flags,
		BytesPerPacket:   uint32(channels * size),
		FramesPerPacket:  1,
		BytesPerFrame:    uint32(channels * size),
		ChannelsPerFrame: uint32(channels),
		BitsPerChannel:   uint32(size * 8),
	}
}

// ---------------------------------------------------------------------------
// The decoder.
// ---------------------------------------------------------------------------

// darwinDecoder is one AudioConverter and the C memory it reads and writes.
type darwinDecoder struct {
	id        uintptr
	converter uintptr
	cfg       Config

	in     unsafe.Pointer // staging for one coded packet
	inCap  int
	desc   *audioStreamPacketDescription
	out    unsafe.Pointer // PCM the converter writes
	outCap int

	// cookieRefused is what AudioToolbox answered when the codec
	// configuration was offered to it, or nil when it took it.
	cookieRefused error

	mu      sync.Mutex
	pending []byte // the packet the input proc is to hand over; nil when there is none
	fed     bool   // the input proc has already handed this packet over
	eos     bool   // the caller has flushed: there will never be another packet

	scratch []byte // the Go-side copy Decode returns
}

// noMoreInput is what the input callback answers when it has nothing left to
// hand over and the stream has NOT ended.
//
// It has to be an error, and the reason is measured. Reporting zero packets
// with noErr — which is what Apple's documentation describes — tells the
// converter the INPUT IS OVER: it decodes what it has, and every later
// FillComplexBuffer returns no frames at all. Measured on a plain AAC MP4, the
// first packet decoded to 1024 frames and the next 12090 decoded to nothing,
// silently, with noErr throughout. A distinctive status instead means "not now"
// rather than "never": FillComplexBuffer hands it back, keeps the frames it
// already wrote, and the next call works.
//
// End of stream is still reachable, and [Decoder.Flush] is what reaches it.
const noMoreInput = 0x6E616461 // 'nada'

// outputSlack is how much more room the output buffer holds than one packet's
// worth of frames. A converter is entitled to emit more than one packet's
// frames in a call — AC-3 and AAC do not, but nothing promises it — and a
// buffer too small is an error status, not a short read.
const outputSlack = 2

func darwinNewDecoder(cfg Config) (decoderHandle, error) {
	if err := load(); err != nil {
		return nil, err
	}
	source, err := sourceFormat(cfg)
	if err != nil {
		return nil, err
	}
	dest := pcmFormat(cfg.SampleRate, cfg.OutputChannels, cfg.Output)

	d := &darwinDecoder{cfg: cfg}
	if st := audioConverterNew(&source, &dest, &d.converter); st != 0 {
		return nil, fmt.Errorf("%w: %v: %w", ErrUnsupportedCodec, cfg.Codec,
			status("AudioConverterNew", st))
	}
	if cookie := cfg.MagicCookie(); len(cookie) > 0 {
		// A cookie the converter turns down is not fatal: AC-3 states
		// everything in its packets, and an AAC track whose configuration
		// the demuxer read is usually described well enough by the ASBD
		// alone. A cookie it ACCEPTS is what makes an unusual AAC channel
		// configuration come out right, so it is offered every time.
		if st := audioConverterSetProperty(d.converter, kAudioConverterDecompressionMagicCookie,
			uint32(len(cookie)), unsafe.Pointer(&cookie[0])); st != 0 {
			d.cookieRefused = status("AudioConverterSetProperty(magic cookie)", st)
		}
	}
	frames := cfg.framesPerPacket() * outputSlack
	d.outCap = frames * cfg.OutputChannels * cfg.Output.Size()
	d.out = cMalloc(uint64(d.outCap))
	d.desc = (*audioStreamPacketDescription)(cMalloc(uint64(unsafe.Sizeof(audioStreamPacketDescription{}))))
	if d.out == nil || d.desc == nil {
		d.freeMemory()
		audioConverterDispose(d.converter)
		return nil, fmt.Errorf("audiotoolbox: out of memory building a %v decoder", cfg.Codec)
	}
	registerDecoder(d)
	return d, nil
}

// darwinCookieRefused is what AudioToolbox answered when the magic cookie was
// offered to it.
func darwinCookieRefused(h decoderHandle) error {
	d, ok := h.(*darwinDecoder)
	if !ok || d == nil {
		return nil
	}
	return d.cookieRefused
}

// stage copies a coded packet into C memory, growing the staging buffer when
// this packet is bigger than any before it.
//
// The copy is deliberate, for the reason videotoolbox copies an access unit:
// the converter reads the bytes after the input callback has returned, and a Go
// slice is memory the runtime is entitled to move.
func (d *darwinDecoder) stage(data []byte) error {
	if len(data) > d.inCap {
		if d.in != nil {
			cFree(d.in)
		}
		d.in = cMalloc(uint64(len(data)))
		if d.in == nil {
			d.inCap = 0
			return fmt.Errorf("audiotoolbox: out of memory staging a %d-byte packet", len(data))
		}
		d.inCap = len(data)
	}
	copy(unsafe.Slice((*byte)(d.in), len(data)), data)
	return nil
}

func (d *darwinDecoder) freeMemory() {
	for _, p := range []*unsafe.Pointer{&d.in, &d.out} {
		if *p != nil {
			cFree(*p)
			*p = nil
		}
	}
	if d.desc != nil {
		cFree(unsafe.Pointer(d.desc))
		d.desc = nil
	}
	d.inCap, d.outCap = 0, 0
}

// converterInput is AudioConverterComplexInputDataProc.
//
// It hands over the one packet the current [darwinDecode] staged, once, and
// then reports no packets at all — which is how a converter is told the input
// it has is all there is for now. Reporting zero with noErr is not an error and
// does not end the converter: the next FillComplexBuffer works.
func converterInput(_ uintptr, packets *uint32, data *audioBufferList,
	descriptions **audioStreamPacketDescription, user uintptr) uintptr {
	d := lookupDecoder(user)
	if d == nil {
		// The decoder was closed while a fill was running. Saying the
		// stream ended is the only honest answer left.
		*packets = 0
		return 0
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fed || len(d.pending) == 0 {
		*packets = 0
		if d.eos {
			return 0
		}
		return noMoreInput
	}
	d.fed = true
	n := uint32(len(d.pending))
	data.NumberBuffers = 1
	data.Buffers[0].NumberChannels = uint32(d.cfg.Channels)
	data.Buffers[0].DataByteSize = n
	data.Buffers[0].Data = d.in
	*packets = 1
	if descriptions != nil {
		d.desc.StartOffset = 0
		d.desc.VariableFramesInPacket = 0
		d.desc.DataByteSize = n
		*descriptions = d.desc
	}
	return 0
}

// darwinDecode converts one packet, or drains the converter when p.Data is
// empty — which is what darwinFlush asks for.
func darwinDecode(h decoderHandle, cfg Config, p Packet) (Buffer, error) {
	d, ok := h.(*darwinDecoder)
	if !ok || d == nil || d.converter == 0 {
		return Buffer{}, ErrClosed
	}
	if len(p.Data) > 0 {
		if err := d.stage(p.Data); err != nil {
			return Buffer{}, err
		}
	}
	d.mu.Lock()
	d.pending, d.fed = p.Data, false
	d.mu.Unlock()

	bytesPerFrame := cfg.OutputChannels * cfg.Output.Size()
	frames := uint32(d.outCap / bytesPerFrame)
	list := audioBufferList{NumberBuffers: 1}
	list.Buffers[0] = audioBuffer{
		NumberChannels: uint32(cfg.OutputChannels),
		DataByteSize:   uint32(d.outCap),
		Data:           d.out,
	}
	st := audioConverterFillComplexBuffer(d.converter, converterInputCallback, d.id, &frames, &list, nil)

	d.mu.Lock()
	d.pending, d.fed = nil, false
	d.mu.Unlock()

	// noMoreInput is this package's own status, handed straight back by
	// FillComplexBuffer when the callback ran out of packets. It is how the
	// fill ENDS on the normal path, not a failure, and the frames written
	// before it are good.
	if st != 0 && st != noMoreInput {
		return Buffer{}, status("AudioConverterFillComplexBuffer", st)
	}
	n := int(frames) * bytesPerFrame
	if n > d.outCap {
		// The converter said it wrote more than the buffer holds. That
		// cannot happen, and if it ever does, reading the excess is reading
		// somebody else's memory.
		return Buffer{}, fmt.Errorf("audiotoolbox: the converter reported %d frames, "+
			"which is %d bytes in a %d-byte buffer", frames, n, d.outCap)
	}
	if cap(d.scratch) < n {
		d.scratch = make([]byte, n)
	}
	d.scratch = d.scratch[:n]
	copy(d.scratch, unsafe.Slice((*byte)(d.out), n))
	return Buffer{
		PCM:        d.scratch,
		Frames:     int(frames),
		Channels:   cfg.OutputChannels,
		SampleRate: cfg.SampleRate,
		Format:     cfg.Output,
		PTS:        p.PTS,
	}, nil
}

// darwinFlush asks the converter for whatever it still holds, by running a fill
// with no input to give it and telling the callback the stream is over — which
// is the one place zero packets with noErr is the right answer.
func darwinFlush(h decoderHandle, cfg Config) (Buffer, error) {
	if d, ok := h.(*darwinDecoder); ok && d != nil {
		d.mu.Lock()
		d.eos = true
		d.mu.Unlock()
	}
	return darwinDecode(h, cfg, Packet{})
}

func darwinCloseDecoder(h decoderHandle) error {
	d, ok := h.(*darwinDecoder)
	if !ok || d == nil {
		return nil
	}
	unregisterDecoder(d)
	if d.converter != 0 {
		audioConverterDispose(d.converter)
		d.converter = 0
	}
	d.freeMemory()
	return nil
}

// ---------------------------------------------------------------------------
// The player.
// ---------------------------------------------------------------------------

// darwinPlayer is one AudioQueue and the buffers it cycles through.
type darwinPlayer struct {
	id    uintptr
	queue uintptr
	cfg   PlayerConfig

	buffers []*audioQueueBuffer
	// free carries the buffers the queue has finished with. Its capacity is
	// the buffer count, so the output callback — which runs on one of
	// CoreAudio's own threads — never blocks.
	free chan *audioQueueBuffer

	mu         sync.Mutex
	running    bool
	lastPlayed time.Duration
}

func darwinNewPlayer(cfg PlayerConfig) (playerHandle, error) {
	if err := load(); err != nil {
		return nil, err
	}
	format := pcmFormat(cfg.SampleRate, cfg.Channels, cfg.Format)
	p := &darwinPlayer{cfg: cfg, free: make(chan *audioQueueBuffer, cfg.BufferCount)}
	registerPlayer(p)
	// A nil run loop asks AudioQueue to call back on a thread of its own,
	// which is what a Go program wants: the alternative is a CFRunLoop
	// somebody has to run, and a Go main goroutine is not one.
	if st := audioQueueNewOutput(&format, queueOutputCallback, p.id, 0, 0, 0, &p.queue); st != 0 {
		unregisterPlayer(p)
		return nil, status("AudioQueueNewOutput", st)
	}
	size := uint32(cfg.BufferFrames * cfg.BytesPerFrame())
	for i := 0; i < cfg.BufferCount; i++ {
		var buf *audioQueueBuffer
		if st := audioQueueAllocateBuffer(p.queue, size, &buf); st != 0 {
			darwinClosePlayer(p)
			return nil, status("AudioQueueAllocateBuffer", st)
		}
		p.buffers = append(p.buffers, buf)
		p.free <- buf
	}
	if st := audioQueueSetParameter(p.queue, kAudioQueueParam_Volume, float32(cfg.Volume)); st != 0 {
		darwinClosePlayer(p)
		return nil, status("AudioQueueSetParameter(volume)", st)
	}
	return p, nil
}

// queueOutput is AudioQueueOutputCallback: the queue has finished with a buffer
// and it can be filled again. It takes three pointers and no struct by value,
// which is why it can be a purego callback at all.
func queueOutput(user, _ uintptr, buffer *audioQueueBuffer) {
	p := lookupPlayer(user)
	if p == nil {
		// The player was disposed of while a buffer was in flight. Nothing
		// owns it; the queue does.
		return
	}
	select {
	case p.free <- buffer:
	default:
		// Unreachable while the channel holds as many buffers as the queue
		// has, and dropping is still better than blocking a CoreAudio thread.
	}
}

func player(h playerHandle) (*darwinPlayer, error) {
	p, ok := h.(*darwinPlayer)
	if !ok || p == nil || p.queue == 0 {
		return nil, ErrClosed
	}
	return p, nil
}

func darwinStart(h playerHandle) error {
	p, err := player(h)
	if err != nil {
		return err
	}
	if st := audioQueueStart(p.queue, 0); st != 0 {
		return status("AudioQueueStart", st)
	}
	p.mu.Lock()
	p.running = true
	p.mu.Unlock()
	return nil
}

// darwinWrite fills queue buffers and enqueues them, waiting for one to come
// free when they are all in flight. That wait is what paces a decode loop to
// real time: a caller that decodes as fast as it can is held here, and holds no
// more than the queue's own latency of audio in front of the device.
func darwinWrite(h playerHandle, pcm []byte) (int, error) {
	p, err := player(h)
	if err != nil {
		return 0, err
	}
	written := 0
	for len(pcm) > 0 {
		buf := <-p.free
		n := len(pcm)
		if c := int(buf.AudioDataBytesCapacity); n > c {
			n = c
		}
		copy(unsafe.Slice((*byte)(buf.AudioData), n), pcm[:n])
		buf.AudioDataByteSize = uint32(n)
		if st := audioQueueEnqueueBuffer(p.queue, buf, 0, nil); st != 0 {
			p.free <- buf
			return written, status("AudioQueueEnqueueBuffer", st)
		}
		pcm = pcm[n:]
		written += n
	}
	return written, nil
}

// darwinPlayed reads the queue's own timeline.
//
// A stopped queue answers kAudioQueueErr_InvalidRunState rather than a time, so
// the last reading is kept: a clock that jumps back to zero the moment
// playback ends is worse than one that stops.
func darwinPlayed(h playerHandle) time.Duration {
	p, err := player(h)
	if err != nil {
		return 0
	}
	var ts audioTimeStamp
	if st := audioQueueGetCurrentTime(p.queue, 0, &ts, nil); st != 0 || ts.Flags&kAudioTimeStampSampleTimeValid == 0 {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.lastPlayed
	}
	played := time.Duration(ts.SampleTime / float64(p.cfg.SampleRate) * float64(time.Second))
	if played < 0 {
		played = 0
	}
	p.mu.Lock()
	p.lastPlayed = played
	p.mu.Unlock()
	return played
}

// darwinDrain waits for the device to finish what was written.
//
// AudioQueueStop with inImmediate false does NOT wait: measured, it returns in
// thirty microseconds and lets the queue finish in its own time, so a drain
// that called it and returned would cut the tail off exactly as an immediate
// stop does. What waits is the buffers themselves. The queue hands each one
// back through the output callback when it has been rendered, so the drain is
// complete when every buffer the player owns is back in hand — at which point
// the device has played everything and there is nothing left to stop for.
func darwinDrain(h playerHandle) error {
	p, err := player(h)
	if err != nil {
		return err
	}
	held := p.reclaim()
	// Read the clock while the queue still answers: a stopped queue reports
	// kAudioQueueErr_InvalidRunState instead of a time, and this reading is
	// what Played gives back afterwards.
	darwinPlayed(p)
	for _, b := range held {
		p.free <- b
	}
	if st := audioQueueStop(p.queue, true); st != 0 {
		return status("AudioQueueStop", st)
	}
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	return nil
}

// reclaim takes every buffer back from the queue, which happens as the device
// finishes with them, and hands them to the caller to put back.
//
// It gives up after the longest the audio in flight can possibly last, with a
// second of slack: a device that has stopped consuming would otherwise hang the
// drain for ever, and a tail that is lost is better than a program that stops.
func (p *darwinPlayer) reclaim() []*audioQueueBuffer {
	inflight := time.Duration(len(p.buffers)*p.cfg.BufferFrames) * time.Second /
		time.Duration(p.cfg.SampleRate)
	deadline := time.NewTimer(2*inflight + time.Second)
	defer deadline.Stop()
	held := make([]*audioQueueBuffer, 0, len(p.buffers))
	for len(held) < len(p.buffers) {
		select {
		case b := <-p.free:
			held = append(held, b)
		case <-deadline.C:
			return held
		}
	}
	return held
}

func darwinStop(h playerHandle) error {
	p, err := player(h)
	if err != nil {
		return err
	}
	darwinPlayed(p)
	if st := audioQueueStop(p.queue, true); st != 0 {
		return status("AudioQueueStop", st)
	}
	p.mu.Lock()
	p.running = false
	p.mu.Unlock()
	return nil
}

func darwinClosePlayer(h playerHandle) error {
	p, ok := h.(*darwinPlayer)
	if !ok || p == nil {
		return nil
	}
	unregisterPlayer(p)
	if p.queue != 0 {
		// Disposing immediately is right here: Close does not drain, and a
		// caller that wanted the tail called Drain first. The queue frees
		// every buffer it allocated.
		audioQueueDispose(p.queue, true)
		p.queue = 0
	}
	p.buffers = nil
	return nil
}
