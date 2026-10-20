package hfy_test

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/alash3al/hfy"
	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/db/dbtest"
)

// A failed listen must stop the process, not leave it hanging on its workers.
func TestServeReturnsWhenTheAddressIsTaken(t *testing.T) {
	store := dbtest.New(t, nil)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	cfg := &config.Config{
		Server:    config.Server{Listen: taken.Addr().String(), Secret: "s"},
		Storage:   config.Storage{URL: store.Pool.Config().ConnString()},
		Queue:     config.Queue{Workers: 1, Lease: config.Duration(5 * time.Second), MaxAttempts: 1},
		Retention: config.Retention{Trace: config.Duration(time.Hour)},
		Providers: map[string]config.Provider{"p": {URL: "http://127.0.0.1:1/v1"}},
		Agent:     config.Agent{Model: "p/m", Settings: config.Settings{ContextWindow: 100}},
	}
	done := make(chan error, 1)
	go func() { done <- hfy.New(cfg).Serve(context.Background(), true) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want the listen error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve hung after the listener failed")
	}
}
