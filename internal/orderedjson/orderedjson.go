// Package orderedjson parses and serializes JSON without losing object key order.
//
// The package is deliberately the only JSON boundary used by the Go rewrite:
// encoding/json's map representation cannot express the insertion order that
// JavaScript objects expose to JSON.stringify.
package orderedjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Object is a JSON object with an explicit property order. Keys contains the
// first-seen order of object members and M contains their last values. This
// mirrors JSON.parse's duplicate-member behavior: a duplicate replaces the
// value but does not move the property.
type Object struct {
	Keys []string
	M    map[string]Value
}

// Value is a JSON value. Numbers are json.Number rather than float64 so that
// parsing never loses the source number before the JS-compatible serializer
// gets a chance to format it.
type Value interface{}

// parser is a small JSON parser. Using a parser here instead of decoding into
// map[string]any is what makes the ordered object representation possible. It
// also lets us retain lone UTF-16 surrogates from \u escapes as WTF-8 bytes;
// those are emitted as the corresponding escape by Serialize, just as
// JSON.stringify does for a lone surrogate in a JavaScript string.
type parser struct {
	data []byte
	pos  int
}

// Parse parses one complete JSON text into an ordered Value.
func Parse(data []byte) (Value, error) {
	p := parser{data: data}
	p.skipWhitespace()
	if p.atEnd() {
		return nil, p.errorf("empty input")
	}

	value, err := p.parseValue()
	if err != nil {
		return nil, err
	}
	p.skipWhitespace()
	if !p.atEnd() {
		return nil, p.errorf("unexpected trailing data")
	}
	return value, nil
}

func (p *parser) parseValue() (Value, error) {
	if p.atEnd() {
		return nil, p.errorf("unexpected end of input")
	}

	switch p.data[p.pos] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		return p.parseString()
	case 't':
		if p.consumeLiteral("true") {
			return true, nil
		}
	case 'f':
		if p.consumeLiteral("false") {
			return false, nil
		}
	case 'n':
		if p.consumeLiteral("null") {
			return nil, nil
		}
	default:
		if p.data[p.pos] == '-' || (p.data[p.pos] >= '0' && p.data[p.pos] <= '9') {
			return p.parseNumber()
		}
	}

	return nil, p.errorf("invalid value")
}

func (p *parser) parseObject() (Value, error) {
	p.pos++ // {
	object := &Object{M: make(map[string]Value)}
	p.skipWhitespace()
	if p.consume('}') {
		return object, nil
	}

	for {
		p.skipWhitespace()
		if p.atEnd() || p.data[p.pos] != '"' {
			return nil, p.errorf("object key must be a string")
		}
		keyValue, err := p.parseString()
		if err != nil {
			return nil, err
		}
		key := keyValue

		p.skipWhitespace()
		if !p.consume(':') {
			return nil, p.errorf("missing ':' after object key")
		}
		p.skipWhitespace()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}

		if _, exists := object.M[key]; !exists {
			object.Keys = append(object.Keys, key)
		}
		object.M[key] = value

		p.skipWhitespace()
		if p.consume('}') {
			return object, nil
		}
		if !p.consume(',') {
			return nil, p.errorf("missing ',' or '}' after object member")
		}
		p.skipWhitespace()
		if p.atEnd() {
			return nil, p.errorf("unterminated object")
		}
	}
}

func (p *parser) parseArray() (Value, error) {
	p.pos++ // [
	array := make([]Value, 0)
	p.skipWhitespace()
	if p.consume(']') {
		return array, nil
	}

	for {
		p.skipWhitespace()
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		array = append(array, value)

		p.skipWhitespace()
		if p.consume(']') {
			return array, nil
		}
		if !p.consume(',') {
			return nil, p.errorf("missing ',' or ']' after array value")
		}
		p.skipWhitespace()
		if p.atEnd() {
			return nil, p.errorf("unterminated array")
		}
	}
}

