// Package rle implements the hybrid RLE/Bit-Packed encoding employed in
// repetition and definition levels, dictionary indexed data pages, and
// boolean values in the PLAIN encoding.
//
// https://github.com/apache/parquet-format/blob/master/Encodings.md#run-length-encoding--bit-packing-hybrid-rle--3
package rle

import (
	"encoding/binary"
	"fmt"
	"io"
	"unsafe"

	"github.com/segmentio/parquet-go/encoding"
	"github.com/segmentio/parquet-go/format"
	"github.com/segmentio/parquet-go/internal/bitpack"
	"github.com/segmentio/parquet-go/internal/bytealg"
	"github.com/segmentio/parquet-go/internal/unsafecast"
)

const (
	// This limit is intended to prevent unbounded memory allocations when
	// decoding runs.
	//
	// We use a generous limit which allows for over 16 million values per page
	// if there is only one run to encode the repetition or definition levels
	// (this should be uncommon).
	maxSupportedValueCount = 16 * 1024 * 1024
)

type Encoding struct {
	encoding.NotSupported
	BitWidth int
}

func (e *Encoding) String() string {
	return "RLE"
}

func (e *Encoding) Encoding() format.Encoding {
	return format.RLE
}

func (e *Encoding) EncodeLevels(dst, src []byte) ([]byte, error) {
	dst, err := encodeBytes(dst[:0], src, uint(e.BitWidth))
	return dst, e.wrap(err)
}

func (e *Encoding) EncodeBoolean(dst, src []byte) ([]byte, error) {
	// In the case of encoding a boolean values, the 4 bytes length of the
	// output is expected by the parquet format. We add the bytes as placeholder
	// before appending the encoded data.
	dst = append(dst[:0], 0, 0, 0, 0)
	dst, err := encodeBits(dst, src)
	binary.LittleEndian.PutUint32(dst, uint32(len(dst))-4)
	return dst, e.wrap(err)
}

func (e *Encoding) EncodeInt32(dst, src []byte) ([]byte, error) {
	if (len(src) % 4) != 0 {
		return dst[:0], encoding.ErrEncodeInvalidInputSize(e, "INT32", len(src))
	}
	dst, err := encodeInt32(dst[:0], unsafecast.BytesToInt32(src), uint(e.BitWidth))
	return dst, e.wrap(err)
}

func (e *Encoding) DecodeLevels(dst, src []byte) ([]byte, error) {
	dst, err := decodeBytes(dst[:0], src, uint(e.BitWidth))
	return dst, e.wrap(err)
}

func (e *Encoding) DecodeBoolean(dst, src []byte) ([]byte, error) {
	if len(src) == 4 {
		return dst[:0], nil
	}
	if len(src) < 4 {
		return dst[:0], fmt.Errorf("input shorter than 4 bytes: %w", io.ErrUnexpectedEOF)
	}
	n := int(binary.LittleEndian.Uint32(src))
	src = src[4:]
	if n > len(src) {
		return dst[:0], fmt.Errorf("input shorter than length prefix: %d < %d: %w", len(src), n, io.ErrUnexpectedEOF)
	}
	dst, err := decodeBits(dst[:0], src[:n])
	return dst, e.wrap(err)
}

func (e *Encoding) DecodeInt32(dst, src []byte) ([]byte, error) {
	dst, err := decodeInt32(dst[:0], src, uint(e.BitWidth))
	return dst, e.wrap(err)
}

func (e *Encoding) wrap(err error) error {
	if err != nil {
		err = encoding.Error(e, err)
	}
	return err
}

