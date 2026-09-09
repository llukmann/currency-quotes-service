package domain

import (
	"fmt"
	"strings"
)

// Pair is a validated currency pair in BASE/QUOTE form, such as "EUR/MXN". A
// rate quoted for it says how many units of the quote currency one unit of the
// base currency buys.
//
// The type is a string rather than a pair of fields so that the value travels
// unchanged from the handler to the text column that stores it, and so that
// reading a row back needs no validation: what the database holds has already
// been through ParsePair, and revalidating it would turn a narrowed whitelist
// into failures on historical rows. Callers that need the halves separately
// split the value themselves; only the provider does.
//
// Every value of this type comes from ParsePair. The zero value is not a pair.
type Pair string

// Currencies supported by the service. The whitelist is kept per currency, not
// per pair: a pair is valid when both of its currencies are listed and they
// differ. The CHECK constraint on the column only knows the shape of a pair, so
// this map is the only place the set is enforced.
var supportedCurrencies = map[string]struct{}{
	"USD": {},
	"EUR": {},
	"MXN": {},
}

// ParsePair validates s and returns it in canonical form. Case is normalised
// upwards, so "eur/mxn" and "EUR/MXN" name the same pair. Every failure wraps
// ErrInvalidPair.
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

	// A rate of a currency against itself is always 1, and the provider has no
	// such pair to answer with.
	if base == quote {
		return "", fmt.Errorf("%w: base and quote currency are both %q", ErrInvalidPair, base)
	}

	return Pair(base + "/" + quote), nil
}

// Currencies splits the pair into its base and quote currency.
//
// It cannot fail, and that is why it lives here rather than in the package that
// needs it: every Pair comes from ParsePair, which is what guarantees the shape
// relied on below. A split written next to the provider would have to deal with
// a malformed value that cannot reach it, or silently assume it cannot -- far
// from the constructor that makes the assumption true.
//
// That guarantee is a convention rather than a rule the compiler enforces: the
// type is exported, so Pair("EUR") compiles, and this method would hand back
// the whole of it as the base currency and an empty quote. The invariant is
// held the same way quotes.pair is held -- by there being exactly one writer --
// and it fails the same gentle way: the request goes out with a currency
// missing and the upstream rejects it, rather than a wrong rate being stored.
func (p Pair) Currencies() (base, quote string) {
	base, quote, _ = strings.Cut(string(p), "/")

	return base, quote
}
