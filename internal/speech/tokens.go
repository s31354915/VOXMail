package speech

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

var (
	spaceBeforePunctRE = regexp.MustCompile(`[ \t]+([,.;!?])`)
	addressBracketsRE  = regexp.MustCompile(`<([^<>\s]+@[^<>\s]+)>`)
	groupedDecimalRE   = regexp.MustCompile(`\b\d{1,3}(?:,\d{3})+\.\d+\b`)
	ordinalRE          = regexp.MustCompile(`\b\d+(?:st|nd|rd|th)\b`)
	emailDigitsRE      = regexp.MustCompile(`\d+`)
	phoneExtensionRE   = regexp.MustCompile(`(?i)\s*(?:ext\.?|x)\s*(\d+)$`)
	identifierRE       = regexp.MustCompile(`(?i)\b(PIN|OTP|code|order|tracking|reference|invoice|account)(?:\s+(?:number|code))?[:# ]+\d+\b`)
	signedNumberRE     = regexp.MustCompile(`(^|[\s(=])[-−](\d)`)
	signedAmountRE     = regexp.MustCompile(`(^|[\s(])[-−]([$€£¥₹₪])`)
	streetRE           = regexp.MustCompile(`(?i)\b\d{1,6}\s+(?:[a-z]+\s+){1,4}(?:st|rd|ave|blvd|ln|dr|ct|hwy)\b\.?`)
)

func speakIdentifier(raw string) string {
	i := strings.IndexAny(raw, "0123456789")
	return strings.TrimRight(raw[:i], ":# ") + " " + groupDigits(raw[i:])
}

func speakStreet(raw string) string {
	words := strings.Fields(strings.TrimSuffix(raw, "."))
	suffixes := map[string]string{"st": "Street", "rd": "Road", "ave": "Avenue", "blvd": "Boulevard", "ln": "Lane", "dr": "Drive", "ct": "Court", "hwy": "Highway"}
	words[len(words)-1] = suffixes[strings.ToLower(words[len(words)-1])]
	spoken := strings.Join(words, " ")
	if strings.HasSuffix(raw, ".") {
		spoken += "."
	}
	return spoken
}

// Email spelling preserves every local-part character, including plus tags
// and leading zeros. Numeric components are identifiers, never quantities.
func spellEmail(value string) string {
	var expanded strings.Builder
	for _, r := range strings.ToLower(value) {
		if name := characterNames[r]; name != "" {
			expanded.WriteString(" " + name + " ")
		} else {
			expanded.WriteRune(r)
		}
	}
	value = expanded.String()
	return emailDigitsRE.ReplaceAllStringFunc(value, func(digits string) string { return " " + groupDigits(digits) + " " })
}

var characterNames = map[rune]string{
	'@': "at", '.': "dot", '-': "dash", '_': "underscore", '+': "plus",
	'/': "slash", '\\': "backslash", ':': "colon", ';': "semicolon",
	',': "comma", '$': "dollar sign", '%': "percent", '&': "ampersand",
	'=': "equals", '?': "question mark", '!': "exclamation mark",
	'(': "open parenthesis", ')': "close parenthesis", '#': "hash",
	'*': "asterisk", '\'': "apostrophe", '"': "quotation mark", ' ': "space",
	'~': "tilde", '^': "caret", '|': "vertical bar", '{': "open brace", '}': "close brace",
}

// SpellCharacters is for exact readback of caller-entered identifiers. Commas
// separate letters so the synthesizer cannot join them into a new word.
func SpellCharacters(value string) string {
	var parts []string
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			parts = append(parts, numberSmall[r-'0'])
		case characterNames[r] != "":
			parts = append(parts, characterNames[r])
		default:
			parts = append(parts, string(unicode.ToUpper(r)))
		}
	}
	return strings.Join(parts, ", ")
}

func groupDigits(value string) string {
	var words []string
	for _, r := range value {
		if r >= '0' && r <= '9' {
			words = append(words, numberSmall[r-'0'])
		} else if r == '+' {
			words = append(words, "plus")
		}
	}
	return strings.Join(words, " ")
}

func speakPhone(value string) string {
	extension := phoneExtensionRE.FindStringSubmatch(value)
	if extension != nil {
		value = value[:len(value)-len(extension[0])]
	}
	// Keep the written grouping as short pauses, useful when taking a number
	// down over the phone. All digits, including country-code zeros, survive.
	groups := strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == '.' || r == '-' || r == '(' || r == ')' })
	for i, group := range groups {
		groups[i] = groupDigits(group)
	}
	spoken := strings.Join(groups, ", ")
	if extension != nil {
		spoken += ", extension " + groupDigits(extension[1])
	}
	return spoken
}

