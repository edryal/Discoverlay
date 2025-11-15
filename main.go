package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/go-gl/gl/v3.3-core/gl"
	"github.com/go-gl/glfw/v3.3/glfw"
	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomonobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	// Speaking detection
	SpeakingGracePeriod   = 500 * time.Millisecond
	TimeoutCheckInterval  = 200 * time.Millisecond
	VoiceReadyWaitTimeout = 10 * time.Second

	// Overlay layout
	OverlayWidth   = 250
	OverlayRowH    = 44
	OverlayPadding = 8
)

// User represents a person in the voice channel.
type User struct {
	ID         string
	Name       string
	IsSpeaking bool
	LastSpeak  time.Time
}

// App holds bot, state, and UI.
type App struct {
	session         *discordgo.Session
	voiceConnection *discordgo.VoiceConnection

	voiceUsers map[string]*User  // userID -> user
	ssrcToUser map[uint32]string // SSRC -> userID

	mu           sync.RWMutex
	botChannelID string
	guildID      string // Store the current guild ID

	// Startup sync
	readyChan         chan struct{}
	expectedGuilds    int
	seenGuildCreates  int
	guildsReadyClosed bool

	// Config
	targetUserID      string
	targetChannelID   string
	targetChannelName string

	// Avatars
	avatarMu    sync.RWMutex
	avatarCache map[string]*Avatar // userID -> avatar

	// UI
	window   *glfw.Window
	renderer *Renderer
	quitCh   chan struct{}
}

// Avatar stores a decoded user avatar.
type Avatar struct {
	Img     image.Image
	LastErr error
}

// RenderUser is a snapshot used by the renderer.
type RenderUser struct {
	Name     string
	Speaking bool
	Avatar   image.Image // may be nil if not loaded
}

func (app *App) runDiscordBot() {
	token := strings.TrimSpace(os.Getenv("BOT_TOKEN"))
	if token == "" {
		log.Fatalf("BOT_TOKEN environment variable is not set.")
	}

	app.targetUserID = strings.TrimSpace(os.Getenv("TARGET_USER_ID"))
	app.targetChannelID = strings.TrimSpace(os.Getenv("TARGET_CHANNEL_ID"))
	app.targetChannelName = strings.TrimSpace(os.Getenv("TARGET_CHANNEL_NAME"))

	var err error
	app.session, err = discordgo.New("Bot " + token)
	if err != nil {
		log.Fatalf("Error creating Discord session: %v", err)
	}

	app.session.StateEnabled = true
	app.voiceUsers = make(map[string]*User)
	app.ssrcToUser = make(map[uint32]string)
	app.avatarCache = make(map[string]*Avatar)

	// Handlers
	app.session.AddHandler(app.onReady)
	app.session.AddHandler(app.onGuildCreate)
	app.session.AddHandler(app.onVoiceStateUpdate)

	// Intents
	app.session.Identify.Intents = discordgo.IntentsGuilds |
		discordgo.IntentsGuildMembers | discordgo.IntentsGuildVoiceStates

	if err = app.session.Open(); err != nil {
		log.Fatalf("Error opening Discord connection: %v", err)
	}
	log.Println("Discord bot session started.")
}

func (app *App) onReady(_ *discordgo.Session, ev *discordgo.Ready) {
	app.mu.Lock()
	app.expectedGuilds = len(ev.Guilds)
	app.mu.Unlock()

	log.Printf("Discord Bot is ready. In %d guild(s). Waiting for GuildCreate events...", app.expectedGuilds)
}

func (app *App) onGuildCreate(_ *discordgo.Session, gc *discordgo.GuildCreate) {
	app.mu.Lock()
	defer app.mu.Unlock()

	app.seenGuildCreates++

	// When all guilds are created, signal "ready"
	if !app.guildsReadyClosed && app.seenGuildCreates >= app.expectedGuilds {
		app.guildsReadyClosed = true
		close(app.readyChan)
	}
}

