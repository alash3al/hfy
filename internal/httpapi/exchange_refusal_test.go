package httpapi

import (
	"net/http"
	"testing"

	"github.com/alash3al/hfy/internal/db"
)

// A live renewable token that is not capped is never a 401: when the exchange
// missed by milliseconds and nothing is left to wait, it is still "too early,
// retry in 1 s", so the client renews instead of asking the host.
func TestExchangeRefusal(t *testing.T) {
	for name, c := range map[string]struct {
		miss          db.ExchangeMissed
		notLive       bool
		code, retryIn int
	}{
		"too early":                {db.ExchangeMissed{Wait: 12}, false, http.StatusTooManyRequests, 12},
		"missed by milliseconds":   {db.ExchangeMissed{Wait: 0}, false, http.StatusTooManyRequests, 1},
		"negative wait":            {db.ExchangeMissed{Wait: -3}, false, http.StatusTooManyRequests, 1},
		"capped":                   {db.ExchangeMissed{Capped: true, Wait: 5}, false, http.StatusConflict, 0},
		"retired":                  {db.ExchangeMissed{Retired: true, Wait: 5}, false, http.StatusUnauthorized, 0},
		"retired and capped":       {db.ExchangeMissed{Retired: true, Capped: true}, false, http.StatusUnauthorized, 0},
		"gone between the lookups": {db.ExchangeMissed{}, true, http.StatusUnauthorized, 0},
	} {
		if code, wait := exchangeRefusal(c.miss, c.notLive); code != c.code || wait != c.retryIn {
			t.Errorf("%s: %d/%d, want %d/%d", name, code, wait, c.code, c.retryIn)
		}
	}
}
