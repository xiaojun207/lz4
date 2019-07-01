package lz4

import (
	"encoding/binary"
	"math/bits"
)

// blockHash hashes 4 bytes into a value < winSize.
func blockHash(x uint32) uint32 {
	const hasher uint32 = 2654435761 // Knuth multiplicative hash.
	return x * hasher >> hashShift
}

// CompressBlockBound returns the maximum size of a given buffer of size n, when not compressible.
func CompressBlockBound(n int) int {
	return n + n/255 + 16
}

// UncompressBlock uncompresses the source buffer into the destination one,
// and returns the uncompressed size.
//
// The destination buffer must be sized appropriately.
//
// An error is returned if the source data is invalid or the destination buffer is too small.
func UncompressBlock(src, dst []byte) (int, error) {
	if len(src) == 0 {
		return 0, nil
	}
	if di := decodeBlock(dst, src); di >= 0 {
		return di, nil
	}
	return 0, ErrInvalidSourceShortBuffer
}

// CompressBlock compresses the source buffer into the destination one.
// This is the fast version of LZ4 compression and also the default one.
// The size of hashTable must be at least 64Kb.
//
// The size of the compressed data is returned. If it is 0 and no error, then the data is incompressible.
//
// An error is returned if the destination buffer is too small.
func CompressBlock(src, dst []byte, hashTable []int) (di int, err error) {
	defer recoverBlock(&err)

	sn, dn := len(src)-mfLimit, len(dst)
	if sn <= 0 || dn == 0 {
		return 0, nil
	}
	var si int

	// Fast scan strategy: the hash table only stores the last 4 bytes sequences.

	anchor := si // Position of the current literals.

	for si < sn {
		// Hash the next 4 bytes (sequence)...
		match := binary.LittleEndian.Uint32(src[si:])
		h := blockHash(match)

		ref := hashTable[h]
		hashTable[h] = si
		if ref >= sn { // Invalid reference (dirty hashtable).
			si++
			continue
		}
		offset := si - ref
		if offset <= 0 || offset >= winSize || // Out of window.
			match != binary.LittleEndian.Uint32(src[ref:]) { // Hash collision on different matches.
			si++
			continue
		}

		// Match found.
		// acc = accInit
		lLen := si - anchor // Literal length.

		// Encode match length part 1.
		si += minMatch
		mLen := si // Match length has minMatch already.
		// Find the longest match, first looking by batches of 8 bytes.
		for si < sn {
			x := binary.LittleEndian.Uint64(src[si:]) ^ binary.LittleEndian.Uint64(src[si-offset:])
			if x == 0 {
				si += 8
			} else {
				// Stop is first non-zero byte.
				si += bits.TrailingZeros64(x) >> 3
				break
			}
		}

		mLen = si - mLen
		if mLen < 0xF {
			dst[di] = byte(mLen)
		} else {
			dst[di] = 0xF
		}

		// Encode literals length.
		if lLen < 0xF {
			dst[di] |= byte(lLen << 4)
		} else {
			dst[di] |= 0xF0
			di++
			l := lLen - 0xF
			for ; l >= 0xFF; l -= 0xFF {
				dst[di] = 0xFF
				di++
			}
			dst[di] = byte(l)
		}
		di++

		// Literals.
		copy(dst[di:di+lLen], src[anchor:anchor+lLen])
		di += lLen + 2
		anchor = si

		// Encode offset.
		_ = dst[di] // Bound check elimination.
		dst[di-2], dst[di-1] = byte(offset), byte(offset>>8)

		// Encode match length part 2.
		if mLen >= 0xF {
			for mLen -= 0xF; mLen >= 0xFF; mLen -= 0xFF {
				dst[di] = 0xFF
				di++
			}
			dst[di] = byte(mLen)
			di++
		}
	}

	if anchor == 0 {
		// Incompressible.
		return 0, nil
	}

	// Last literals.
	lLen := len(src) - anchor
	if lLen < 0xF {
		dst[di] = byte(lLen << 4)
	} else {
		dst[di] = 0xF0
		di++
		for lLen -= 0xF; lLen >= 0xFF; lLen -= 0xFF {
			dst[di] = 0xFF
			di++
		}
		dst[di] = byte(lLen)
	}
	di++

	// Write the last literals.
	if di >= anchor {
		// Incompressible.
		return 0, nil
	}
	di += copy(dst[di:di+len(src)-anchor], src[anchor:])
	return di, nil
}

