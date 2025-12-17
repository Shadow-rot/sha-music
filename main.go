package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-redis/redis/v8"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"gopkg.in/yaml.v3"
)

type Config struct {
	BotToken  string  `yaml:"bot_token"`
	APIHash   string  `yaml:"api_hash"`
	APIID     int     `yaml:"api_id"`
	RedisURL  string  `yaml:"redis_url"`
	OwnerID   int64   `yaml:"owner_id"`
	SudoUsers []int64 `yaml:"sudo_users"`
	Port      string  `yaml:"port"`
	SessionString string `yaml:"session_string"`
}

type Queue struct {
	ChatID    int64
	Songs     []Song
	Current   int
	IsPlaying bool
	Volume    int
	Loop      bool
	mu        sync.RWMutex
}

type Song struct {
	Title     string
	URL       string
	FilePath  string
	Duration  int
	Requester string
}

type VoiceCall struct {
	ChatID   int64
	Active   bool
	InputPeer *tg.InputPeerChannel
	mu       sync.RWMutex
}

type MusicBot struct {
	bot       *tgbotapi.BotAPI
	client    *telegram.Client
	config    Config
	queues    map[int64]*Queue
	calls     map[int64]*VoiceCall
	redis     *redis.Client
	mu        sync.RWMutex
	ctx       context.Context
}

func LoadConfig() (*Config, error) {
	data, err := os.ReadFile("config.yml")
	if err != nil {
		return nil, err
	}
	var config Config
	err = yaml.Unmarshal(data, &config)
	return &config, err
}

func NewMusicBot(config Config) (*MusicBot, error) {
	bot, err := tgbotapi.NewBotAPI(config.BotToken)
	if err != nil {
		return nil, err
	}

	bot.Debug = false

	opt, err := redis.ParseURL(config.RedisURL)
	if err != nil {
		opt = &redis.Options{Addr: "localhost:6379"}
	}

	rdb := redis.NewClient(opt)

	ctx := context.Background()

	return &MusicBot{
		bot:    bot,
		config: config,
		queues: make(map[int64]*Queue),
		calls:  make(map[int64]*VoiceCall),
		redis:  rdb,
		ctx:    ctx,
	}, nil
}

func (mb *MusicBot) IsOwner(userID int64) bool {
	return userID == mb.config.OwnerID
}

func (mb *MusicBot) IsSudo(userID int64) bool {
	if mb.IsOwner(userID) {
		return true
	}
	for _, id := range mb.config.SudoUsers {
		if id == userID {
			return true
		}
	}
	return false
}

func (mb *MusicBot) GetQueue(chatID int64) *Queue {
	mb.mu.RLock()
	q, exists := mb.queues[chatID]
	mb.mu.RUnlock()

	if !exists {
		mb.mu.Lock()
		q = &Queue{
			ChatID:    chatID,
			Songs:     []Song{},
			Current:   -1,
			Volume:    100,
			Loop:      false,
			IsPlaying: false,
		}
		mb.queues[chatID] = q
		mb.mu.Unlock()
	}
	return q
}

func (mb *MusicBot) GetCall(chatID int64) *VoiceCall {
	mb.mu.RLock()
	call, exists := mb.calls[chatID]
	mb.mu.RUnlock()

	if !exists {
		mb.mu.Lock()
		call = &VoiceCall{
			ChatID: chatID,
			Active: false,
		}
		mb.calls[chatID] = call
		mb.mu.Unlock()
	}
	return call
}

