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
