package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeAPI is an in-memory Discord stand-in: channels and roles by id, plus a
// per-channel message list. No network.
type fakeAPI struct {
	channels map[string]*Channel
	roles    map[string][]Role // guildID -> roles
	messages map[string][]Message
	threads  map[string][]Channel // guildID -> active threads
}

func (f *fakeAPI) Channel(id string) (*Channel, error) {
	if c, ok := f.channels[id]; ok {
		return c, nil
	}
	return nil, errNotFound
}
func (f *fakeAPI) GuildChannels(g string) ([]Channel, error) {
	var out []Channel
	for _, c := range f.channels {
		if c.GuildID == g {
			out = append(out, *c)
		}
	}
	return out, nil
}
func (f *fakeAPI) GuildRoles(g string) ([]Role, error) { return f.roles[g], nil }
func (f *fakeAPI) ChannelMessages(id string, limit int, before, after string) ([]Message, error) {
	return f.messages[id], nil
}
func (f *fakeAPI) ActiveThreads(g string) ([]Channel, error) { return f.threads[g], nil }

const guild = "G"

// everyoneCanView with @everyone base granting VIEW (1<<10 = 1024).
func baseRoles() map[string][]Role {
	return map[string][]Role{guild: {{ID: guild, Permissions: "1024"}}}
}

func TestParsePerm(t *testing.T) {
	if got := parsePerm("1024"); got != 1024 {
		t.Fatalf("parsePerm(1024) = %d", got)
	}
	if got := parsePerm(""); got != 0 {
		t.Fatalf("parsePerm(empty) = %d, want 0", got)
	}
	if got := parsePerm("garbage"); got != 0 {
		t.Fatalf("parsePerm(garbage) = %d, want 0 (default-deny)", got)
	}
}

func TestViewable(t *testing.T) {
	cases := []struct {
		name  string
		base  int64
		chain [][2]int64 // {allow, deny}
		want  bool
	}{
		{"base grants view", permView, nil, true},
		{"base lacks view", 0, nil, false},
		{"admin overrides missing view", permAdmin, nil, true},
		{"channel deny removes view", permView, [][2]int64{{0, permView}}, false},
		{"channel allow restores view", 0, [][2]int64{{permView, 0}}, true},
		{"category denies, channel re-allows (order matters)", permView,
			[][2]int64{{0, permView}, {permView, 0}}, true},
		{"category allows, channel denies", 0,
			[][2]int64{{permView, 0}, {0, permView}}, false},
		{"admin beats channel deny", permAdmin, [][2]int64{{0, permView}}, true},
	}
	for _, c := range cases {
		if got := viewable(c.base, c.chain); got != c.want {
			t.Errorf("%s: viewable(%d,%v) = %v, want %v", c.name, c.base, c.chain, got, c.want)
		}
	}
}

func TestEveryoneOverwrite(t *testing.T) {
	ch := &Channel{
		PermissionOverwrites: []Overwrite{
			{ID: "someuser", Type: 1, Allow: "1024", Deny: "0"},
			{ID: guild, Type: 0, Allow: "0", Deny: "1024"},
		},
	}
	a, d := everyoneOverwrite(guild, ch)
	if a != 0 || d != permView {
		t.Fatalf("everyoneOverwrite = (allow %d, deny %d), want (0, %d)", a, d, permView)
	}
	// No @everyone overwrite -> zero.
	a, d = everyoneOverwrite(guild, &Channel{})
	if a != 0 || d != 0 {
		t.Fatalf("everyoneOverwrite(none) = (%d,%d), want (0,0)", a, d)
	}
}

func publicTextChannel(id string) *Channel {
	return &Channel{ID: id, Name: "chan-" + id, Type: typeText, GuildID: guild}
}

