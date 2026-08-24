// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

// Command auprobe demuxes a real file, decodes its audio through AudioToolbox
// and plays it on the system output.
//
// It is this package's dogfood and its end-to-end proof: an MKV or an MP4 goes
// in, sound comes out, and nothing in between is AVFoundation — which cannot
// read Matroska at all. Demuxing is github.com/go-avkit/avkit/container's;
// decoding and playing are this package's; everything it does goes through the
// public API of both.
//
// Because a report nobody can hear is not a result, it also counts: packets
// submitted, frames decoded, seconds of audio produced against the duration the
// container states, and — with -wav — a file the reader can open and inspect.
//
//	auprobe movie.mp4                       # decode and play the whole track
//	auprobe -for 10s movie.mkv              # play ten seconds and stop
//	auprobe -wav out.wav -play=false f.mp4  # decode as fast as possible to a WAV
//	auprobe -stereo film.mkv                # downmix a 5.1 track to the speakers
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/go-avkit/avkit/container"
	"github.com/go-macos/audiotoolbox"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "auprobe:", err)
		os.Exit(1)
	}
}

// options are what the flags say, gathered so run stays readable.
type options struct {
	track  int
	limit  time.Duration
	wav    string
	play   bool
	stereo bool
	volume float64
}

func run() error {
	var o options
	flag.IntVar(&o.track, "track", 0, "which audio track to decode, in file order")
	flag.DurationVar(&o.limit, "for", 0, "stop after this much audio (0 decodes the whole track)")
	flag.StringVar(&o.wav, "wav", "", "also write the decoded PCM to this WAV file")
	flag.BoolVar(&o.play, "play", true, "play the decoded audio on the system output")
	flag.BoolVar(&o.stereo, "stereo", false, "ask the decoder to downmix to 2 channels")
	flag.Float64Var(&o.volume, "volume", 1, "output volume, 0 to 1")
	flag.Parse()
	if flag.NArg() != 1 {
		return errors.New("usage: auprobe [flags] <file>")
	}
	path := flag.Arg(0)

	// The demuxer reads from a byte slice, so the file is read whole. That is
	// the honest limit of this tool, not of the package.
	start := time.Now()
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	read := time.Since(start)

	start = time.Now()
	r, err := container.NewReader(data)
	if err != nil {
		return fmt.Errorf("demux: %w", err)
	}
	demux := time.Since(start)

	file := r.File()
	audio := file.AudioTracks()
	fmt.Printf("%s\n", path)
	fmt.Printf("  %s, %.3f s, %d tracks, %d audio (read in %v, demuxed in %v)\n",
		file.Format, file.DurationSeconds(), len(file.Tracks), len(audio),
		read.Round(time.Millisecond), demux.Round(time.Millisecond))
	if len(audio) == 0 {
		return fmt.Errorf("%s holds no audio track at all — there is nothing here to play", path)
	}
	if o.track < 0 || o.track >= len(audio) {
		return fmt.Errorf("audio track %d asked for, and the file has %d", o.track, len(audio))
	}
	track := audio[o.track]
	cfg, err := r.TrackConfig(track.ID)
	if err != nil {
		return fmt.Errorf("track config: %w", err)
	}
	codec, ok := audiotoolbox.CodecFor(cfg.Codec)
	if !ok {
		return fmt.Errorf("%q is not a codec this decodes (aac, ac-3, ec-3 and opus are)", cfg.Codec)
	}
	packets, err := r.Samples(track.ID)
	if err != nil {
		return fmt.Errorf("samples: %w", err)
	}
	fmt.Printf("  audio track %d: %s %d ch %d Hz, %.3f s, %d packets, aot %d, %d bytes of codec config\n",
		track.ID, cfg.Codec, cfg.Channels, cfg.SampleRate, track.DurationSeconds(),
		len(packets), cfg.AudioObjectType, len(cfg.CodecConfig))

	dcfg := audiotoolbox.Config{
		Codec:           codec,
		SampleRate:      cfg.SampleRate,
		Channels:        cfg.Channels,
		AudioObjectType: cfg.AudioObjectType,
		CodecConfig:     cfg.CodecConfig,
	}
	if o.stereo {
		dcfg.OutputChannels = 2
	}
	dec, err := audiotoolbox.NewDecoder(dcfg)
	if err != nil {
		return err
	}
	defer dec.Close()
	cookie := dcfg.MagicCookie()
	fmt.Printf("  decoder: %v -> %d ch %v, magic cookie %d bytes %x\n",
		codec, dec.Config().OutputChannels, dec.Config().Output, len(cookie), cookie)
	if refused := dec.CodecConfigRefused(); refused != nil {
		fmt.Printf("  NOTE: AudioToolbox turned the codec configuration down: %v\n", refused)
	}

	return decode(dec, packets, track, o)
}

// wavHeaderBytes is what audiotoolbox.WAV writes before the samples: twelve for
// the RIFF header, twenty-four for a PCM fmt chunk, eight for the data header.
const wavHeaderBytes = 44

// sink is where the decoded PCM goes: the output, a WAV file, both, or neither.
type sink struct {
	player *audiotoolbox.Player
	wav    *audiotoolbox.WAV
	file   *os.File
	played time.Duration
}