// React in realtime: if the target user joins/moves, follow them
func (app *App) onVoiceStateUpdate(_ *discordgo.Session, vs *discordgo.VoiceStateUpdate) {
	app.mu.Lock()
	currentGuildID := app.guildID
	app.mu.Unlock()

	// Use vs.GuildID if available, otherwise fall back to the stored one.
	// vs.GuildID is usually populated, making this more robust.
	guildID := vs.GuildID
	if guildID == "" {
		guildID = currentGuildID
	}

	// Track users in our current channel for overlay
	app.mu.Lock()
	inOurChannel := app.botChannelID != "" && vs.ChannelID == app.botChannelID
	leftOurChannel := app.botChannelID != "" && vs.BeforeUpdate != nil && vs.BeforeUpdate.ChannelID == app.botChannelID && vs.ChannelID != app.botChannelID
	app.mu.Unlock()

	if inOurChannel && guildID != "" {
		app.addUserIfHuman(guildID, vs.UserID)
	} else if leftOurChannel {
		app.removeUser(vs.UserID)
	}

	// Follow target user if configured
	if app.targetUserID != "" && vs.UserID == app.targetUserID && vs.ChannelID != "" && guildID != "" {
		app.JoinVoiceChannel(guildID, vs.ChannelID)
	}
}

func (app *App) addUserIfHuman(guildID, userID string) {
	// Force a fresh API call to get the most up-to-date member object
	member, err := app.session.GuildMember(guildID, userID)
	if err != nil {
		log.Printf("Failed to get member %s in guild %s: %v. Nickname may not be available.", userID, guildID, err)
		// As a fallback, try to get the user object directly
		u, userErr := app.session.User(userID)
		if userErr != nil {
			log.Printf("Also failed to get user %s: %v", userID, userErr)
			return
		}
		// Create a member object with just the user info
		member = &discordgo.Member{User: u}
	}

	if member.User.Bot {
		return
	}

	displayName := member.User.Username
	if member.Nick != "" {
		displayName = member.Nick
	}

	app.mu.Lock()
	defer app.mu.Unlock()

	if u, exists := app.voiceUsers[userID]; exists {
		// If user exists, just update their name in case it changed
		if u.Name != displayName {
			log.Printf("Updating name for %s: %s -> %s", u.ID, u.Name, displayName)
			u.Name = displayName
		}
		return
	}

	app.voiceUsers[userID] = &User{
		ID:         userID,
		Name:       displayName,
		IsSpeaking: false,
		LastSpeak:  time.Now(),
	}
	log.Printf("[VoiceState] JOIN: %s (username: %s)", displayName, member.User.Username)

	// Kick off avatar fetch (async)
	go app.fetchAndStoreAvatar(member.User)
}

func (app *App) removeUser(userID string) {
	app.mu.Lock()
	defer app.mu.Unlock()
	if u, ok := app.voiceUsers[userID]; ok {
		log.Printf("[VoiceState] LEAVE: %s", u.Name)
		delete(app.voiceUsers, userID)
		// Clean SSRC mappings for this user
		for ssrc, uid := range app.ssrcToUser {
			if uid == userID {
				delete(app.ssrcToUser, ssrc)
			}
		}
	}
}

func (app *App) onVoiceSpeakingUpdate(_ *discordgo.VoiceConnection, ev *discordgo.VoiceSpeakingUpdate) {
	app.mu.Lock()
	defer app.mu.Unlock()

	if ev.SSRC != 0 && ev.UserID != "" {
		app.ssrcToUser[uint32(ev.SSRC)] = ev.UserID
	}

	if user, ok := app.voiceUsers[ev.UserID]; ok {
		user.LastSpeak = time.Now()
		if user.IsSpeaking != ev.Speaking {
			user.IsSpeaking = ev.Speaking
		}
	}
}

func (app *App) runOpusReceiver() {
	vc := app.voiceConnection
	if vc == nil {
		return
	}
	for pkt := range vc.OpusRecv {
		userID := app.userIDForSSRC(pkt.SSRC)
		if userID == "" {
			continue
		}
		app.mu.Lock()
		if user, ok := app.voiceUsers[userID]; ok {
			now := time.Now()
			user.LastSpeak = now
			if !user.IsSpeaking {
				user.IsSpeaking = true
			}
		}
		app.mu.Unlock()
	}
}

