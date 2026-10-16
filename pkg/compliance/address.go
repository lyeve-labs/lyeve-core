package compliance

import (
	"encoding/json"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// Matching a subject's address
//
// A subject request names a person by address, and a row belongs to the
// subject only when it holds that address and no other. SQL cannot decide
// that alone. MySQL's collations ignore accents and read ß as ss, and SQL
// Server's default collation reads ß as ss, so josé@ matches jose@ and
// straße@ matches strasse@, each a different person. The same collations
// also match the subject's own rows stored decomposed or with a trailing
// space, which a binary comparison would lose. So a subject eraser or
// exporter selects a superset in SQL, confirms each row in Go, and acts on
// the confirmed rows by id.
//
// For a column that holds one address:
//
//	rows, err := q.Query(ctx, `SELECT id, email FROM t WHERE `+
//		compliance.AddressCandidates("email", 1), compliance.AddressForms(address)...)
//	// keep a row only when compliance.SameAddress(stored, address)
//
// For free text, such as a log line or a JSON payload, the prefilter is a
// case-insensitive LIKE for each of AddressTextForms, and NamesAddress
// confirms each row.

// AddressCandidates returns a SQL condition that is true for every row whose
// column may hold the address AddressForms binds. Its placeholders start at
// first: $first, $first+1 and $first+2, so it composes inside a larger
// statement. column is a trusted column expression, never request input.
//
// It compares lowered values under the column's collation, which on MySQL
// and SQL Server also selects other people's addresses. Confirm each row with
// SameAddress before acting on it. The condition holds no index-friendly
// comparison, so give it a narrower predicate beside it where one exists.
func AddressCandidates(column string, first int) string {
	p := func(i int) string { return "LOWER($" + strconv.Itoa(first+i) + ")" }
	lowered := "LOWER(" + column + ")"
	return "(" + lowered + " = " + p(0) + " OR " + lowered + " = " + p(1) + " OR " + lowered + " = " + p(2) +
		" OR LOWER(LTRIM(RTRIM(" + column + "))) = " + p(1) + ")"
}

// AddressForms returns the three values AddressCandidates binds, in order:
// the address as requested, and the address trimmed and lowered in Go, in
// Unicode normalization forms C and D. The request as given keeps every row
// the column's collation folds to it. The forms lowered in Go also reach a
// row the database cannot lower to the request, such as straße@ for STRAẞE@
// where the collation does not know ẞ, and a row stored decomposed on
// PostgreSQL, which compares lowered values byte for byte. The trimmed
// column reaches a row stored with a space around the address on the
// dialects whose comparison keeps the space.
func AddressForms(address string) []any {
	lower := strings.ToLower(canonicalAddress(address))
	return []any{address, lower, norm.NFD.String(lower)}
}

// SameAddress reports whether stored and requested name one address: equal
// with case folded, once each is trimmed and in Unicode normalization form C.
// Accents and ß stay significant, so josé@ is not jose@ and straße@ is not
// strasse@. A decomposed é, a capital ẞ and a trailing space still match.
func SameAddress(stored, requested string) bool {
	return strings.EqualFold(canonicalAddress(stored), canonicalAddress(requested))
}

// canonicalAddress trims the address and puts it in Unicode normalization
// form C. A sender may store an address with a trailing space, which SQL
// Server and a padding MySQL collation ignore. Every dialect's LOWER folds the
// capital İ to i, but Go's simple case folding keeps them apart, so İ becomes
// I. That runs after NFC, which composes I and a combining dot into İ.
func canonicalAddress(address string) string {
	return dottedCapitalI.Replace(norm.NFC.String(strings.TrimSpace(address)))
}

var dottedCapitalI = strings.NewReplacer("\u0130", "I")

// AddressTextForms returns the spellings of address to look for in free
// text with a case-insensitive LIKE: as given, and, when it differs, as Go's
// JSON encoder writes it inside a string, once and twice. SQL Server keeps
// JSON text as written, so a&b@x.example is stored as a\u0026b@x.example. On
// every dialect a quote or a backslash in the address stays escaped, and a
// JSON body sent inside a JSON string escapes the backslash of the inner
// escape once more. An escaped form always holds a backslash, so the twice
// escaped form differs whenever the once escaped one does.
//
// The address lowered in Unicode normalization form D is added when it
// differs from the address lowered in form C. PostgreSQL compares LIKE
// operands code point by code point, so only that form finds text that spells
// the address decomposed. MySQL's and SQL Server's collations treat the two
// forms as equal.
//
// Each form still matches inside a longer address, and on MySQL an accented
// spelling, so confirm every row with NamesAddress.
func AddressTextForms(address string) []string {
	forms := []string{address}
	lower := strings.ToLower(canonicalAddress(address))
	if nfd := norm.NFD.String(lower); nfd != lower {
		forms = append(forms, nfd)
	}
	if escaped := jsonEscaped(address); escaped != address {
		forms = append(forms, escaped, jsonEscaped(escaped))
	}
	return forms
}

// jsonEscaped is text as it reads inside a JSON string Go's encoder wrote.
func jsonEscaped(text string) string {
	b, _ := json.Marshal(text)
	return string(b[1 : len(b)-1])
}

// NamesAddress reports whether text names address as a whole, with case
// folded: ann@x.example is not named by joann@x.example, jo.ann@x.example or
// ann@x.example.org, and is named by "Ann <ann@x.example>" and by a sentence
// ending "ann@x.example.". Text that is JSON is read decoded, every string
// and key, because a stored payload may escape the character beside the
// address ("\u003c" for "<") and the escape's last letter would read as part
// of it. A string inside it that holds the address beside an escape is
// decoded again, up to three levels. Text that is not JSON is read as it is.
//
// The address and every string read are compared in the form SameAddress
// uses, so text that spells the address decomposed, or with a capital İ,
// still names it, as it did under the collations of MySQL and SQL Server.
func NamesAddress(text, address string) bool {
	if text == "" {
		return false
	}
	address = canonicalAddress(address)
	var doc any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return stringNames(text, address, 1)
	}
	return valueNames(doc, address, 1)
}