func encodeBits(dst, src []byte) ([]byte, error) {
	if len(src) == 0 || isZero(src) || isOnes(src) {
		dst = appendUvarint(dst, uint64(8*len(src))<<1)
		if len(src) > 0 {
			dst = append(dst, src[0])
		}
		return dst, nil
	}

	for i := 0; i < len(src); {
		j := i + 1

		// Look for contiguous sections of 8 bits, all zeros or ones; these
		// are run-length encoded as it only takes 2 or 3 bytes to store these
		// sequences.
		if src[i] == 0 || src[i] == 0xFF {
			for j < len(src) && src[i] == src[j] {
				j++
			}

			if n := j - i; n > 1 {
				dst = appendRunLengthBits(dst, 8*n, src[i])
				i = j
				continue
			}
		}

		// Sequences of bits that are neither all zeroes or ones are bit-packed,
		// which is a simple copy of the input to the output preceded with the
		// bit-pack header.
		for j < len(src) && (src[j-1] != src[j] || (src[j] != 0 && src[j] == 0xFF)) {
			j++
		}

		if (j-i) > 1 && j < len(src) {
			j--
		}

		dst = appendBitPackedBits(dst, src[i:j])
		i = j
	}
	return dst, nil
}

func encodeBytes(dst, src []byte, bitWidth uint) ([]byte, error) {
	if bitWidth > 8 {
		return dst, errEncodeInvalidBitWidth("INT8", bitWidth)
	}
	if bitWidth == 0 {
		if !isZero(src) {
			return dst, errEncodeInvalidBitWidth("INT8", bitWidth)
		}
		return appendUvarint(dst, uint64(len(src))<<1), nil
	}

	if len(src) >= 8 {
		words := unsafe.Slice((*uint64)(unsafe.Pointer(&src[0])), len(src)/8)

		for i := 0; i < len(words); {
			j := i
			pattern := broadcast8x1(words[i])

			for j < len(words) && words[j] == pattern {
				j++
			}

			if i < j {
				dst = appendRunLengthBytes(dst, 8*(j-i), byte(pattern))
			} else {
				j++

				for j < len(words) && words[j] != broadcast8x1(words[j-1]) {
					j++
				}

				dst = appendBitPackedBytes(dst, words[i:j], bitWidth)
			}

			i = j
		}
	}

	for i := (len(src) / 8) * 8; i < len(src); {
		j := i + 1

		for j < len(src) && src[i] == src[j] {
			j++
		}

		dst = appendRunLengthBytes(dst, j-i, src[i])
		i = j
	}

	return dst, nil
}

func encodeInt32(dst []byte, src []int32, bitWidth uint) ([]byte, error) {
	if bitWidth > 32 {
		return dst, errEncodeInvalidBitWidth("INT32", bitWidth)
	}
	if bitWidth == 0 {
		if !isZero(unsafecast.Int32ToBytes(src)) {
			return dst, errEncodeInvalidBitWidth("INT32", bitWidth)
		}
		return appendUvarint(dst, uint64(len(src))<<1), nil
	}

	if len(src) >= 8 {
		words := unsafe.Slice((*[8]int32)(unsafe.Pointer(&src[0])), len(src)/8)

		for i := 0; i < len(words); {
			j := i
			pattern := broadcast8x4(words[i][0])

			for j < len(words) && words[j] == pattern {
				j++
			}

			if i < j {
				dst = appendRunLengthInt32(dst, 8*(j-i), pattern[0], bitWidth)
			} else {
				j += 1
				j += encodeInt32IndexEqual8Contiguous(words[j:])
				dst = appendBitPackedInt32(dst, words[i:j], bitWidth)
			}

			i = j
		}
	}

	for i := (len(src) / 8) * 8; i < len(src); {
		j := i + 1

		for j < len(src) && src[i] == src[j] {
			j++
		}

		dst = appendRunLengthInt32(dst, j-i, src[i], bitWidth)
		i = j
	}

	return dst, nil
}

func decodeBits(dst, src []byte) ([]byte, error) {
	for i := 0; i < len(src); {
		u, n := binary.Uvarint(src[i:])
		if n == 0 {
			return dst, fmt.Errorf("decoding run-length block header: %w", io.ErrUnexpectedEOF)
		}
		if n < 0 {
			return dst, fmt.Errorf("overflow after decoding %d/%d bytes of run-length block header", -n+i, len(src))
		}
		i += n

		count, bitpacked := uint(u>>1), (u&1) != 0
		if count > maxSupportedValueCount {
			return dst, fmt.Errorf("decoded run-length block cannot have more than %d values", maxSupportedValueCount)
		}
		if bitpacked {
			n := int(count)
			j := i + n

			if j > len(src) {
				return dst, fmt.Errorf("decoding bit-packed block of %d values: %w", n, io.ErrUnexpectedEOF)
			}

			dst = append(dst, src[i:j]...)
			i = j
		} else {
			word := byte(0)
			if i < len(src) {
				word = src[i]
				i++
			}

			offset := len(dst)
			length := bitpack.ByteCount(count)
			dst = resize(dst, offset+length)
			bytealg.Broadcast(dst[offset:], word)
		}
	}
	return dst, nil
}