func (app *App) userIDForSSRC(ssrc uint32) string {
	app.mu.RLock()
	defer app.mu.RUnlock()
	return app.ssrcToUser[ssrc]
}

func (app *App) checkSpeakingTimeouts() {
	ticker := time.NewTicker(TimeoutCheckInterval)
	defer ticker.Stop()

	for range ticker.C {
		now := time.Now()
		app.mu.Lock()
		for _, user := range app.voiceUsers {
			if user.IsSpeaking && now.Sub(user.LastSpeak) > SpeakingGracePeriod {
				user.IsSpeaking = false
			}
		}
		app.mu.Unlock()
	}
}

// After startup either join a configured channel or follow TARGET_USER_ID
func (app *App) doInitialJoin() {
	// If a channel ID is provided, join it
	if app.targetChannelID != "" {
		gid, cid := app.findGuildForChannelID(app.targetChannelID)
		if gid != "" && cid != "" {
			app.JoinVoiceChannel(gid, cid)
			return
		}
		log.Printf("Configured TARGET_CHANNEL_ID=%s not found in cached guilds.", app.targetChannelID)
	}

	// If a channel name is provided, find and join by name
	if app.targetChannelName != "" {
		if gid, cid := app.findChannelByName(app.targetChannelName); gid != "" && cid != "" {
			app.JoinVoiceChannel(gid, cid)
			return
		}
		log.Printf("Could not find voice channel named '%s'.", app.targetChannelName)
	}

	// If a target user is configured, try to find them in current voice states
	if app.targetUserID != "" {
		if gid, cid := app.findUserInVoice(app.targetUserID); gid != "" && cid != "" {
			app.JoinVoiceChannel(gid, cid)
			return
		}
		log.Printf("Could not find user %s in any voice channel (yet). Will follow on next VoiceStateUpdate.", app.targetUserID)
	}
}

func (app *App) findUserInVoice(userID string) (guildID, channelID string) {
	app.session.RLock()
	defer app.session.RUnlock()
	for _, g := range app.session.State.Guilds {
		for _, vs := range g.VoiceStates {
			if vs.UserID == userID && vs.ChannelID != "" {
				return g.ID, vs.ChannelID
			}
		}
	}
	return "", ""
}

func (app *App) findGuildForChannelID(channelID string) (guildID, foundChannelID string) {
	app.session.RLock()
	defer app.session.RUnlock()
	for _, g := range app.session.State.Guilds {
		for _, ch := range g.Channels {
			if ch.ID == channelID {
				return g.ID, channelID
			}
		}
	}
	return "", ""
}

func (app *App) findChannelByName(name string) (guildID, channelID string) {
	app.session.RLock()
	defer app.session.RUnlock()
	for _, g := range app.session.State.Guilds {
		for _, ch := range g.Channels {
			if ch != nil && ch.Type == discordgo.ChannelTypeGuildVoice && strings.EqualFold(ch.Name, name) {
				return g.ID, ch.ID
			}
		}
	}
	return "", ""
}

