package maa

import (
	"fmt"
	"math"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

func readOutputBytes(name string, read func(buf []byte) uint64, nullSize uint64, limits BufferLimits) ([]byte, error) {
	return probeOutputBuffer(name, "bytes", 1, read, nullSize, limits)
}

func readOutputInt32s(name string, read func(buf []int32) uint64, nullSize uint64, limits BufferLimits) ([]int32, error) {
	return probeOutputBuffer(name, "int32 elements", 4, read, nullSize, limits)
}

func probeOutputBuffer[T any](
	name string,
	unit string,
	elemSize uint64,
	read func(buf []T) uint64,
	nullSize uint64,
	limits BufferLimits,
) ([]T, error) {
	if limits.Initial == 0 || limits.Initial > limits.Max {
		return nil, fmt.Errorf("maa: %s output buffer limits are invalid (Initial=%d, Max=%d): %w",
			name, limits.Initial, limits.Max, native.ErrProtocol)
	}

	capacity := limits.Initial
	attempts := 0
	for {
		attempts++
		if capacity > uint64(math.MaxInt)/elemSize {
			return nil, fmt.Errorf("maa: %s buffer capacity %d %s exceeds Go int range: %w",
				name, capacity, unit, native.ErrProtocol)
		}
		buf := make([]T, int(capacity))
		ret := read(buf)

		if ret == nullSize {
			if capacity >= limits.Max || uint64(attempts-1) >= uint64(limits.GrowthRetries) {
				retText := "NullSize"
				return nil, fmt.Errorf("maa: %s output buffer probe exhausted after %d attempt(s): "+
					"last capacity %d %s, Max %d, native return %s; "+
					"the NullSize reason is unknown and may be a native state/parameter rejection or insufficient capacity: %w",
					name, attempts, capacity, unit, limits.Max, retText, native.ErrBufferLimit)
			}
			if limits.Max/2 < capacity {
				capacity = limits.Max
			} else {
				capacity *= 2
			}
			continue
		}
		if ret > capacity {
			return nil, fmt.Errorf("maa: %s native return %d exceeds the actual buffer capacity %d %s: %w",
				name, ret, capacity, unit, native.ErrProtocol)
		}
		if ret > uint64(math.MaxInt) {
			return nil, fmt.Errorf("maa: %s native return %d exceeds Go int range: %w",
				name, ret, native.ErrProtocol)
		}
		out := make([]T, int(ret))
		copy(out, buf)
		return out, nil
	}
}
