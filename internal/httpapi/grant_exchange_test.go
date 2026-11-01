package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/httpapi"
	"github.com/alash3al/hfy/internal/provider/fakeprov"
)

const exchange = client + "/grant/exchange"

// renewable mints a renewable grant: tokens of ttl seconds, up to life seconds in all.
func (a *api) renewable(conv string, ttl, life int) (token, id string) {
	a.t.Helper()
	return a.grant(conv, map[string]any{"expires_in": ttl, "max_lifetime": life})
}

// swap exchanges token and returns the response.
func (a *api) swap(token string) (int, map[string]any) {
	a.t.Helper()
	return a.do("POST", exchange, token, nil)
}

// live counts the grants of a chain that still hold.
func (a *api) live(chain string) int {
	a.t.Helper()
	var n int
	if err := a.rt.Store.Pool.QueryRow(context.Background(),
		"SELECT count(*) FROM grants WHERE root_id = $1 AND expires_at > now()", chain).Scan(&n); err != nil {
		a.t.Fatal(err)
	}
	return n
}

func withGrace(t *testing.T, d time.Duration) {
	t.Helper()
	old := httpapi.SetExchangeGrace(d)
	t.Cleanup(func() { httpapi.SetExchangeGrace(old) })
}

// max_lifetime makes a grant renewable, and only whole seconds in a sane
// range do; the other forms a JSON number can take are refused.
func TestRenewableGrantValidation(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()
	post := func(extra string) (int, map[string]any) {
		a.t.Helper()
		return a.do("POST", "/v1/grants", secret, json.RawMessage(`{"conversation":"`+conv+`","actor":"u1"`+extra+`}`))
	}
	for name, c := range map[string]struct {
		extra string
		want  int
	}{
		"renewable":                      {`,"expires_in":60,"max_lifetime":3600`, 201},
		"max_lifetime equals expires_in": {`,"expires_in":60,"max_lifetime":60`, 201},
		"seven days":                     {`,"expires_in":3600,"max_lifetime":604800`, 201},
		"default ttl is an hour":         {`,"max_lifetime":3600`, 201},
		"null is absent":                 {`,"expires_in":60,"max_lifetime":null`, 201},
		"over seven days":                {`,"expires_in":60,"max_lifetime":604801`, 400},
		"below expires_in":               {`,"expires_in":120,"max_lifetime":119`, 400},
		"below the default ttl":          {`,"max_lifetime":3599`, 400},
		"ttl under a minute":             {`,"expires_in":59,"max_lifetime":3600`, 400},
		"zero":                           {`,"expires_in":60,"max_lifetime":0`, 400},
		"negative":                       {`,"expires_in":60,"max_lifetime":-1`, 400},
		"float":                          {`,"expires_in":60,"max_lifetime":3600.5`, 400},
		"float zero fraction":            {`,"expires_in":60,"max_lifetime":3600.0`, 400},
		"exponent":                       {`,"expires_in":60,"max_lifetime":1e3`, 400},
		"string":                         {`,"expires_in":60,"max_lifetime":"3600"`, 400},
		"bool":                           {`,"expires_in":60,"max_lifetime":true`, 400},
		"array":                          {`,"expires_in":60,"max_lifetime":[3600]`, 400},
		"int64 max":                      {`,"expires_in":60,"max_lifetime":9223372036854775807`, 400},
		"past int64":                     {`,"expires_in":60,"max_lifetime":9223372036854775808`, 400},
		"huge":                           {`,"expires_in":60,"max_lifetime":1e999`, 400},
		"huge digits":                    {`,"expires_in":60,"max_lifetime":` + strings.Repeat("9", 400), 400},
		// n seconds as a time.Duration wraps around int64 for n over ~9.2e9: these once passed
		"expires_in float":     {`,"expires_in":60.5`, 400},
		"expires_in exponent":  {`,"expires_in":1e3`, 400},
		"expires_in string":    {`,"expires_in":"60"`, 400},
		"expires_in negative":  {`,"expires_in":-60`, 400},
		"expires_in int64 max": {`,"expires_in":9223372036854775807`, 400},
		"expires_in wraps":     {`,"expires_in":18446744075`, 400},
		"expires_in wraps 2":   {`,"expires_in":9223372036`, 400},
	} {
		if code, out := post(c.extra); code != c.want {
			t.Errorf("%s: got %d %v, want %d", name, code, out, c.want)
		}
	}
}