func (p *parser) parseString() (string, error) {
	if !p.consume('"') {
		return "", p.errorf("string must begin with a quote")
	}

	out := make([]byte, 0, 16)
	for !p.atEnd() {
		c := p.data[p.pos]
		switch c {
		case '"':
			p.pos++
			return string(out), nil
		case '\\':
			p.pos++
			if p.atEnd() {
				return "", p.errorf("unterminated escape")
			}
			escape := p.data[p.pos]
			p.pos++
			switch escape {
			case '"', '\\', '/':
				out = append(out, escape)
			case 'b':
				out = append(out, '\b')
			case 'f':
				out = append(out, '\f')
			case 'n':
				out = append(out, '\n')
			case 'r':
				out = append(out, '\r')
			case 't':
				out = append(out, '\t')
			case 'u':
				unit, err := p.parseUnicodeEscape()
				if err != nil {
					return "", err
				}
				if isHighSurrogate(unit) {
					if low, ok := p.consumeLowSurrogateEscape(); ok {
						out = appendRune(out, combineSurrogates(unit, low))
					} else {
						out = appendWTF8Surrogate(out, unit)
					}
				} else {
					out = appendUTF16Unit(out, unit)
				}
			default:
				return "", p.errorf("invalid escape")
			}
		case 0x00:
			return "", p.errorf("control character in string")
		default:
			if c < 0x20 {
				return "", p.errorf("control character in string")
			}
			if c < utf8.RuneSelf {
				out = append(out, c)
				p.pos++
				continue
			}

			// JSON text is UTF-8. Keep the original UTF-8 bytes so that
			// non-ASCII characters are not needlessly rewritten.
			runeValue, size := utf8.DecodeRune(p.data[p.pos:])
			if runeValue == utf8.RuneError && size == 1 {
				return "", p.errorf("invalid UTF-8 in string")
			}
			out = append(out, p.data[p.pos:p.pos+size]...)
			p.pos += size
		}
	}

	return "", p.errorf("unterminated string")
}

func (p *parser) parseUnicodeEscape() (uint16, error) {
	if p.pos+4 > len(p.data) {
		return 0, p.errorf("short Unicode escape")
	}
	var unit uint16
	for i := 0; i < 4; i++ {
		digit, ok := hexDigit(p.data[p.pos+i])
		if !ok {
			return 0, p.errorf("invalid Unicode escape")
		}
		unit = unit<<4 | uint16(digit)
	}
	p.pos += 4
	return unit, nil
}

// consumeLowSurrogateEscape consumes exactly a second \uXXXX escape when it
// contains a low surrogate. A high surrogate followed by anything else is a
// valid lone JavaScript code unit, so it must not consume input on failure.
func (p *parser) consumeLowSurrogateEscape() (uint16, bool) {
	start := p.pos
	if start+6 > len(p.data) || p.data[start] != '\\' || p.data[start+1] != 'u' {
		return 0, false
	}
	var unit uint16
	for i := 0; i < 4; i++ {
		digit, ok := hexDigit(p.data[start+2+i])
		if !ok {
			return 0, false
		}
		unit = unit<<4 | uint16(digit)
	}
	if !isLowSurrogate(unit) {
		return 0, false
	}
	p.pos += 6
	return unit, true
}

func (p *parser) parseNumber() (Value, error) {
	start := p.pos
	if p.consume('-') && p.atEnd() {
		return nil, p.errorf("incomplete number")
	}

	if p.consume('0') {
		if !p.atEnd() && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			return nil, p.errorf("leading zero in number")
		}
	} else {
		if p.atEnd() || p.data[p.pos] < '1' || p.data[p.pos] > '9' {
			return nil, p.errorf("invalid number")
		}
		for !p.atEnd() && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
	}

	if p.consume('.') {
		fractionStart := p.pos
		for !p.atEnd() && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
		if fractionStart == p.pos {
			return nil, p.errorf("fraction has no digits")
		}
	}

	if !p.atEnd() && (p.data[p.pos] == 'e' || p.data[p.pos] == 'E') {
		p.pos++
		if !p.atEnd() && (p.data[p.pos] == '+' || p.data[p.pos] == '-') {
			p.pos++
		}
		exponentStart := p.pos
		for !p.atEnd() && p.data[p.pos] >= '0' && p.data[p.pos] <= '9' {
			p.pos++
		}
		if exponentStart == p.pos {
			return nil, p.errorf("exponent has no digits")
		}
	}

	return json.Number(string(p.data[start:p.pos])), nil
}

func (p *parser) consumeLiteral(literal string) bool {
	if len(p.data)-p.pos < len(literal) || string(p.data[p.pos:p.pos+len(literal)]) != literal {
		return false
	}
	p.pos += len(literal)
	return true
}

func (p *parser) consume(want byte) bool {
	if !p.atEnd() && p.data[p.pos] == want {
		p.pos++
		return true
	}
	return false
}

