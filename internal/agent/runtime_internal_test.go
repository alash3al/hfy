package agent

import (
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"gocloud.dev/blob/memblob"

	"github.com/alash3al/hfy/internal/config"
	"github.com/alash3al/hfy/internal/files"
	"github.com/alash3al/hfy/internal/types"
)

func signingRuntime(t *testing.T) *Runtime {
	t.Helper()
	return &Runtime{Cfg: &config.Config{
		Server: config.Server{Secret: "s"},
		Filesystem: &config.Filesystem{DSN: "file://", PublicURL: "https://h.example",
			DownloadTTL: config.Duration(15 * time.Minute)},
	}, bucket: memblob.OpenBucket(nil)}
}

func expOf(t *testing.T, u string) (key, sig string, exp int64) {
	t.Helper()
	q, err := url.Parse(u)
	if err != nil {
		t.Fatalf("bad url %q: %v", u, err)
	}
	v := q.Query()
	n, err := strconv.ParseInt(v.Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("bad exp in %q: %v", u, err)
	}
	return strings.TrimPrefix(q.Path, "/v1/files/"), v.Get("sig"), n
}

// downloadURL signs with the get-only domain, stays stable within the TTL
// window (the provider's prompt cache must not be busted), and the cache
// lets go of expired entries.
func TestDownloadURLSignsGetAndCaches(t *testing.T) {
	ctx := t.Context()
	rt := signingRuntime(t)
	key := "conversations/c/x.png"
	key, sig, exp := expOf(t, rt.downloadURL(ctx, key))
	if key != "conversations/c/x.png" {
		t.Fatalf("bad url key: %q", key)
	}
	if !files.VerifyGet(rt.Cfg.Server.Secret, key, sig, time.Unix(exp, 0)) {
		t.Error("sig is not a valid get signature")
	}
	if files.VerifyPut(rt.Cfg.Server.Secret, key, "image/png", sig, time.Unix(exp, 0)) {
		t.Error("a get signature may not authorize an upload")
	}
	if again := rt.downloadURL(ctx, key); again == "" || !strings.Contains(again, "sig="+sig) {
		t.Error("the same URL must be reused within the TTL window")
	}
	// expiring entries are pruned after enough stores
	rt.signedURLs.Store("gone", signedURL{"", time.Now().Add(-time.Second)})
	for range signedURLCachePruneEvery {
		rt.storeSignedURL("k", "u", time.Now().Add(time.Hour))
	}
	if _, ok := rt.signedURLs.Load("gone"); ok {
		t.Error("expired entry not pruned")
	}
}

// Only an image that is actually sent gets a signed URL: a non-image file is
// named by its key, and even a current image is a placeholder without vision.
func TestUserMessageSignsOnlyWhatItSends(t *testing.T) {
	ctx := t.Context()
	rt := signingRuntime(t)
	m := types.MessageText{Attachments: []types.Attachment{
		{File: "conversations/c/x.png", Mime: "image/png"},
		{File: "conversations/c/a.pdf", Mime: "application/pdf"},
	}}
	spec := &Spec{Settings: config.Settings{Vision: true}}

	later := rt.userMessage(ctx, m, false, spec)
	if len(later.Parts) != 0 || strings.Contains(later.Content, "sig=") ||
		strings.Contains(later.Content, "https://") || !strings.Contains(later.Content, "[image: image/png]") {
		t.Fatalf("a later turn must use placeholders only: %+v", later)
	}

	now := rt.userMessage(ctx, m, true, spec)
	if len(now.Parts) != 2 || now.Parts[1].ImageURL == nil {
		t.Fatalf("the current turn must send the image: %+v", now)
	}
	key, sig, exp := expOf(t, now.Parts[1].ImageURL.URL)
	if key != "conversations/c/x.png" || !files.VerifyGet(rt.Cfg.Server.Secret, key, sig, time.Unix(exp, 0)) {
		t.Fatalf("the sent image is not get-signed: %s", now.Parts[1].ImageURL.URL)
	}
	if text := now.Parts[0].Text; !strings.Contains(text, "[attachment: conversations/c/a.pdf (application/pdf)]") ||
		strings.Contains(text, "sig=") {
		t.Fatalf("the pdf must stay key-named and unsigned: %q", text)
	}

	blind := rt.userMessage(ctx, m, true, &Spec{})
	if strings.Contains(blind.Content, "sig=") || strings.Contains(blind.Content, "https://") {
		t.Fatalf("no vision, no signing: %+v", blind)
	}
}

// hfy.ask_user reaches only top agents that opt in: never sub-agents (they
// cannot pause) nor other specs.
func TestAskUserIsOptInAndTopOnly(t *testing.T) {
	cfg := &config.Config{
		Server: config.Server{Secret: "s"},
		Providers: map[string]config.Provider{
			"fake": {Type: "mock", Mock: config.MockProvider{Reply: "ok", EmbedDimensions: 4}},
		},
		Embeddings: &config.Embeddings{Model: "fake/e", Dimensions: 4},
		Agent: config.Agent{Model: "fake/m", Settings: config.Settings{ContextWindow: 1000},
			AskUser: true, Approval: &config.Approval{Tools: []string{"x.y"}}},
		Subagents: map[string]config.Subagent{
			"helper": {Description: "helps", Model: "fake/m", Settings: config.Settings{ContextWindow: 1000}},
		},
	}
	cfg.SetDefaults()
	rt, err := NewRuntime(cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !rt.agents["main"].Builtins["ask_user"] || !rt.agents["main"].Top {
		t.Fatalf("an opted-in top agent must have ask_user: %+v", rt.agents["main"])
	}
	if b, ok := rt.findBuiltin("hfy.ask_user"); !ok || b.tool.Name != "ask_user" || b.tool.Schema == nil {
		t.Fatalf("ask_user not registered: %+v %+v", b, ok)
	}
	if rt.subs["helper"].Builtins["ask_user"] {
		t.Fatal("a sub-agent must never get ask_user")
	}
}

// cleanupUploads deletes expired upload slots only: a fresh slot stays and
// the verified copies under conversations/ are never touched.
func TestCleanupUploads(t *testing.T) {
	ctx := t.Context()
	rt := signingRuntime(t)
	rt.Cfg.Filesystem.UploadTTL = config.Duration(time.Hour)
	b := rt.bucket
	write := func(key string) {
		if err := b.WriteAll(ctx, key, []byte("x"), nil); err != nil {
			t.Fatal(err)
		}
	}
	read := func(key string) bool { _, err := b.ReadAll(ctx, key); return err == nil }
	write("uploads/old.png")
	write("conversations/perm.png")
	rt.cleanupUploads(ctx, time.Now()) // nothing is old yet
	if !read("uploads/old.png") {
		t.Fatal("a fresh slot was deleted")
	}
	rt.cleanupUploads(ctx, time.Now().Add(3*time.Hour)) // old now, in fake time
	if read("uploads/old.png") {
		t.Fatal("an expired slot was not deleted")
	}
	if !read("conversations/perm.png") {
		t.Fatal("verified copies must not be touched")
	}
	write("uploads/new.png")
	rt.cleanupUploads(ctx, time.Now()) // a real "now" keeps what was just uploaded
	if !read("uploads/new.png") {
		t.Fatal("a fresh slot was deleted")
	}

	// without a bucket the cleanup is a no-op
	(&Runtime{Cfg: rt.Cfg}).cleanupUploads(ctx, time.Now().Add(3*time.Hour))
}