type currencyUnits struct{ one, many, minorOne, minorMany string }

var currencies = map[string]currencyUnits{
	"$":   {"dollar", "dollars", "cent", "cents"},
	"€":   {"euro", "euros", "euro cent", "euro cents"},
	"£":   {"pound", "pounds", "penny", "pence"},
	"¥":   {"yen", "yen", "sen", "sen"},
	"₹":   {"rupee", "rupees", "paisa", "paise"},
	"₪":   {"shekel", "shekels", "agora", "agorot"},
	"USD": {"U S dollar", "U S dollars", "cent", "cents"},
	"EUR": {"euro", "euros", "euro cent", "euro cents"},
	"GBP": {"pound", "pounds", "penny", "pence"},
	"CAD": {"Canadian dollar", "Canadian dollars", "cent", "cents"},
	"AUD": {"Australian dollar", "Australian dollars", "cent", "cents"},
	"JPY": {"yen", "yen", "sen", "sen"},
	"INR": {"rupee", "rupees", "paisa", "paise"},
	"ILS": {"shekel", "shekels", "agora", "agorot"},
}

func speakCurrency(raw string) string {
	var units currencyUnits
	body := raw
	for symbol, candidate := range currencies {
		if strings.HasPrefix(raw, symbol) {
			units, body = candidate, strings.TrimSpace(strings.TrimPrefix(raw, symbol))
			break
		}
	}
	sign := ""
	if strings.HasPrefix(body, "-") {
		sign, body = "minus ", body[1:]
	}
	parts := strings.SplitN(body, ".", 2)
	whole := strings.ReplaceAll(parts[0], ",", "")
	if whole == "" {
		whole = "0"
	}
	major, err := strconv.ParseInt(whole, 10, 64)
	majorText := groupDigits(whole)
	unit := units.many
	if err == nil {
		majorText = intToWords(major)
		if major == 1 {
			unit = units.one
		}
	}
	spoken := sign + majorText + " " + unit
	if len(parts) == 2 {
		fraction := parts[1]
		// Preserve prices with precision beyond minor currency units, such
		// as a per-unit fuel price. Never silently round or truncate them.
		if len(fraction) > 2 {
			return sign + majorText + " point " + groupDigits(fraction) + " " + unit
		}
		if len(fraction) == 1 {
			fraction += "0"
		}
		minor, _ := strconv.ParseInt(fraction, 10, 64)
		if minor != 0 {
			unit = units.minorMany
			if minor == 1 {
				unit = units.minorOne
			}
			spoken += " and " + intToWords(minor) + " " + unit
		}
	}
	return spoken
}

func speakOrdinal(raw string) string {
	n, err := strconv.ParseInt(raw[:len(raw)-2], 10, 64)
	if err != nil {
		return raw
	}
	return ordinalWords(n)
}

func ordinalWords(n int64) string {
	words := intToWords(n)
	cut := strings.LastIndexAny(words, " -") + 1
	last := words[cut:]
	special := map[string]string{"zero": "zeroth", "one": "first", "two": "second", "three": "third", "five": "fifth", "eight": "eighth", "nine": "ninth", "twelve": "twelfth"}
	if ordinal, ok := special[last]; ok {
		return words[:cut] + ordinal
	}
	if strings.HasSuffix(last, "y") {
		return words[:len(words)-1] + "ieth"
	}
	return words + "th"
}

func speakSymbols(text string) string {
	// Keep sentence punctuation and hyphens in prose. Mathematical and
	// standalone symbols need explicit names; amounts and addresses are already
	// expanded above so this cannot turn an email plus-tag into arithmetic.
	return strings.NewReplacer(
		"&", " and ", "@", " at ", "%", " percent ", "+", " plus ",
		"=", " equals ", "×", " times ", "÷", " divided by ", "−", " minus ",
		"≤", " less than or equal to ", "≥", " greater than or equal to ",
		"°C", " degrees Celsius ", "°F", " degrees Fahrenheit ", "°", " degrees ",
		"#", " hash ", "_", " underscore ", "*", " asterisk ",
		"<", " less than ", ">", " greater than ", "/", " slash ",
		"$", " dollar sign ", "€", " euro sign ", "£", " pound sign ",
	).Replace(text)
}