func (p *parser) skipWhitespace() {
	for !p.atEnd() {
		switch p.data[p.pos] {
		case ' ', '\t', '\n', '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *parser) atEnd() bool { return p.pos >= len(p.data) }

func (p *parser) errorf(format string, args ...interface{}) error {
	return fmt.Errorf("orderedjson: %s at byte %d", fmt.Sprintf(format, args...), p.pos)
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func isHighSurrogate(unit uint16) bool { return unit >= 0xd800 && unit <= 0xdbff }
func isLowSurrogate(unit uint16) bool  { return unit >= 0xdc00 && unit <= 0xdfff }

func combineSurrogates(high, low uint16) rune {
	return rune(0x10000 + (uint32(high-0xd800) << 10) + uint32(low-0xdc00))
}

func appendUTF16Unit(dst []byte, unit uint16) []byte {
	if isHighSurrogate(unit) || isLowSurrogate(unit) {
		return appendWTF8Surrogate(dst, unit)
	}
	return appendRune(dst, rune(unit))
}

func appendWTF8Surrogate(dst []byte, unit uint16) []byte {
	// WTF-8 is only used internally for a lone UTF-16 code unit. It is
	// intentionally not accepted as raw input by parseString.
	return append(dst,
		0xe0|byte(unit>>12),
		0x80|byte((unit>>6)&0x3f),
		0x80|byte(unit&0x3f),
	)
}

func appendRune(dst []byte, value rune) []byte {
	var encoded [utf8.UTFMax]byte
	size := utf8.EncodeRune(encoded[:], value)
	return append(dst, encoded[:size]...)
}

// Serialize returns the compact-independent, two-space-indented JSON form
// produced by JSON.stringify(value, null, 2). It intentionally has no trailing
// newline; SerializeFile adds the newline used for on-disk JSON files.
func Serialize(value Value) []byte {
	var out bytes.Buffer
	writeValue(&out, value, 0)
	return out.Bytes()
}

// SerializeFile is Serialize followed by one newline.
func SerializeFile(value Value) []byte {
	out := Serialize(value)
	out = append(out, '\n')
	return out
}

func writeValue(out *bytes.Buffer, value Value, depth int) {
	switch typed := value.(type) {
	case nil:
		out.WriteString("null")
	case *Object:
		if typed == nil {
			out.WriteString("null")
			return
		}
		writeObject(out, typed, depth)
	case []Value:
		writeArray(out, typed, depth)
	case json.Number:
		out.WriteString(formatJSNumber(typed))
	case string:
		writeString(out, typed)
	case bool:
		if typed {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	default:
		// Value's documented set is closed. Treat an accidental foreign value
		// like JSON.stringify treats a non-JSON value in an array: null is
		// safer than emitting invalid JSON from a no-error API.
		out.WriteString("null")
	}
}

func writeObject(out *bytes.Buffer, object *Object, depth int) {
	keys := objectKeys(object)
	if len(keys) == 0 {
		out.WriteString("{}")
		return
	}

	out.WriteString("{\n")
	for i, key := range keys {
		writeIndent(out, depth+1)
		writeString(out, key)
		out.WriteString(": ")
		writeValue(out, object.M[key], depth+1)
		if i+1 != len(keys) {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	writeIndent(out, depth)
	out.WriteByte('}')
}

func writeArray(out *bytes.Buffer, array []Value, depth int) {
	if len(array) == 0 {
		out.WriteString("[]")
		return
	}

	out.WriteString("[\n")
	for i, item := range array {
		writeIndent(out, depth+1)
		writeValue(out, item, depth+1)
		if i+1 != len(array) {
			out.WriteString(",")
		}
		out.WriteString("\n")
	}
	writeIndent(out, depth)
	out.WriteByte(']')
}

func writeIndent(out *bytes.Buffer, depth int) {
	for i := 0; i < depth; i++ {
		out.WriteString("  ")
	}
}

func writeString(out *bytes.Buffer, value string) {
	out.WriteByte('"')
	for pos := 0; pos < len(value); {
		if unit, size, ok := decodeWTF8Surrogate(value[pos:]); ok {
			writeUnicodeEscape(out, rune(unit))
			pos += size
			continue
		}

		runeValue, size := utf8.DecodeRuneInString(value[pos:])
		if runeValue == utf8.RuneError && size == 1 {
			// A Value string is expected to be UTF-8. If a caller supplied an
			// invalid byte sequence, match the replacement behavior of JS text
			// conversion rather than generating malformed JSON.
			writeUnicodeEscape(out, utf8.RuneError)
			pos++
			continue
		}
		pos += size

		switch runeValue {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if runeValue < 0x20 {
				writeUnicodeEscape(out, runeValue)
			} else {
				out.WriteString(string(runeValue))
			}
		}
	}
	out.WriteByte('"')
}

func writeUnicodeEscape(out *bytes.Buffer, value rune) {
	if value <= 0xffff {
		const digits = "0123456789abcdef"
		out.WriteString(`\u`)
		out.WriteByte(digits[(value>>12)&0xf])
		out.WriteByte(digits[(value>>8)&0xf])
		out.WriteByte(digits[(value>>4)&0xf])
		out.WriteByte(digits[value&0xf])
		return
	}

	// This branch is only used for invalid UTF-8 replacement runes in
	// practice; valid supplementary runes are intentionally emitted as UTF-8
	// by JSON.stringify. Keep it correct if called directly nevertheless.
	out.WriteString(string(value))
}

func decodeWTF8Surrogate(value string) (unit uint16, size int, ok bool) {
	if len(value) < 3 || value[0] != 0xed {
		return 0, 0, false
	}
	if value[1] < 0xa0 || value[1] > 0xbf || value[2] < 0x80 || value[2] > 0xbf {
		return 0, 0, false
	}
	unit = uint16(value[0]&0x0f)<<12 | uint16(value[1]&0x3f)<<6 | uint16(value[2]&0x3f)
	if !isHighSurrogate(unit) && !isLowSurrogate(unit) {
		return 0, 0, false
	}
	return unit, 3, true
}

// objectKeys returns each M key once. Keys is authoritative for known keys;
// map-only keys are appended in a deterministic order so hand-built Objects
// cannot produce random JSON. Numeric index names are placed first in numeric
// order, matching JavaScript's own-property enumeration used by JSON.stringify.
func objectKeys(object *Object) []string {
	if object == nil || len(object.M) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(object.M))
	keys := make([]string, 0, len(object.M))
	for _, key := range object.Keys {
		if _, exists := seen[key]; exists {
			continue
		}
		if _, exists := object.M[key]; !exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	missing := make([]string, 0)
	for key := range object.M {
		if _, exists := seen[key]; !exists {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	keys = append(keys, missing...)
	return javascriptPropertyOrder(keys)
}

func javascriptPropertyOrder(keys []string) []string {
	indices := make([]uint32, 0)
	nonIndices := make([]string, 0, len(keys))
	for _, key := range keys {
		if index, ok := arrayIndex(key); ok {
			indices = append(indices, index)
		} else {
			nonIndices = append(nonIndices, key)
		}
	}
	if len(indices) == 0 {
		return keys
	}
	sort.Slice(indices, func(i, j int) bool { return indices[i] < indices[j] })
	ordered := make([]string, 0, len(keys))
	for _, index := range indices {
		ordered = append(ordered, strconv.FormatUint(uint64(index), 10))
	}
	ordered = append(ordered, nonIndices...)
	return ordered
}

func arrayIndex(key string) (uint32, bool) {
	if key == "" || (len(key) > 1 && key[0] == '0') {
		return 0, false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < '0' || key[i] > '9' {
			return 0, false
		}
	}
	value, err := strconv.ParseUint(key, 10, 32)
	if err != nil || value == math.MaxUint32 {
		return 0, false
	}
	if strconv.FormatUint(value, 10) != key {
		return 0, false
	}
	return uint32(value), true
}

// formatJSNumber converts the IEEE-754 value represented by a json.Number to
// the spelling used by ECMAScript's Number::toString/JSON.stringify. Parsing
// into float64 here is intentional: JSON.parse also produces an IEEE-754
// Number, so 1.0, 1e0, and large integers must follow the same rounding and
// exponent thresholds rather than retaining their source spelling.
func formatJSNumber(number json.Number) string {
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return "null"
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return "null"
	}
	if value == 0 {
		// JSON.stringify(-0) is "0".
		return "0"
	}

	formatted := strconv.FormatFloat(value, 'e', -1, 64)
	sign := ""
	if formatted[0] == '-' {
		sign = "-"
		formatted = formatted[1:]
	}
	parts := strings.SplitN(formatted, "e", 2)
	mantissa := parts[0]
	exponent, err := strconv.Atoi(parts[1])
	if err != nil {
		return "null"
	}

	digits := strings.ReplaceAll(mantissa, ".", "")
	// FormatFloat with precision -1 does not add insignificant trailing
	// digits, but trimming is harmless and protects this helper if that
	// implementation detail changes.
	digits = strings.TrimRight(digits, "0")
	if digits == "" {
		return "0"
	}

	// ECMAScript uses decimal notation for 1e-6 <= abs(x) < 1e21.
	if exponent >= -6 && exponent < 21 {
		decimalPosition := exponent + 1
		var body string
		switch {
		case decimalPosition <= 0:
			body = "0." + strings.Repeat("0", -decimalPosition) + digits
		case decimalPosition >= len(digits):
			body = digits + strings.Repeat("0", decimalPosition-len(digits))
		default:
			body = digits[:decimalPosition] + "." + digits[decimalPosition:]
		}
		return sign + body
	}

	body := string(digits[0])
	if len(digits) > 1 {
		body += "." + digits[1:]
	}
	return sign + body + "e" + exponentString(exponent)
}

func exponentString(exponent int) string {
	if exponent >= 0 {
		return "+" + strconv.Itoa(exponent)
	}
	return strconv.Itoa(exponent)
}

// DeepEqual compares JSON values using JavaScript/JSON value semantics. Object
// member order is not semantic; arrays retain order. Number spellings compare
// as the same IEEE-754 Number, except +0 and -0 remain distinct like
// JavaScript's Object.is used by the merge engine.
func DeepEqual(a, b Value) bool {
	return deepEqual(a, b)
}

func deepEqual(a, b Value) bool {
	switch left := a.(type) {
	case nil:
		return isNilObject(b)
	case *Object:
		if left == nil {
			return isNilObject(b)
		}
		right, ok := b.(*Object)
		if !ok || right == nil {
			return false
		}
		if len(left.M) != len(right.M) {
			return false
		}
		for key, leftValue := range left.M {
			rightValue, exists := right.M[key]
			if !exists || !deepEqual(leftValue, rightValue) {
				return false
			}
		}
		return true
	case []Value:
		right, ok := b.([]Value)
		if !ok || len(left) != len(right) {
			return false
		}
		for i := range left {
			if !deepEqual(left[i], right[i]) {
				return false
			}
		}
		return true
	case json.Number:
		right, ok := b.(json.Number)
		return ok && equalNumbers(left, right)
	case string:
		right, ok := b.(string)
		return ok && left == right
	case bool:
		right, ok := b.(bool)
		return ok && left == right
	default:
		return false
	}
}

func isNilObject(value Value) bool {
	if value == nil {
		return true
	}
	object, ok := value.(*Object)
	return ok && object == nil
}

func equalNumbers(left, right json.Number) bool {
	leftValue, leftErr := strconv.ParseFloat(left.String(), 64)
	rightValue, rightErr := strconv.ParseFloat(right.String(), 64)
	if (leftErr != nil && !errors.Is(leftErr, strconv.ErrRange)) ||
		(rightErr != nil && !errors.Is(rightErr, strconv.ErrRange)) {
		return left.String() == right.String()
	}
	if leftValue == 0 && rightValue == 0 {
		return math.Signbit(leftValue) == math.Signbit(rightValue)
	}
	return leftValue == rightValue
}

// StripTopKeys removes only named members from the root object and returns a
// deep copy. Nested objects and arrays are copied unchanged in shape/order;
// nested members with the same names are retained.
func StripTopKeys(value Value, keys []string) Value {
	strip := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		strip[key] = struct{}{}
	}
	return stripValue(value, strip, true)
}

func stripValue(value Value, strip map[string]struct{}, topLevel bool) Value {
	switch typed := value.(type) {
	case *Object:
		if typed == nil {
			return nil
		}
		copyObject := &Object{Keys: make([]string, 0, len(typed.Keys)), M: make(map[string]Value, len(typed.M))}
		for _, key := range objectKeysForCopy(typed) {
			if topLevel {
				if _, remove := strip[key]; remove {
					continue
				}
			}
			copyObject.Keys = append(copyObject.Keys, key)
			copyObject.M[key] = stripValue(typed.M[key], strip, false)
		}
		return copyObject
	case []Value:
		copyArray := make([]Value, len(typed))
		for i, item := range typed {
			copyArray[i] = stripValue(item, strip, false)
		}
		return copyArray
	default:
		return value
	}
}

func objectKeysForCopy(object *Object) []string {
	// Copying should retain the explicit object order, not apply JavaScript's
	// serialization ordering. objectKeys is only needed to safely handle a
	// hand-built Object whose map has keys not listed in Keys.
	if object == nil || len(object.M) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(object.M))
	keys := make([]string, 0, len(object.M))
	for _, key := range object.Keys {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		if _, exists := object.M[key]; !exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	missing := make([]string, 0)
	for key := range object.M {
		if _, exists := seen[key]; !exists {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return append(keys, missing...)
}