func (app *App) JoinVoiceChannel(guildID, channelID string) {
	app.mu.Lock()
	if app.voiceConnection != nil && app.voiceConnection.ChannelID == channelID {
		app.mu.Unlock()
		return // Already in the correct channel
	}
	app.mu.Unlock()

	// Join muted (no transmit) and not deafened (so we can receive)
	vc, err := app.session.ChannelVoiceJoin(guildID, channelID, true, false)
	if err != nil {
		log.Printf("Error joining voice channel: %v", err)
		return
	}

	log.Printf("Joining voice channel %s, waiting for connection...", channelID)
	deadline := time.Now().Add(VoiceReadyWaitTimeout)
	for !vc.Ready {
		if time.Now().After(deadline) {
			log.Println("Voice connection timed out waiting for Ready.")
			_ = vc.Disconnect()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	log.Println("Voice connection is ready.")
	app.mu.Lock()
	app.voiceConnection = vc
	app.botChannelID = channelID
	app.guildID = guildID // Store guildID
	app.voiceUsers = make(map[string]*User)
	app.ssrcToUser = make(map[uint32]string)
	app.mu.Unlock()

	vc.AddHandler(app.onVoiceSpeakingUpdate)

	// Seed users currently in the channel
	app.seedUsersInChannel(guildID, channelID)

	// Start receiver
	go app.runOpusReceiver()
}

func (app *App) seedUsersInChannel(guildID, channelID string) {
	guild, err := app.session.State.Guild(guildID)
	if err != nil {
		log.Printf("Failed to get guild state: %v", err)
		return
	}

	for _, vs := range guild.VoiceStates {
		if vs.ChannelID == channelID {
			app.addUserIfHuman(guildID, vs.UserID)
		}
	}
}

// snapshotUsers returns a stable slice for rendering.
func (app *App) snapshotUsers() []RenderUser {
	app.mu.RLock()
	users := make([]*User, 0, len(app.voiceUsers))
	for _, u := range app.voiceUsers {
		users = append(users, u)
	}
	app.mu.RUnlock()

	// Sort users alphabetically by name for a static order.
	sort.Slice(users, func(i, j int) bool {
		return strings.ToLower(users[i].Name) < strings.ToLower(users[j].Name)
	})

	out := make([]RenderUser, 0, len(users))
	for _, u := range users {
		var av image.Image
		app.avatarMu.RLock()
		if a := app.avatarCache[u.ID]; a != nil && a.Img != nil {
			av = a.Img
		}
		app.avatarMu.RUnlock()

		out = append(out, RenderUser{
			Name:     u.Name,
			Speaking: u.IsSpeaking,
			Avatar:   av,
		})
	}
	return out
}

func (app *App) fetchAndStoreAvatar(user *discordgo.User) {
	if user == nil {
		return
	}
	url := discordAvatarURL(user)

	// No avatar URL; leave nil to render placeholder
	if url == "" {
		return
	}

	resp, err := http.Get(url)
	if err != nil {
		app.avatarMu.Lock()
		app.avatarCache[user.ID] = &Avatar{Img: nil, LastErr: err}
		app.avatarMu.Unlock()
		return
	}

	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		app.avatarMu.Lock()
		app.avatarCache[user.ID] = &Avatar{Img: nil, LastErr: fmt.Errorf("status %s", resp.Status)}
		app.avatarMu.Unlock()
		return
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		app.avatarMu.Lock()
		app.avatarCache[user.ID] = &Avatar{Img: nil, LastErr: err}
		app.avatarMu.Unlock()
		return
	}

	img, _, err := image.Decode(bytes.NewReader(data))
	app.avatarMu.Lock()
	if err != nil {
		app.avatarCache[user.ID] = &Avatar{Img: nil, LastErr: err}
	} else {
		app.avatarCache[user.ID] = &Avatar{Img: img}
	}
	app.avatarMu.Unlock()
}

func discordAvatarURL(user *discordgo.User) string {
	if user.Avatar != "" {
		return fmt.Sprintf("https://cdn.discordapp.com/avatars/%s/%s.png?size=64", user.ID, user.Avatar)
	}

	// Default avatar index: discriminator mod 5, else 0
	idx := 0
	if user.Discriminator != "" && user.Discriminator != "0" {
		if n, err := strconv.Atoi(user.Discriminator); err == nil {
			idx = n % 5
		}
	}
	return fmt.Sprintf("https://cdn.discordapp.com/embed/avatars/%d.png", idx)
}

type Renderer struct {
	win                 *glfw.Window
	face                font.Face
	offscreen           *image.RGBA
	tex                 uint32
	vao, vbo            uint32
	prog                uint32
	uResLoc             int32
	widthPx, heightPx   int
	lastCompose         time.Time
	placeholderAvatar   image.Image
	placeholderSpeaking image.Image
}

func NewRenderer(win *glfw.Window) (*Renderer, error) {
	fbW, fbH := win.GetFramebufferSize()

	var fontData []byte
	var err error
	fontPath := os.Getenv("OVERLAY_FONT_PATH")
	if fontPath != "" {
		log.Printf("Loading custom font from: %s", fontPath)
		fontData, err = os.ReadFile(fontPath)
		if err != nil {
			log.Printf("WARN: Failed to load custom font, falling back to default: %v", err)
			fontData = gomonobold.TTF
		}
	} else {
		fontData = gomonobold.TTF
	}

	tt, err := opentype.Parse(fontData)
	if err != nil {
		return nil, fmt.Errorf("parsing font: %w", err)
	}
	face, err := opentype.NewFace(tt, &opentype.FaceOptions{
		Size:    14,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("creating font face: %w", err)
	}

	r := &Renderer{
		win:               win,
		face:              face,
		widthPx:           fbW,
		heightPx:          fbH,
		offscreen:         image.NewRGBA(image.Rect(0, 0, fbW, fbH)),
		placeholderAvatar: genPlaceholderAvatar(36, 36),
	}

	if err := r.initGL(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *Renderer) initGL() error {
	if err := gl.Init(); err != nil {
		return fmt.Errorf("gl init: %w", err)
	}

	gl.Enable(gl.BLEND)
	gl.BlendFunc(gl.SRC_ALPHA, gl.ONE_MINUS_SRC_ALPHA)
	gl.Viewport(0, 0, int32(r.widthPx), int32(r.heightPx))

	// Magic stuff to be honest I got no clue
	vsSrc := `
		#version 330 core
		layout(location=0) in vec2 aPos;
		layout(location=1) in vec2 aUV;
		out vec2 vUV;
		uniform vec2 uResolution;
		void main() {
			vec2 pos = aPos / uResolution * 2.0 - 1.0;
			gl_Position = vec4(pos.x, -pos.y, 0.0, 1.0);
			vUV = aUV;
		}
	`

	fsSrc := `
		#version 330 core
		in vec2 vUV;
		out vec4 FragColor;
		uniform sampler2D uTex;
		void main() {
			FragColor = texture(uTex, vUV);
		}
	`

	vs, err := compileShader(vsSrc, gl.VERTEX_SHADER)
	if err != nil {
		return err
	}

	fs, err := compileShader(fsSrc, gl.FRAGMENT_SHADER)
	if err != nil {
		return err
	}

	prog := gl.CreateProgram()
	gl.AttachShader(prog, vs)
	gl.AttachShader(prog, fs)
	gl.LinkProgram(prog)

	var status int32
	gl.GetProgramiv(prog, gl.LINK_STATUS, &status)
	if status == gl.FALSE {
		var logLen int32
		gl.GetProgramiv(prog, gl.INFO_LOG_LENGTH, &logLen)
		log := strings.Repeat("\x00", int(logLen+1))
		gl.GetProgramInfoLog(prog, logLen, nil, gl.Str(log))
		return fmt.Errorf("program link error: %s", log)
	}

	gl.DeleteShader(vs)
	gl.DeleteShader(fs)
	r.prog = prog
	r.uResLoc = gl.GetUniformLocation(r.prog, gl.Str("uResolution\x00"))

	// Full-screen quad with UVs: TL(0,0) TR(1,0) BR(1,1) BL(0,1)
	verts := []float32{
		// x, y, u, v
		0, 0, 0, 0, // TL
		float32(r.widthPx), 0, 1, 0, // TR
		float32(r.widthPx), float32(r.heightPx), 1, 1, // BR

		0, 0, 0, 0, // TL
		float32(r.widthPx), float32(r.heightPx), 1, 1, // BR
		0, float32(r.heightPx), 0, 1, // BL
	}

	gl.GenVertexArrays(1, &r.vao)
	gl.GenBuffers(1, &r.vbo)

	gl.BindVertexArray(r.vao)
	gl.BindBuffer(gl.ARRAY_BUFFER, r.vbo)
	gl.BufferData(gl.ARRAY_BUFFER, len(verts)*4, gl.Ptr(verts), gl.STATIC_DRAW)

	stride := 4 * 4
	gl.EnableVertexAttribArray(0)
	gl.VertexAttribPointer(0, 2, gl.FLOAT, false, int32(stride), gl.PtrOffset(0))
	gl.EnableVertexAttribArray(1)
	gl.VertexAttribPointer(1, 2, gl.FLOAT, false, int32(stride), gl.PtrOffset(8))

	gl.BindVertexArray(0)

	// Texture
	gl.GenTextures(1, &r.tex)
	gl.BindTexture(gl.TEXTURE_2D, r.tex)
	gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_MIN_FILTER, gl.LINEAR)
	gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_MAG_FILTER, gl.LINEAR)
	gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_S, gl.CLAMP_TO_EDGE)
	gl.TexParameteri(gl.TEXTURE_2D, gl.TEXTURE_WRAP_T, gl.CLAMP_TO_EDGE)
	gl.TexImage2D(gl.TEXTURE_2D, 0, gl.RGBA, int32(r.widthPx), int32(r.heightPx), 0, gl.RGBA, gl.UNSIGNED_BYTE, nil)

	return nil
}

