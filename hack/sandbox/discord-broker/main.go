// Command discord-broker is a read-only Discord HTTP broker for quack guest
// sandboxes. It runs as a per-session sidecar that holds quack's bot token and
// exposes a tiny read-only API the agent reaches over the internal network — so
// no Discord credential ever enters the agent container.
//
// Two hard gates run on every request, default-deny:
//   - guild scope: only the configured GUILD_ID is reachable;
//   - public-only: a channel is served only if @everyone can VIEW_CHANNEL it
//     (private channels, private threads, DMs, and other guilds are refused).
//
// Only GET is routed; there are no write endpoints by construction.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Discord permission bits (https://discord.com/developers/docs/topics/permissions).
const (
	permView  int64 = 1 << 10 // VIEW_CHANNEL
	permAdmin int64 = 1 << 3  // ADMINISTRATOR (bypasses overwrites)
)

// Discord channel types (https://discord.com/developers/docs/resources/channel).
const (
	typeText          = 0
	typeDM            = 1
	typeVoice         = 2
	typeGroupDM       = 3
	typeCategory      = 4
	typeNews          = 5
	typeNewsThread    = 10
	typePublicThread  = 11
	typePrivateThread = 12
	typeForum         = 15
)

var errNotFound = errors.New("not found")

type Channel struct {
	ID                   string      `json:"id"`
	Name                 string      `json:"name"`
	Type                 int         `json:"type"`
	GuildID              string      `json:"guild_id,omitempty"`
	ParentID             string      `json:"parent_id,omitempty"`
	PermissionOverwrites []Overwrite `json:"permission_overwrites,omitempty"`
}

type Overwrite struct {
	ID    string `json:"id"`
	Type  int    `json:"type"` // 0 = role, 1 = member
	Allow string `json:"allow"`
	Deny  string `json:"deny"`
}

type Role struct {
	ID          string `json:"id"`
	Permissions string `json:"permissions"`
}

type Author struct {
	ID       string `json:"id"`
	Username string `json:"username"`
}

type Message struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Timestamp string `json:"timestamp"`
	Author    Author `json:"author,omitempty"`
}

// discordAPI is the slice of Discord's REST surface the broker needs. Abstracted
// so the handler is testable with an in-memory fake (no network).
type discordAPI interface {
	Channel(id string) (*Channel, error)
	GuildChannels(guildID string) ([]Channel, error)
	GuildRoles(guildID string) ([]Role, error)
	ChannelMessages(id string, limit int, before, after string) ([]Message, error)
	ActiveThreads(guildID string) ([]Channel, error)
}

type broker struct {
	api     discordAPI
	guildID string
}

// parsePerm decodes a Discord permission bitfield (sent as a decimal string).
// Unparseable input is treated as no permissions (default-deny).
func parsePerm(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// viewable reports whether @everyone ends up with VIEW_CHANNEL after applying a
// chain of permission overwrites (category first, then channel) over a base
// permission set. ADMINISTRATOR in the base grants everything and bypasses
// overwrites, matching Discord's own evaluation.
func viewable(base int64, chain [][2]int64) bool {
	if base&permAdmin != 0 {
		return true
	}
	perms := base
	for _, ow := range chain {
		allow, deny := ow[0], ow[1]
		perms = (perms &^ deny) | allow
	}
	return perms&permView != 0
}

// everyoneOverwrite returns the (allow, deny) bits of the @everyone role
// overwrite (role id == guild id) on a channel, or (0,0) if absent.
func everyoneOverwrite(guildID string, ch *Channel) (allow, deny int64) {
	for _, ow := range ch.PermissionOverwrites {
		if ow.Type == 0 && ow.ID == guildID {
			return parsePerm(ow.Allow), parsePerm(ow.Deny)
		}
	}
	return 0, 0
}

// everyoneBase fetches the @everyone role's base permissions for the guild.
func (b *broker) everyoneBase() (int64, error) {
	roles, err := b.api.GuildRoles(b.guildID)
	if err != nil {
		return 0, err
	}
	for _, r := range roles {
		if r.ID == b.guildID { // @everyone role id == guild id
			return parsePerm(r.Permissions), nil
		}
	}
	return 0, nil // no @everyone role found -> default-deny
}

// channelPublic reports whether a channel is in the configured guild and
// readable by @everyone. Threads inherit their parent channel's visibility;
// private threads, DMs, and other guilds are always rejected.
func (b *broker) channelPublic(ch *Channel) (bool, error) {
	switch ch.Type {
	case typePrivateThread:
		return false, nil
	case typePublicThread, typeNewsThread:
		parent, err := b.api.Channel(ch.ParentID)
		if err != nil {
			return false, err
		}
		return b.channelPublic(parent)
	case typeDM, typeGroupDM:
		return false, nil
	}
	if ch.GuildID != "" && ch.GuildID != b.guildID {
		return false, nil
	}
	base, err := b.everyoneBase()
	if err != nil {
		return false, err
	}
	var chain [][2]int64
	if ch.ParentID != "" {
		parent, err := b.api.Channel(ch.ParentID)
		if err != nil {
			return false, err
		}
		if parent.Type == typeCategory {
			a, d := everyoneOverwrite(b.guildID, parent)
			chain = append(chain, [2]int64{a, d})
		}
	}
	a, d := everyoneOverwrite(b.guildID, ch)
	chain = append(chain, [2]int64{a, d})
	return viewable(base, chain), nil
}

func (b *broker) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		switch {
		case len(parts) == 1 && parts[0] == "channels":
			b.listChannels(w, r)
		case len(parts) == 2 && parts[0] == "channels":
			b.gatedChannel(w, parts[1], b.getChannel)
		case len(parts) == 3 && parts[0] == "channels" && parts[2] == "messages":
			b.gatedChannel(w, parts[1], func(w http.ResponseWriter, ch *Channel) {
				b.getMessages(w, r, ch)
			})
		case len(parts) == 3 && parts[0] == "channels" && parts[2] == "threads":
			b.gatedChannel(w, parts[1], b.getThreads)
		default:
			http.NotFound(w, r)
		}
	})
}