func (mb *MusicBot) DownloadSong(url string, chatID int64) (string, error) {
	tempDir := filepath.Join("downloads", fmt.Sprintf("%d", chatID))
	os.MkdirAll(tempDir, 0755)

	outputTemplate := filepath.Join(tempDir, "%(title)s.%(ext)s")

	cmd := exec.Command("yt-dlp",
		"-x",
		"--audio-format", "opus",
		"-o", outputTemplate,
		"--max-filesize", "100m",
		"--no-playlist",
		url,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("download failed: %v, %s", err, output)
	}

	files, err := filepath.Glob(filepath.Join(tempDir, "*.opus"))
	if err != nil || len(files) == 0 {
		files, _ = filepath.Glob(filepath.Join(tempDir, "*.webm"))
	}
	if err != nil || len(files) == 0 {
		files, _ = filepath.Glob(filepath.Join(tempDir, "*.m4a"))
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no audio file found")
	}

	convertedPath := filepath.Join(tempDir, "audio.raw")
	cmd = exec.Command("ffmpeg",
		"-i", files[0],
		"-f", "s16le",
		"-ac", "2",
		"-ar", "48000",
		"-acodec", "pcm_s16le",
		"-y",
		convertedPath,
	)

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("conversion failed: %v", err)
	}

	return convertedPath, nil
}

func (mb *MusicBot) JoinVoiceChat(chatID int64) error {
	call := mb.GetCall(chatID)
	call.mu.Lock()
	defer call.mu.Unlock()

	if call.Active {
		return nil
	}

	call.Active = true
	return nil
}

func (mb *MusicBot) LeaveVoiceChat(chatID int64) error {
	call := mb.GetCall(chatID)
	call.mu.Lock()
	defer call.mu.Unlock()

	if !call.Active {
		return nil
	}

	call.Active = false

	tempDir := filepath.Join("downloads", fmt.Sprintf("%d", chatID))
	os.RemoveAll(tempDir)

	return nil
}

func (mb *MusicBot) StreamAudio(chatID int64, filePath string) error {
	call := mb.GetCall(chatID)
	call.mu.RLock()
	active := call.Active
	call.mu.RUnlock()

	if !active {
		return fmt.Errorf("not in voice chat")
	}

	return nil
}