func (r *Renderer) resizeIfNeeded() {
	fbW, fbH := r.win.GetFramebufferSize()
	if fbW == r.widthPx && fbH == r.heightPx {
		return
	}

	r.widthPx, r.heightPx = fbW, fbH
	r.offscreen = image.NewRGBA(image.Rect(0, 0, fbW, fbH))
	gl.Viewport(0, 0, int32(fbW), int32(fbH))

	// Rebuild quad VBO with the same TL->TR->BR->TL->BR->BL pattern
	verts := []float32{
		0, 0, 0, 0,
		float32(fbW), 0, 1, 0,
		float32(fbW), float32(fbH), 1, 1,

		0, 0, 0, 0,
		float32(fbW), float32(fbH), 1, 1,
		0, float32(fbH), 0, 1,
	}

	gl.BindBuffer(gl.ARRAY_BUFFER, r.vbo)
	gl.BufferData(gl.ARRAY_BUFFER, len(verts)*4, gl.Ptr(verts), gl.STATIC_DRAW)

	// Reallocate texture
	gl.BindTexture(gl.TEXTURE_2D, r.tex)
	gl.TexImage2D(gl.TEXTURE_2D, 0, gl.RGBA, int32(fbW), int32(fbH), 0, gl.RGBA, gl.UNSIGNED_BYTE, nil)
}