func TestChannelPublic(t *testing.T) {
	pub := publicTextChannel("c1")
	priv := &Channel{ID: "c2", Name: "secret", Type: typeText, GuildID: guild,
		PermissionOverwrites: []Overwrite{{ID: guild, Type: 0, Deny: "1024"}}}
	otherGuild := &Channel{ID: "c3", Name: "elsewhere", Type: typeText, GuildID: "OTHER"}
	privThread := &Channel{ID: "t1", Type: typePrivateThread, GuildID: guild, ParentID: "c1"}
	pubThread := &Channel{ID: "t2", Type: typePublicThread, GuildID: guild, ParentID: "c1"}
	threadOnPriv := &Channel{ID: "t3", Type: typePublicThread, GuildID: guild, ParentID: "c2"}

	f := &fakeAPI{
		roles:    baseRoles(),
		channels: map[string]*Channel{"c1": pub, "c2": priv, "c3": otherGuild, "t1": privThread, "t2": pubThread, "t3": threadOnPriv},
	}
	b := &broker{api: f, guildID: guild}

	cases := []struct {
		name string
		ch   *Channel
		want bool
	}{
		{"public text channel", pub, true},
		{"private channel (@everyone view denied)", priv, false},
		{"channel in another guild", otherGuild, false},
		{"private thread always rejected", privThread, false},
		{"public thread inherits public parent", pubThread, true},
		{"public thread on private parent is private", threadOnPriv, false},
	}
	for _, c := range cases {
		got, err := b.channelPublic(c.ch)
		if err != nil {
			t.Fatalf("%s: unexpected err %v", c.name, err)
		}
		if got != c.want {
			t.Errorf("%s: channelPublic = %v, want %v", c.name, got, c.want)
		}
	}
}

func newTestServer() (*broker, http.Handler) {
	pub := publicTextChannel("c1")
	priv := &Channel{ID: "c2", Name: "secret", Type: typeText, GuildID: guild,
		PermissionOverwrites: []Overwrite{{ID: guild, Type: 0, Deny: "1024"}}}
	cat := &Channel{ID: "cat", Type: typeCategory, GuildID: guild}
	voice := &Channel{ID: "v", Name: "voice", Type: 2, GuildID: guild}
	f := &fakeAPI{
		roles:    baseRoles(),
		channels: map[string]*Channel{"c1": pub, "c2": priv, "cat": cat, "v": voice},
		messages: map[string][]Message{
			"c1": {
				{ID: "m1", Content: "hello world"},
				{ID: "m2", Content: "fix the bug"},
			},
		},
	}
	b := &broker{api: f, guildID: guild}
	return b, b.handler()
}

func TestHTTPMessagesPublic(t *testing.T) {
	_, h := newTestServer()
	r := httptest.NewRequest("GET", "/channels/c1/messages", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var msgs []Message
	if err := json.Unmarshal(w.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
}

func TestHTTPMessagesContainsFilter(t *testing.T) {
	_, h := newTestServer()
	r := httptest.NewRequest("GET", "/channels/c1/messages?contains=bug", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var msgs []Message
	_ = json.Unmarshal(w.Body.Bytes(), &msgs)
	if len(msgs) != 1 || msgs[0].ID != "m2" {
		t.Fatalf("contains=bug returned %+v, want only m2", msgs)
	}
}

func TestHTTPPrivateChannelForbidden(t *testing.T) {
	_, h := newTestServer()
	r := httptest.NewRequest("GET", "/channels/c2/messages", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
}

func TestHTTPWriteMethodRejected(t *testing.T) {
	_, h := newTestServer()
	r := httptest.NewRequest("POST", "/channels/c1/messages", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
}

func TestHTTPListChannelsFiltersNonPublic(t *testing.T) {
	_, h := newTestServer()
	r := httptest.NewRequest("GET", "/channels", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	var chans []Channel
	_ = json.Unmarshal(w.Body.Bytes(), &chans)
	for _, c := range chans {
		if c.ID == "c2" || c.ID == "cat" || c.ID == "v" {
			t.Fatalf("listing leaked non-public/non-text channel %q (type %d)", c.ID, c.Type)
		}
	}
	// The one public text channel must be present.
	found := false
	for _, c := range chans {
		if c.ID == "c1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("public text channel c1 missing from listing: %+v", chans)
	}
}