// An exchange hands back the same grant under a new token: same id (the one
// the backend stored), a fresh lifetime, identity copied as it was.
func TestExchangeRotatesTheToken(t *testing.T) {
	a := start(t, nil)
	withGrace(t, 30*time.Second)
	conv := a.newConversation()
	old, id := a.grant(conv, map[string]any{"actor": "alice", "scope": "room", "vars": map[string]string{"user": "Zoë \u2028 \"q\""},
		"expires_in": 60, "max_lifetime": 3600})

	code, out := a.swap(old)
	if code != 200 {
		t.Fatalf("exchange: %d %v", code, out)
	}
	fresh, _ := out["token"].(string)
	if fresh == "" || fresh == old || !strings.HasPrefix(fresh, "hfy_") || len(fresh) != 47 {
		t.Fatalf("token %q", fresh)
	}
	if out["id"] != id || out["renewable"] != true {
		t.Fatalf("id %v (want %s), renewable %v", out["id"], id, out["renewable"])
	}
	exp, err := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	if err != nil || time.Until(exp) < 55*time.Second || time.Until(exp) > 61*time.Second {
		t.Fatalf("expires_at %v: %v", out["expires_at"], err)
	}

	// two rows of one chain with the identity copied verbatim
	rows, err := a.rt.Store.Pool.Query(context.Background(), `SELECT actor_id || '|' || scope || '|' || agent || '|' || vars::text || '|' ||
		root_id || '|' || renew_secs || '|' || (renew_until IS NOT NULL) FROM grants ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	var seen []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, s)
	}
	rows.Close()
	if len(seen) != 2 || seen[0] != seen[1] || !strings.Contains(seen[0], id+"|60|true") || !strings.Contains(seen[0], "Zoë") {
		t.Fatalf("rows: %q", seen)
	}

	if code, _ := a.do("GET", client+"/history", fresh, nil); code != 200 {
		t.Fatalf("new token: %d", code)
	}
	if code, _ := a.do("GET", client+"/history", old, nil); code != 200 {
		t.Fatalf("the old token works during its grace: %d", code)
	}
	// it is retired: it cannot be exchanged again, and says what any dead token says
	code, out = a.swap(old)
	_, unknown := a.swap("hfy_nope")
	if code != 401 || out["error"] != "invalid or expired token" || fmt.Sprint(out) != fmt.Sprint(unknown) {
		t.Fatalf("retired token: %d %v (unknown: %v)", code, out, unknown)
	}
	if n := a.live(id); n != 2 {
		t.Fatalf("%d live rows, want the retired one in grace and the new one", n)
	}
}

// After the grace the retired token is dead for everything.
func TestRetiredTokenDiesAfterTheGrace(t *testing.T) {
	a := start(t, nil)
	withGrace(t, time.Second)
	old, id := a.renewable(a.newConversation(), 60, 3600)
	code, out := a.swap(old)
	if code != 200 {
		t.Fatalf("exchange: %d %v", code, out)
	}
	fresh := out["token"].(string)
	if code, _ := a.do("GET", client+"/history", old, nil); code != 200 {
		t.Fatalf("inside the grace: %d", code)
	}
	time.Sleep(1200 * time.Millisecond)
	for _, r := range []struct{ method, path string }{{"GET", "/history"}, {"POST", "/grant/exchange"}} {
		if code, out := a.do(r.method, client+r.path, old, nil); code != 401 || out["error"] != "invalid or expired token" {
			t.Errorf("%s after the grace: %d %v", r.path, code, out)
		}
	}
	if code, _ := a.do("GET", client+"/history", fresh, nil); code != 200 {
		t.Fatalf("the new token: %d", code)
	}
	if n := a.live(id); n != 1 {
		t.Fatalf("%d live rows", n)
	}
}

// A live grant that cannot renew is a 409; a token that is dead or was never
// issued is the 401 everyone gets.
func TestExchangeRefusals(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()
	plain, plainID := a.grant(conv, nil)

	if code, out := a.swap(plain); code != 409 || out["error"] != "grant is not renewable" {
		t.Fatalf("non-renewable: %d %v", code, out)
	}
	if code, _ := a.do("GET", client+"/history", plain, nil); code != 200 {
		t.Fatalf("a refused exchange must leave the grant alone: %d", code)
	}
	if code, _ := a.do("DELETE", "/v1/grants/"+plainID, secret, nil); code != 204 {
		t.Fatal("revoke")
	}
	renewed, renewedID := a.renewable(conv, 60, 3600)
	if code, _ := a.do("DELETE", "/v1/grants/"+renewedID, secret, nil); code != 204 {
		t.Fatal("revoke")
	}
	expired, expiredID := a.renewable(conv, 60, 3600)
	if _, err := a.rt.Store.Pool.Exec(context.Background(), "UPDATE grants SET expires_at = now() - interval '1 second' WHERE root_id = $1", expiredID); err != nil {
		t.Fatal(err)
	}
	var bodies []string
	for name, tok := range map[string]string{"revoked non-renewable": plain, "revoked renewable": renewed, "expired": expired, "unknown": "hfy_nope",
		"the secret": secret, "a uuid": expiredID} {
		code, out := a.swap(tok)
		if code != 401 {
			t.Errorf("%s: %d %v", name, code, out)
		}
		bodies = append(bodies, fmt.Sprint(out))
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("dead tokens answer differently: %q", bodies)
		}
	}
}

// The token travels in the Authorization header only: the stream's ?token=
// exception stops at the stream, and odd header forms exchange nothing.
func TestExchangeTokenForms(t *testing.T) {
	a := start(t, nil)
	tok, id := a.renewable(a.newConversation(), 60, 3600)

	do := func(url string, hdr http.Header) int {
		a.t.Helper()
		req, _ := http.NewRequest(http.MethodPost, a.url+url, nil)
		req.Header = hdr
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			a.t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for name, c := range map[string]struct {
		url string
		hdr http.Header
	}{
		"query only":          {exchange + "?token=" + tok, http.Header{}},
		"query beside header": {exchange + "?token=" + tok, http.Header{"Authorization": {"Bearer hfy_nope"}}},
		"empty":               {exchange, http.Header{"Authorization": {"Bearer "}}},
		"scheme only":         {exchange, http.Header{"Authorization": {"Bearer"}}},
		"no scheme":           {exchange, http.Header{"Authorization": {tok}}},
		"other scheme":        {exchange, http.Header{"Authorization": {"Basic " + tok}}},
		"token with suffix":   {exchange, http.Header{"Authorization": {"Bearer " + tok + "x"}}},
		"token truncated":     {exchange, http.Header{"Authorization": {"Bearer " + tok[:20]}}},
		"huge":                {exchange, http.Header{"Authorization": {"Bearer " + strings.Repeat("a", 4000)}}},
		"first header wrong":  {exchange, http.Header{"Authorization": {"Bearer hfy_nope", "Bearer " + tok}}},
	} {
		if code := do(c.url, c.hdr); code != 401 {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	var total int
	a.rt.Store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM grants").Scan(&total)
	if n := a.live(id); n != 1 || total != 1 {
		t.Fatalf("a refused exchange rotated something: %d live rows of %d", n, total)
	}
	// the one form that is right
	if code := do(exchange, http.Header{"Authorization": {"Bearer " + tok}}); code != 200 {
		t.Fatalf("plain header: %d", code)
	}
}

// Two tabs (or a retry) present the same token at once: exactly one gets the
// successor, the rest are refused, and the chain has one new row.
func TestConcurrentExchangeHasOneWinner(t *testing.T) {
	a := start(t, nil)
	tok, id := a.renewable(a.newConversation(), 60, 3600)
	const n = 24
	var wg sync.WaitGroup
	var won atomic.Int32
	codes := make([]int, n)
	begin := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-begin
			code, _ := a.swap(tok)
			codes[i] = code
			if code == 200 {
				won.Add(1)
			}
		})
	}
	close(begin)
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("%d winners: %v", won.Load(), codes)
	}
	for _, c := range codes {
		if c != 200 && c != 401 {
			t.Fatalf("losers must be 401: %v", codes)
		}
	}
	var rows int
	a.rt.Store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM grants WHERE root_id = $1", id).Scan(&rows)
	if rows != 2 {
		t.Fatalf("%d rows in the chain, want 2", rows)
	}
}

// Revoking by the id the backend stored ends the whole chain, a row still in
// its grace included; unknown ids stay 404 and revoking twice is fine.
func TestRevokeEndsTheChain(t *testing.T) {
	a := start(t, nil)
	withGrace(t, 30*time.Second)
	first, id := a.renewable(a.newConversation(), 60, 3600)
	tokens := []string{first}
	for range 3 {
		code, out := a.swap(tokens[len(tokens)-1])
		if code != 200 || out["id"] != id {
			t.Fatalf("exchange: %d %v", code, out)
		}
		tokens = append(tokens, out["token"].(string))
	}
	other, otherID := a.renewable(a.newConversation(), 60, 3600) // a second chain is not touched
	for _, tok := range tokens[len(tokens)-2:] {                 // the head and the one in grace
		if code, _ := a.do("GET", client+"/history", tok, nil); code != 200 {
			t.Fatalf("before revoke: %d", code)
		}
	}
	events, closeStream := a.stream(client+"/events", bearer(tokens[len(tokens)-1]))
	defer closeStream()

	for range 2 {
		if code, _ := a.do("DELETE", "/v1/grants/"+id, secret, nil); code != 204 {
			t.Fatalf("revoke: %d", code)
		}
	}
	for i, tok := range tokens {
		if code, _ := a.do("GET", client+"/history", tok, nil); code != 401 {
			t.Errorf("token %d after revoke: %d", i, code)
		}
		if code, _ := a.swap(tok); code != 401 {
			t.Errorf("token %d exchanged after revoke: %d", i, code)
		}
	}
	if n := a.live(id); n != 0 {
		t.Fatalf("%d live rows after revoke", n)
	}
	waitClosed(t, events, 2*time.Second)
	if code, _ := a.do("GET", client+"/history", other, nil); code != 200 || a.live(otherID) != 1 {
		t.Fatalf("another chain was revoked with it: %d", code)
	}
	for _, bad := range []string{"00000000-0000-4000-8000-000000000000", "not-a-uuid"} {
		if code, _ := a.do("DELETE", "/v1/grants/"+bad, secret, nil); code != 404 {
			t.Fatalf("revoke %s: %d", bad, code)
		}
	}
}

// Whenever a revoke returns, nothing in its chain holds, however exchanges
// race it: neither the row it began with nor one made while it ran.
func TestRevokeWinsAgainstExchange(t *testing.T) {
	a := start(t, nil)
	withGrace(t, 30*time.Second)
	conv := a.newConversation()
	for round := range 25 {
		func() {
			tok, chain := a.renewable(conv, 60, 3600)
			id := chain       // what the revoke names
			if round%2 == 1 { // name the chain by a later row's uuid: the lock is the chain's, whichever id is given
				if code, out := a.swap(tok); code != 200 {
					t.Fatalf("exchange: %d %v", code, out)
				} else {
					tok = out["token"].(string)
				}
				a.rt.Store.Pool.QueryRow(context.Background(), "SELECT uuid::text FROM grants WHERE root_id = $1 AND uuid <> root_id", chain).Scan(&id)
			}
			var wg sync.WaitGroup
			stop := make(chan struct{})
			var once sync.Once
			halt := func() { once.Do(func() { close(stop) }) }
			defer func() { halt(); wg.Wait() }() // also when a check below fails: no request outlives the test
			for range 4 {                        // each follows the chain from the token it has, as clients do
				wg.Go(func() {
					mine := tok
					for {
						select {
						case <-stop:
							return
						default:
						}
						code, out := a.swap(mine)
						switch code {
						case 200:
							mine = out["token"].(string)
						case 401:
							mine = tok // lost a race or revoked: try the original once more
						default:
							t.Errorf("exchange: %d %v", code, out)
							return
						}
					}
				})
			}
			time.Sleep(time.Duration(round%5) * 4 * time.Millisecond)
			if code, _ := a.do("DELETE", "/v1/grants/"+id, secret, nil); code != 204 {
				t.Errorf("revoke: %d", code)
			}
			if n := a.live(chain); n != 0 {
				t.Errorf("round %d: %d live rows right after the revoke returned", round, n)
			}
			time.Sleep(30 * time.Millisecond) // exchanges keep coming
			halt()
			wg.Wait()
			if n := a.live(chain); n != 0 {
				t.Errorf("round %d: %d live rows after the exchanges stopped", round, n)
			}
		}()
		if t.Failed() {
			return
		}
	}
}

// A token never lives past the grant's cap: the last one is cut to it and says
// it cannot renew; at the cap the grant is simply not renewable (409), however
// often it is asked, until the cap passes and it is dead.
func TestExchangeStopsAtTheCap(t *testing.T) {
	a := start(t, nil)
	withGrace(t, 30*time.Second)
	tok, id := a.renewable(a.newConversation(), 60, 90)
	pool := a.rt.Store.Pool
	ctx := context.Background()

	if _, err := pool.Exec(ctx, "UPDATE grants SET renew_until = now() + interval '20 seconds', expires_at = now() + interval '10 seconds' WHERE root_id = $1", id); err != nil {
		t.Fatal(err)
	}
	code, out := a.swap(tok)
	if code != 200 || out["renewable"] != false {
		t.Fatalf("the cut exchange: %d %v", code, out)
	}
	exp, _ := time.Parse(time.RFC3339Nano, out["expires_at"].(string))
	if d := time.Until(exp); d < 15*time.Second || d > 21*time.Second {
		t.Fatalf("expires in %s, want the cap (about 20s)", d)
	}
	tok = out["token"].(string)
	for range 50 { // at the cap: the same answer as a grant that never renewed, and nothing grows
		if code, out := a.swap(tok); code != 409 || out["error"] != "grant is not renewable" {
			t.Fatalf("at the cap: %d %v", code, out)
		}
	}
	var over, rows int
	pool.QueryRow(ctx, "SELECT count(*) FILTER (WHERE expires_at > renew_until), count(*) FROM grants").Scan(&over, &rows)
	if over != 0 || rows != 2 {
		t.Fatalf("%d grants outlive the cap, %d rows", over, rows)
	}
	if code, _ := a.do("GET", client+"/history", tok, nil); code != 200 {
		t.Fatalf("the capped token still works: %d", code)
	}
	if _, err := pool.Exec(ctx, "UPDATE grants SET expires_at = now() - interval '1 second', renew_until = now() - interval '1 second' WHERE root_id = $1", id); err != nil {
		t.Fatal(err)
	}
	if code, _ := a.swap(tok); code != 401 {
		t.Fatalf("past the cap: %d", code)
	}
}

func withGap(t *testing.T, f float64) {
	t.Helper()
	old := httpapi.SetExchangeGap(f)
	t.Cleanup(func() { httpapi.SetExchangeGap(old) })
}

// A grant is exchanged once a quarter of its lifetime has passed; asking sooner
// is a 429 that says how long to wait, in the body and in Retry-After.
func TestExchangeIsRateLimited(t *testing.T) {
	a := start(t, nil)
	withGap(t, 0.25)
	withGrace(t, 30*time.Second)
	tok, id := a.renewable(a.newConversation(), 60, 3600)
	pool := a.rt.Store.Pool
	age := func(secs float64) {
		t.Helper()
		if _, err := pool.Exec(context.Background(), "UPDATE grants SET created_at = now() - make_interval(secs => $2) WHERE root_id = $1 AND revoked_at IS NULL", id, secs); err != nil {
			t.Fatal(err)
		}
	}
	try := func() (int, map[string]any, string) {
		req, _ := http.NewRequest(http.MethodPost, a.url+exchange, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out, resp.Header.Get("Retry-After")
	}
	wait := func(secs float64, header string, out map[string]any) {
		t.Helper()
		got, _ := out["retry_after"].(float64)
		if out["error"] != "too early to renew" || got < secs-1 || got > secs || header != fmt.Sprint(int(got)) {
			t.Fatalf("want %v..%v seconds in the body and Retry-After, got %v / %q", secs-1, secs, out, header)
		}
	}

	code, out, hdr := try() // a quarter of 60 s
	if code != 429 {
		t.Fatalf("a fresh grant: %d %v", code, out)
	}
	wait(15, hdr, out)
	age(10)
	code, out, hdr = try()
	if code != 429 {
		t.Fatalf("at 10 s: %d %v", code, out)
	}
	wait(5, hdr, out)
	age(14.5)
	if code, out, hdr = try(); code != 429 {
		t.Fatalf("at 14.5 s: %d %v", code, out)
	} else if wait(1, hdr, out); out["retry_after"] != 1.0 {
		t.Fatalf("half a second short must say 1: %v", out)
	}
	if n := a.live(id); n != 1 {
		t.Fatalf("a refused exchange rotated: %d live", n)
	}
	// the real client asks at half the lifetime or later, and a clock up to half
	// a lifetime fast brings that to a quarter at worst: this must pass
	age(15.5)
	if code, out, _ = try(); code != 200 {
		t.Fatalf("at a quarter: %d %v", code, out)
	}
	tok = out["token"].(string)
	// the new token starts its own wait
	if code, out, hdr = try(); code != 429 {
		t.Fatalf("right after: %d %v", code, out)
	} else {
		wait(15, hdr, out)
	}
	// dead tokens are not told how long to wait: 401, as ever
	if code, out := a.swap("hfy_nope"); code != 401 || out["retry_after"] != nil {
		t.Fatalf("unknown token: %d %v", code, out)
	}
	if _, err := pool.Exec(context.Background(), "UPDATE grants SET revoked_at = now() WHERE token_hash = sha256($1::bytea)", tok); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := try(); code != 401 || out["retry_after"] != nil {
		t.Fatalf("a retired token: %d %v", code, out)
	}
}

// However hard a token is hammered, a chain does not grow: one exchange per
// quarter lifetime at most, so at most the head and the one in its grace are live.
func TestExchangeHammeringDoesNotGrowAChain(t *testing.T) {
	a := start(t, nil)
	withGap(t, 0.25)
	withGrace(t, 30*time.Second)
	tok, id := a.renewable(a.newConversation(), 60, 3600)
	pool := a.rt.Store.Pool
	if _, err := pool.Exec(context.Background(), "UPDATE grants SET created_at = now() - interval '20 seconds' WHERE root_id = $1", id); err != nil {
		t.Fatal(err)
	}
	var ok, early int
	for range 1000 {
		code, out := a.swap(tok)
		switch code {
		case 200:
			ok++
			tok = out["token"].(string) // a client that always follows the chain
		case 429:
			early++
		default:
			t.Fatalf("%d %v", code, out)
		}
	}
	var rows int
	pool.QueryRow(context.Background(), "SELECT count(*) FROM grants WHERE root_id = $1", id).Scan(&rows)
	if ok != 1 || early != 999 || rows != 2 || a.live(id) > 3 {
		t.Fatalf("%d exchanges, %d refused, %d rows, %d live", ok, early, rows, a.live(id))
	}
}

// Scheme names are case-insensitive, on every route that takes a token.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	a := start(t, nil)
	conv := a.newConversation()
	tok, _ := a.grant(conv, nil)
	call := func(method, path string, headers ...string) int {
		t.Helper()
		req, _ := http.NewRequest(method, a.url+path, strings.NewReader("{}"))
		for _, h := range headers {
			req.Header.Add("Authorization", h)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER", "bEaReR"} {
		if code := call("GET", client+"/history", scheme+" "+tok); code != 200 {
			t.Errorf("%s with a grant token: %d", scheme, code)
		}
		if code := call("POST", "/v1/conversations", scheme+" "+secret); code != 201 {
			t.Errorf("%s with the secret: %d", scheme, code)
		}
	}
	for name, hs := range map[string][]string{
		"no token":           {"Bearer"},
		"scheme, space":      {"bearer "},
		"another scheme":     {"Basic " + tok},
		"bare token":         {tok},
		"short header":       {"Bear"},
		"first is wrong":     {"Bearer hfy_nope", "Bearer " + tok},
		"tab is not a space": {"Bearer	" + tok},
	} {
		if code := call("GET", client+"/history", hs...); code != 401 {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if code := call("GET", client+"/history", "bearer "+tok, "Bearer hfy_nope"); code != 200 {
		t.Errorf("the first header counts: %d", code)
	}
}

// A grant whose agent left the config is refused everywhere, exchange
// included, and nothing rotates.
func TestExchangeFailsClosedForAMissingAgent(t *testing.T) {
	a := start(t, func(c *config.Config) {
		c.Agents = map[string]config.Agent{"store": {Model: "fake/small", System: "A store.", Settings: config.Settings{ContextWindow: 1000}}}
	})
	tok, id := a.grant(a.newConversation(), map[string]any{"agent": "store", "expires_in": 60, "max_lifetime": 3600})
	code, out := a.swap(tok)
	if code != 200 {
		t.Fatalf("exchange: %d %v", code, out)
	}
	tok = out["token"].(string)
	a.rt.Store.Pool.Exec(context.Background(), "UPDATE grants SET agent = 'gone' WHERE root_id = $1", id)
	if code, out := a.swap(tok); code != 403 {
		t.Fatalf("exchange for a missing agent: %d %v", code, out)
	}
	var rows int
	a.rt.Store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM grants WHERE root_id = $1", id).Scan(&rows)
	if rows != 2 {
		t.Fatalf("%d rows: a refused exchange rotated", rows)
	}
}

// The token is in the response and nowhere else: not in logs, not in the table.
func TestExchangeNeverLogsTheToken(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := start(t, nil)
	old, _ := a.renewable(a.newConversation(), 60, 3600)
	req, _ := http.NewRequest(http.MethodPost, a.url+exchange, nil)
	req.Header.Set("Authorization", "Bearer "+old)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	fresh, _ := out["token"].(string)
	if resp.StatusCode != http.StatusOK || fresh == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange: %d %v, Cache-Control %q", resp.StatusCode, out, resp.Header.Get("Cache-Control"))
	}
	a.swap(old)
	a.swap(fresh + "x")
	for _, tok := range []string{old, fresh} {
		if strings.Contains(logs.String(), strings.TrimPrefix(tok, "hfy_")) {
			t.Fatalf("a token is in the logs:\n%s", logs.String())
		}
		var n int
		a.rt.Store.Pool.QueryRow(context.Background(), "SELECT count(*) FROM grants g WHERE g::text LIKE '%' || $1 || '%'",
			strings.TrimPrefix(tok, "hfy_")).Scan(&n)
		if n != 0 {
			t.Fatal("a token is stored")
		}
	}
}

// A stream outlives an exchange: the client reconnects with the new token
// from the last event it saw and loses and repeats nothing, while the old
// stream ends once the grace is over.
func TestStreamSurvivesAnExchange(t *testing.T) {
	a := start(t, nil, fakeprov.Step{Content: "one"}, fakeprov.Step{Content: "two"})
	withGrace(t, time.Second)
	tok, _ := a.renewable(a.newConversation(), 60, 3600)

	oldStream, closeOld := a.stream(client+"/events", bearer(tok))
	defer closeOld()
	if code, _ := a.do("POST", client+"/messages", tok, map[string]any{"text": "first"}); code != 202 {
		t.Fatal("post")
	}
	got := waitFor(t, oldStream, completed)
	ids := map[string]bool{}
	for _, ev := range got {
		ids[ev.ID] = true
	}
	last := got[len(got)-1].ID

	code, out := a.swap(tok)
	if code != 200 {
		t.Fatalf("exchange: %d %v", code, out)
	}
	fresh := out["token"].(string)
	newStream, closeNew := a.stream(client+"/events", map[string]string{"Authorization": "Bearer " + fresh, "Last-Event-ID": last})
	defer closeNew()
	if code, _ := a.do("POST", client+"/messages", fresh, map[string]any{"text": "second"}); code != 202 {
		t.Fatal("post with the new token")
	}
	more := waitFor(t, newStream, func(ev sseEvent) bool { return completed(ev) && !ids[ev.ID] })
	sawSecond := false
	for _, ev := range more {
		if ids[ev.ID] {
			t.Fatalf("event %s %s delivered twice", ev.Type, ev.ID)
		}
		ids[ev.ID] = true
		if ev.Type == "message.text" && strings.Contains(fmt.Sprint(ev.Env["data"]), "two") {
			sawSecond = true
		}
	}
	if !sawSecond {
		t.Fatalf("the second reply did not arrive on the new stream: %d events", len(more))
	}
	waitClosed(t, oldStream, 3*time.Second) // grace over: the old stream ends
}
