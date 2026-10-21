package httpapi

import "time"

// SetGrantCheckEvery lets tests see a revocation reach an open stream quickly.
func SetGrantCheckEvery(d time.Duration) { grantCheckEvery = d }