func (s *sink) write(pcm []byte) error {
	if s.wav != nil {
		if _, err := s.wav.Write(pcm); err != nil {
			return err
		}
	}
	if s.player != nil {
		if _, err := s.player.Write(pcm); err != nil {
			return err
		}
	}
	return nil
}

func (s *sink) close() error {
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	if s.player != nil {
		keep(s.player.Drain())
		// The clock is read AFTER the drain and before the close: during
		// the loop it is behind by whatever is still in the queue, and
		// after the close there is no queue to ask.
		s.played = s.player.Played()
		keep(s.player.Close())
	}
	if s.wav != nil {
		keep(s.wav.Close())
		keep(s.file.Close())
	}
	return first
}

// open builds the sink the flags ask for.
func open(cfg audiotoolbox.Config, o options) (*sink, error) {
	s := &sink{}
	pc := audiotoolbox.PlayerConfigFor(cfg)
	pc.Volume = o.volume
	if o.wav != "" {
		f, err := os.Create(o.wav)
		if err != nil {
			return nil, err
		}
		w, err := audiotoolbox.NewWAV(f, pc)
		if err != nil {
			f.Close()
			return nil, err
		}
		s.file, s.wav = f, w
	}
	if o.play {
		p, err := audiotoolbox.NewPlayer(pc)
		if err != nil {
			s.close()
			return nil, err
		}
		if err := p.Start(); err != nil {
			p.Close()
			s.close()
			return nil, err
		}
		s.player = p
	}
	return s, nil
}

// decode runs the loop and reports what it measured.
func decode(dec *audiotoolbox.Decoder, packets []container.Sample, track container.Track, o options) error {
	s, err := open(dec.Config(), o)
	if err != nil {
		return err
	}
	var (
		start     = time.Now()
		submitted int
		bytesIn   int
		empty     int
	)
	for i, pkt := range packets {
		if o.limit > 0 && dec.Decoded() >= o.limit {
			break
		}
		buf, err := dec.Decode(audiotoolbox.Packet{
			Data: pkt.Data,
			PTS:  scale(int64(i)*int64(pkt.Duration), track.Timescale),
		})
		if err != nil {
			s.close()
			return fmt.Errorf("packet %d of %d: %w", i, len(packets), err)
		}
		submitted++
		bytesIn += len(pkt.Data)
		if buf.Frames == 0 {
			empty++
		}
		if err := s.write(buf.PCM); err != nil {
			s.close()
			return err
		}
	}
	// Whatever the converter is still holding is the tail of the track.
	tail, err := dec.Flush()
	if err != nil {
		s.close()
		return err
	}
	if err := s.write(tail.PCM); err != nil {
		s.close()
		return err
	}
	decoding := time.Since(start)
	queued := time.Duration(0)
	if s.player != nil {
		queued = s.player.Queued()
	}
	if err := s.close(); err != nil {
		return err
	}

	fmt.Printf("  submitted %d of %d packets (%d bytes), %d produced no frame\n",
		submitted, len(packets), bytesIn, empty)
	fmt.Printf("  decoded %d frames = %v of audio, in %v (%.0fx real time)\n",
		dec.Frames(), dec.Decoded().Round(time.Millisecond), decoding.Round(time.Millisecond),
		dec.Decoded().Seconds()/decoding.Seconds())
	fmt.Printf("  track states %.3f s; decoded %.3f s; difference %.3f s\n",
		track.DurationSeconds(), dec.Decoded().Seconds(),
		dec.Decoded().Seconds()-track.DurationSeconds())
	if s.player != nil {
		fmt.Printf("  the output played %v of the %v written; %v was still queued when the loop ended\n",
			s.played.Round(time.Millisecond),
			(time.Duration(s.player.Written()) * time.Second /
				time.Duration(dec.Config().SampleRate)).Round(time.Millisecond),
			queued.Round(time.Millisecond))
	}
	if s.wav != nil {
		ch, rate := dec.Config().OutputChannels, dec.Config().SampleRate
		bytesPerSample := dec.Config().Output.Size()
		fmt.Printf("  wrote %s: %d frames, %d ch, %d Hz, %v\n", o.wav, s.wav.Frames(),
			ch, rate,
			(time.Duration(s.wav.Frames()) * time.Second / time.Duration(rate)).Round(time.Millisecond))
		// The size is the one arithmetic a reader can repeat without ears:
		// a WAV header and then a sample per channel per frame.
		want := int64(wavHeaderBytes) + s.wav.Frames()*int64(ch*bytesPerSample)
		got := int64(-1)
		if st, err := os.Stat(o.wav); err == nil {
			got = st.Size()
		}
		verdict := "MISMATCH"
		if got == want {
			verdict = "as expected"
		}
		fmt.Printf("    %d bytes on disk, %d expected (44 + %d frames x %d ch x %d): %s\n",
			got, want, s.wav.Frames(), ch, bytesPerSample, verdict)
		fmt.Printf("    listen with: afplay %s\n", o.wav)
	}
	if dec.Frames() == 0 {
		return errors.New("every packet went in and not one frame came out")
	}
	return nil
}

// scale turns a count of timescale units into a duration.
func scale(units int64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	return time.Duration(float64(units) / float64(timescale) * float64(time.Second))
}