func (r *Renderer) compose(users []RenderUser) {
	now := time.Now()
	if now.Sub(r.lastCompose) < 100*time.Millisecond {
		return
	}
	r.lastCompose = now

	draw.Draw(r.offscreen, r.offscreen.Bounds(), image.Transparent, image.Point{}, draw.Src)

	x := OverlayPadding
	y := OverlayPadding
	rowH := OverlayRowH
	avatarSize := rowH - 8
	textColor := color.NRGBA{R: 230, G: 230, B: 230, A: 255}
	activeIndicatorColor := color.NRGBA{R: 60, G: 255, B: 120, A: 255}

	maxRows := max((r.heightPx - 2*OverlayPadding) / rowH, 1)
	if len(users) > maxRows {
		users = users[:maxRows]
	}

	for _, ru := range users {
		// Calculate positions
		indicatorRadius := float64(avatarSize)/2.0 + 3.0 // 3px gap
		indicatorThickness := 2.0
		avatarRadius := float64(avatarSize) / 2.0

		totalVisualWidth := int(math.Ceil((indicatorRadius + indicatorThickness) * 2))

		centerX := x + totalVisualWidth/2
		centerY := y + rowH/2

		if ru.Speaking {
			drawAntiAliasedRing(r.offscreen, centerX, centerY, indicatorRadius, indicatorThickness, activeIndicatorColor)
		}

		avatarImg := ru.Avatar
		if avatarImg == nil {
			avatarImg = r.placeholderAvatar
		}

		scaledAvatar := image.NewRGBA(image.Rect(0, 0, avatarSize, avatarSize))
		xdraw.CatmullRom.Scale(scaledAvatar, scaledAvatar.Bounds(), avatarImg, avatarImg.Bounds(), draw.Over, nil)

		drawAntiAliasedCircleMask(r.offscreen, centerX, centerY, avatarRadius, scaledAvatar)

		nameX := x + totalVisualWidth + 8
		metrics := r.face.Metrics()
		textHeight := (metrics.Ascent + metrics.Descent).Ceil()
		baselineY := y + (rowH-textHeight)/2 + metrics.Ascent.Ceil()
		drawText(r.offscreen, nameX, baselineY, ru.Name, r.face, textColor)

		y += rowH
	}
}

