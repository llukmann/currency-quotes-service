package domain

import (
	"fmt"
	"strings"
)

// Pair is a currency pair in BASE/QUOTE form. Every value comes from ParsePair,
// and the zero value is not a pair.
type Pair string

// The CHECK constraint on the column knows only the shape of a pair, so this
// map is the only place the supported set is enforced.
var supportedCurrencies = map[string]struct{}{
	"USD": {},
	"EUR": {},
	"MXN": {},
}

func ParsePair(s string) (Pair, error) {
	base, quote, ok := strings.Cut(strings.ToUpper(s), "/")
	if !ok {
		return "", fmt.Errorf("%w: %q is not in BASE/QUOTE form", ErrInvalidPair, s)
	}

	if _, ok := supportedCurrencies[base]; !ok {
		return "", fmt.Errorf("%w: unsupported currency %q", ErrInvalidPair, base)
	}

	if _, ok := supportedCurrencies[quote]; !ok {
		return "", fmt.Errorf("%w: unsupported currency %q", ErrInvalidPair, quote)
	}

	if base == quote {
		return "", fmt.Errorf("%w: base and quote currency are both %q", ErrInvalidPair, base)
	}

	return Pair(base + "/" + quote), nil
}

func (p Pair) Currencies() (base, quote string) {
	base, quote, _ = strings.Cut(string(p), "/")

	return base, quote
}
