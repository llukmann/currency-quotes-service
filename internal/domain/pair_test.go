package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestParsePair pins the whitelist and the normalisation, which is the whole
// of what makes a Pair trustworthy further down: every value of the type comes
// through here, and nothing below revalidates one. The cases that matter are
// the refusals -- the constraint in the schema knows only the shape of a pair
// and would accept every unsupported currency below.
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
			name: "a supported pair passes through unchanged",
			in:   "EUR/MXN",
			want: Pair("EUR/MXN"),
		},
		{
			name: "the reverse of a supported pair is a pair of its own",
			in:   "MXN/EUR",
			want: Pair("MXN/EUR"),
		},
		{
			name: "the third combination is supported as well",
			in:   "USD/MXN",
			want: Pair("USD/MXN"),
		},
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

// TestPairCurrencies checks the split the provider builds its request from.
// The halves have to come back in the order they were written in, since one is
// the currency being priced and the other the currency it is priced in.
func TestPairCurrencies(t *testing.T) {
	pair, err := ParsePair("eur/mxn")
	require.NoError(t, err)

	base, quote := pair.Currencies()

	require.Equal(t, "EUR", base)
	require.Equal(t, "MXN", quote)
}