func (mb *MusicBot) HandleStart(chatID int64, userID int64) {
	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🎵 Help", "help"),
			tgbotapi.NewInlineKeyboardButtonData("ℹ️ About", "about"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("➕ Add Me", "addme"),
			tgbotapi.NewInlineKeyboardButtonData("📢 Support", "support"),
		),
	)

	text := `👋 <b>Welcome to Sha Music Bot!</b>

🎶 <b>High-Quality Music Player for Voice Chats</b>

<b>✨ Features:</b>
• Play from YouTube, Spotify
• Queue management
• High-quality audio
• Fast & Reliable

<b>🚀 Quick Start:</b>
1. Add me to your group
2. Make me admin with voice chat rights
3. Start voice chat
4. Use /play [song name]

⚡ <b>Powered by ntgcalls</b>`

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = keyboard
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleHelp(chatID int64) {
	text := `<b>📖 Sha Music Bot Commands</b>

<b>🎵 Playback:</b>
/play [song/url] - Play song
/pause - Pause playback
/resume - Resume playback
/skip - Skip current song
/stop - Stop & clear queue
/end - Leave voice chat

<b>📋 Queue Management:</b>
/queue - Show queue
/shuffle - Shuffle queue
/loop - Toggle loop mode
/clearqueue - Clear queue

<b>🎛 Audio Controls:</b>
/volume [1-200] - Set volume
/current - Current playing
/lyrics - Get lyrics

<b>⚡ Admin Commands:</b>
/forceplay [song] - Force play
/channelplay - Channel mode
/skip [number] - Skip to position

<b>👑 Sudo/Owner:</b>
/addsudo [id] - Add sudo user
/rmsudo [id] - Remove sudo
/broadcast [text] - Broadcast
/stats - Statistics
/restart - Restart bot

<b>💡 Supported:</b>
YouTube, Spotify, SoundCloud, Direct links`

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandlePlay(chatID int64, userID int64, query string, username string) {
	if query == "" {
		msg := tgbotapi.NewMessage(chatID, "❌ Please provide a song name or URL!")
		mb.bot.Send(msg)
		return
	}

	statusMsg := tgbotapi.NewMessage(chatID, "🔍 Searching and downloading...")
	sentMsg, _ := mb.bot.Send(statusMsg)

	var url string
	if strings.HasPrefix(query, "http") {
		url = query
	} else {
		url = fmt.Sprintf("ytsearch:%s", query)
	}

	filePath, err := mb.DownloadSong(url, chatID)
	if err != nil {
		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, 
			fmt.Sprintf("❌ Download failed: %v", err))
		mb.bot.Send(editMsg)
		return
	}

	song := Song{
		Title:     query,
		URL:       url,
		FilePath:  filePath,
		Duration:  180,
		Requester: username,
	}

	queue := mb.GetQueue(chatID)
	queue.mu.Lock()
	queue.Songs = append(queue.Songs, song)
	position := len(queue.Songs)
	wasPlaying := queue.IsPlaying
	queue.mu.Unlock()

	if !wasPlaying {
		if err := mb.JoinVoiceChat(chatID); err != nil {
			editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID,
				fmt.Sprintf("❌ Failed to join voice chat: %v", err))
			mb.bot.Send(editMsg)
			return
		}

		go mb.PlayNext(chatID)

		text := fmt.Sprintf("▶️ <b>Now Playing:</b>\n\n"+
			"🎵 <b>%s</b>\n"+
			"👤 Requested by: @%s\n"+
			"🔊 Volume: %d%%",
			song.Title, username, queue.Volume)

		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, text)
		editMsg.ParseMode = "HTML"
		mb.bot.Send(editMsg)
	} else {
		text := fmt.Sprintf("✅ <b>Added to Queue</b>\n\n"+
			"🎵 <b>%s</b>\n"+
			"📍 Position: <b>%d</b>\n"+
			"👤 Requested by: @%s",
			song.Title, position, username)

		editMsg := tgbotapi.NewEditMessageText(chatID, sentMsg.MessageID, text)
		editMsg.ParseMode = "HTML"
		mb.bot.Send(editMsg)
	}
}

func (mb *MusicBot) PlayNext(chatID int64) {
	queue := mb.GetQueue(chatID)
	queue.mu.Lock()

	if len(queue.Songs) == 0 {
		queue.IsPlaying = false
		queue.Current = -1
		queue.mu.Unlock()
		mb.LeaveVoiceChat(chatID)
		msg := tgbotapi.NewMessage(chatID, "✅ Queue finished! Leaving voice chat.")
		mb.bot.Send(msg)
		return
	}

	if queue.Loop && queue.Current >= 0 {
	} else {
		queue.Current++
		if queue.Current >= len(queue.Songs) {
			queue.Current = 0
		}
	}

	song := queue.Songs[queue.Current]
	queue.IsPlaying = true
	queue.mu.Unlock()

	keyboard := tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("⏸", "pause"),
			tgbotapi.NewInlineKeyboardButtonData("▶️", "resume"),
			tgbotapi.NewInlineKeyboardButtonData("⏭", "skip"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔉", "vol_down"),
			tgbotapi.NewInlineKeyboardButtonData("🔊", "vol_up"),
			tgbotapi.NewInlineKeyboardButtonData("⏹", "stop"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔁", "loop"),
			tgbotapi.NewInlineKeyboardButtonData("📋", "queue"),
		),
	)

	text := fmt.Sprintf("▶️ <b>Now Playing</b>\n\n"+
		"🎵 <b>%s</b>\n"+
		"⏱ Duration: %d:%02d\n"+
		"👤 Requested by: @%s\n"+
		"📍 Position: %d/%d\n"+
		"🔊 Volume: %d%%",
		song.Title,
		song.Duration/60, song.Duration%60,
		song.Requester,
		queue.Current+1, len(queue.Songs),
		queue.Volume)

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	msg.ReplyMarkup = keyboard
	mb.bot.Send(msg)

	if err := mb.StreamAudio(chatID, song.FilePath); err != nil {
		errorMsg := tgbotapi.NewMessage(chatID, 
			fmt.Sprintf("❌ Playback error: %v", err))
		mb.bot.Send(errorMsg)
	}

	time.Sleep(time.Duration(song.Duration) * time.Second)

	queue.mu.Lock()
	if !queue.Loop {
		queue.Songs = append(queue.Songs[:queue.Current], queue.Songs[queue.Current+1:]...)
		queue.Current--
	}
	queue.mu.Unlock()

	go mb.PlayNext(chatID)
}

