package httpapi

import "time"

// SetGrantCheckEvery lets tests see a revocation reach an open stream quickly.
func SetGrantCheckEvery(d time.Duration) { grantCheckEvery = d }

// SetExchangeGrace shortens how long a retired token works; it returns the old value.
func SetExchangeGrace(d time.Duration) time.Duration {
	old := exchangeGrace
	exchangeGrace = d
	return old
}

// SetExchangeGap changes the share of a grant's lifetime that must pass before
// it can be exchanged (tests that exchange in a tight loop set 0); it returns the old value.
func SetExchangeGap(f float64) float64 {
	old := exchangeGap
	exchangeGap = f
	return old
}