func decodeBytes(dst, src []byte, bitWidth uint) ([]byte, error) {
	if bitWidth > 8 {
		return dst, errDecodeInvalidBitWidth("INT8", bitWidth)
	}

	for i := 0; i < len(src); {
		u, n := binary.Uvarint(src[i:])
		if n == 0 {
			return dst, fmt.Errorf("decoding run-length block header: %w", io.ErrUnexpectedEOF)
		}
		if n < 0 {
			return dst, fmt.Errorf("overflow after decoding %d/%d bytes of run-length block header", -n+i, len(src))
		}
		i += n

		count, bitpacked := uint(u>>1), (u&1) != 0
		if count > maxSupportedValueCount {
			return dst, fmt.Errorf("decoded run-length block cannot have more than %d values", maxSupportedValueCount)
		}
		if bitpacked {
			count *= 8
			j := i + bitpack.ByteCount(count*bitWidth)

			if j > len(src) {
				return dst, fmt.Errorf("decoding bit-packed block of %d values: %w", 8*count, io.ErrUnexpectedEOF)
			}

			offset := len(dst)
			length := int(count)
			dst = resize(dst, offset+length)
			decodeBytesBitpack(dst[offset:], src[i:j], count, bitWidth)

			i = j
		} else {
			if bitWidth != 0 && (i+1) > len(src) {
				return dst, fmt.Errorf("decoding run-length block of %d values: %w", count, io.ErrUnexpectedEOF)
			}

			word := byte(0)
			if bitWidth != 0 {
				word = src[i]
				i++
			}

			offset := len(dst)
			length := int(count)
			dst = resize(dst, offset+length)
			bytealg.Broadcast(dst[offset:], word)
		}
	}

	return dst, nil
}

func decodeInt32(dst, src []byte, bitWidth uint) ([]byte, error) {
	if bitWidth > 32 {
		return dst, errDecodeInvalidBitWidth("INT32", bitWidth)
	}

	buf := make([]byte, 2*bitpack.Padding)

	for i := 0; i < len(src); {
		u, n := binary.Uvarint(src[i:])
		if n == 0 {
			return dst, fmt.Errorf("decoding run-length block header: %w", io.ErrUnexpectedEOF)
		}
		if n < 0 {
			return dst, fmt.Errorf("overflow after decoding %d/%d bytes of run-length block header", -n+i, len(src))
		}
		i += n

		count, bitpacked := uint(u>>1), (u&1) != 0
		if count > maxSupportedValueCount {
			return dst, fmt.Errorf("decoded run-length block cannot have more than %d values", maxSupportedValueCount)
		}
		if bitpacked {
			offset := len(dst)
			length := int(count * bitWidth)
			dst = resize(dst, offset+4*8*int(count))

			// The bitpack.UnpackInt32 function requires the input to be padded
			// or the function panics. If there is enough room in the input
			// buffer we can use it, otherwise we have to copy it to a larger
			// location (which should rarely happen).
			in := src[i : i+length]
			if (cap(in) - len(in)) >= bitpack.Padding {
				in = in[:cap(in)]
			} else {
				buf = resize(buf, len(in)+bitpack.Padding)
				copy(buf, in)
				in = buf
			}

			out := unsafecast.BytesToInt32(dst[offset:])
			bitpack.UnpackInt32(out, in, bitWidth)
			i += length
		} else {
			j := i + bitpack.ByteCount(bitWidth)

			if j > len(src) {
				return dst, fmt.Errorf("decoding run-length block of %d values: %w", count, io.ErrUnexpectedEOF)
			}

			bits := [4]byte{}
			copy(bits[:], src[i:j])
			dst = appendRepeat(dst, bits[:], count)
			i = j
		}
	}

	return dst, nil
}

