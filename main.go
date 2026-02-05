package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/telegram/message"
	"github.com/gotd/td/telegram/updates"
	"github.com/gotd/td/tg"
)

const (
	CommandPrefix = "."
)

type AFKState struct {
	mu      sync.RWMutex
	isAFK   bool
	reason  string
	since   time.Time
	pings   map[int64]int // chatID -> ping count
}

func NewAFKState() *AFKState {
	return &AFKState{
		pings: make(map[int64]int),
	}
}

func (a *AFKState) SetAFK(reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.isAFK = true
	a.reason = reason
	a.since = time.Now()
	a.pings = make(map[int64]int)
}

func (a *AFKState) UnsetAFK() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.isAFK = false
	a.reason = ""
	a.pings = make(map[int64]int)
}

func (a *AFKState) IsAFK() bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isAFK
}

func (a *AFKState) GetInfo() (string, time.Time) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.reason, a.since
}

func (a *AFKState) IncrementPing(chatID int64) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pings[chatID]++
	return a.pings[chatID]
}

type Userbot struct {
	client *telegram.Client
	api    *tg.Client
	sender *message.Sender
	afk    *AFKState
	self   *tg.User
}

func NewUserbot(client *telegram.Client) *Userbot {
	return &Userbot{
		client: client,
		afk:    NewAFKState(),
	}
}

func (u *Userbot) handleUpdate(ctx context.Context, update tg.UpdatesClass) error {
	switch upd := update.(type) {
	case *tg.Updates:
		for _, msg := range upd.Updates {
			u.processUpdate(ctx, msg)
		}
	case *tg.UpdateShort:
		u.processUpdate(ctx, upd.Update)
	}
	return nil
}

func (u *Userbot) processUpdate(ctx context.Context, update tg.UpdateClass) {
	switch upd := update.(type) {
	case *tg.UpdateNewMessage:
		u.handleMessage(ctx, upd.Message)
	case *tg.UpdateEditMessage:
		u.handleMessage(ctx, upd.Message)
	}
}

func (u *Userbot) handleMessage(ctx context.Context, msg tg.MessageClass) {
	message, ok := msg.(*tg.Message)
	if !ok || message.Out {
		return
	}

	text := message.Message
	chatID := u.getChatID(message.PeerID)

	// Handle AFK mentions in DMs
	if u.afk.IsAFK() && u.isDM(message.PeerID) {
		reason, since := u.afk.GetInfo()
		duration := time.Since(since)
		
		afkMsg := fmt.Sprintf("🌙 I'm currently AFK")
		if reason != "" {
			afkMsg += fmt.Sprintf(": %s", reason)
		}
		afkMsg += fmt.Sprintf("\n⏰ Since: %s ago", formatDuration(duration))
		
		count := u.afk.IncrementPing(chatID)
		if count == 1 {
			u.sender.Reply(u.api, message).Text(ctx, afkMsg)
		}
	}

	// Handle commands from self
	if !u.isFromSelf(message) {
		return
	}

	if !strings.HasPrefix(text, CommandPrefix) {
		// If user is AFK and sends a message, unset AFK
		if u.afk.IsAFK() {
			u.afk.UnsetAFK()
			u.sender.Reply(u.api, message).Text(ctx, "✅ AFK mode disabled")
		}
		return
	}

	parts := strings.Fields(text)
	if len(parts) == 0 {
		return
	}

	command := strings.TrimPrefix(parts[0], CommandPrefix)
	args := parts[1:]

	switch command {
	case "del", "delete":
		u.handleDelete(ctx, message)
	case "alive":
		u.handleAlive(ctx, message)
	case "afk":
		u.handleAFK(ctx, message, args)
	}
}

func (u *Userbot) handleDelete(ctx context.Context, msg *tg.Message) {
	// Delete the command message
	peer := u.getPeer(msg.PeerID)
	u.api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
		ID: []int{msg.ID},
	})

	// If replying to a message, delete that too
	if msg.ReplyTo != nil {
		if replyTo, ok := msg.ReplyTo.(*tg.MessageReplyHeader); ok {
			u.api.MessagesDeleteMessages(ctx, &tg.MessagesDeleteMessagesRequest{
				ID: []int{replyTo.ReplyToMsgID},
			})
		}
	}

	log.Printf("Deleted message(s) in chat %v", peer)
}

