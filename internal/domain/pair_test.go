package domain

import (
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every value of the type comes through here and nothing below revalidates
// one, so the refusals are the cases that matter: the schema constraint knows
// only the shape of a pair and would accept every unsupported currency below.
func TestParsePair(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want Pair
		// wantErr is a fragment of the message a client is shown, empty when
		// the pair is expected to parse. The message is served as is by the
		// API, so the currency at fault has to be named in it.
		wantErr string
	}{
		{
			// What keeps "eur/mxn" from conflicting with "EUR/MXN" over an
			// idempotency key, and what keeps two spellings of one pair from
			// becoming two rows.
			name: "case is normalised upwards",
			in:   "eur/mxn",
			want: Pair("EUR/MXN"),
		},
		{
			name: "mixed case is normalised as well",
			in:   "Usd/Eur",
			want: Pair("USD/EUR"),
		},
		{
			name:    "a value with no separator is refused",
			in:      "EURMXN",
			wantErr: `"EURMXN" is not in BASE/QUOTE form`,
		},
		{
			// What a missing "pair" field and a missing query parameter both
			// arrive as.
			name:    "an empty value is refused",
			in:      "",
			wantErr: `"" is not in BASE/QUOTE form`,
		},
		{
			name:    "an unsupported base currency is named",
			in:      "RUB/USD",
			wantErr: `unsupported currency "RUB"`,
		},
		{
			name:    "an unsupported quote currency is named",
			in:      "USD/RUB",
			wantErr: `unsupported currency "RUB"`,
		},
		{
			// The provider has no such pair to answer with, and the rate would
			// be 1 without asking it.
			name:    "a currency against itself is refused",
			in:      "USD/USD",
			wantErr: `base and quote currency are both "USD"`,
		},
		{
			// The separator is there, so the value has the shape of a pair and
			// is refused by the whitelist rather than by the form -- with an
			// empty name where the currency should be.
			name:    "a missing quote currency is refused",
			in:      "EUR/",
			wantErr: `unsupported currency ""`,
		},
		{
			name:    "a missing base currency is refused",
			in:      "/MXN",
			wantErr: `unsupported currency ""`,
		},
		{
			// Cut splits at the first separator, so everything after it is one
			// currency name and fails as one.
			name:    "a third component lands in the quote currency",
			in:      "EUR/MXN/USD",
			wantErr: `unsupported currency "MXN/USD"`,
		},
		{
			// Deliberately not trimmed. A whitelist that accepted surrounding
			// space would be a second spelling of a pair that the normalised
			// form is meant to rule out.
			name:    "surrounding space is not trimmed away",
			in:      " EUR/MXN",
			wantErr: `unsupported currency " EUR"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePair(tt.in)

			if tt.wantErr == "" {
				require.NoError(t, err)
				require.Equal(t, tt.want, got)

				return
			}

			// Every failure has to wrap the sentinel: the API branches on it
			// with errors.Is to tell a 400 from a 500.
			require.ErrorIs(t, err, ErrInvalidPair)
			require.Contains(t, err.Error(), tt.wantErr)
			require.Empty(t, got)
		})
	}
}

// The only guard the whitelist has: the CHECK constraint knows the shape of a
// pair and not the set, the provider quotes whatever it is asked for, and
// every other test goes on passing with a longer list. Pinning it here makes
// widening the set a decision rather than an edit.
func TestSupportedCurrencies(t *testing.T) {
	got := make([]string, 0, len(supportedCurrencies))
	for currency := range supportedCurrencies {
		got = append(got, currency)
	}

	sort.Strings(got)

	require.Equal(t, []string{"EUR", "MXN", "USD"}, got)
}

// What the whitelist adds up to. It is kept per currency, so the pairs are a
// consequence rather than a list anybody wrote down -- three currencies, each
// priced in either of the other two, and these six are the whole of what the
// service quotes.
func TestParsePairAcceptsEveryCombination(t *testing.T) {
	pairs := []string{"USD/EUR", "USD/MXN", "EUR/USD", "EUR/MXN", "MXN/USD", "MXN/EUR"}

	for _, want := range pairs {
		t.Run(want, func(t *testing.T) {
			got, err := ParsePair(want)

			require.NoError(t, err)
			require.Equal(t, Pair(want), got)
		})
	}
}

// The split the provider builds its request from. The halves have to come back
// in the order they were written in, since one is the currency being priced
// and the other the currency it is priced in.
func TestPairCurrencies(t *testing.T) {
	pair, err := ParsePair("eur/mxn")
	require.NoError(t, err)

	base, quote := pair.Currencies()

	require.Equal(t, "EUR", base)
	require.Equal(t, "MXN", quote)
}

// The type is exported, so a Pair that never came from ParsePair compiles.
// This records the shape of that failure rather than blessing it: a broken
// value leaves a currency missing rather than silently naming the wrong one,
// so the request goes out incomplete and the upstream refuses it.
func TestPairCurrenciesOfAValueThatNeverParsed(t *testing.T) {
	tests := []struct {
		name      string
		pair      Pair
		wantBase  string
		wantQuote string
	}{
		{
			// Cut hands the whole value back as the base when it finds no
			// separator, so it is the quote that goes missing.
			name:     "a value with no separator keeps everything as the base",
			pair:     Pair("EUR"),
			wantBase: "EUR",
		},
		{
			name: "the zero value is not a pair",
			pair: Pair(""),
		},
		{
			name:      "a missing base currency stays missing",
			pair:      Pair("/MXN"),
			wantQuote: "MXN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base, quote := tt.pair.Currencies()

			require.Equal(t, tt.wantBase, base)
			require.Equal(t, tt.wantQuote, quote)
		})
	}
}