func errEncodeInvalidBitWidth(typ string, bitWidth uint) error {
	return errInvalidBitWidth("encode", typ, bitWidth)
}

func errDecodeInvalidBitWidth(typ string, bitWidth uint) error {
	return errInvalidBitWidth("decode", typ, bitWidth)
}

func errInvalidBitWidth(op, typ string, bitWidth uint) error {
	return fmt.Errorf("cannot %s %s with invalid bit-width=%d", op, typ, bitWidth)
}

func appendRepeat(dst, pattern []byte, count uint) []byte {
	offset := len(dst)
	length := int(count) * len(pattern)
	dst = resize(dst, offset+length)
	i := offset + copy(dst[offset:], pattern)
	for i < len(dst) {
		i += copy(dst[i:], dst[offset:i])
	}
	return dst
}

func appendUvarint(dst []byte, u uint64) []byte {
	var b [binary.MaxVarintLen64]byte
	var n = binary.PutUvarint(b[:], u)
	return append(dst, b[:n]...)
}

func appendRunLengthBits(dst []byte, count int, value byte) []byte {
	return appendRunLengthBytes(dst, count, value)
}

func appendBitPackedBits(dst []byte, words []byte) []byte {
	n := len(dst)
	dst = resize(dst, n+binary.MaxVarintLen64+len(words))
	n += binary.PutUvarint(dst[n:], uint64(len(words)<<1)|1)
	n += copy(dst[n:], words)
	return dst[:n]
}

func appendRunLengthBytes(dst []byte, count int, value byte) []byte {
	n := len(dst)
	dst = resize(dst, n+binary.MaxVarintLen64+1)
	n += binary.PutUvarint(dst[n:], uint64(count)<<1)
	dst[n] = value
	return dst[:n+1]
}

func appendBitPackedBytes(dst []byte, words []uint64, bitWidth uint) []byte {
	n := len(dst)
	dst = resize(dst, n+binary.MaxVarintLen64+(len(words)*int(bitWidth))+8)
	n += binary.PutUvarint(dst[n:], uint64(len(words)<<1)|1)
	n += encodeBytesBitpack(dst[n:], words, bitWidth)
	return dst[:n]
}

func appendRunLengthInt32(dst []byte, count int, value int32, bitWidth uint) []byte {
	n := len(dst)
	dst = resize(dst, n+binary.MaxVarintLen64+4)
	n += binary.PutUvarint(dst[n:], uint64(count)<<1)
	binary.LittleEndian.PutUint32(dst[n:], uint32(value))
	return dst[:n+bitpack.ByteCount(bitWidth)]
}

func appendBitPackedInt32(dst []byte, words [][8]int32, bitWidth uint) []byte {
	n := len(dst)
	dst = resize(dst, n+binary.MaxVarintLen64+(len(words)*int(bitWidth))+32)
	n += binary.PutUvarint(dst[n:], uint64(len(words))<<1|1)
	n += encodeInt32Bitpack(dst[n:], words, bitWidth)
	return dst[:n]
}

func broadcast8x1(v uint64) uint64 {
	return (v & 0xFF) * 0x0101010101010101
}

func broadcast8x4(v int32) [8]int32 {
	return [8]int32{v, v, v, v, v, v, v, v}
}

func isZero(data []byte) bool {
	return bytealg.Count(data, 0x00) == len(data)
}

func isOnes(data []byte) bool {
	return bytealg.Count(data, 0xFF) == len(data)
}

func resize(buf []byte, size int) []byte {
	if cap(buf) < size {
		return grow(buf, size)
	}
	return buf[:size]
}

func grow(buf []byte, size int) []byte {
	newCap := 2 * cap(buf)
	if newCap < size {
		newCap = size
	}
	newBuf := make([]byte, size, newCap)
	copy(newBuf, buf)
	return newBuf
}

func encodeInt32BitpackDefault(dst []byte, src [][8]int32, bitWidth uint) int {
	bits := unsafe.Slice((*int32)(unsafe.Pointer(&src[0])), len(src)*8)
	bitpack.PackInt32(dst, bits, bitWidth)
	return bitpack.ByteCount(uint(len(src)*8) * bitWidth)
}