func (u *Userbot) handleAlive(ctx context.Context, msg *tg.Message) {
	uptime := time.Since(time.Now().Add(-5 * time.Minute)) // Simplified uptime
	
	aliveMsg := fmt.Sprintf(
		"🤖 **Userbot Status**\n\n"+
			"✅ Online and Running\n"+
			"⏱ Uptime: %s\n"+
			"📝 Commands: .del, .alive, .afk",
		formatDuration(uptime),
	)

	u.sender.Reply(u.api, msg).Text(ctx, aliveMsg)
}

func (u *Userbot) handleAFK(ctx context.Context, msg *tg.Message, args []string) {
	if u.afk.IsAFK() {
		u.afk.UnsetAFK()
		u.sender.Reply(u.api, msg).Text(ctx, "✅ AFK mode disabled")
		return
	}

	reason := strings.Join(args, " ")
	u.afk.SetAFK(reason)

	afkMsg := "🌙 AFK mode enabled"
	if reason != "" {
		afkMsg += fmt.Sprintf(": %s", reason)
	}

	u.sender.Reply(u.api, msg).Text(ctx, afkMsg)
}

func (u *Userbot) getChatID(peerID tg.PeerClass) int64 {
	switch peer := peerID.(type) {
	case *tg.PeerUser:
		return peer.UserID
	case *tg.PeerChat:
		return peer.ChatID
	case *tg.PeerChannel:
		return peer.ChannelID
	}
	return 0
}

func (u *Userbot) getPeer(peerID tg.PeerClass) tg.InputPeerClass {
	switch peer := peerID.(type) {
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: peer.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: peer.ChatID}
	case *tg.PeerChannel:
		return &tg.InputPeerChannel{ChannelID: peer.ChannelID}
	}
	return &tg.InputPeerEmpty{}
}

func (u *Userbot) isDM(peerID tg.PeerClass) bool {
	_, ok := peerID.(*tg.PeerUser)
	return ok
}

func (u *Userbot) isFromSelf(msg *tg.Message) bool {
	if u.self == nil {
		return false
	}
	return msg.FromID != nil && u.getSenderID(msg.FromID) == u.self.ID
}

func (u *Userbot) getSenderID(fromID tg.PeerClass) int64 {
	switch peer := fromID.(type) {
	case *tg.PeerUser:
		return peer.UserID
	}
	return 0
}

func formatDuration(d time.Duration) string {
	hours := int(d.Hours())
	minutes := int(d.Minutes()) % 60
	
	if hours > 0 {
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
	return fmt.Sprintf("%dm", minutes)
}

func main() {
	apiID := os.Getenv("API_ID")
	apiHash := os.Getenv("API_HASH")
	phone := os.Getenv("PHONE")

	if apiID == "" || apiHash == "" || phone == "" {
		log.Fatal("Please set API_ID, API_HASH, and PHONE environment variables")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	client := telegram.NewClient(mustParseInt(apiID), apiHash, telegram.Options{
		SessionStorage: &telegram.FileSessionStorage{
			Path: "session.json",
		},
	})

	bot := NewUserbot(client)

	if err := client.Run(ctx, func(ctx context.Context) error {
		// Get API client
		bot.api = client.API()
		bot.sender = message.NewSender(bot.api)

		// Auth
		if err := client.Auth().IfNecessary(ctx, auth.SendCodeOptions{
			Phone: phone,
		}); err != nil {
			return fmt.Errorf("auth error: %w", err)
		}

		// Get self info
		self, err := bot.api.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
		if err != nil {
			return fmt.Errorf("failed to get self: %w", err)
		}
		if len(self) > 0 {
			if user, ok := self[0].(*tg.User); ok {
				bot.self = user
				log.Printf("Logged in as: %s %s (@%s)", user.FirstName, user.LastName, user.Username)
			}
		}

		// Setup update handler
		gaps := updates.New(updates.Config{
			Handler: bot.handleUpdate,
		})

		// Start receiving updates
		return gaps.Run(ctx, bot.api, bot.self.ID, updates.AuthOptions{
			Phone: phone,
		})
	}); err != nil {
		log.Fatal(err)
	}
}

func mustParseInt(s string) int {
	var i int
	fmt.Sscanf(s, "%d", &i)
	return i
}