func drawAntiAliasedCircleMask(dst *image.RGBA, cx, cy int, r float64, src image.Image) {
	bounds := image.Rect(int(float64(cx)-r-1), int(float64(cy)-r-1), int(float64(cx)+r+1), int(float64(cy)+r+1))
	for py := bounds.Min.Y; py < bounds.Max.Y; py++ {
		for px := bounds.Min.X; px < bounds.Max.X; px++ {
			dist := math.Sqrt(math.Pow(float64(px-cx)+0.5, 2) + math.Pow(float64(py-cy)+0.5, 2))

			alpha := 1.0 - (dist - r)
			if alpha > 1 {
				alpha = 1
			}

			if alpha < 0 {
				alpha = 0
			}

			if alpha > 0 {
				sx, sy := px-(cx-int(r)), py-(cy-int(r))
				if sx >= 0 && sy >= 0 && sx < src.Bounds().Dx() && sy < src.Bounds().Dy() {
					sR, sG, sB, sA := src.At(src.Bounds().Min.X+sx, src.Bounds().Min.Y+sy).RGBA()

					finalA := uint8(float64(sA>>8) * alpha)
					if finalA > 0 {
						finalR := uint8(float64(sR>>8) * float64(finalA) / 255.0)
						finalG := uint8(float64(sG>>8) * float64(finalA) / 255.0)
						finalB := uint8(float64(sB>>8) * float64(finalA) / 255.0)

						dst.SetRGBA(px, py, color.RGBA{
							R: finalR,
							G: finalG,
							B: finalB,
							A: finalA,
						})
					}
				}
			}
		}
	}
}

func drawAntiAliasedRing(dst *image.RGBA, cx, cy int, r, thickness float64, c color.Color) {
	r0, g0, b0, a0 := c.RGBA()

	outerR := r + thickness/2.0
	innerR := r - thickness/2.0
	bounds := image.Rect(int(float64(cx)-outerR-1), int(float64(cy)-outerR-1), int(float64(cx)+outerR+1), int(float64(cy)+outerR+1))

	for py := bounds.Min.Y; py < bounds.Max.Y; py++ {
		for px := bounds.Min.X; px < bounds.Max.X; px++ {
			dist := math.Sqrt(math.Pow(float64(px-cx)+0.5, 2) + math.Pow(float64(py-cy)+0.5, 2))

			// Calculate coverage for outer and inner edges
			outerAlpha := 1.0 - (dist - outerR)
			innerAlpha := (dist - innerR)

			alpha := math.Min(outerAlpha, innerAlpha)
			if alpha > 1 {
				alpha = 1
			}

			if alpha < 0 {
				alpha = 0
			}

			if alpha > 0 {
				finalA := uint8(float64(a0>>8) * alpha)
				if finalA > 0 {
					finalR := uint8(float64(r0>>8) * float64(finalA) / 255.0)
					finalG := uint8(float64(g0>>8) * float64(finalA) / 255.0)
					finalB := uint8(float64(b0>>8) * float64(finalA) / 255.0)

					dst.SetRGBA(px, py, color.RGBA{
						R: finalR,
						G: finalG,
						B: finalB,
						A: finalA,
					})
				}
			}
		}
	}
}

func (r *Renderer) uploadAndDraw() {
	gl.BindTexture(gl.TEXTURE_2D, r.tex)
	gl.PixelStorei(gl.UNPACK_ALIGNMENT, 1)
	gl.TexSubImage2D(gl.TEXTURE_2D, 0, 0, 0, int32(r.widthPx), int32(r.heightPx), gl.RGBA, gl.UNSIGNED_BYTE, gl.Ptr(r.offscreen.Pix))

	gl.UseProgram(r.prog)
	gl.Uniform2f(r.uResLoc, float32(r.widthPx), float32(r.heightPx))
	gl.BindVertexArray(r.vao)
	gl.DrawArrays(gl.TRIANGLES, 0, 6)
	gl.BindVertexArray(0)
}