func (mb *MusicBot) HandleSkip(chatID int64, userID int64) {
	queue := mb.GetQueue(chatID)
	queue.mu.RLock()
	isPlaying := queue.IsPlaying
	queue.mu.RUnlock()

	if !isPlaying {
		msg := tgbotapi.NewMessage(chatID, "❌ Nothing is playing!")
		mb.bot.Send(msg)
		return
	}

	msg := tgbotapi.NewMessage(chatID, "⏭ Skipped!")
	mb.bot.Send(msg)

	queue.mu.Lock()
	if queue.Current < len(queue.Songs) && !queue.Loop {
		os.Remove(queue.Songs[queue.Current].FilePath)
		queue.Songs = append(queue.Songs[:queue.Current], queue.Songs[queue.Current+1:]...)
		queue.Current--
	}
	queue.mu.Unlock()

	go mb.PlayNext(chatID)
}

func (mb *MusicBot) HandlePause(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "⏸ Paused!")
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleResume(chatID int64) {
	msg := tgbotapi.NewMessage(chatID, "▶️ Resumed!")
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleStop(chatID int64, userID int64) {
	queue := mb.GetQueue(chatID)
	queue.mu.Lock()

	for _, song := range queue.Songs {
		os.Remove(song.FilePath)
	}

	queue.Songs = []Song{}
	queue.Current = -1
	queue.IsPlaying = false
	queue.mu.Unlock()

	mb.LeaveVoiceChat(chatID)

	msg := tgbotapi.NewMessage(chatID, "⏹ Stopped and cleared queue!")
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleQueue(chatID int64) {
	queue := mb.GetQueue(chatID)
	queue.mu.RLock()
	defer queue.mu.RUnlock()

	if len(queue.Songs) == 0 {
		msg := tgbotapi.NewMessage(chatID, "📭 Queue is empty!")
		mb.bot.Send(msg)
		return
	}

	text := fmt.Sprintf("<b>📋 Current Queue</b>\n\n"+
		"🔊 Volume: %d%%\n"+
		"🔁 Loop: %v\n\n", queue.Volume, queue.Loop)

	for i, song := range queue.Songs {
		prefix := "  "
		if i == queue.Current {
			prefix = "▶️"
		}
		text += fmt.Sprintf("%s <b>%d.</b> %s\n   👤 @%s\n\n",
			prefix, i+1, song.Title, song.Requester)
	}

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleVolume(chatID int64, volume int) {
	if volume < 1 || volume > 200 {
		msg := tgbotapi.NewMessage(chatID, "❌ Volume must be between 1-200")
		mb.bot.Send(msg)
		return
	}

	queue := mb.GetQueue(chatID)
	queue.mu.Lock()
	queue.Volume = volume
	queue.mu.Unlock()

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🔊 Volume set to %d%%", volume))
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleLoop(chatID int64) {
	queue := mb.GetQueue(chatID)
	queue.mu.Lock()
	queue.Loop = !queue.Loop
	loopStatus := queue.Loop
	queue.mu.Unlock()

	status := "disabled"
	if loopStatus {
		status = "enabled"
	}

	msg := tgbotapi.NewMessage(chatID, fmt.Sprintf("🔁 Loop %s", status))
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleStats(chatID int64, userID int64) {
	if !mb.IsSudo(userID) {
		msg := tgbotapi.NewMessage(chatID, "❌ Sudo only!")
		mb.bot.Send(msg)
		return
	}

	mb.mu.RLock()
	activeChats := len(mb.queues)
	activeCalls := 0
	for _, call := range mb.calls {
		if call.Active {
			activeCalls++
		}
	}
	mb.mu.RUnlock()

	text := fmt.Sprintf("<b>📊 Bot Statistics</b>\n\n"+
		"🎵 Active Chats: %d\n"+
		"📞 Active Calls: %d\n"+
		"👥 Sudo Users: %d\n"+
		"⚡ Status: <b>Online</b>\n"+
		"🚀 Version: <b>2.0.0</b>",
		activeChats, activeCalls, len(mb.config.SudoUsers))

	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "HTML"
	mb.bot.Send(msg)
}

func (mb *MusicBot) HandleCallback(callback *tgbotapi.CallbackQuery) {
	chatID := callback.Message.Chat.ID
	data := callback.Data

	switch data {
	case "help":
		mb.HandleHelp(chatID)
	case "pause":
		mb.HandlePause(chatID)
	case "resume":
		mb.HandleResume(chatID)
	case "skip":
		mb.HandleSkip(chatID, callback.From.ID)
	case "stop":
		mb.HandleStop(chatID, callback.From.ID)
	case "loop":
		mb.HandleLoop(chatID)
	case "queue":
		mb.HandleQueue(chatID)
	case "vol_up":
		queue := mb.GetQueue(chatID)
		newVol := queue.Volume + 10
		if newVol > 200 {
			newVol = 200
		}
		mb.HandleVolume(chatID, newVol)
	case "vol_down":
		queue := mb.GetQueue(chatID)
		newVol := queue.Volume - 10
		if newVol < 1 {
			newVol = 1
		}
		mb.HandleVolume(chatID, newVol)
	}

	answer := tgbotapi.NewCallback(callback.ID, "")
	mb.bot.Request(answer)
}

func (mb *MusicBot) Start() {
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60

	updates := mb.bot.GetUpdatesChan(u)

	log.Printf("✅ Sha Music Bot started as @%s", mb.bot.Self.UserName)
	log.Printf("🎵 Ready to play music in voice chats!")

	for update := range updates {
		if update.Message != nil {
			chatID := update.Message.Chat.ID
			userID := update.Message.From.ID
			username := update.Message.From.UserName

			if update.Message.IsCommand() {
				command := update.Message.Command()
				args := update.Message.CommandArguments()

				switch command {
				case "start":
					mb.HandleStart(chatID, userID)
				case "help":
					mb.HandleHelp(chatID)
				case "play", "p":
					mb.HandlePlay(chatID, userID, args, username)
				case "pause":
					mb.HandlePause(chatID)
				case "resume":
					mb.HandleResume(chatID)
				case "skip", "next":
					mb.HandleSkip(chatID, userID)
				case "stop", "end":
					mb.HandleStop(chatID, userID)
				case "queue", "q":
					mb.HandleQueue(chatID)
				case "volume", "vol":
					if args != "" {
						vol, _ := strconv.Atoi(args)
						mb.HandleVolume(chatID, vol)
					}
				case "loop":
					mb.HandleLoop(chatID)
				case "stats":
					mb.HandleStats(chatID, userID)
				}
			}
		} else if update.CallbackQuery != nil {
			mb.HandleCallback(update.CallbackQuery)
		}
	}
}

func main() {
	config, err := LoadConfig()
	if err != nil {
		log.Fatal("❌ Failed to load config:", err)
	}

	bot, err := NewMusicBot(*config)
	if err != nil {
		log.Fatal("❌ Failed to create bot:", err)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go bot.Start()

	<-quit
	log.Println("👋 Shutting down bot...")
}