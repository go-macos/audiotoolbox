// Copyright (c) the go-macos/audiotoolbox authors.
// SPDX-License-Identifier: BSD-3-Clause

//go:build !darwin

package audiotoolbox

import "time"

// On every non-darwin platform the seams answer [ErrUnsupported] rather than
// being nil, so a consumer that cross-compiles for Linux or Windows gets a
// clean error from the same API instead of a nil-func panic.
func init() {
	newDecoder = func(Config) (decoderHandle, error) { return nil, ErrUnsupported }
	decodePacket = func(decoderHandle, Config, Packet) (Buffer, error) { return Buffer{}, ErrUnsupported }
	flushDecoder = func(decoderHandle, Config) (Buffer, error) { return Buffer{}, ErrUnsupported }
	closeDecoder = func(decoderHandle) error { return nil }
	codecConfigRefused = func(decoderHandle) error { return nil }

	newPlayer = func(PlayerConfig) (playerHandle, error) { return nil, ErrUnsupported }
	startPlayer = func(playerHandle) error { return ErrUnsupported }
	writePlayer = func(playerHandle, []byte) (int, error) { return 0, ErrUnsupported }
	playedPlayer = func(playerHandle) time.Duration { return 0 }
	drainPlayer = func(playerHandle) error { return ErrUnsupported }
	stopPlayer = func(playerHandle) error { return ErrUnsupported }
	closePlayer = func(playerHandle) error { return nil }
}