func drawText(dst *image.RGBA, x, y int, s string, face font.Face, col color.Color) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  &image.Uniform{C: col},
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func genPlaceholderAvatar(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	bg := color.NRGBA{R: 48, G: 51, B: 57, A: 255} // Darker discord-like background
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)
	return img
}

func compileShader(source string, shaderType uint32) (uint32, error) {
	shader := gl.CreateShader(shaderType)
	csources, free := gl.Strs(source + "\x00")

	gl.ShaderSource(shader, 1, csources, nil)
	free()
	gl.CompileShader(shader)

	var status int32
	gl.GetShaderiv(shader, gl.COMPILE_STATUS, &status)
	if status == gl.FALSE {
		var logLen int32
		gl.GetShaderiv(shader, gl.INFO_LOG_LENGTH, &logLen)
		logStr := strings.Repeat("\x00", int(logLen+1))
		gl.GetShaderInfoLog(shader, logLen, nil, gl.Str(logStr))
		return 0, fmt.Errorf("shader compile error: %s", logStr)
	}

	return shader, nil
}

func init() {
	runtime.LockOSThread()
}

func main() {
	app := &App{
		readyChan: make(chan struct{}),
		quitCh:    make(chan struct{}),
	}

	go app.runDiscordBot()
	go app.checkSpeakingTimeouts()

	log.Println("Main: Waiting for guild caches to be ready...")
	<-app.readyChan

	app.doInitialJoin()

	if err := glfw.Init(); err != nil {
		log.Fatalln("Failed to initialize GLFW:", err)
	}
	defer glfw.Terminate()

	glfw.WindowHint(glfw.Resizable, glfw.False)
	glfw.WindowHint(glfw.ContextVersionMajor, 3)
	glfw.WindowHint(glfw.ContextVersionMinor, 3)
	glfw.WindowHint(glfw.OpenGLProfile, glfw.OpenGLCoreProfile)
	glfw.WindowHint(glfw.OpenGLForwardCompatible, glfw.True)
	glfw.WindowHint(glfw.TransparentFramebuffer, glfw.True)
	glfw.WindowHint(glfw.Decorated, glfw.False)
	glfw.WindowHint(glfw.Floating, glfw.True)
	glfw.WindowHint(glfw.TransparentFramebuffer, glfw.True)

	// GLFW_TRANSPARENT_WINDOW_HINT
	if runtime.GOOS == "windows" {
		glfw.WindowHint(0x0002000D, 1)
	}

	initHeight := OverlayPadding*2 + 3*OverlayRowH
	window, err := glfw.CreateWindow(OverlayWidth, initHeight, "Discord Overlay", nil, nil)
	if err != nil {
		log.Fatalln("Failed to create window:", err)
	}

	app.window = window
	window.MakeContextCurrent()

	renderer, err := NewRenderer(window)
	if err != nil {
		log.Fatalf("renderer init: %v", err)
	}
	app.renderer = renderer

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	log.Println("Main: Entering render loop.")
	for !window.ShouldClose() {
		users := app.snapshotUsers()
		desiredHeight := OverlayPadding*2 + max(1, len(users))*OverlayRowH
		if desiredHeight != initHeight {
			window.SetSize(OverlayWidth, desiredHeight)
			initHeight = desiredHeight
		}

		app.renderer.resizeIfNeeded()

		select {
		case <-ticker.C:
			app.renderer.compose(users)
		default:
		}

		gl.ClearColor(0, 0, 0, 0)
		gl.Clear(gl.COLOR_BUFFER_BIT)

		app.renderer.uploadAndDraw()

		window.SwapBuffers()
		glfw.PollEvents()
	}

	log.Println("Main: Quit render loop.")
	cleanup(app)
}

func cleanup(app *App) {
	if app.voiceConnection != nil {
		_ = app.voiceConnection.Disconnect()
	}

	if app.session != nil {
		_ = app.session.Close()
	}
}