// CompressBlockHC compresses the source buffer src into the destination dst
// with max search depth (use 0 or negative value for no max).
//
// CompressBlockHC compression ratio is better than CompressBlock but it is also slower.
//
// The size of the compressed data is returned. If it is 0 and no error, then the data is not compressible.
//
// An error is returned if the destination buffer is too small.
func CompressBlockHC(src, dst []byte, depth int) (di int, err error) {
	defer recoverBlock(&err)

	sn, dn := len(src)-mfLimit, len(dst)
	if sn <= 0 || dn == 0 {
		return 0, nil
	}
	var si int

	// hashTable: stores the last position found for a given hash
	// chainTable: stores previous positions for a given hash
	var hashTable, chainTable [winSize]int

	if depth <= 0 {
		depth = winSize
	}

	anchor := si
	for si < sn {
		// Hash the next 4 bytes (sequence).
		match := binary.LittleEndian.Uint32(src[si:])
		h := blockHash(match)

		// Follow the chain until out of window and give the longest match.
		mLen := 0
		offset := 0
		for next, try := hashTable[h], depth; try > 0 && next > 0 && si-next < winSize; next = chainTable[next&winMask] {
			// The first (mLen==0) or next byte (mLen>=minMatch) at current match length
			// must match to improve on the match length.
			if src[next+mLen] != src[si+mLen] {
				continue
			}
			ml := 0
			// Compare the current position with a previous with the same hash.
			for ml < sn-si {
				x := binary.LittleEndian.Uint64(src[next+ml:]) ^ binary.LittleEndian.Uint64(src[si+ml:])
				if x == 0 {
					ml += 8
				} else {
					// Stop is first non-zero byte.
					ml += bits.TrailingZeros64(x) >> 3
					break
				}
			}
			if ml < minMatch || ml <= mLen {
				// Match too small (<minMath) or smaller than the current match.
				continue
			}
			// Found a longer match, keep its position and length.
			mLen = ml
			offset = si - next
			// Try another previous position with the same hash.
			try--
		}
		chainTable[si&winMask] = hashTable[h]
		hashTable[h] = si

		// No match found.
		if mLen == 0 {
			si++
			continue
		}

		// Match found.
		// Update hash/chain tables with overlapping bytes:
		// si already hashed, add everything from si+1 up to the match length.
		winStart := si + 1
		if ws := si + mLen - winSize; ws > winStart {
			winStart = ws
		}
		for si, ml := winStart, si+mLen; si < ml; {
			match >>= 8
			match |= uint32(src[si+3]) << 24
			h := blockHash(match)
			chainTable[si&winMask] = hashTable[h]
			hashTable[h] = si
			si++
		}

		lLen := si - anchor
		si += mLen
		mLen -= minMatch // Match length does not include minMatch.

		if mLen < 0xF {
			dst[di] = byte(mLen)
		} else {
			dst[di] = 0xF
		}

		// Encode literals length.
		if lLen < 0xF {
			dst[di] |= byte(lLen << 4)
		} else {
			dst[di] |= 0xF0
			di++
			l := lLen - 0xF
			for ; l >= 0xFF; l -= 0xFF {
				dst[di] = 0xFF
				di++
			}
			dst[di] = byte(l)
		}
		di++

		// Literals.
		copy(dst[di:di+lLen], src[anchor:anchor+lLen])
		di += lLen
		anchor = si

		// Encode offset.
		di += 2
		dst[di-2], dst[di-1] = byte(offset), byte(offset>>8)

		// Encode match length part 2.
		if mLen >= 0xF {
			for mLen -= 0xF; mLen >= 0xFF; mLen -= 0xFF {
				dst[di] = 0xFF
				di++
			}
			dst[di] = byte(mLen)
			di++
		}
	}

	if anchor == 0 {
		// Incompressible.
		return 0, nil
	}

	// Last literals.
	lLen := len(src) - anchor
	if lLen < 0xF {
		dst[di] = byte(lLen << 4)
	} else {
		dst[di] = 0xF0
		di++
		lLen -= 0xF
		for ; lLen >= 0xFF; lLen -= 0xFF {
			dst[di] = 0xFF
			di++
		}
		dst[di] = byte(lLen)
	}
	di++

	// Write the last literals.
	if di >= anchor {
		// Incompressible.
		return 0, nil
	}
	di += copy(dst[di:di+len(src)-anchor], src[anchor:])
	return di, nil
}