// maxPayloadDepth bounds how many times text is decoded, so a hostile
// payload cannot make the check recurse without limit. A service that sends
// a JSON body inside a JSON field takes two.
const maxPayloadDepth = 3

func valueNames(v any, address string, depth int) bool {
	switch v := v.(type) {
	case string:
		return stringNames(v, address, depth)
	case []any:
		for _, e := range v {
			if valueNames(e, address, depth) {
				return true
			}
		}
	case map[string]any:
		for k, e := range v {
			if stringNames(k, address, depth) || valueNames(e, address, depth) {
				return true
			}
		}
	}
	return false
}

// stringNames reports whether a decoded string holds the address. When the
// address is there but an escape sits beside it, or the string holds it only
// JSON escaped, the string may be a JSON document or a string body encoded
// once more, so each is decoded and read again, up to maxPayloadDepth.
// Decoding reads the real neighbor, so an escaped letter ("\u00e9" from an
// encoder that escapes non-ASCII) still refuses a longer address. A string
// that does not decode, being truncated, holding a raw quote or nested too
// deep, is read with a JSON escape beside the match counted as a boundary.
func stringNames(text, address string, depth int) bool {
	if holdsAddress(text, address, false) {
		return true
	}
	lower := foldedText(text)
	if !strings.Contains(lower, strings.ToLower(address)) &&
		!(strings.Contains(text, `\`) && strings.Contains(lower, strings.ToLower(jsonEscaped(address)))) {
		return false
	}
	if depth < maxPayloadDepth {
		var doc any
		if json.Unmarshal([]byte(text), &doc) == nil {
			return valueNames(doc, address, depth+1)
		}
		var body string
		if json.Unmarshal([]byte(`"`+text+`"`), &body) == nil && body != text {
			return stringNames(body, address, depth+1)
		}
	}
	return holdsAddress(text, address, true)
}

// holdsAddress reports whether text holds address, case folded, with no
// address character on either side. Every occurrence is tried, so a text
// naming "joann@x.example" before "ann@x.example" still holds the second.
// With escapes set, a JSON escape just before the match is a boundary.
func holdsAddress(text, address string, escapes bool) bool {
	text, want := foldedText(text), strings.ToLower(address)
	if want == "" {
		return false
	}
	for from := 0; from < len(text); {
		i := strings.Index(text[from:], want)
		if i < 0 {
			return false
		}
		start := from + i
		end := start + len(want)
		if !extendsBefore(text[:start], escapes) && !extendsAfter(text[end:]) {
			return true
		}
		_, size := utf8.DecodeRuneInString(text[start:])
		from = start + size
	}
	return false
}

// foldedText is text in Unicode normalization form C, with İ mapped to I as
// canonicalAddress maps it, and lowered. It is not trimmed, because the
// characters around a match decide whether it is the whole address.
func foldedText(text string) string {
	return strings.ToLower(dottedCapitalI.Replace(norm.NFC.String(text)))
}

// The two sides of a match continue an address on different characters.
// Only the characters that commonly join a longer address count, because a
// match refused here is the subject's own data left out of the export and
// left behind by the erasure. Symbols an address may hold, such as "=", "&",
// "?", "/", "'" and "|", far more often sit beside it in a URL, a form body,
// quotes or a list. Letters of the Han, Hiragana, Katakana, Hangul, Thai,
// Lao, Khmer and Myanmar scripts are boundaries too: those scripts are
// written without spaces, so such text often runs into an address, and a
// local part ending in one of them just before the subject's is far rarer.
// A percent-encoded byte ("%20", "%3C") beside the match is a boundary,
// because a URL or form body encodes the space or bracket around an address.
// A backslash or a percent sign after the match never continues it, so only
// the left side needs those rules.

// wordRune reports whether r is a letter, mark or digit that can join two
// parts of one address.
func wordRune(r rune) bool {
	if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul,
		unicode.Thai, unicode.Lao, unicode.Khmer, unicode.Myanmar) {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsDigit(r)
}

// continuesLeft reports whether r, just before a match, makes it the tail
// of a longer local part.
func continuesLeft(r rune) bool { return wordRune(r) || r == '_' || r == '-' || r == '+' }

// continuesRight reports whether r, just after a match, makes it the head
// of a longer domain.
func continuesRight(r rune) bool { return wordRune(r) || r == '-' }

// extendsBefore reports whether the text just before a match continues the
// address leftward. A dot continues it only when a continuing character lies
// beyond the dot, because an address never starts or ends with one and a
// sentence often does. Text is lower case here.
func extendsBefore(text string, escapes bool) bool {
	if endsWithPercentByte(text) || (escapes && endsWithJSONEscape(text)) {
		return false
	}
	r, size := utf8.DecodeLastRuneInString(text)
	if size == 0 {
		return false
	}
	if r == '.' {
		r, size = utf8.DecodeLastRuneInString(text[:len(text)-size])
		return size > 0 && continuesLeft(r)
	}
	return continuesLeft(r)
}

// endsWithPercentByte reports whether text ends in "%" and two hex digits.
func endsWithPercentByte(text string) bool {
	n := len(text)
	return n >= 3 && text[n-3] == '%' && isLowerHex(text[n-2:])
}

// endsWithJSONEscape reports whether text ends in a JSON escape: "\u" and
// four hex digits, or a backslash and one escaped character.
func endsWithJSONEscape(text string) bool {
	n := len(text)
	if n >= 6 && text[n-6] == '\\' && text[n-5] == 'u' && isLowerHex(text[n-4:]) {
		return true
	}
	return n >= 2 && text[n-2] == '\\' && strings.IndexByte(`bfnrt"\/`, text[n-1]) >= 0
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		if (s[i] < '0' || s[i] > '9') && (s[i] < 'a' || s[i] > 'f') {
			return false
		}
	}
	return true
}

// extendsAfter reports whether the text just after a match continues the
// address rightward, with the same rule for a dot.
func extendsAfter(text string) bool {
	r, size := utf8.DecodeRuneInString(text)
	if size == 0 {
		return false
	}
	if r == '.' {
		r, size = utf8.DecodeRuneInString(text[size:])
		return size > 0 && continuesRight(r)
	}
	return continuesRight(r)
}
