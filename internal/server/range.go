package server

import (
	"math"
	"net/http"
	"strconv"
	"strings"
)

// byteRange is one accepted Range spec. For "start-end" and "start-" start is
// set; end is -1 for the open form. For "-suffix" suffix is true and
// suffixLen carries the tail length.
type byteRange struct {
	start     int64
	end       int64
	suffix    bool
	suffixLen int64
}

// parseRangeHeader interprets the Range header of an object read. A missing
// header returns (nil, true): the caller serves the whole object. A present
// header must be exactly one byte range in one of the three forms
// "bytes=start-end", "bytes=start-" or "bytes=-suffixLength". Anything else
// (another unit, multiple ranges, empty bounds, signs or non-digits, a zero
// suffix, stray whitespace or other formatting) returns ok=false so the
// request fails 400 instead of degrading to a full read.
//
// Bounds that parse but cannot be satisfied (start at/past the end, start
// greater than end) are returned for the caller to answer 416, since that
// judgement needs the selected version's length.
func parseRangeHeader(h http.Header) (*byteRange, bool) {
	values, present := h["Range"]
	if !present {
		return nil, true
	}
	if len(values) != 1 {
		return nil, false
	}
	value := values[0]
	const prefix = "bytes="
	if !strings.HasPrefix(value, prefix) || len(value) <= len(prefix) {
		return nil, false
	}
	spec := value[len(prefix):]
	// The whole spec must be decimal digits with exactly one hyphen. This
	// rejects whitespace, commas (multiple ranges), signs, hex and any other
	// character without allowing a malformed range to be ignored.
	if strings.Count(spec, "-") != 1 {
		return nil, false
	}
	left, right, _ := strings.Cut(spec, "-")
	switch {
	case left == "":
		// Suffix form "-suffixLength": non-empty digits, never zero.
		if !isDecimalDigits(right) {
			return nil, false
		}
		n := parseDecimalSaturating(right)
		if n == 0 {
			return nil, false
		}
		return &byteRange{suffix: true, suffixLen: n}, true
	case right == "":
		// Open-ended form "start-".
		if !isDecimalDigits(left) {
			return nil, false
		}
		return &byteRange{start: parseDecimalSaturating(left), end: -1}, true
	default:
		// Closed form "start-end". start > end is syntactically legal and is
		// answered 416 once the object length is known.
		if !isDecimalDigits(left) || !isDecimalDigits(right) {
			return nil, false
		}
		return &byteRange{start: parseDecimalSaturating(left), end: parseDecimalSaturating(right)}, true
	}
}

// parseDecimalSaturating converts a pre-validated non-empty digit string,
// clamping values that overflow int64 to math.MaxInt64. An oversized suffix
// therefore reads the whole object and an oversized start yields 416, rather
// than the request being rejected as malformed.
func parseDecimalSaturating(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		// isDecimalDigits guarantees the only possible error is overflow.
		return math.MaxInt64
	}
	return n
}

// isDecimalDigits reports whether s is a non-empty run of ASCII digits 0-9.
func isDecimalDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
