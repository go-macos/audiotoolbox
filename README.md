# go-macos/audiotoolbox

Decode compressed audio and play PCM on macOS from pure Go — `CGO_ENABLED=0`,
via [purego](https://github.com/ebitengine/purego). You bring the coded packets;
this brings the decoder and the output.

```go
cfg, _ := reader.TrackConfig(track.ID)        // go-avkit/avkit/container
codec, _ := audiotoolbox.CodecFor(cfg.Codec)  // "mp4a" -> AAC

dcfg := audiotoolbox.Config{
        Codec: codec, SampleRate: cfg.SampleRate, Channels: cfg.Channels,
        AudioObjectType: cfg.AudioObjectType, CodecConfig: cfg.CodecConfig,
}
dec, _ := audiotoolbox.NewDecoder(dcfg)
p, _ := audiotoolbox.NewPlayer(audiotoolbox.PlayerConfigFor(dcfg))
defer dec.Close()
defer p.Close()
p.Start()

for _, pkt := range packets {
        buf, _ := dec.Decode(audiotoolbox.Packet{Data: pkt.Data})
        p.Write(buf.PCM)        // blocks on the queue, which paces the loop
        video.SyncTo(p.Played()) // the master clock
}
tail, _ := dec.Flush()
p.Write(tail.PCM)
p.Drain()
```

## Why this exists

It is the audio half of the hole
[`go-macos/videotoolbox`](https://github.com/go-macos/videotoolbox) fills for
pictures. AVFoundation plays an MP4 end to end and is the right tool when it
works; **it will not demux Matroska at all**. So a player that wants sound out
of an MKV has to demux the file itself and hand the coded packets to a decoder
directly.

[`go-avkit/avkit/container`](https://github.com/go-avkit/avkit) does the
demuxing — MP4, Matroska/WebM and MPEG-TS, in pure Go — and already reports
audio tracks, their codec, channel count, sample rate and coded packets. This
package is the other half: `AudioConverter` to turn those packets into PCM, and
`AudioQueue` to put the PCM on the system output.

```
$ auprobe -for 5s "The.Addams.Family.2.2021.mkv"
  matroska, 5563.456 s, 3 tracks, 1 audio (read in 1.133s, demuxed in 435ms)
  audio track 2: ac-3 6 ch 48000 Hz, 5563.456 s, 173858 packets
  decoder: ac-3 -> 6 ch s16
  decoded 240864 frames = 5.018s of audio, in 18ms (276x real time)
  the output played 5.011s of the 5.018s written
```

## What it covers

| | |
|---|---|
| Codecs in | AAC (`mp4a`, LC / HE / HE v2), AC-3 (`ac-3`), Enhanced AC-3 (`ec-3`), [Opus](#opus-and-what-has-not-been-proved) |
| Input | one coded packet at a time — an AAC access unit, an AC-3 sync frame |
| Output | interleaved PCM, `Int16` or `Float32`, at the coded rate |
| Downmix | `Config.OutputChannels` asks the decoder for fewer, so 5.1 reaches two speakers |
| Playback | `AudioQueue` on the system output, with a real clock |
| Files | `WAV` writes the PCM where it can be looked at |
| Platforms | darwin arm64 and amd64; every other platform builds and answers `ErrUnsupported` |

Not here: encoding, resampling, mixing, and any format whose packets this
does not name — a codec `CodecFor` does not know is refused up front, with a
reason, rather than decoded into noise.

## Audio is the master clock

`Player.Played` is how much audio has left the device, read from the
`AudioQueue`'s own timeline. It is **not** a count of what was written: the two
differ by whatever is still queued — a quarter of a second by default — and that
difference is the whole of the drift a naive player accumulates. The ear hears a
drift the eye does not, so video follows this and never the other way round.

Two things were measured and are worth knowing.

**`AudioQueueStop` does not wait.** Asked to stop gently (`inImmediate` false)
it returns in *thirty microseconds* and lets the queue finish in its own time,
so a `Drain` that called it and returned would cut the tail off exactly as an
immediate stop does. This one waits for the queue to hand every buffer back —
which is when the device has actually finished with them — and only then stops.
Before that fix a three-second decode reported 2.938 s played; after it, 3.001 s.

**The timeline is the device's.** A device that runs out of audio does not stop:
it plays silence and the clock goes on advancing. That is right for a master
clock, which must not stall, and `Player.Queued` is what says whether there is
anything left to hear.

## The format is stated, never sniffed

A `Config` says what the packets hold, the way `videotoolbox` takes its
bitstream form as stated and for the same reason: a demuxer already knows, and a
decoder that guesses is a decoder that is occasionally, silently wrong.

AAC in an MP4 or a Matroska file carries no in-band configuration — no ADTS
header, nothing before the first packet — so the decoder is set up from an
`AudioSpecificConfig`. When the demuxer states one it is used as it stands; when
it does not, one is built from the sample rate, the channel count and the AAC
profile the demuxer *did* state. `Config.AudioSpecificConfig` is exported so a
caller can check it against a hex dump.

The wrapping is not decoration. Offered the bare `AudioSpecificConfig`,
`AudioConverterSetProperty` answers `kAudioCodecBadPropertySizeError` (`'!dat'`)
and the configuration is **not applied**; wrapped in an MPEG-4 `ES_Descriptor`,
the same two bytes are taken. A cookie that is refused is reported through
`Decoder.CodecConfigRefused` rather than swallowed, because the alternative is a
caller who believes the configuration took effect and finds out from a 5.1 track
that comes back as noise.

## Two callbacks, and what they cost

`purego.NewCallback` cannot describe a struct passed **by value** as a callback
argument. Neither of this package's two callbacks takes one — five pointers for
the converter's input proc, three for the queue's output callback — so both are
expressible, and every C structure here is passed by pointer.

purego never frees a callback and allows a bounded number of them, so one per
decoder would be a leak with a hard ceiling. There are exactly two for the
process; each carries an **integer key** and looks its owner up in a registry.
An integer, not a Go pointer: handing C a pointer into the Go heap and expecting
it back later is what the cgo pointer rules forbid. The coded packet and the PCM
the converter reads and writes are `malloc`ed for the same reason.

### The input proc must say "not now", not "never"

Apple's documentation describes reporting **zero packets with `noErr`** when the
input proc has nothing to hand over. Measured, that tells the converter the
input is *over*: it decodes what it has, and every later `FillComplexBuffer`
returns no frames at all. On a plain AAC MP4 the first packet decoded to 1024
frames and the next 12 090 decoded to **nothing**, silently, with `noErr`
throughout. A distinctive status instead means "not now": `FillComplexBuffer`
hands it straight back, keeps the frames it already wrote, and the next call
works. End of stream is still reachable, and `Decoder.Flush` is what reaches it.

## Buffers alias

`Decoder.Decode` returns PCM that lives in a scratch buffer the decoder owns,
and the next call overwrites it. A player copies the samples into its own queue
anyway, and allocating a fresh slice for forty-seven packets a second buys
nothing. `Buffer.Clone` copies for a caller that wants to keep one.

## Measured

M4 Max, macOS 26.6.2, Go 1.26.4, `CGO_ENABLED=0`, decoding through the public
API of this package and of `go-avkit/avkit/container`:

| file | | packets | audio | rate |
|---|---|---|---|---|
| 4-minute presentation | MP4, AAC-LC 2.0 48 kHz | 12 091 / 12 091 | 4 m 17.941 s | 192 ms, **1345×** real time |
| feature film, 1 h 32 | **MKV**, AC-3 5.1 48 kHz, 2.0 GB | 173 858 / 173 858 | 1 h 32 m 43.45 s | 17.9 s, **310×** real time |

The film's decoded length comes out **6 ms** from the 5563.456 s the container
states, over an hour and a half.

### Against Apple's own decoder

A decoder nobody can check is a decoder nobody should trust, and a report of
"it plays" is worth nothing to a reader who cannot hear it. So whole tracks were
decoded here and by `afconvert`, and the samples compared:

| | samples compared | identical | largest difference |
|---|---|---|---|
| AC-3 5.1, **MKV** | 27 646 272 | **100.0000 %** | 0 |
| AAC-LC 2.0, MP4 | 24 760 320 | 99.986 % – 100 % | **1** (of 32 767) |
| AAC-LC 1.0, MP4 | 24 000 | **100.0000 %** | 0 |

The AC-3 track is bit-for-bit Apple's output, on every run.

The AAC figure is a range on purpose, and the reason is worth writing down:
**AudioToolbox's own AAC decoder is not bit-reproducible between processes.**
Three runs of the same decode against the same `afconvert` output gave
99.9858 %, 100.0000 % and 99.9987 %; two runs compared against *each other* vary
the same way. Every difference, in every direction, is exactly one
least-significant bit of the final rounding to 16 bits. So the honest claim is
not "identical" but "within one LSB of Apple on every one of 24.7 million
samples" — and anyone quoting a single 100 % run of an AAC decode has measured
it once.

Ours also runs 1024 frames — one AAC frame — longer than `afconvert`'s, because
`afconvert` trims the encoder's last frame to the duration the container states
and this hands back everything the decoder produced. The mono comparison
likewise allows for the 2112 frames of encoder priming, and is exact after it.

`afconvert` cannot open the MKV at all; what it was given was the AC-3
elementary stream this fleet's demuxer produced, which is the honest comparison.

### Opus, and what has not been proved

macOS 26.6.2 builds an Opus converter, and the framing is confirmed with no Opus
media anywhere: RFC 6716 says a packet that is nothing but its TOC byte carries
one frame of length zero, which is legal, so `0xf8` — CELT-only, fullband,
20 ms — must decode to exactly 960 frames at 48 kHz, and does, mono and stereo,
after the pre-skip the `OpusHead` states. Rate, framing and channel layout all
confirmed at once.

What that does **not** prove is that a real Opus bitstream decodes to the right
samples. Nothing on a Mac encodes Opus, so there was no file to check against —
`TestLiveDecode` is where it would run, and it has not been run on Opus. Treat
Opus here as wired up and framed correctly, not as measured.

## `cmd/auprobe`

```
auprobe movie.mp4                       # decode and play the whole track
auprobe -for 10s movie.mkv              # play ten seconds and stop
auprobe -wav out.wav -play=false f.mp4  # decode as fast as possible to a WAV
auprobe -stereo film.mkv                # downmix a 5.1 track to the speakers
auprobe -track 1 film.mkv               # the second audio track
auprobe -volume 0.3 movie.mp4
```

It counts what it did — packets submitted, frames decoded, seconds of audio
against the duration the container states — and with `-wav` it writes a file,
checks its size against `44 + frames × channels × 2`, and prints the command to
play it. It reads the file whole, because the demuxer takes a byte slice; that
is the tool's limit, not the package's.

## Testing

The portable layer is at **100 % statement coverage** behind platform seams, and
CI gates on those files rather than on the total: the purego bindings answer
`OSStatus` failure paths that cannot be reached without making a framework fail,
and a total-coverage gate would either be a lie or force media into the
repository.

The bindings are covered three ways. A **real** `AudioConverter` decodes a
**real** AAC bitstream on every CI run — 732 bytes of a 1 kHz sine at −6 dBFS,
committed to the test — and the decoded signal's frequency is then asserted with
a Goertzel filter rather than described: 1 kHz comes back at 16 230 of full
scale and 6.5 kHz at 5. A decoder that is silently wrong (wrong channel count,
wrong rate, samples read at the wrong width) cannot pass that, and it needs no
media on the runner. A real `AudioQueue` is opened, written to, clocked and
drained, and skips itself on a machine with no output device. A real Opus
converter decodes a bare TOC byte to the 960 frames RFC 6716 says it must. The
end-to-end decode is opt-in:

```
AUDIOTOOLBOX_TEST_FILE=/path/to/movie.mkv go test -race ./...
```

Licence: BSD-3-Clause.