// gatedChannel fetches a channel, enforces the guild+public gates, and only then
// calls fn. Any gate failure is a 403; a missing channel is 404.
func (b *broker) gatedChannel(w http.ResponseWriter, id string, fn func(http.ResponseWriter, *Channel)) {
	ch, err := b.api.Channel(id)
	if errors.Is(err, errNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	pub, err := b.channelPublic(ch)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if !pub {
		http.Error(w, "forbidden: not a public channel of the allowed guild", http.StatusForbidden)
		return
	}
	fn(w, ch)
}

func (b *broker) getChannel(w http.ResponseWriter, ch *Channel) {
	writeJSON(w, Channel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID})
}

func (b *broker) getMessages(w http.ResponseWriter, r *http.Request, ch *Channel) {
	q := r.URL.Query()
	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = n
	}
	if limit > 100 {
		limit = 100
	}
	msgs, err := b.api.ChannelMessages(ch.ID, limit, q.Get("before"), q.Get("after"))
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	if sub := q.Get("contains"); sub != "" {
		sub = strings.ToLower(sub)
		filtered := msgs[:0]
		for _, m := range msgs {
			if strings.Contains(strings.ToLower(m.Content), sub) {
				filtered = append(filtered, m)
			}
		}
		msgs = filtered
	}
	writeJSON(w, msgs)
}

func (b *broker) getThreads(w http.ResponseWriter, ch *Channel) {
	threads, err := b.api.ActiveThreads(b.guildID)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	out := []Channel{}
	for _, t := range threads {
		if t.ParentID != ch.ID || t.Type == typePrivateThread {
			continue
		}
		out = append(out, Channel{ID: t.ID, Name: t.Name, Type: t.Type, ParentID: t.ParentID})
	}
	writeJSON(w, out)
}

func (b *broker) listChannels(w http.ResponseWriter, _ *http.Request) {
	chans, err := b.api.GuildChannels(b.guildID)
	if err != nil {
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}
	out := []Channel{}
	for i := range chans {
		ch := chans[i]
		switch ch.Type {
		case typeText, typeNews, typeForum:
		default:
			continue // skip categories, voice, threads, etc.
		}
		pub, err := b.channelPublic(&ch)
		if err != nil || !pub {
			continue
		}
		out = append(out, Channel{ID: ch.ID, Name: ch.Name, Type: ch.Type, ParentID: ch.ParentID})
	}
	writeJSON(w, out)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// --- real Discord REST client ---

type httpDiscord struct {
	token  string
	base   string
	client *http.Client
}

func (d httpDiscord) get(path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, d.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+d.token)
	req.Header.Set("User-Agent", "quack-discord-broker/1.0")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden {
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("discord GET %s: %d %s", path, resp.StatusCode, body)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (d httpDiscord) Channel(id string) (*Channel, error) {
	var c Channel
	if err := d.get("/channels/"+id, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

func (d httpDiscord) GuildChannels(g string) ([]Channel, error) {
	var c []Channel
	return c, d.get("/guilds/"+g+"/channels", &c)
}

func (d httpDiscord) GuildRoles(g string) ([]Role, error) {
	var r []Role
	return r, d.get("/guilds/"+g+"/roles", &r)
}

func (d httpDiscord) ChannelMessages(id string, limit int, before, after string) ([]Message, error) {
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	if before != "" {
		q.Set("before", before)
	}
	if after != "" {
		q.Set("after", after)
	}
	var m []Message
	return m, d.get("/channels/"+id+"/messages?"+q.Encode(), &m)
}

func (d httpDiscord) ActiveThreads(g string) ([]Channel, error) {
	var resp struct {
		Threads []Channel `json:"threads"`
	}
	return resp.Threads, d.get("/guilds/"+g+"/threads/active", &resp)
}

func main() {
	token := os.Getenv("DISCORD_BOT_TOKEN")
	guildID := os.Getenv("GUILD_ID")
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if token == "" || guildID == "" {
		log.Fatal("discord-broker: DISCORD_BOT_TOKEN and GUILD_ID are required")
	}
	b := &broker{
		api: httpDiscord{
			token:  token,
			base:   "https://discord.com/api/v10",
			client: &http.Client{Timeout: 15 * time.Second},
		},
		guildID: guildID,
	}
	log.Printf("discord-broker on %s, guild=%s (read-only, public channels)", addr, guildID)
	log.Fatal(http.ListenAndServe(addr, b.handler()))
}
