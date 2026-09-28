package sourcemap

import "fmt"

// Base64 VLQ: the encoding the mapping table is written in.
//
// Each value is a run of base64 digits. The low bit of the first digit is the
// sign, the next five bits are magnitude, and the high bit of every digit says
// whether another one follows. It is a compact encoding of small signed
// integers, and it is also the part of a source map most likely to be
// malformed — truncated by a broken upload, hand-edited, or chosen by whoever
// is testing what this endpoint does with rubbish.

const (
	continuationBit = 1 << 5
	valueMask       = continuationBit - 1
	// maxDigits bounds one value's digit run. Six digits carry 31 bits of
	// magnitude, which is everything an int32 can hold; a seventh could only
	// overflow. Without this ceiling a run of continuation digits is an
	// unbounded loop over attacker-supplied bytes that silently wraps.
	maxDigits = 6
	// maxInt32 is the ceiling an accumulated value is checked against before
	// it is shifted into place.
	maxInt32 = int32(1<<31 - 1)
)

// base64Values maps a byte to its base64 digit, with -1 for everything that is
// not one. A table rather than a strings.IndexByte because this runs once per
// digit of a mapping table with hundreds of thousands of segments, and the
// table is the difference between a lookup and a scan.
var base64Values = buildBase64Table()

func buildBase64Table() [256]int8 {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var table [256]int8
	for index := range table {
		table[index] = -1
	}
	for index := 0; index < len(alphabet); index++ {
		table[alphabet[index]] = int8(index) // #nosec G115 -- the alphabet is 64 entries.
	}
	return table
}

// decodeSegment reads one comma-separated segment into fields, and reports how
// many it held.
//
// It refuses a segment with more than five fields rather than ignoring the
// extras: the format defines exactly one, four or five, and a sixth means the
// document is not what it claims to be — at which point the four fields before
// it are not trustworthy either.
func decodeSegment(segment string, fields *[5]int32) (int, error) {
	count := 0
	for position := 0; position < len(segment); {
		if count == len(fields) {
			return 0, fmt.Errorf("%w: a segment has more than %d fields", ErrInvalid, len(fields))
		}
		value, width, err := decodeVLQ(segment[position:])
		if err != nil {
			return 0, err
		}
		fields[count] = value
		count++
		position += width
	}
	return count, nil
}

// decodeVLQ reads one value and returns how many bytes it consumed.
func decodeVLQ(encoded string) (value int32, width int, err error) {
	var (
		result   int32
		shift    uint
		consumed int
	)
	for consumed < len(encoded) {
		if consumed == maxDigits {
			return 0, 0, fmt.Errorf("%w: a VLQ value is longer than %d digits", ErrInvalid, maxDigits)
		}
		digit := base64Values[encoded[consumed]]
		if digit < 0 {
			return 0, 0, fmt.Errorf("%w: %q is not a base64 digit", ErrInvalid, encoded[consumed])
		}
		consumed++

		chunk := int32(digit) & valueMask
		// Checked before shifting, not after. Letting `chunk << shift`
		// overflow an int32 wraps silently, and a wrapped column is not a
		// missing mapping — it is a mapping that points somewhere real and
		// wrong, which nobody downstream can detect.
		if shift >= 32 || chunk > maxInt32>>shift {
			return 0, 0, fmt.Errorf("%w: a VLQ value does not fit in 32 bits", ErrInvalid)
		}
		result |= chunk << shift
		shift += 5

		if int32(digit)&continuationBit == 0 {
			negative := result&1 == 1
			result >>= 1
			if negative {
				// Negative zero decodes to zero. No encoder emits it, but
				// refusing the whole document over one is the wrong trade: a
				// zero delta is a zero delta whichever sign it carries, so
				// tolerating it cannot move a single position, while
				// rejecting costs every frame in the file (ADR 002's rule,
				// applied to a build artefact).
				result = -result
			}
			return result, consumed, nil
		}
	}
	return 0, 0, fmt.Errorf("%w: a VLQ value is truncated", ErrInvalid)
}
