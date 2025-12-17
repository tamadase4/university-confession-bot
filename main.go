package main

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	_ "github.com/mattn/go-sqlite3"
)

// Configuration variables
var (
	bot          *tgbotapi.BotAPI
	db           *sql.DB
	adminGroupID int64 = -1003206729690
	channelID    int64 = -1003440301269
)

// User states for conversation flow
type UserState struct {
	Step       string
	Data       map[string]interface{}
	LastActive time.Time
}

var (
	waitingUser       int64
	pairs             = make(map[int64]BlindChatPair)
	reports           = make(map[int64]int)
	userStates        = make(map[int64]*UserState)
	confessionWaiting = make(map[int64]string) // userID -> confessionType or empty
	commentWaiting    = make(map[int64]CommentData)
	activeKeyboards   = make(map[int64]tgbotapi.ReplyKeyboardMarkup)
	botUsername       string
)

// Blind Chat Pair structure
type BlindChatPair struct {
	PartnerID       int64
	PartnerUsername string
	PartnerName     string
}

// Comment data structure
type CommentData struct {
	ConfessionID       int
	MessageID          int
	ConfessionText     string
	UserID             int64
	Username           string
	WaitingForComment  bool
	IsViewingComments  bool
}

// Structs
type Confession struct {
	ID               int
	UserID           int64
	Text             string
	VoiceID          string
	Type             string // "text" or "voice"
	Date             time.Time
	Approved         bool
	ChannelMessageID int
	Comments         []Comment
}

type Comment struct {
	ID           int
	ConfessionID int
	UserID       int64
	Username     string
	Text         string
	Date         time.Time
	Anonymous    bool
}

type BlindProfile struct {
	UserID        int64
	Gender        string // "male" or "female"
	Age           int
	YearsOnCampus int
	YearOfStudy   string
	PrefGender    string // "male", "female", or "both"
	PrefAgeMin    int
	PrefAgeMax    int
	ProfileSet    bool
	CreatedAt     time.Time
}

func main() {
	var err error

	// Initialize bot
	bot, err = tgbotapi.NewBotAPI("7922496272:AAFXpU02Y4v8P_7ubKhwVKzmHDKa36a_Ie4")
	if err != nil {
		log.Fatal("Failed to create bot:", err)
	}

	bot.Debug = true
	botUsername = bot.Self.UserName
	log.Printf("Authorized as @%s", botUsername)

	// Initialize database
	db, err = sql.Open("sqlite3", "confess.db?_busy_timeout=5000&_journal_mode=WAL")
	if err != nil {
		log.Fatal("Failed to open database:", err)
	}
	defer db.Close()

	initDB()

	// Start polling
	u := tgbotapi.NewUpdate(0)
	u.Timeout = 60
	updates := bot.GetUpdatesChan(u)

	log.Println("🤫 Frosted Mirror Confession Bot running...")

	// Cleanup routines
	go cleanupRoutine()

	for update := range updates {
		if update.Message != nil {
			handleMessage(update.Message)
		}
		if update.CallbackQuery != nil {
			handleCallback(update.CallbackQuery)
		}
	}
}

// ----------------- DATABASE FUNCTIONS -----------------
func initDB() {
	// Drop old tables to ensure clean schema
	dropQueries := []string{
		"DROP TABLE IF EXISTS confession_comments;",
		"DROP TABLE IF EXISTS confession_reactions;",
		"DROP TABLE IF EXISTS blind_profiles;",
		"DROP TABLE IF EXISTS reports;",
		"DROP TABLE IF EXISTS confessions;",
		"DROP TABLE IF EXISTS users;",
	}

	for _, q := range dropQueries {
		if _, err := db.Exec(q); err != nil {
			log.Println("DB error dropping table:", err)
		}
	}

	// Create tables with new schema
	queries := []string{
		// Users table with gender
		`CREATE TABLE IF NOT EXISTS users (
			user_id INTEGER PRIMARY KEY,
			username TEXT,
			first_name TEXT,
			last_name TEXT,
			gender TEXT CHECK(gender IN ('male', 'female')),
			banned INTEGER DEFAULT 0,
			admin_contact_allowed INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,

		// Confessions table with channel message ID
		`CREATE TABLE IF NOT EXISTS confessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER,
			text TEXT,
			voice_id TEXT,
			type TEXT DEFAULT 'text',
			date TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			approved INTEGER DEFAULT 0,
			posted_at TIMESTAMP,
			channel_message_id INTEGER,
			FOREIGN KEY (user_id) REFERENCES users(user_id)
		);`,

		// Blind profiles table
		`CREATE TABLE IF NOT EXISTS blind_profiles (
			user_id INTEGER PRIMARY KEY,
			gender TEXT CHECK(gender IN ('male', 'female')),
			age INTEGER CHECK(age >= 18 AND age <= 50),
			years_on_campus INTEGER CHECK(years_on_campus >= 0),
			year_of_study TEXT,
			pref_gender TEXT CHECK(pref_gender IN ('male', 'female', 'both')),
			pref_age_min INTEGER CHECK(pref_age_min >= 18),
			pref_age_max INTEGER CHECK(pref_age_max <= 50),
			profile_set INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			last_updated TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(user_id)
		);`,

		// Reports table
		`CREATE TABLE IF NOT EXISTS reports (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			reporter_id INTEGER,
			reported_id INTEGER,
			reason TEXT,
			context TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (reporter_id) REFERENCES users(user_id),
			FOREIGN KEY (reported_id) REFERENCES users(user_id)
		);`,

		// Reactions table
		`CREATE TABLE IF NOT EXISTS confession_reactions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			confession_id INTEGER,
			user_id INTEGER,
			emoji TEXT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			UNIQUE(confession_id, user_id, emoji),
			FOREIGN KEY (confession_id) REFERENCES confessions(id),
			FOREIGN KEY (user_id) REFERENCES users(user_id)
		);`,

		// Comments table
		`CREATE TABLE IF NOT EXISTS confession_comments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			confession_id INTEGER,
			user_id INTEGER,
			username TEXT,
			text TEXT,
			anonymous INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (confession_id) REFERENCES confessions(id),
			FOREIGN KEY (user_id) REFERENCES users(user_id)
		);`,

		// Admin contacts table
		`CREATE TABLE IF NOT EXISTS admin_contacts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER,
			message TEXT,
			status TEXT DEFAULT 'pending',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(user_id)
		);`,
	}

	for _, q := range queries {
		if _, err := db.Exec(q); err != nil {
			log.Println("DB error creating table:", err)
		}
	}
	log.Println("💾 Database initialized with enhanced schema")
}

// ----------------- CORE HANDLERS -----------------
func handleMessage(msg *tgbotapi.Message) {
	user := msg.From
	chatID := msg.Chat.ID
	userID := user.ID

	// Check if user needs to set gender
	if chatID == userID && !hasGenderSet(userID) && !msg.IsCommand() {
		// Check if this is gender selection
		if msg.Text == "👨 Male" || msg.Text == "👩 Female" {
			handleGenderSelection(userID, chatID, msg.Text)
			return
		}
	}

	// Initialize user state if not exists
	if _, exists := userStates[userID]; !exists {
		userStates[userID] = &UserState{
			Step:       "idle",
			Data:       make(map[string]interface{}),
			LastActive: time.Now(),
		}
	}

	// Update last active
	userStates[userID].LastActive = time.Now()

	// Save user to database
	saveUser(user)

	// Check if user needs to set gender (before any other checks)
	if chatID == userID && !hasGenderSet(userID) {
		sendGenderSelection(chatID)
		return
	}

	// Check if user is banned
	if isBanned(userID) {
		sendMessage(chatID, "🤫 *Your account has been restricted.*\n\nContact admin for appeal.")
		return
	}

	// Handle start command with parameters FIRST
	if msg.IsCommand() && msg.Command() == "start" {
		handleStartCommand(msg)
		return
	}

	// Check if user is waiting to comment
	if commentData, ok := commentWaiting[userID]; ok && commentData.WaitingForComment {
		handleUserComment(userID, chatID, msg)
		return
	}

	// Handle state machine
	if userStates[userID].Step != "idle" {
		handleUserState(userID, chatID, msg)
		return
	}

	// Handle commands
	if msg.IsCommand() {
		handleCommand(msg)
		return
	}

	// Handle confession waiting state
	if confessionType, ok := confessionWaiting[userID]; ok && confessionType != "" {
		handleConfessionContent(userID, chatID, msg, confessionType)
		return
	}

	// Handle button text - IMPORTANT: Handle buttons BEFORE blind chat forwarding
	handleButtonMessage(userID, chatID, msg)

	// Handle blind chat messages (only if not a button and not in special state)
	if partner, ok := pairs[userID]; ok && !isButtonText(msg.Text) {
		handleBlindChatMessage(userID, partner, msg)
		return
	}
}

// Handle button messages separately with proper state checking
func handleButtonMessage(userID int64, chatID int64, msg *tgbotapi.Message) {
	if msg.Text == "" {
		return
	}

	// Set appropriate keyboard based on context
	if _, inPair := pairs[userID]; inPair {
		// User is in blind chat - use romantic keyboard
		activeKeyboards[chatID] = createRomanticChatKeyboard()
	} else {
		// User is not in chat - use main menu
		activeKeyboards[chatID] = createMainMenuKeyboard()
	}

	// Handle button presses
	switch msg.Text {
	case "📝 Text Confession":
		handleTextConfessionButton(userID, chatID)

	case "🎤 Voice Confession":
		handleVoiceConfessionButton(userID, chatID)

	case "💝 Blind Connections":
		handleBlindDatingCommand(userID, chatID)

	case "📞 Contact Admin":
		startAdminContact(userID, chatID)

	case "📊 My Stats":
		sendEnhancedStatusMessage(userID, chatID)

	case "📜 Guidelines":
		sendEnhancedRulesMessage(chatID)

	case "⭐ Rate Us":
		sendFeedbackMessage(chatID)

	case "❌ Cancel Search":
		if waitingUser == userID {
			waitingUser = 0
			mainMenuKeyboard := createMainMenuKeyboard()
			activeKeyboards[chatID] = mainMenuKeyboard
			sendMessageWithKeyboard(chatID,
				"✅ *Search cancelled*\n\nYou left the waiting queue.",
				mainMenuKeyboard)
		} else {
			activeKeyboards[chatID] = createMainMenuKeyboard()
			sendMessageWithKeyboard(chatID,
				"⚠️ *Not searching*\n\nYou're not currently searching.",
				createMainMenuKeyboard())
		}

	case "🏠 Main Menu":
		handleMainMenuButton(userID, chatID)

	case "💔 End Chat":
		handleEndBlindChatButton(userID, chatID)

	case "🚨 Report User":
		handleReportButton(userID, chatID)

	case "❤️ Send Heart":
		handleSendHeartButton(userID, chatID)

	case "😊 Send Smile":
		handleSendSmileButton(userID, chatID)

	case "💬 Send Voice":
		handleSendVoiceButton(userID, chatID)

	case "📸 Send Photo":
		handleSendPhotoButton(userID, chatID)

	case "❌ Cancel":
		handleCancelButton(userID, chatID)

	case "👨 Male", "👩 Female":
		// Gender selection handled separately
		return

	case "1st Year", "2nd Year", "3rd Year", "4th Year", "5th+ Year":
		if userStates[userID].Step == "profile_year_study" {
			handleProfileYearStudy(userID, chatID, msg.Text)
		}

	case "👨 Male Only", "👩 Female Only", "👫 Both Genders":
		if userStates[userID].Step == "profile_pref_gender" {
			handleProfilePrefGender(userID, chatID, msg.Text)
		}
	}
}

func handleGenderSelection(userID int64, chatID int64, genderText string) {
	gender := "male"
	if genderText == "👩 Female" {
		gender = "female"
	}

	// Save gender permanently
	err := saveUserGender(userID, gender)
	if err != nil {
		log.Println("Error saving gender:", err)
		sendMessage(chatID, "❌ *Error saving gender. Please try again.*")
		return
	}

	// Send welcome message
	sendEnhancedWelcomeMessage(chatID)
}

func sendGenderSelection(chatID int64) {
	genderKeyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("👨 Male"),
			tgbotapi.NewKeyboardButton("👩 Female"),
		),
	)

	msg := tgbotapi.NewMessage(chatID, `🎭 *WELCOME TO FROSTED MIRROR*
─────────────────────────────

✨ *Before we begin, please select your gender:*

🔒 *Important Notes:*
• Gender selection is **PERMANENT**
• Cannot be changed later
• Used for voice anonymization
• Required for blind connections
• Affects voice processing

⚠️ *Choose carefully!*`)

	msg.ParseMode = "Markdown"
	msg.ReplyMarkup = genderKeyboard
	bot.Send(msg)
}

func handleStartCommand(msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID

	// Check if user needs to set gender
	if !hasGenderSet(userID) {
		sendGenderSelection(chatID)
		return
	}

	// Check if we have a parameter after /start
	if len(msg.CommandArguments()) > 0 {
		args := msg.CommandArguments()
		
		// Check if it's a comment or view command
		if strings.HasPrefix(args, "comment") {
			confessionIDStr := strings.TrimPrefix(args, "comment")
			confessionID, err := strconv.Atoi(confessionIDStr)
			if err == nil {
				handleCommentDeepLink(userID, chatID, confessionID)
				return
			}
		} else if strings.HasPrefix(args, "view") {
			confessionIDStr := strings.TrimPrefix(args, "view")
			confessionID, err := strconv.Atoi(confessionIDStr)
			if err == nil {
				handleViewCommentsDeepLink(userID, chatID, confessionID)
				return
			}
		}
	}

	// Regular start command
	sendEnhancedWelcomeMessage(chatID)
}

func handleCommand(msg *tgbotapi.Message) {
	userID := msg.From.ID
	chatID := msg.Chat.ID
	chatType := msg.Chat.Type

	// Set appropriate keyboard based on context
	if _, inPair := pairs[userID]; inPair {
		activeKeyboards[chatID] = createRomanticChatKeyboard()
	} else {
		activeKeyboards[chatID] = createMainMenuKeyboard()
	}

	switch msg.Command() {
	case "start":
		// Already handled separately
		return

	case "confess":
		if chatType != "private" {
			sendMessage(chatID, "🔒 *Privacy First*\n\nPlease use private chat for confessions.")
			return
		}
		startConfessionFlow(userID, chatID)

	case "blind":
		if chatType != "private" {
			sendMessage(chatID, "🔒 *Private Only*\n\nBlind connections work only in private messages.")
			return
		}
		handleBlindDatingCommand(userID, chatID)

	case "end":
		handleEndBlindChatButton(userID, chatID)

	case "report":
		handleReportButton(userID, chatID)

	case "contact_admin":
		if chatType != "private" {
			sendMessage(chatID, "🔒 *Private Only*\n\nPlease contact admin in private.")
			return
		}
		startAdminContact(userID, chatID)

	case "profile":
		if chatType != "private" {
			sendMessage(chatID, "🔒 *Private Only*\n\nProfile viewing in private only.")
			return
		}
		showBlindProfile(userID, chatID)

	case "help":
		sendEnhancedHelpMessage(chatID)

	case "status":
		sendEnhancedStatusMessage(userID, chatID)

	case "rules":
		sendEnhancedRulesMessage(chatID)

	default:
		sendMessage(chatID, "❓ *Command not recognized*\n\nUse /help for available commands.")
	}
}

// ----------------- FIXED VOICE ANONYMIZATION WITH RUBBER BAND -----------------
func anonymizeVoice(voiceFileID string, gender string) (string, error) {
	// Get the voice file from Telegram
	file, err := bot.GetFile(tgbotapi.FileConfig{FileID: voiceFileID})
	if err != nil {
		return "", fmt.Errorf("failed to get file: %v", err)
	}

	// Create a temporary directory for this voice processing
	tempDir, err := os.MkdirTemp(os.TempDir(), "voice_*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp directory: %v", err)
	}
	
	// Clean up temp directory after processing
	defer func() {
		// Wait a bit to ensure file is no longer in use
		time.Sleep(2 * time.Second)
		os.RemoveAll(tempDir)
	}()

	// Create distinct file paths
	inputFile := filepath.Join(tempDir, "input.ogg")
	tempWav := filepath.Join(tempDir, "temp.wav")
	outputWav := filepath.Join(tempDir, "output.wav")
	finalOgg := filepath.Join(tempDir, "final.ogg")
	
	log.Printf("Processing voice: gender=%s", gender)

	// Download the voice file
	fileURL := fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", bot.Token, file.FilePath)
	
	// Try wget first, then curl
	downloadCmd := exec.Command("wget", "-q", "-O", inputFile, fileURL)
	if output, err := downloadCmd.CombinedOutput(); err != nil {
		log.Printf("Wget failed, trying curl: %v, output: %s", err, string(output))
		downloadCmd = exec.Command("curl", "-s", "-o", inputFile, fileURL)
		if output, err := downloadCmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("failed to download voice file: %v, output: %s", err, string(output))
		}
	}

	// Verify input file
	fileInfo, err := os.Stat(inputFile)
	if err != nil || fileInfo.Size() == 0 {
		return "", fmt.Errorf("input file invalid or empty: %v, size: %d", err, fileInfo.Size())
	}
	
	log.Printf("Input file downloaded: %d bytes", fileInfo.Size())

	// STEP 1 — Normalize & convert with FFmpeg (NO pitch, NO tempo)
	sendMessageWithKeyboard(adminGroupID, "🔄 STEP 1/3: FFmpeg normalization...", createMainMenuKeyboard())
	
	ffmpegCmd1 := exec.Command("ffmpeg",
		"-y", "-i", inputFile,
		"-ac", "1",                    // Mono
		"-ar", "48000",                // 48kHz sample rate
		"-af", "highpass=f=80, lowpass=f=14000",  // Clean frequency range
		tempWav)
	
	var stderr1 bytes.Buffer
	ffmpegCmd1.Stderr = &stderr1
	
	ctx1, cancel1 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel1()
	ffmpegCmd1 = exec.CommandContext(ctx1, ffmpegCmd1.Path, ffmpegCmd1.Args[1:]...)
	
	if err := ffmpegCmd1.Run(); err != nil {
		log.Printf("FFmpeg STEP 1 failed: %v", err)
		log.Printf("FFmpeg stderr: %s", stderr1.String())
		return "", fmt.Errorf("ffmpeg normalization failed: %v", err)
	}
	
	// Verify temp WAV file
	if _, err := os.Stat(tempWav); err != nil {
		return "", fmt.Errorf("temp WAV file not created: %v", err)
	}

	// STEP 2 — Gender-aware pitch shift using Rubber Band
	sendMessageWithKeyboard(adminGroupID, "🎵 STEP 2/3: Rubber Band pitch shifting...", createMainMenuKeyboard())
	
	// Random pitch factor for natural effect
	var pitchFactor string
	if gender == "male" {
		// Male: slight pitch variations (0.97 to 1.03)
		maleFactors := []string{"0.97", "0.99", "1.01", "1.03"}
		pitchFactor = maleFactors[rand.Intn(len(maleFactors))]
		log.Printf("Male voice: using pitch factor %s", pitchFactor)
	} else {
		// Female: slight pitch up variations (1.05 to 1.09)
		femaleFactors := []string{"1.05", "1.07", "1.09"}
		pitchFactor = femaleFactors[rand.Intn(len(femaleFactors))]
		log.Printf("Female voice: using pitch factor %s", pitchFactor)
	}

	// Rubber Band command with formant preservation
	rubberbandCmd := exec.Command("rubberband",
		"-t", "1.0",          // Tempo unchanged (100% speed)
		"-p", pitchFactor,    // Pitch factor
		"-F",                 // Formant preservation
		tempWav,
		outputWav)
	
	var stderr2 bytes.Buffer
	rubberbandCmd.Stderr = &stderr2
	
	ctx2, cancel2 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel2()
	rubberbandCmd = exec.CommandContext(ctx2, rubberbandCmd.Path, rubberbandCmd.Args[1:]...)
	
	if err := rubberbandCmd.Run(); err != nil {
		log.Printf("Rubber Band failed: %v", err)
		log.Printf("Rubber Band stderr: %s", stderr2.String())
		
		// Fallback to FFmpeg if Rubber Band not available
		sendMessageWithKeyboard(adminGroupID, "⚠️ Rubber Band not found, using FFmpeg fallback...", createMainMenuKeyboard())
		
		// Simple FFmpeg fallback that maintains speed
		if gender == "male" {
			ffmpegFallback := exec.Command("ffmpeg",
				"-y", "-i", tempWav,
				"-af", "asetrate=44100*0.85,aresample=44100,atempo=1/0.85",
				"-ar", "24000",
				outputWav)
			ffmpegFallback.Stderr = &stderr2
			ffmpegFallback = exec.CommandContext(ctx2, ffmpegFallback.Path, ffmpegFallback.Args[1:]...)
			if err := ffmpegFallback.Run(); err != nil {
				return "", fmt.Errorf("ffmpeg fallback also failed: %v", err)
			}
		} else {
			ffmpegFallback := exec.Command("ffmpeg",
				"-y", "-i", tempWav,
				"-af", "asetrate=44100*1.15,aresample=44100,atempo=1/1.15",
				"-ar", "24000",
				outputWav)
			ffmpegFallback.Stderr = &stderr2
			ffmpegFallback = exec.CommandContext(ctx2, ffmpegFallback.Path, ffmpegFallback.Args[1:]...)
			if err := ffmpegFallback.Run(); err != nil {
				return "", fmt.Errorf("ffmpeg fallback also failed: %v", err)
			}
		}
	}

	// STEP 3 — Output encoding (Telegram-ready)
	sendMessageWithKeyboard(adminGroupID, "🎧 STEP 3/3: Encoding to OGG...", createMainMenuKeyboard())
	
	ffmpegCmd3 := exec.Command("ffmpeg",
		"-y", "-i", outputWav,
		"-c:a", "libopus",    // Opus codec
		"-b:a", "64k",        // Bitrate
		finalOgg)
	
	var stderr3 bytes.Buffer
	ffmpegCmd3.Stderr = &stderr3
	
	ctx3, cancel3 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel3()
	ffmpegCmd3 = exec.CommandContext(ctx3, ffmpegCmd3.Path, ffmpegCmd3.Args[1:]...)
	
	if err := ffmpegCmd3.Run(); err != nil {
		log.Printf("FFmpeg STEP 3 failed: %v", err)
		log.Printf("FFmpeg stderr: %s", stderr3.String())
		return "", fmt.Errorf("ffmpeg encoding failed: %v", err)
	}

	// Verify output file
	if _, err := os.Stat(finalOgg); err != nil {
		return "", fmt.Errorf("output file not created: %v", err)
	}
	
	// Check output file size
	outputInfo, err := os.Stat(finalOgg)
	if err == nil {
		log.Printf("Output file size: %d bytes", outputInfo.Size())
	}

	// Read the processed file
	fileBytes, err := os.ReadFile(finalOgg)
	if err != nil {
		return "", fmt.Errorf("failed to read output file: %v", err)
	}

	// Create a FileBytes for uploading
	voiceMsg := tgbotapi.NewVoice(adminGroupID, tgbotapi.FileBytes{
		Name:  "anonymized_voice.ogg",
		Bytes: fileBytes,
	})
	
	// Add caption with processing details
	voiceMsg.Caption = fmt.Sprintf("✅ Voice Processing Complete\n\n"+
		"Gender: %s\n"+
		"Pitch factor: %s\n"+
		"Speed: 100%% (unchanged)\n"+
		"Natural voice: ✅", gender, pitchFactor)
	
	msg, err := bot.Send(voiceMsg)
	if err != nil {
		log.Printf("Telegram upload error: %v", err)
		return "", fmt.Errorf("failed to upload processed voice: %v", err)
	}

	// STEP 4 — Cleanup (important for free hosting)
	os.Remove(tempWav)
	os.Remove(outputWav)

	// Return the new file ID
	if msg.Voice != nil {
		log.Printf("Successfully processed and uploaded voice. New FileID: %s", msg.Voice.FileID)
		return msg.Voice.FileID, nil
	}
	
	return "", fmt.Errorf("no voice in response")
}

// ----------------- FIXED BLIND CHAT BUTTON HANDLERS -----------------

func handleMainMenuButton(userID int64, chatID int64) {
	if _, inPair := pairs[userID]; inPair {
		// User is in blind chat - warn them
		romanticKeyboard := createRomanticChatKeyboard()
		activeKeyboards[chatID] = romanticKeyboard
		sendMessageWithKeyboard(chatID,
			"⚠️ *You are in a blind chat!*\n\nUse '💔 End Chat' button to leave the chat first before returning to main menu.",
			romanticKeyboard)
		return
	}
	
	// Not in chat, show main menu
	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendEnhancedWelcomeMessage(chatID)
}

func handleEndBlindChatButton(userID int64, chatID int64) {
	partner, ok := pairs[userID]
	if !ok {
		if waitingUser == userID {
			waitingUser = 0
			mainMenuKeyboard := createMainMenuKeyboard()
			activeKeyboards[chatID] = mainMenuKeyboard
			sendMessageWithKeyboard(chatID,
				"❌ *Search Cancelled*\n\nYou left the waiting queue.",
				mainMenuKeyboard)
		} else {
			mainMenuKeyboard := createMainMenuKeyboard()
			activeKeyboards[chatID] = mainMenuKeyboard
			sendMessageWithKeyboard(chatID,
				"⚠️ *Not in Chat*\n\nYou're not currently in a chat.",
				mainMenuKeyboard)
		}
		return
	}

	// Remove from pairs
	delete(pairs, userID)
	delete(pairs, partner.PartnerID)

	// Clean up keyboards - set main menu for both users
	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[userID] = mainMenuKeyboard
	activeKeyboards[partner.PartnerID] = mainMenuKeyboard

	// Send romantic goodbye messages
	goodbyeMessages := []string{
		"✨ Hope you had a meaningful connection!",
		"💖 Every conversation teaches us something new!",
		"🌟 Stay positive, stay hopeful!",
		"💫 Better connections await you!",
		"🌸 Thank you for being respectful!",
	}

	randomMsg := goodbyeMessages[time.Now().Unix()%int64(len(goodbyeMessages))]

	sendMessageWithKeyboard(userID,
		fmt.Sprintf("👋 *Chat Ended*\n──────────────\n\n"+
			"✨ *Thank you for chatting with %s!*\n\n"+
			"%s\n\n"+
			"──────────────\n"+
			"Want to chat again? Use /blind", partner.PartnerUsername, randomMsg),
		mainMenuKeyboard)

	sendMessageWithKeyboard(partner.PartnerID,
		fmt.Sprintf("⚠️ *Chat Ended*\n──────────────\n\n"+
			"💬 *%s has left the chat*\n\n"+
			"%s\n\n"+
			"──────────────\n"+
			"Use /blind to find someone new", partner.PartnerUsername, randomMsg),
		mainMenuKeyboard)
}

func handleReportButton(userID int64, chatID int64) {
	partner, ok := pairs[userID]
	if !ok {
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"⚠️ *Not in Chat*\n\nYou need to be in a chat to report someone.",
			mainMenuKeyboard)
		return
	}

	// Show report reasons with inline keyboard
	reportMsg := tgbotapi.NewMessage(chatID,
		fmt.Sprintf("🚨 *Report %s*\n──────────────\n\n"+
			"⚠️ *Please select the reason:*\n\n"+
			"• Harassment or bullying\n"+
			"• Inappropriate content\n"+
			"• Fake profile/gender\n"+
			"• Personal info sharing\n"+
			"• Spamming\n"+
			"• Other violation", partner.PartnerUsername))
	reportMsg.ParseMode = "Markdown"
	reportMsg.ReplyMarkup = createReportReasonsKeyboard(partner.PartnerID)
	bot.Send(reportMsg)
}

func handleSendHeartButton(userID int64, chatID int64) {
	if partner, ok := pairs[userID]; ok {
		// Send heart to partner
		heartMsg := tgbotapi.NewMessage(partner.PartnerID, 
			fmt.Sprintf("❤️ *%s sent you a heart!*", partner.PartnerUsername))
		heartMsg.ParseMode = "Markdown"
		heartMsg.ReplyMarkup = createRomanticChatKeyboard()
		bot.Send(heartMsg)
		
		// Confirm to sender
		romanticKeyboard := createRomanticChatKeyboard()
		activeKeyboards[chatID] = romanticKeyboard
		sendMessageWithKeyboard(chatID,
			"❤️ *Heart sent to your partner!*",
			romanticKeyboard)
	} else {
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"⚠️ *Not in Chat*\n\nYou need to be in a chat to send hearts.",
			mainMenuKeyboard)
	}
}

func handleSendSmileButton(userID int64, chatID int64) {
	if partner, ok := pairs[userID]; ok {
		// Send smile to partner
		smileMsg := tgbotapi.NewMessage(partner.PartnerID, 
			fmt.Sprintf("😊 *%s sent you a smile!*", partner.PartnerUsername))
		smileMsg.ParseMode = "Markdown"
		smileMsg.ReplyMarkup = createRomanticChatKeyboard()
		bot.Send(smileMsg)
		
		// Confirm to sender
		romanticKeyboard := createRomanticChatKeyboard()
		activeKeyboards[chatID] = romanticKeyboard
		sendMessageWithKeyboard(chatID,
			"😊 *Smile sent to your partner!*",
			romanticKeyboard)
	} else {
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"⚠️ *Not in Chat*\n\nYou need to be in a chat to send smiles.",
			mainMenuKeyboard)
	}
}

func handleSendVoiceButton(userID int64, chatID int64) {
	if _, ok := pairs[userID]; ok {
		romanticKeyboard := createRomanticChatKeyboard()
		activeKeyboards[chatID] = romanticKeyboard
		sendMessageWithKeyboard(chatID,
			"🎤 *Hold the microphone button to record and send a voice message*\n\nYour voice will be anonymized automatically with Rubber Band!",
			romanticKeyboard)
	} else {
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"⚠️ *Not in Chat*\n\nYou need to be in a chat to send voice messages.",
			mainMenuKeyboard)
	}
}

func handleSendPhotoButton(userID int64, chatID int64) {
	if _, ok := pairs[userID]; ok {
		romanticKeyboard := createRomanticChatKeyboard()
		activeKeyboards[chatID] = romanticKeyboard
		sendMessageWithKeyboard(chatID,
			"📸 *Tap the attachment icon to send a photo*\n\nPhotos are not anonymized - share carefully!",
			romanticKeyboard)
	} else {
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"⚠️ *Not in Chat*\n\nYou need to be in a chat to send photos.",
			mainMenuKeyboard)
	}
}

func handleCancelButton(userID int64, chatID int64) {
	// Handle cancellation
	if confessionType, ok := confessionWaiting[userID]; ok && confessionType != "" {
		delete(confessionWaiting, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nConfession cancelled.",
			mainMenuKeyboard)
	} else if userStates[userID].Step != "idle" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nOperation cancelled.",
			mainMenuKeyboard)
	} else if _, ok := commentWaiting[userID]; ok {
		delete(commentWaiting, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nComment cancelled.",
			mainMenuKeyboard)
	} else {
		// Just show main menu
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"🏠 *Returned to main menu*",
			mainMenuKeyboard)
	}
}

// ----------------- FROSTED MIRROR STYLE -----------------
func createFrostedMirrorStyle(confessionText string) string {
	if confessionText == "" {
		return "──────────────\n🤫 Anonymous Confession\n──────────────"
	}

	// Format text with generous spacing
	formattedText := formatConfessionText(confessionText)
	
	return fmt.Sprintf("──────────────\n🤫 Anonymous Confession\n──────────────\n\n%s\n\n──────────────", formattedText)
}

func formatConfessionText(text string) string {
	// Clean and format text with proper spacing
	lines := strings.Split(text, "\n")
	var cleanLines []string
	
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			cleanLines = append(cleanLines, trimmed)
		}
	}
	
	// Join with double line breaks for generous spacing
	return strings.Join(cleanLines, "\n\n")
}

func postFrostedMirrorConfession(confessionID int, confessionType, content string, voiceID string) (int, error) {
	var messageID int

	if confessionType == "voice" {
		// Voice confession
		voiceConfig := tgbotapi.NewVoice(channelID, tgbotapi.FileID(voiceID))
		voiceConfig.Caption = createFrostedMirrorStyle("")
		voiceConfig.ParseMode = "Markdown"

		msg, err := bot.Send(voiceConfig)
		if err != nil {
			return 0, err
		}
		messageID = msg.MessageID

	} else {
		// Text confession
		fullMessage := createFrostedMirrorStyle(content)

		textConfig := tgbotapi.NewMessage(channelID, fullMessage)
		textConfig.ParseMode = "Markdown"
		textConfig.DisableWebPagePreview = true

		msg, err := bot.Send(textConfig)
		if err != nil {
			return 0, err
		}
		messageID = msg.MessageID
	}

	// Add reaction buttons for both text and voice
	addReactionButtons(confessionID, messageID)

	return messageID, nil
}

func addReactionButtons(confessionID int, messageID int) {
	reactions := []string{"❤️", "😔", "🤍", "🌫️", "🌙"}
	
	var rows [][]tgbotapi.InlineKeyboardButton
	var currentRow []tgbotapi.InlineKeyboardButton
	
	for i, reaction := range reactions {
		btn := tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%s 0", reaction), 
			fmt.Sprintf("react:%d:%s", confessionID, reaction))
		currentRow = append(currentRow, btn)
		
		// Create rows of 3 buttons each
		if (i+1)%3 == 0 || i == len(reactions)-1 {
			rows = append(rows, currentRow)
			currentRow = []tgbotapi.InlineKeyboardButton{}
		}
	}
	
	// Get comment count
	var commentCount int
	db.QueryRow("SELECT COUNT(*) FROM confession_comments WHERE confession_id = ?", confessionID).Scan(&commentCount)
	
	// Create URL buttons for comment and view comments
	commentURL := fmt.Sprintf("https://t.me/%s?start=comment%d", botUsername, confessionID)
	viewCommentsURL := fmt.Sprintf("https://t.me/%s?start=view%d", botUsername, confessionID)
	
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonURL(fmt.Sprintf("💬 Comment (%d)", commentCount), commentURL),
	})
	
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonURL(fmt.Sprintf("📊 View Comments (%d)", commentCount), viewCommentsURL),
	})
	
	keyboard := tgbotapi.NewInlineKeyboardMarkup(rows...)
	
	// Edit the message to add buttons
	editMsg := tgbotapi.NewEditMessageReplyMarkup(channelID, messageID, keyboard)
	bot.Send(editMsg)
}

// ----------------- ENHANCED COMMENTING SYSTEM -----------------
func handleCommentDeepLink(userID int64, chatID int64, confessionID int) {
	// Get confession details
	var confessionText string
	var channelMessageID int

	err := db.QueryRow(`
		SELECT text, channel_message_id 
		FROM confessions 
		WHERE id = ?`, confessionID).Scan(&confessionText, &channelMessageID)

	if err != nil {
		sendMessage(chatID, "❌ *Confession not found*\n\nThe confession you're trying to comment on doesn't exist.")
		return
	}

	// Set user to waiting for comment
	commentWaiting[userID] = CommentData{
		ConfessionID:      confessionID,
		MessageID:        channelMessageID,
		ConfessionText:   confessionText,
		UserID:          userID,
		WaitingForComment: true,
		IsViewingComments: false,
	}

	// Send comment interface
	msg := tgbotapi.NewMessage(chatID,
		fmt.Sprintf(`💬 *ADD ANONYMOUS COMMENT*
──────────────

📜 *Confession #%d:*
%s

──────────────
✨ *Write your comment below:*

📋 *Guidelines:*
• Keep it respectful & constructive
• Stay anonymous (no one sees your identity)
• Max 500 characters
• No personal information
• No harassment

🎯 *Comment Flow:*
1. Write comment below
2. Submit anonymously
3. Only count updates in channel

──────────────
*Your voice matters. Comment respectfully.*`,
			confessionID, formatConfessionText(confessionText)))
	msg.ParseMode = "Markdown"
	activeKeyboards[chatID] = createMainMenuKeyboard()
	msg.ReplyMarkup = createMainMenuKeyboard()
	bot.Send(msg)
}

func handleViewCommentsDeepLink(userID int64, chatID int64, confessionID int) {
	// Get comments from database
	rows, err := db.Query(`
		SELECT text, created_at 
		FROM confession_comments 
		WHERE confession_id = ? 
		ORDER BY created_at DESC 
		LIMIT 20`, confessionID)

	if err != nil {
		sendMessage(chatID, "❌ *Error loading comments*")
		return
	}
	defer rows.Close()

	var comments []string
	for rows.Next() {
		var text, createdAt string
		rows.Scan(&text, &createdAt)

		// Format time
		t, _ := time.Parse("2006-01-02 15:04:05", createdAt)
		timeStr := t.Format("3:04 PM")

		comments = append(comments, fmt.Sprintf("💬 *Anonymous* (%s):\n%s", timeStr, text))
	}

	if len(comments) == 0 {
		sendMessage(chatID, fmt.Sprintf("💭 *No comments yet for Confession #%d*\n\nBe the first to comment! 🤫", confessionID))
		return
	}

	// Create comment summary
	commentText := fmt.Sprintf("📊 *Comments on Confession #%d*\n──────────────\n\n", confessionID)

	for i, comment := range comments {
		commentText += fmt.Sprintf("%s\n──────────────\n", comment)
		if i == 9 { // Show only first 10 comments
			commentText += "... and more comments available\n\n"
			break
		}
	}

	commentText += fmt.Sprintf("📊 *Total: %d comments*\n", len(comments))
	commentText += "──────────────\n"
	commentText += "💬 *Want to add a comment?*\n"
	commentText += "Click the '💬 Comment' button in the channel!"

	// Send comment summary
	sendMessage(chatID, commentText)
}

func handleUserComment(userID int64, chatID int64, msg *tgbotapi.Message) {
	if chatID != userID {
		sendMessage(chatID, "🔒 *Please send comments in private chat only*")
		return
	}

	// Check for cancel
	if msg.Text == "❌ Cancel" || msg.Text == "🏠 Main Menu" {
		delete(commentWaiting, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Comment cancelled*\n\nReturned to main menu.",
			mainMenuKeyboard)
		return
	}

	if msg.Text == "" || len(msg.Text) < 1 {
		sendMessage(chatID, "📝 *Please write a valid comment (1-500 characters)*")
		return
	}

	if len(msg.Text) > 500 {
		sendMessage(chatID, "📏 *Too long*\n\nComments must be under 500 characters.")
		return
	}

	commentData := commentWaiting[userID]

	// Save comment to database
	err := saveComment(commentData.ConfessionID, userID, msg.From.UserName, msg.Text)
	if err != nil {
		log.Println("Error saving comment:", err)
		sendMessage(chatID, "❌ *Error*\n\nFailed to save comment. Please try again.")
		delete(commentWaiting, userID)
		return
	}

	// Update comment count in the channel
	updateCommentCount(commentData.ConfessionID, commentData.MessageID)

	// Clear waiting state
	delete(commentWaiting, userID)

	// Send confirmation
	confirmationMsg := fmt.Sprintf(`✅ *COMMENT ADDED ANONYMOUSLY*
──────────────

💭 *Your anonymous comment has been added to Confession #%d*

📊 *Only the comment count will update in the channel*
🔒 *No one can see your identity*
💬 *Comment is stored privately in our database*

──────────────
*Thank you for contributing respectfully!* ✨`,
		commentData.ConfessionID)

	activeKeyboards[chatID] = createMainMenuKeyboard()
	sendMessageWithKeyboard(chatID, confirmationMsg, createMainMenuKeyboard())
}

func saveComment(confessionID int, userID int64, username string, text string) error {
	_, err := db.Exec(`
		INSERT INTO confession_comments (confession_id, user_id, username, text, anonymous)
		VALUES (?, ?, ?, ?, 1)`,
		confessionID, userID, username, text)
	return err
}

func updateCommentCount(confessionID int, channelMessageID int) {
	// Get current comment count
	var count int
	db.QueryRow("SELECT COUNT(*) FROM confession_comments WHERE confession_id = ?", confessionID).Scan(&count)

	// Update the buttons in the channel message
	updateChannelButtons(confessionID, channelMessageID, count)
}

func updateChannelButtons(confessionID int, channelMessageID int, commentCount int) {
	// Get reaction counts from database
	reactions := []string{"❤️", "😔", "🤍", "🌫️", "🌙"}
	var rows [][]tgbotapi.InlineKeyboardButton
	var currentRow []tgbotapi.InlineKeyboardButton
	
	for i, reaction := range reactions {
		// Get count for this reaction
		var reactionCount int
		db.QueryRow("SELECT COUNT(*) FROM confession_reactions WHERE confession_id = ? AND emoji = ?", 
			confessionID, reaction).Scan(&reactionCount)
			
		btn := tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("%s %d", reaction, reactionCount), 
			fmt.Sprintf("react:%d:%s", confessionID, reaction))
		currentRow = append(currentRow, btn)
		
		if (i+1)%3 == 0 || i == len(reactions)-1 {
			rows = append(rows, currentRow)
			currentRow = []tgbotapi.InlineKeyboardButton{}
		}
	}
	
	// Create updated URL buttons with new count
	commentURL := fmt.Sprintf("https://t.me/%s?start=comment%d", botUsername, confessionID)
	viewCommentsURL := fmt.Sprintf("https://t.me/%s?start=view%d", botUsername, confessionID)
	
	// Update comment button with new count
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonURL(fmt.Sprintf("💬 Comment (%d)", commentCount), commentURL),
	})
	
	// Update view comments button
	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonURL(fmt.Sprintf("📊 View Comments (%d)", commentCount), viewCommentsURL),
	})
	
	// Update the message with new buttons
	editMsg := tgbotapi.NewEditMessageReplyMarkup(channelID, channelMessageID, 
		tgbotapi.NewInlineKeyboardMarkup(rows...))
	bot.Send(editMsg)
}

// ----------------- ENHANCED CONFESSION SYSTEM -----------------
func startConfessionFlow(userID int64, chatID int64) {
	activeKeyboards[chatID] = createConfessionTypeKeyboard()
	sendMessageWithKeyboard(chatID,
		"🤫 *Choose Confession Type*\n──────────────\n\n"+
			"✨ *Express yourself anonymously*\n\n"+
			"📝 *Text Confession* - Write your thoughts\n"+
			"🎤 *Voice Confession* - Speak from the heart\n\n"+
			"──────────────\n"+
			"*Both are presented in the frosted mirror style*",
		createConfessionTypeKeyboard())
}

func handleTextConfessionButton(userID int64, chatID int64) {
	confessionWaiting[userID] = "text"
	activeKeyboards[chatID] = createCancelKeyboard()
	sendMessageWithKeyboard(chatID,
		"📝 *Text Confession*\n──────────────\n\n"+
			"✨ *Write your heart out anonymously!*\n\n"+
			"📋 *Guidelines:*\n"+
			"• Min 10 characters\n"+
			"• Max 2000 characters\n"+
			"• Be respectful\n"+
			"• No personal info\n\n"+
			"💫 *Your confession will use frosted mirror style*\n"+
			"✅ *Approved confessions go to channel*\n\n"+
			"──────────────\n"+
			"*Write your confession now...*",
		createCancelKeyboard())
}

func handleVoiceConfessionButton(userID int64, chatID int64) {
	confessionWaiting[userID] = "voice"
	activeKeyboards[chatID] = createCancelKeyboard()
	sendMessageWithKeyboard(chatID,
		"🎤 *Voice Confession*\n──────────────\n\n"+
			"✨ *Speak your heart out anonymously!*\n\n"+
			"🔊 *Voice Anonymization:*\n"+
			"• Your voice will be processed with Rubber Band\n"+
			"• Converted to gender-specific anonymous voice\n"+
			"• Speed unchanged (100% natural)\n"+
			"• 100% untraceable to your real voice\n\n"+
			"📝 *Instructions:*\n"+
			"1. Press and hold microphone button\n"+
			"2. Record your confession (max 2 minutes)\n"+
			"3. Release to send\n\n"+
			"💫 *Your voice will use frosted mirror style*\n"+
			"✅ *Approved voice confessions go to channel*\n\n"+
			"──────────────\n"+
			"*Record your voice confession now...*",
		createCancelKeyboard())
}

func handleConfessionContent(userID int64, chatID int64, msg *tgbotapi.Message, confessionType string) {
	// Check if user wants to cancel
	if msg.Text == "❌ Cancel" {
		delete(confessionWaiting, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID, "❌ *Cancelled*\n\nConfession cancelled.", mainMenuKeyboard)
		return
	}

	// Handle voice confession
	if confessionType == "voice" && msg.Voice != nil {
		voiceID := msg.Voice.FileID
		duration := msg.Voice.Duration

		if duration > 120 {
			sendMessageWithKeyboard(chatID,
				"⏱️ *Too Long*\n\nVoice confession must be under 2 minutes.",
				createCancelKeyboard())
			return
		}

		// Get user's gender for voice processing
		gender, err := getUserGender(userID)
		if err != nil {
			log.Println("Error getting user gender:", err)
			gender = "male" // default
		}

		// Anonymize voice using Rubber Band pipeline
		sendMessageWithKeyboard(chatID,
			"🔊 *Processing your voice...*\n\nApplying Rubber Band voice anonymization...",
			createCancelKeyboard())

		anonymizedVoiceID, err := anonymizeVoice(voiceID, gender)
		if err != nil {
			log.Println("Error anonymizing voice:", err)
			sendMessageWithKeyboard(chatID,
				"❌ *Voice Processing Failed*\n\nPlease try again or use text confession.",
				createCancelKeyboard())
			delete(confessionWaiting, userID)
			return
		}

		// Save voice confession with anonymized voice ID
		confessionID, err := saveVoiceConfession(userID, anonymizedVoiceID)
		if err != nil {
			log.Println("Error saving voice confession:", err)
			sendMessageWithKeyboard(chatID,
				"❌ *Error*\n\nFailed to save voice confession. Please try again.",
				createCancelKeyboard())
			delete(confessionWaiting, userID)
			return
		}

		// Send to admin for approval with the ANONYMIZED voice
		sendVoiceToAdmin(int(confessionID), userID, anonymizedVoiceID, duration)
		sendConfessionSubmittedMessage(chatID, "voice")
		delete(confessionWaiting, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		return
	}

	// Handle text confession
	if confessionType == "text" && msg.Text != "" {
		text := msg.Text

		if len(text) < 10 {
			sendMessageWithKeyboard(chatID,
				"📏 *Too Short*\n\nConfession must be at least 10 characters.",
				createCancelKeyboard())
			return
		}

		if len(text) > 2000 {
			sendMessageWithKeyboard(chatID,
				"📏 *Too Long*\n\nConfession must be under 2000 characters.",
				createCancelKeyboard())
			return
		}

		confessionID, err := saveTextConfession(userID, text)
		if err != nil {
			log.Println("Error saving confession:", err)
			sendMessageWithKeyboard(chatID,
				"❌ *Error*\n\nFailed to save confession. Please try again.",
				createCancelKeyboard())
			delete(confessionWaiting, userID)
			return
		}

		// Send to admin for approval
		sendTextToAdmin(int(confessionID), userID, text)
		sendConfessionSubmittedMessage(chatID, "text")
		delete(confessionWaiting, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		return
	}

	// If no valid content
	if confessionType == "voice" {
		sendMessageWithKeyboard(chatID,
			"❓ *Invalid Content*\n\nPlease send a voice message.",
			createCancelKeyboard())
	} else {
		sendMessageWithKeyboard(chatID,
			"❓ *Invalid Content*\n\nPlease send text.",
			createCancelKeyboard())
	}
}

func sendConfessionSubmittedMessage(chatID int64, confessionType string) {
	message := "🤫 *Confession Received*\n──────────────\n\n"

	if confessionType == "voice" {
		message += "🎤 *Voice confession captured & anonymized*\n\n"
	} else {
		message += "📝 *Text confession written*\n\n"
	}

	message += "✨ *Your words are safe with us*\n\n" +
		"📋 *Status:* Awaiting approval\n" +
		"🎨 *Style:* Frosted mirror presentation\n" +
		"💫 *Reactions:* Emotional responses enabled\n" +
		"👁️ *Note:* 100% anonymous\n\n" +
		"──────────────\n" +
		"*Your confession will appear in the channel when approved.*"

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, message, mainMenuKeyboard)
}

// ----------------- ENHANCED BLIND DATING SYSTEM -----------------
func handleBlindDatingCommand(userID int64, chatID int64) {
	// Check if user has gender set
	gender, err := getUserGender(userID)
	if err != nil {
		sendMessageWithKeyboard(chatID,
			"🎭 *Gender Required*\n\nPlease set your gender first to use blind connections.",
			createMainMenuKeyboard())
		return
	}

	// Check if user has profile
	profile, err := getBlindProfile(userID)
	if err != nil || !profile.ProfileSet {
		// Start profile creation with pre-filled gender
		startProfileCreation(userID, chatID, gender)
		return
	}

	// Check if already in chat
	if _, ok := pairs[userID]; ok {
		romanticKeyboard := createRomanticChatKeyboard()
		activeKeyboards[chatID] = romanticKeyboard
		sendMessageWithKeyboard(chatID,
			"💬 *Already Connected*\n\nYou're in a chat. Use '💔 End Chat' button to leave.",
			romanticKeyboard)
		return
	}

	// Find matching partner with opposite gender restriction
	partnerID := findMatchingPartner(userID, profile)
	if partnerID == 0 {
		// No match found, join waiting
		waitingUser = userID
		cancelSearchKeyboard := createCancelSearchKeyboard()
		activeKeyboards[chatID] = cancelSearchKeyboard
		sendMessageWithKeyboard(chatID,
			"🔍 *Finding Your Match...*\n──────────────\n\n"+
				"✨ *Based on your preferences:*\n"+
				fmt.Sprintf("• 👫 Gender: %s\n", profile.PrefGender)+
				fmt.Sprintf("• 🎂 Age: %d-%d years\n", profile.PrefAgeMin, profile.PrefAgeMax)+
				fmt.Sprintf("• 🎓 Year: %s\n\n", profile.YearOfStudy)+
				"💝 *Looking for compatible heart...*\n\n"+
				"──────────────\n"+
				"*This may take a few moments*",
			cancelSearchKeyboard)
		return
	}

	// Connect the pair
	connectBlindPair(userID, partnerID)
}

func startProfileCreation(userID int64, chatID int64, gender string) {
	userStates[userID] = &UserState{
		Step:       "profile_age",
		Data:       make(map[string]interface{}),
		LastActive: time.Now(),
	}

	// Pre-fill gender from user's profile
	userStates[userID].Data["gender"] = gender

	cancelKeyboard := createCancelKeyboard()
	activeKeyboards[chatID] = cancelKeyboard
	sendMessageWithKeyboard(chatID,
		fmt.Sprintf(`💝 *Blind Connection Profile*
─────────────────────────────

✨ *Create your connection profile*
✅ *Gender:* %s (permanent)

─────────────────────────────
*Step 1 of 6:* What is your age?
✨ *Please enter your age (18-50):*`,
			map[string]string{"male": "👨 Male", "female": "👩 Female"}[gender]),
		cancelKeyboard)
}

func handleUserState(userID int64, chatID int64, msg *tgbotapi.Message) {
	state := userStates[userID]
	if state == nil {
		return
	}

	switch state.Step {
	case "profile_age":
		handleProfileAge(userID, chatID, msg.Text)

	case "profile_years_campus":
		handleProfileYearsCampus(userID, chatID, msg.Text)

	case "profile_year_study":
		handleProfileYearStudy(userID, chatID, msg.Text)

	case "profile_pref_gender":
		handleProfilePrefGender(userID, chatID, msg.Text)

	case "profile_pref_age":
		handleProfilePrefAge(userID, chatID, msg.Text)

	case "admin_contact":
		handleAdminContactMessage(userID, chatID, msg)
	}
}

func handleProfileAge(userID int64, chatID int64, ageStr string) {
	// Check for cancel
	if ageStr == "❌ Cancel" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nProfile creation cancelled.",
			mainMenuKeyboard)
		return
	}

	age, err := strconv.Atoi(ageStr)
	if err != nil || age < 18 || age > 50 {
		sendMessageWithKeyboard(chatID,
			"❌ *Invalid age*\n\nPlease enter a valid age between 18 and 50.",
			createCancelKeyboard())
		return
	}

	userStates[userID].Data["age"] = age
	userStates[userID].Step = "profile_years_campus"

	sendMessageWithKeyboard(chatID,
		"✅ *Age saved*\n──────────────\n\n"+
			"*Step 2 of 6:* How many years on campus?\n\n"+
			"✨ *Enter number of years (0-10):*",
		createCancelKeyboard())
}

func handleProfileYearsCampus(userID int64, chatID int64, yearsStr string) {
	// Check for cancel
	if yearsStr == "❌ Cancel" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nProfile creation cancelled.",
			mainMenuKeyboard)
		return
	}

	years, err := strconv.Atoi(yearsStr)
	if err != nil || years < 0 || years > 10 {
		sendMessageWithKeyboard(chatID,
			"❌ *Invalid years*\n\nPlease enter valid years (0-10).",
			createCancelKeyboard())
		return
	}

	userStates[userID].Data["years_on_campus"] = years
	userStates[userID].Step = "profile_year_study"

	yearStudyKeyboard := createYearStudyKeyboard()
	activeKeyboards[chatID] = yearStudyKeyboard
	sendMessageWithKeyboard(chatID,
		"✅ *Years saved*\n──────────────\n\n"+
			"*Step 3 of 6:* Current year of study?\n\n"+
			"✨ *Select your current year:*",
		yearStudyKeyboard)
}

func handleProfileYearStudy(userID int64, chatID int64, year string) {
	// Check for cancel
	if year == "❌ Cancel" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nProfile creation cancelled.",
			mainMenuKeyboard)
		return
	}

	// Validate year
	validYears := []string{"1st Year", "2nd Year", "3rd Year", "4th Year", "5th+ Year"}
	valid := false
	for _, y := range validYears {
		if y == year {
			valid = true
			break
		}
	}

	if !valid {
		sendMessageWithKeyboard(chatID,
			"❌ *Please select a valid year*",
			createYearStudyKeyboard())
		return
	}

	userStates[userID].Data["year_of_study"] = year
	userStates[userID].Step = "profile_pref_gender"

	prefGenderKeyboard := createPrefGenderKeyboard()
	activeKeyboards[chatID] = prefGenderKeyboard
	sendMessageWithKeyboard(chatID,
		"✅ *Year saved*\n──────────────\n\n"+
			"*Step 4 of 6:* Preferred gender to meet?\n\n"+
			"✨ *Who would you like to connect with?*",
		prefGenderKeyboard)
}

func handleProfilePrefGender(userID int64, chatID int64, pref string) {
	// Check for cancel
	if pref == "❌ Cancel" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nProfile creation cancelled.",
			mainMenuKeyboard)
		return
	}

	var prefValue string
	switch pref {
	case "👨 Male Only":
		prefValue = "male"
	case "👩 Female Only":
		prefValue = "female"
	case "👫 Both Genders":
		prefValue = "both"
	default:
		sendMessageWithKeyboard(chatID,
			"❌ *Please select a valid preference*",
			createPrefGenderKeyboard())
		return
	}

	userStates[userID].Data["pref_gender"] = prefValue
	userStates[userID].Step = "profile_pref_age"

	cancelKeyboard := createCancelKeyboard()
	activeKeyboards[chatID] = cancelKeyboard
	sendMessageWithKeyboard(chatID,
		"✅ *Preference saved*\n──────────────\n\n"+
			"*Step 5 of 6:* Preferred age range?\n\n"+
			"✨ *Enter minimum age (18-50):*",
		cancelKeyboard)
}

func handleProfilePrefAge(userID int64, chatID int64, ageStr string) {
	// Check for cancel
	if ageStr == "❌ Cancel" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nProfile creation cancelled.",
			mainMenuKeyboard)
		return
	}

	state := userStates[userID]

	// First message is min age
	if _, hasMin := state.Data["pref_age_min"]; !hasMin {
		minAge, err := strconv.Atoi(ageStr)
		if err != nil || minAge < 18 || minAge > 50 {
			sendMessageWithKeyboard(chatID,
				"❌ *Invalid age*\n\nPlease enter valid age (18-50).",
				createCancelKeyboard())
			return
		}

		state.Data["pref_age_min"] = minAge
		sendMessageWithKeyboard(chatID,
			fmt.Sprintf("✅ *Min age: %d*\n\n✨ *Now enter maximum age (18-50, >= %d):*", minAge, minAge),
			createCancelKeyboard())
		return
	}

	// Second message is max age
	maxAge, err := strconv.Atoi(ageStr)
	if err != nil {
		sendMessageWithKeyboard(chatID,
			"❌ *Invalid age*\n\nPlease enter a valid number.",
			createCancelKeyboard())
		return
	}

	minAge := state.Data["pref_age_min"].(int)
	if maxAge < minAge || maxAge > 50 {
		sendMessageWithKeyboard(chatID,
			fmt.Sprintf("❌ *Invalid range*\n\nMax age must be between %d and 50.", minAge),
			createCancelKeyboard())
		return
	}

	state.Data["pref_age_max"] = maxAge

	// Save complete profile
	saveBlindProfile(userID, state.Data)
	delete(userStates, userID)

	// Show profile summary
	showBlindProfile(userID, chatID)
}

func findMatchingPartner(userID int64, profile *BlindProfile) int64 {
	// Don't match with yourself
	if waitingUser != 0 && waitingUser != userID {
		waitingProfile, err := getBlindProfile(waitingUser)
		if err == nil && waitingProfile.ProfileSet {
			// Check mutual compatibility with opposite gender restriction
			if areProfilesCompatible(profile, waitingProfile) {
				return waitingUser
			}
		}
	}
	return 0
}

func areProfilesCompatible(p1, p2 *BlindProfile) bool {
	// Gender compatibility - ENFORCE OPPOSITE GENDER ONLY
	if p1.Gender == p2.Gender {
		return false // Same gender not allowed
	}

	// Check preferences
	if !isGenderCompatible(p1, p2) || !isGenderCompatible(p2, p1) {
		return false
	}

	// Age compatibility
	if !isAgeCompatible(p1, p2) || !isAgeCompatible(p2, p1) {
		return false
	}

	return true
}

func isGenderCompatible(p1, p2 *BlindProfile) bool {
	switch p1.PrefGender {
	case "male":
		return p2.Gender == "male"
	case "female":
		return p2.Gender == "female"
	case "both":
		return true
	default:
		return false
	}
}

func isAgeCompatible(p1, p2 *BlindProfile) bool {
	return p2.Age >= p1.PrefAgeMin && p2.Age <= p1.PrefAgeMax
}

func connectBlindPair(userID, partnerID int64) {
	// Get usernames for both users
	var userUsername, partnerUsername string
	var userName, partnerName string

	// Get user's username from state or database
	if state, ok := userStates[userID]; ok && state.Data["username"] != nil {
		userUsername = state.Data["username"].(string)
	} else {
		db.QueryRow("SELECT username, first_name FROM users WHERE user_id = ?", userID).Scan(&userUsername, &userName)
	}

	if state, ok := userStates[partnerID]; ok && state.Data["username"] != nil {
		partnerUsername = state.Data["username"].(string)
	} else {
		db.QueryRow("SELECT username, first_name FROM users WHERE user_id = ?", partnerID).Scan(&partnerUsername, &partnerName)
	}

	// If no username, use first name
	if userUsername == "" {
		userUsername = userName
	}
	if partnerUsername == "" {
		partnerUsername = partnerName
	}

	pairs[userID] = BlindChatPair{
		PartnerID:       partnerID,
		PartnerUsername: partnerUsername,
		PartnerName:     partnerName,
	}

	pairs[partnerID] = BlindChatPair{
		PartnerID:       userID,
		PartnerUsername: userUsername,
		PartnerName:     userName,
	}

	waitingUser = 0

	// Log the connection
	log.Printf("💝 Blind pair connected: %d + %d", userID, partnerID)

	// Send connection messages
	connectionMsg := fmt.Sprintf(`💖 *Connection Made!*
──────────────

✨ *You're now connected with %s*

🌹 *This is a safe, anonymous space*
💬 *Chat freely and respectfully*
🎤 *Voice messages allowed for verification*
📸 *Photos allowed for authenticity*

──────────────
*Use the buttons below to interact:*
❤️ Send Heart - Express affection
😊 Send Smile - Send a smile
💬 Send Voice - Record voice message
📸 Send Photo - Share a photo
💔 End Chat - Leave the chat
🚨 Report User - Report inappropriate behavior

──────────────
*Ground Rules:*
1. Be respectful always
2. No personal information
3. Report any discomfort
4. Enjoy the connection!`, partnerUsername)

	// Set romantic keyboard for both users
	romanticKeyboard := createRomanticChatKeyboard()
	activeKeyboards[userID] = romanticKeyboard
	activeKeyboards[partnerID] = romanticKeyboard

	// Send to both users with romantic keyboards
	sendMessageWithKeyboard(userID, connectionMsg, romanticKeyboard)
	sendMessageWithKeyboard(partnerID, connectionMsg, romanticKeyboard)
}

// ----------------- ADMIN CONTACT SYSTEM -----------------
func startAdminContact(userID int64, chatID int64) {
	// Check if user can contact admin
	if !canContactAdmin(userID) {
		sendMessage(chatID,
			"⏳ *Rate Limited*\n\n"+
				"You can contact admin once per week.\n"+
				"Please wait before sending another message.")
		return
	}

	userStates[userID] = &UserState{
		Step:       "admin_contact",
		Data:       make(map[string]interface{}),
		LastActive: time.Now(),
	}

	cancelKeyboard := createCancelKeyboard()
	activeKeyboards[chatID] = cancelKeyboard
	sendMessageWithKeyboard(chatID,
		"📞 *Contact Admin*\n──────────────\n\n"+
			"✨ *Send one message to admin team*\n\n"+
			"📝 *Please write your message below:*\n"+
			"• Questions\n"+
			"• Suggestions\n"+
			"• Reports\n"+
			"• Feedback\n\n"+
			"──────────────\n"+
			"*Note:* One-way message only\n"+
			"Admin will contact you if needed.",
		cancelKeyboard)
}

func handleAdminContactMessage(userID int64, chatID int64, msg *tgbotapi.Message) {
	// Check for cancel
	if msg.Text == "❌ Cancel" {
		delete(userStates, userID)
		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[chatID] = mainMenuKeyboard
		sendMessageWithKeyboard(chatID,
			"❌ *Cancelled*\n\nAdmin contact cancelled.",
			mainMenuKeyboard)
		return
	}

	delete(userStates, userID)

	// Save admin contact
	err := saveAdminContact(userID, msg.Text)
	if err != nil {
		sendMessage(chatID, "❌ *Error*\n\nFailed to send message. Please try again.")
		return
	}

	// Forward to admin group
	adminMsg := fmt.Sprintf(
		"📬 *ADMIN MESSAGE*\n──────────────\n\n"+
			"👤 *From:* `%d`\n"+
			"📱 *Username:* @%s\n"+
			"🕐 *Time:* %s\n\n"+
			"💭 *Message:*\n%s\n\n"+
			"──────────────",
		userID, msg.From.UserName, time.Now().Format("Jan 2, 3:04 PM"), msg.Text)

	sendMessage(adminGroupID, adminMsg)

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	// Notify user
	sendMessageWithKeyboard(chatID,
		"✅ *Message Sent*\n──────────────\n\n"+
			"✨ *Your message has been delivered to admin team*\n\n"+
			"📋 *Status:* Received\n"+
			"⏳ *Response:* If needed\n\n"+
			"──────────────\n"+
			"Thank you for your feedback! 💖",
		mainMenuKeyboard)
}

// ----------------- KEYBOARD FUNCTIONS -----------------
func createMainMenuKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("📝 Text Confession"),
			tgbotapi.NewKeyboardButton("🎤 Voice Confession"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("💝 Blind Connections"),
			tgbotapi.NewKeyboardButton("📞 Contact Admin"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("📊 My Stats"),
			tgbotapi.NewKeyboardButton("📜 Guidelines"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createConfessionTypeKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("📝 Text Confession"),
			tgbotapi.NewKeyboardButton("🎤 Voice Confession"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("❌ Cancel"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createCancelKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("❌ Cancel"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createGenderKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("👨 Male"),
			tgbotapi.NewKeyboardButton("👩 Female"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createYearStudyKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("1st Year"),
			tgbotapi.NewKeyboardButton("2nd Year"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("3rd Year"),
			tgbotapi.NewKeyboardButton("4th Year"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("5th+ Year"),
			tgbotapi.NewKeyboardButton("❌ Cancel"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createPrefGenderKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("👨 Male Only"),
			tgbotapi.NewKeyboardButton("👩 Female Only"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("👫 Both Genders"),
			tgbotapi.NewKeyboardButton("❌ Cancel"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createRomanticChatKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("❤️ Send Heart"),
			tgbotapi.NewKeyboardButton("😊 Send Smile"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("💬 Send Voice"),
			tgbotapi.NewKeyboardButton("📸 Send Photo"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("💔 End Chat"),
			tgbotapi.NewKeyboardButton("🚨 Report User"),
		),
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("🏠 Main Menu"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createCancelSearchKeyboard() tgbotapi.ReplyKeyboardMarkup {
	keyboard := tgbotapi.NewReplyKeyboard(
		tgbotapi.NewKeyboardButtonRow(
			tgbotapi.NewKeyboardButton("❌ Cancel Search"),
		),
	)
	keyboard.ResizeKeyboard = true
	keyboard.OneTimeKeyboard = false
	return keyboard
}

func createAdminApprovalKeyboard(confessionID int, confessionType string) tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		{
			tgbotapi.NewInlineKeyboardButtonData("✅ Approve", fmt.Sprintf("approve:%d:%s", confessionID, confessionType)),
			tgbotapi.NewInlineKeyboardButtonData("❌ Reject", fmt.Sprintf("reject:%d", confessionID)),
		},
	}

	if confessionType == "voice" {
		rows = append(rows, []tgbotapi.InlineKeyboardButton{
			tgbotapi.NewInlineKeyboardButtonData("🎤 Listen", fmt.Sprintf("listen:%d", confessionID)),
		})
	}

	rows = append(rows, []tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardButtonData("🚫 Ban User", fmt.Sprintf("ban:%d", confessionID)),
	})

	return tgbotapi.NewInlineKeyboardMarkup(rows...)
}

func createReportReasonsKeyboard(reportedID int64) tgbotapi.InlineKeyboardMarkup {
	return tgbotapi.NewInlineKeyboardMarkup(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🚫 Harassment", fmt.Sprintf("report_reason:Harassment:%d", reportedID)),
			tgbotapi.NewInlineKeyboardButtonData("🎭 Fake Profile", fmt.Sprintf("report_reason:Fake Profile:%d", reportedID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("🔞 Inappropriate", fmt.Sprintf("report_reason:Inappropriate:%d", reportedID)),
			tgbotapi.NewInlineKeyboardButtonData("🔒 Personal Info", fmt.Sprintf("report_reason:Personal Info:%d", reportedID)),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("📢 Spamming", fmt.Sprintf("report_reason:Spamming:%d", reportedID)),
			tgbotapi.NewInlineKeyboardButtonData("⚠️ Other", fmt.Sprintf("report_reason:Other:%d", reportedID)),
		),
	)
}

// ----------------- UTILITY FUNCTIONS -----------------
func sendMessage(chatID int64, text string) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true

	// Check if we have an active keyboard for this chat
	if keyboard, ok := activeKeyboards[chatID]; ok {
		msg.ReplyMarkup = keyboard
	} else if chatID > 0 {
		// Default to main menu for private chats
		msg.ReplyMarkup = createMainMenuKeyboard()
	}

	_, err := bot.Send(msg)
	if err != nil {
		log.Println("Error sending message:", err)
	}
}

func sendMessageWithKeyboard(chatID int64, text string, keyboard tgbotapi.ReplyKeyboardMarkup) {
	msg := tgbotapi.NewMessage(chatID, text)
	msg.ParseMode = "Markdown"
	msg.DisableWebPagePreview = true
	msg.ReplyMarkup = keyboard

	// Store this keyboard as active for the chat
	activeKeyboards[chatID] = keyboard

	_, err := bot.Send(msg)
	if err != nil {
		log.Println("Error sending message:", err)
	}
}

func sendEnhancedWelcomeMessage(chatID int64) {
	welcomeText := `🤫 *FROSTED MIRROR CONFESSION BOT*
─────────────────────────────

✨ *A minimal, professional space for anonymous expression*

─────────────────────────────
*FEATURES:*
📝 *Text Confessions* - Write anonymously 
🎤 *Voice Confessions* - Speak softly with Rubber Band anonymization
💝 *Blind Connections* - Verified matches
🎨 *Frosted Mirror Style* - Clean, professional presentation

─────────────────────────────
*THE EXPERIENCE:*
✅ 100% Anonymous • 🎨 Minimal design
🔊 Rubber Band Voice Protection • 🔒 Safe space
📊 Comment system • ✨ Premium aesthetic

─────────────────────────────
*Express yourself. Anonymously.* 👇`

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, welcomeText, mainMenuKeyboard)
}

// ----------------- DATABASE OPERATIONS -----------------
func saveUser(user *tgbotapi.User) {
	_, err := db.Exec(`
		INSERT OR IGNORE INTO users (user_id, username, first_name, last_name) 
		VALUES (?, ?, ?, ?)`,
		user.ID, user.UserName, user.FirstName, user.LastName)
	if err != nil {
		log.Println("Failed to save user:", err)
	}
}

func saveUserGender(userID int64, gender string) error {
	_, err := db.Exec(`
		UPDATE users SET gender = ? WHERE user_id = ?`,
		gender, userID)
	return err
}

func hasGenderSet(userID int64) bool {
	var gender string
	err := db.QueryRow("SELECT gender FROM users WHERE user_id = ?", userID).Scan(&gender)
	if err != nil && err != sql.ErrNoRows {
		log.Println("Error checking gender:", err)
	}
	return gender != ""
}

func getUserGender(userID int64) (string, error) {
	var gender string
	err := db.QueryRow("SELECT gender FROM users WHERE user_id = ?", userID).Scan(&gender)
	if err != nil {
		return "", err
	}
	return gender, nil
}

func isBanned(userID int64) bool {
	var banned int
	err := db.QueryRow("SELECT banned FROM users WHERE user_id = ?", userID).Scan(&banned)
	if err != nil && err != sql.ErrNoRows {
		log.Println("Error checking ban:", err)
	}
	return banned == 1
}

func saveTextConfession(userID int64, text string) (int64, error) {
	result, err := db.Exec(`
		INSERT INTO confessions (user_id, text, type, date) 
		VALUES (?, ?, 'text', datetime('now'))`,
		userID, text)
	if err != nil {
		return 0, err
	}

	confessionID, _ := result.LastInsertId()
	return confessionID, nil
}

func saveVoiceConfession(userID int64, voiceID string) (int64, error) {
	result, err := db.Exec(`
		INSERT INTO confessions (user_id, voice_id, type, date) 
		VALUES (?, ?, 'voice', datetime('now'))`,
		userID, voiceID)
	if err != nil {
		return 0, err
	}

	confessionID, _ := result.LastInsertId()
	return confessionID, nil
}

func sendTextToAdmin(confessionID int, userID int64, text string) {
	adminText := fmt.Sprintf(
		"📝 *NEW TEXT CONFESSION* #%d\n──────────────\n\n"+
			"💭 *Content:*\n%s\n\n"+
			"──────────────\n"+
			"👤 *Sender ID:* `%d`\n"+
			"🕐 *Time:* %s\n"+
			"📊 *Type:* Text Confession\n"+
			"──────────────",
		confessionID, text, userID, time.Now().Format("Jan 2, 3:04 PM"))

	adminMsg := tgbotapi.NewMessage(adminGroupID, adminText)
	adminMsg.ParseMode = "Markdown"
	adminMsg.ReplyMarkup = createAdminApprovalKeyboard(confessionID, "text")
	bot.Send(adminMsg)
}

func sendVoiceToAdmin(confessionID int, userID int64, voiceID string, duration int) {
	adminText := fmt.Sprintf(
		"🎤 *NEW VOICE CONFESSION* #%d\n──────────────\n\n"+
			"⏱️ *Duration:* %d seconds\n"+
			"🔊 *Status:* Rubber Band-anonymized\n"+
			"⚡ *Speed:* 100%% (unchanged)\n\n"+
			"──────────────\n"+
			"👤 *Sender ID:* `%d`\n"+
			"🕐 *Time:* %s\n"+
			"📊 *Type:* Voice Confession\n"+
			"──────────────",
		confessionID, duration, userID, time.Now().Format("Jan 2, 3:04 PM"))

	// Send voice with caption - using the ANONYMIZED voice ID
	voiceMsg := tgbotapi.NewVoice(adminGroupID, tgbotapi.FileID(voiceID))
	voiceMsg.Caption = adminText
	voiceMsg.ParseMode = "Markdown"
	voiceMsg.ReplyMarkup = createAdminApprovalKeyboard(confessionID, "voice")
	bot.Send(voiceMsg)
}

func saveBlindProfile(userID int64, data map[string]interface{}) error {
	_, err := db.Exec(`
		INSERT OR REPLACE INTO blind_profiles 
		(user_id, gender, age, years_on_campus, year_of_study, pref_gender, pref_age_min, pref_age_max, profile_set, last_updated)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1, datetime('now'))`,
		userID,
		data["gender"],
		data["age"],
		data["years_on_campus"],
		data["year_of_study"],
		data["pref_gender"],
		data["pref_age_min"],
		data["pref_age_max"])

	if err != nil {
		log.Println("Error saving blind profile:", err)
		return err
	}

	return nil
}

func getBlindProfile(userID int64) (*BlindProfile, error) {
	var profile BlindProfile
	var createdAtStr string

	err := db.QueryRow(`
		SELECT user_id, gender, age, years_on_campus, year_of_study, 
		       pref_gender, pref_age_min, pref_age_max, profile_set, created_at
		FROM blind_profiles WHERE user_id = ?`, userID).Scan(
		&profile.UserID,
		&profile.Gender,
		&profile.Age,
		&profile.YearsOnCampus,
		&profile.YearOfStudy,
		&profile.PrefGender,
		&profile.PrefAgeMin,
		&profile.PrefAgeMax,
		&profile.ProfileSet,
		&createdAtStr)

	if err != nil {
		return nil, err
	}

	profile.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", createdAtStr)
	return &profile, nil
}

func showBlindProfile(userID int64, chatID int64) {
	profile, err := getBlindProfile(userID)
	if err != nil {
		sendMessage(chatID, "📋 *No Profile Found*\n\nCreate your blind connection profile first!")
		return
	}

	genderEmoji := "👨"
	if profile.Gender == "female" {
		genderEmoji = "👩"
	}

	prefGenderText := profile.PrefGender
	switch profile.PrefGender {
	case "male":
		prefGenderText = "👨 Male Only"
	case "female":
		prefGenderText = "👩 Female Only"
	case "both":
		prefGenderText = "👫 Both Genders"
	}

	profileText := fmt.Sprintf(`📋 *YOUR CONNECTION PROFILE*
─────────────────────────────

%s *Gender:* %s (permanent)
🎂 *Age:* %d years
🎓 *Years on Campus:* %d
📚 *Year of Study:* %s

─────────────────────────────
*PREFERENCES:*
👫 *Looking for:* %s
🎂 *Age Range:* %d - %d years

─────────────────────────────
*PROFILE INFO:*
✅ *Status:* Verified
📅 *Created:* %s
🔒 *Note:* Profile cannot be changed

─────────────────────────────
*Ready to find your match?*
Use /blind to start searching!`,
		genderEmoji, strings.Title(profile.Gender),
		profile.Age,
		profile.YearsOnCampus,
		profile.YearOfStudy,
		prefGenderText,
		profile.PrefAgeMin,
		profile.PrefAgeMax,
		profile.CreatedAt.Format("Jan 2, 2006"))

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, profileText, mainMenuKeyboard)
}

func saveAdminContact(userID int64, message string) error {
	_, err := db.Exec(`
		INSERT INTO admin_contacts (user_id, message, status)
		VALUES (?, ?, 'pending')`,
		userID, message)

	if err != nil {
		log.Println("Error saving admin contact:", err)
		return err
	}

	// Update user's last contact time
	_, err = db.Exec(`
		UPDATE users SET admin_contact_allowed = 0 
		WHERE user_id = ?`, userID)

	return err
}

func canContactAdmin(userID int64) bool {
	var allowed int
	err := db.QueryRow(`
		SELECT admin_contact_allowed FROM users 
		WHERE user_id = ?`, userID).Scan(&allowed)

	if err != nil {
		return true
	}

	return allowed == 1
}

// ----------------- BLIND CHAT MESSAGE HANDLING -----------------
func handleBlindChatMessage(senderID int64, partner BlindChatPair, msg *tgbotapi.Message) {
	// Always keep romantic keyboard for blind chats
	romanticKeyboard := createRomanticChatKeyboard()
	activeKeyboards[senderID] = romanticKeyboard
	activeKeyboards[partner.PartnerID] = romanticKeyboard

	// Forward text messages with partner's username
	if msg.Text != "" {
		// Don't forward button texts
		if isButtonText(msg.Text) {
			return
		}
		formattedMsg := fmt.Sprintf("💬 *From %s:*\n%s", partner.PartnerUsername, msg.Text)
		sendMessageWithKeyboard(partner.PartnerID, formattedMsg, romanticKeyboard)
		return
	}

	// Forward voice messages (allowed for verification)
	if msg.Voice != nil {
		if msg.Voice.Duration > 60 {
			sendMessageWithKeyboard(senderID,
				"⏱️ *Voice too long*\n\nKeep voice messages under 1 minute.",
				romanticKeyboard)
			return
		}

		// Get sender's gender for voice anonymization
		gender, err := getUserGender(senderID)
		if err == nil {
			// Anonymize the voice
			anonymizedVoiceID, err := anonymizeVoice(msg.Voice.FileID, gender)
			if err == nil {
				voiceMsg := tgbotapi.NewVoice(partner.PartnerID, tgbotapi.FileID(anonymizedVoiceID))
				voiceMsg.Caption = fmt.Sprintf("🎤 *Voice from %s*", partner.PartnerUsername)
				voiceMsg.ReplyMarkup = romanticKeyboard
				bot.Send(voiceMsg)
			} else {
				log.Printf("Voice anonymization failed: %v", err)
				// If anonymization fails, send original voice
				voiceMsg := tgbotapi.NewVoice(partner.PartnerID, tgbotapi.FileID(msg.Voice.FileID))
				voiceMsg.Caption = fmt.Sprintf("🎤 *Voice from %s*", partner.PartnerUsername)
				voiceMsg.ReplyMarkup = romanticKeyboard
				bot.Send(voiceMsg)
			}
		} else {
			// If can't get gender, send original voice
			voiceMsg := tgbotapi.NewVoice(partner.PartnerID, tgbotapi.FileID(msg.Voice.FileID))
			voiceMsg.Caption = fmt.Sprintf("🎤 *Voice from %s*", partner.PartnerUsername)
			voiceMsg.ReplyMarkup = romanticKeyboard
			bot.Send(voiceMsg)
		}

		// Send tip to partner
		sendMessageWithKeyboard(partner.PartnerID,
			"💡 *Tip:* Voice messages are anonymized for privacy using Rubber Band!",
			romanticKeyboard)
		return
	}

	// Forward photos (allowed for authenticity)
	if msg.Photo != nil {
		photo := tgbotapi.NewPhoto(partner.PartnerID, tgbotapi.FileID(msg.Photo[len(msg.Photo)-1].FileID))
		photo.Caption = msg.Caption
		if photo.Caption != "" {
			photo.Caption = fmt.Sprintf("📸 *Photo from %s:*\n%s", partner.PartnerUsername, photo.Caption)
		} else {
			photo.Caption = fmt.Sprintf("📸 *Photo from %s*", partner.PartnerUsername)
		}
		photo.ReplyMarkup = romanticKeyboard
		bot.Send(photo)
		return
	}

	// Forward other media types
	if msg.Document != nil {
		doc := tgbotapi.NewDocument(partner.PartnerID, tgbotapi.FileID(msg.Document.FileID))
		doc.Caption = fmt.Sprintf("📎 *File from %s*", partner.PartnerUsername)
		doc.ReplyMarkup = romanticKeyboard
		bot.Send(doc)
		return
	}

	if msg.Sticker != nil {
		sticker := tgbotapi.NewSticker(partner.PartnerID, tgbotapi.FileID(msg.Sticker.FileID))
		sticker.ReplyMarkup = romanticKeyboard
		bot.Send(sticker)
		return
	}
}

func isButtonText(text string) bool {
	buttonTexts := []string{
		"📝 Text Confession", "🎤 Voice Confession", "💝 Blind Connections",
		"📞 Contact Admin", "📊 My Stats", "📜 Guidelines", "⭐ Rate Us",
		"❌ Cancel Search", "🏠 Main Menu", "💔 End Chat", "🚨 Report User",
		"❤️ Send Heart", "😊 Send Smile", "💬 Send Voice", "📸 Send Photo",
		"❌ Cancel", "👨 Male", "👩 Female", "1st Year", "2nd Year",
		"3rd Year", "4th Year", "5th+ Year", "👨 Male Only", "👩 Female Only",
		"👫 Both Genders",
	}
	
	for _, buttonText := range buttonTexts {
		if text == buttonText {
			return true
		}
	}
	return false
}

// ----------------- FIXED CALLBACK HANDLER -----------------
func handleCallback(cb *tgbotapi.CallbackQuery) {
	data := cb.Data
	parts := strings.Split(data, ":")

	if len(parts) < 1 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	action := parts[0]

	switch action {
	case "approve":
		handleApproveCallback(parts, cb)

	case "reject":
		handleRejectCallback(parts, cb)

	case "ban":
		handleBanCallback(parts, cb)

	case "listen":
		handleListenCallback(parts, cb)

	case "react":
		handleReactionCallback(parts, cb)

	case "report_reason":
		handleReportReasonCallback(parts, cb)

	default:
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Unknown action"))
	}
}

func handleApproveCallback(parts []string, cb *tgbotapi.CallbackQuery) {
	if len(parts) < 3 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	confessionID, _ := strconv.Atoi(parts[1])
	confessionType := parts[2]

	// Get confession from database
	var confession Confession
	var voiceID, text string

	if confessionType == "voice" {
		err := db.QueryRow(`
			SELECT id, user_id, voice_id, date 
			FROM confessions WHERE id = ?`, confessionID).Scan(
			&confession.ID, &confession.UserID, &voiceID, &confession.Date)
		if err != nil {
			log.Println("Error getting voice confession:", err)
			bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
			return
		}
	} else {
		err := db.QueryRow(`
			SELECT id, user_id, text, date 
			FROM confessions WHERE id = ?`, confessionID).Scan(
			&confession.ID, &confession.UserID, &text, &confession.Date)
		if err != nil {
			log.Println("Error getting text confession:", err)
			bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
			return
		}
	}

	// Update confession status
	_, err := db.Exec(`
		UPDATE confessions 
		SET approved = 1, posted_at = datetime('now')
		WHERE id = ?`, confessionID)
	if err != nil {
		log.Println("Error approving confession:", err)
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	// Post to channel with FROSTED MIRROR style
	channelMsgID, err := postFrostedMirrorConfession(confessionID, confessionType, text, voiceID)
	if err != nil {
		log.Println("Error posting confession:", err)
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	// Save channel message ID
	_, err = db.Exec("UPDATE confessions SET channel_message_id = ? WHERE id = ?", channelMsgID, confessionID)
	if err != nil {
		log.Println("Error saving channel message ID:", err)
	}

	// Update admin message
	statusText := "✅"
	if confessionType == "voice" {
		statusText = "✅ *VOICE APPROVED*"
	} else {
		statusText = "✅ *TEXT APPROVED*"
	}

	editMsg := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("%s #%d\n──────────────\n\n"+
			"✨ *Published with frosted mirror style*\n\n"+
			"👤 *Sender ID:* `%d`\n"+
			"✅ *Status:* Posted to channel\n"+
			"🎨 *Style:* Minimal, professional\n"+
			"💬 *System:* Reaction & comment enabled\n"+
			"🕐 *Time:* %s",
			statusText, confessionID, confession.UserID, time.Now().Format("3:04 PM")))
	editMsg.ParseMode = "Markdown"
	bot.Send(editMsg)

	// Remove buttons
	editMarkup := tgbotapi.NewEditMessageReplyMarkup(cb.Message.Chat.ID, cb.Message.MessageID,
		tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
	bot.Send(editMarkup)

	// Notify user
	userMsg := tgbotapi.NewMessage(confession.UserID,
		fmt.Sprintf("✅ *CONFESSION PUBLISHED*\n──────────────\n\n"+
			"✨ *Your %s confession is now live*\n\n"+
			"📜 *Confession ID:* #%d\n"+
			"✅ *Status:* Published anonymously\n"+
			"🎨 *Style:* Frosted mirror presentation\n"+
			"💫 *People can react with emotional responses*\n"+
			"💬 *Comments:* Viewable via comment button\n\n"+
			"──────────────\n"+
			"*Thank you for sharing.*",
			confessionType, confessionID))
	userMsg.ParseMode = "Markdown"
	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[confession.UserID] = mainMenuKeyboard
	userMsg.ReplyMarkup = mainMenuKeyboard
	bot.Send(userMsg)

	bot.Send(tgbotapi.NewCallback(cb.ID, "✅ Published"))
}

func handleRejectCallback(parts []string, cb *tgbotapi.CallbackQuery) {
	if len(parts) < 2 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	confessionID, _ := strconv.Atoi(parts[1])

	// Get confession
	var confession Confession
	err := db.QueryRow(`
		SELECT id, user_id, type, date 
		FROM confessions WHERE id = ?`, confessionID).Scan(
		&confession.ID, &confession.UserID, &confession.Type, &confession.Date)
	if err != nil {
		log.Println("Error getting confession:", err)
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	// Update confession status
	_, err = db.Exec(`
		UPDATE confessions 
		SET approved = 0
		WHERE id = ?`, confessionID)
	if err != nil {
		log.Println("Error rejecting confession:", err)
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	// Update admin message
	editMsg := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("❌ *REJECTED* #%d\n──────────────\n\n"+
			"👤 *Sender ID:* `%d`\n"+
			"❌ *Status:* Not approved\n"+
			"📊 *Type:* %s\n"+
			"🕐 *Time:* %s",
			confessionID, confession.UserID, strings.Title(confession.Type),
			time.Now().Format("3:04 PM")))
	editMsg.ParseMode = "Markdown"
	bot.Send(editMsg)

	// Remove buttons
	editMarkup := tgbotapi.NewEditMessageReplyMarkup(cb.Message.Chat.ID, cb.Message.MessageID,
		tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
	bot.Send(editMarkup)

	// Notify user
	userMsg := tgbotapi.NewMessage(confession.UserID,
		fmt.Sprintf("❌ *CONFESSION REVIEWED*\n──────────────\n\n"+
			"📋 *Status:* Not Approved\n\n"+
			"⚠️ *Reason:* Content didn't meet guidelines\n\n"+
			"💡 *Tips:*\n"+
			"• Keep it respectful\n"+
			"• Be positive\n"+
			"• Avoid personal info\n\n"+
			"──────────────\n"+
			"✨ *You can try again now!*"))
	userMsg.ParseMode = "Markdown"
	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[confession.UserID] = mainMenuKeyboard
	userMsg.ReplyMarkup = mainMenuKeyboard
	bot.Send(userMsg)

	bot.Send(tgbotapi.NewCallback(cb.ID, "✅ Rejected"))
}

func handleBanCallback(parts []string, cb *tgbotapi.CallbackQuery) {
	if len(parts) < 2 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	confessionID, _ := strconv.Atoi(parts[1])

	// Get confession
	var confession Confession
	err := db.QueryRow(`
		SELECT id, user_id, type, date 
		FROM confessions WHERE id = ?`, confessionID).Scan(
		&confession.ID, &confession.UserID, &confession.Type, &confession.Date)
	if err != nil {
		log.Println("Error getting confession:", err)
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	// Ban user
	_, err = db.Exec("UPDATE users SET banned = 1 WHERE user_id = ?", confession.UserID)
	if err != nil {
		log.Println("Error banning user:", err)
	}

	// Update confession status
	_, err = db.Exec(`
		UPDATE confessions 
		SET approved = 0
		WHERE id = ?`, confessionID)
	if err != nil {
		log.Println("Error rejecting confession:", err)
	}

	// Update admin message
	editMsg := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
		fmt.Sprintf("🚫 *USER BANNED* #%d\n──────────────\n\n"+
			"👤 *User ID:* `%d`\n"+
			"🚫 *Status:* Banned & Rejected\n"+
			"📊 *Type:* %s\n"+
			"🕐 *Time:* %s\n\n"+
			"──────────────\n"+
			"User can no longer use the bot.",
			confessionID, confession.UserID, strings.Title(confession.Type),
			time.Now().Format("3:04 PM")))
	editMsg.ParseMode = "Markdown"
	bot.Send(editMsg)

	// Remove buttons
	editMarkup := tgbotapi.NewEditMessageReplyMarkup(cb.Message.Chat.ID, cb.Message.MessageID,
		tgbotapi.InlineKeyboardMarkup{InlineKeyboard: [][]tgbotapi.InlineKeyboardButton{}})
	bot.Send(editMarkup)

	// Notify user
	userMsg := tgbotapi.NewMessage(confession.UserID,
		"🚫 *ACCOUNT BANNED*\n──────────────\n\n"+
			"⛔ *Your account has been banned.*\n\n"+
			"📋 *Reason:* Violation of community guidelines\n"+
			"⏳ *Duration:* Permanent\n\n"+
			"📞 *Contact admin for appeal*\n\n"+
			"──────────────\n"+
			"⚠️ *You can no longer use this bot.*")
	userMsg.ParseMode = "Markdown"
	bot.Send(userMsg)

	bot.Send(tgbotapi.NewCallback(cb.ID, "✅ User banned"))
}

func handleListenCallback(parts []string, cb *tgbotapi.CallbackQuery) {
	if len(parts) < 2 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	confessionID, _ := strconv.Atoi(parts[1])

	// Get voice file ID
	var voiceID string
	err := db.QueryRow(`
		SELECT voice_id FROM confessions 
		WHERE id = ?`, confessionID).Scan(&voiceID)

	if err != nil || voiceID == "" {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Voice not available"))
		return
	}

	// Send voice to admin
	voiceMsg := tgbotapi.NewVoice(cb.Message.Chat.ID, tgbotapi.FileID(voiceID))
	voiceMsg.Caption = fmt.Sprintf("🎤 *Voice Confession #%d*\n\nClick ▶️ to listen", confessionID)
	bot.Send(voiceMsg)

	bot.Send(tgbotapi.NewCallback(cb.ID, "✅ Voice sent"))
}

func handleReactionCallback(parts []string, cb *tgbotapi.CallbackQuery) {
	if len(parts) < 3 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	confessionID, _ := strconv.Atoi(parts[1])
	emoji := parts[2]
	userID := cb.From.ID

	// Check if user already reacted with this emoji
	var exists int
	db.QueryRow(`
		SELECT COUNT(*) FROM confession_reactions 
		WHERE confession_id = ? AND user_id = ? AND emoji = ?`,
		confessionID, userID, emoji).Scan(&exists)

	if exists > 0 {
		// Remove reaction
		db.Exec(`
			DELETE FROM confession_reactions 
			WHERE confession_id = ? AND user_id = ? AND emoji = ?`,
			confessionID, userID, emoji)
	} else {
		// Add reaction
		db.Exec(`
			INSERT INTO confession_reactions (confession_id, user_id, emoji)
			VALUES (?, ?, ?)`,
			confessionID, userID, emoji)
	}

	// Get comment count
	var commentCount int
	db.QueryRow("SELECT COUNT(*) FROM confession_comments WHERE confession_id = ?", confessionID).Scan(&commentCount)

	// Update the channel message with new counts
	updateChannelButtons(confessionID, cb.Message.MessageID, commentCount)

	bot.Send(tgbotapi.NewCallback(cb.ID, "✅ Reaction updated"))
}

func handleReportReasonCallback(parts []string, cb *tgbotapi.CallbackQuery) {
	if len(parts) < 3 {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error"))
		return
	}

	reason := parts[1]
	reportedID, _ := strconv.ParseInt(parts[2], 10, 64)
	reporterID := cb.From.ID

	// Get reporter's current partner to verify they're in chat
	reporterPartner, ok := pairs[reporterID]
	if !ok {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ You're not in a chat"))
		return
	}

	// Verify reportedID matches the partner
	if reporterPartner.PartnerID != reportedID {
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Invalid user to report"))
		return
	}

	// Increment report count
	reports[reportedID]++

	// Save report to database
	_, err := db.Exec(`
		INSERT INTO reports (reporter_id, reported_id, reason, context)
		VALUES (?, ?, ?, ?)`,
		reporterID, reportedID, reason, "Blind chat")

	if err != nil {
		log.Println("Error saving report:", err)
		bot.Send(tgbotapi.NewCallback(cb.ID, "❌ Error saving report"))
		return
	}

	// Get reported user's username
	var reportedUsername string
	db.QueryRow("SELECT username FROM users WHERE user_id = ?", reportedID).Scan(&reportedUsername)
	if reportedUsername == "" {
		reportedUsername = "Unknown"
	}

	// Notify reporter
	sendMessage(reporterID,
		fmt.Sprintf("✅ *Report Submitted*\n──────────────\n\n"+
			"📋 *Reported:* %s\n"+
			"📝 *Reason:* %s\n"+
			"📊 *Reports against user:* %d/3\n\n"+
			"⚠️ *User will be banned after 3 reports*\n\n"+
			"──────────────\n"+
			"Thank you for keeping our community safe! 💖",
			reportedUsername, reason, reports[reportedID]))

	// Check if user should be banned
	if reports[reportedID] >= 3 {
		banUser(reportedID)
		endBlindChatForUser(reportedID)

		// Notify admin
		sendMessage(adminGroupID,
			fmt.Sprintf("🚫 *USER AUTO-BANNED*\n──────────────\n\n"+
				"👤 *User ID:* `%d`\n"+
				"👤 *Username:* %s\n"+
				"📋 *Reason:* 3+ blind chat reports\n"+
				"🚨 *Last Report:* %s\n"+
				"🕐 *Time:* %s\n\n"+
				"──────────────\n"+
				"User has been automatically banned.",
				reportedID, reportedUsername, reason, time.Now().Format("3:04 PM")))
	}

	// Edit original message
	editMsg := tgbotapi.NewEditMessageText(cb.Message.Chat.ID, cb.Message.MessageID,
		"✅ *Report Received*\n\nThank you for your report! The user has been reported.")
	editMsg.ParseMode = "Markdown"
	bot.Send(editMsg)

	bot.Send(tgbotapi.NewCallback(cb.ID, "✅ Report submitted"))
}

func endBlindChatForUser(userID int64) {
	if partner, ok := pairs[userID]; ok {
		delete(pairs, userID)
		delete(pairs, partner.PartnerID)
		delete(activeKeyboards, userID)
		delete(activeKeyboards, partner.PartnerID)

		mainMenuKeyboard := createMainMenuKeyboard()
		activeKeyboards[partner.PartnerID] = mainMenuKeyboard
		sendMessageWithKeyboard(partner.PartnerID,
			fmt.Sprintf("⚠️ *Chat Ended*\n──────────────\n\n"+
				"💬 *%s has been removed*\n\n"+
				"🔒 *Reason:* Multiple user reports\n"+
				"✨ *You can find a new partner with /blind*", partner.PartnerUsername),
			mainMenuKeyboard)
	}
}

func banUser(userID int64) {
	db.Exec("UPDATE users SET banned = 1 WHERE user_id = ?", userID)
}

// ----------------- CLEANUP ROUTINES -----------------
func cleanupRoutine() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		cleanupOldStates()
		cleanupWaitingUsers()
		cleanupExpiredContacts()
		cleanupOldCommentWaiting()
		cleanupStaleKeyboards()
	}
}

func cleanupOldStates() {
	now := time.Now()
	for userID, state := range userStates {
		if now.Sub(state.LastActive) > 30*time.Minute {
			delete(userStates, userID)
			delete(confessionWaiting, userID)
		}
	}
}

func cleanupWaitingUsers() {
	// Remove users who have been waiting too long
	if waitingUser != 0 {
		if state, exists := userStates[waitingUser]; exists {
			if time.Since(state.LastActive) > 30*time.Minute {
				waitingUser = 0
			}
		} else {
			// If no state exists, assume they left
			waitingUser = 0
		}
	}
}

func cleanupExpiredContacts() {
	// Allow users to contact admin again after 7 days
	db.Exec(`
		UPDATE users 
		SET admin_contact_allowed = 1 
		WHERE user_id IN (
			SELECT user_id FROM admin_contacts 
			WHERE created_at < datetime('now', '-7 days')
		)`)
}

func cleanupOldCommentWaiting() {
	// Clean up old comment waiting states (older than 10 minutes)
	now := time.Now()
	for userID, data := range commentWaiting {
		if state, exists := userStates[userID]; exists {
			if now.Sub(state.LastActive) > 10*time.Minute {
				// Notify user
				if data.WaitingForComment {
					sendMessage(userID,
						"⏰ *Comment session expired*\n\n"+
							"Your comment session has timed out. Please click the comment button again if you still want to comment.")
				}
				delete(commentWaiting, userID)
			}
		} else {
			// If no user state exists, remove
			delete(commentWaiting, userID)
		}
	}
}

func cleanupStaleKeyboards() {
	// Clean up keyboards for users who haven't been active
	now := time.Now()
	for userID := range activeKeyboards {
		if state, exists := userStates[userID]; exists {
			if now.Sub(state.LastActive) > 30*time.Minute {
				delete(activeKeyboards, userID)
			}
		} else {
			// If no user state exists, remove after 1 hour
			delete(activeKeyboards, userID)
		}
	}
}

// ----------------- MESSAGE HELPERS -----------------
func sendEnhancedHelpMessage(chatID int64) {
	helpText := `📚 *FROSTED MIRROR HELP GUIDE*
─────────────────────────────

✨ *CONFESSION SYSTEM*
📝 *Text Confessions*
• Click "Text Confession" button
• Write anonymously (10-2000 chars)
• Reviewed by admins
• Posted with frosted mirror style

🎤 *Voice Confessions*
• Click "Voice Confession" button
• Rubber Band voice anonymization
• Gender-specific voice conversion
• 100% untraceable to your real voice
• Speed unchanged (100% natural)

─────────────────────────────
💝 *BLIND CONNECTION SYSTEM*
• Permanent gender selection required
• Username collection during registration
• Opposite gender matching only
• Voice messages anonymized with Rubber Band
• Safe, respectful environment
• Report fake profiles

─────────────────────────────
💬 *COMMENT SYSTEM*
• Click "💬 Comment" URL button in channel
• Opens bot in private chat
• Anonymous commenting
• Only comment count updates in channel
• Comments stored privately

─────────────────────────────
📊 *REACTION SYSTEM*
• ❤️ Like
• 😔 Sad
• 🤍 Support
• 🌫️ Confused
• 🌙 Relate
• Click to react, click again to remove

─────────────────────────────
📞 *ADMIN CONTACT*
• One message per week
• Direct to admin team
• No back-and-forth
• For feedback/questions

─────────────────────────────
*Need more help?*
Use /contact_admin to message us directly.

*Enjoy the minimal, professional experience!* 🤫✨`

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, helpText, mainMenuKeyboard)
}

func sendEnhancedStatusMessage(userID, chatID int64) {
	// Get user stats
	var confessionCount, voiceCount, approvedCount int
	var lastConfession string

	db.QueryRow(`
		SELECT 
			COUNT(*),
			SUM(CASE WHEN type = 'voice' THEN 1 ELSE 0 END),
			SUM(CASE WHEN approved = 1 THEN 1 ELSE 0 END),
			MAX(date)
		FROM confessions 
		WHERE user_id = ?`, userID).Scan(
		&confessionCount, &voiceCount, &approvedCount, &lastConfession)

	// Get user gender
	var gender string
	db.QueryRow("SELECT gender FROM users WHERE user_id = ?", userID).Scan(&gender)
	genderDisplay := "Not set"
	if gender == "male" {
		genderDisplay = "👨 Male (permanent)"
	} else if gender == "female" {
		genderDisplay = "👩 Female (permanent)"
	}

	// Get blind profile status
	var hasProfile bool
	db.QueryRow(`
		SELECT COUNT(*) FROM blind_profiles 
		WHERE user_id = ? AND profile_set = 1`, userID).Scan(&hasProfile)

	// Check if in chat
	inChat := "❌ No"
	if _, ok := pairs[userID]; ok {
		inChat = "✅ Yes"
	}

	// Get comment count
	var commentCount int
	db.QueryRow(`
		SELECT COUNT(*) FROM confession_comments 
		WHERE user_id = ?`, userID).Scan(&commentCount)

	// Get reaction count
	var reactionCount int
	db.QueryRow(`
		SELECT COUNT(*) FROM confession_reactions 
		WHERE user_id = ?`, userID).Scan(&reactionCount)

	statusText := fmt.Sprintf(`📊 *USER STATISTICS*
─────────────────────────────

*👤 PROFILE*
• Gender: %s
• Status: %s

*📝 CONFESSIONS*
• Total: %d confessions
• Voice: %d voice notes
• Approved: %d published
• Rate Limit: ✅ Unlimited

*💬 ENGAGEMENT*
• Comments: %d comments
• Reactions: %d reactions
• Blind Chats: %s

*💝 BLIND CONNECTIONS*
• Profile: %s
• Reports: %d/3

*📞 ADMIN CONTACT*
• Status: %s

─────────────────────────────
*TIPS FOR SUCCESS:*
• Be authentic in confessions
• Complete your connection profile
• Respect community guidelines
• Engage with reactions & comments

*KEEP SHARING RESPECTFULLY!* ✨`,
		genderDisplay,
		func() string {
			if isBanned(userID) {
				return "🚫 Banned"
			} else {
				return "✅ Active"
			}
		}(),
		confessionCount, voiceCount, approvedCount,
		commentCount, reactionCount, inChat,
		func() string {
			if hasProfile {
				return "✅ Set"
			} else {
				return "❌ Not set"
			}
		}(),
		reports[userID],
		func() string {
			if canContactAdmin(userID) {
				return "✅ Allowed"
			} else {
				return "⏳ Weekly limit"
			}
		}())

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, statusText, mainMenuKeyboard)
}

func sendEnhancedRulesMessage(chatID int64) {
	rulesText := `📜 *COMMUNITY GUIDELINES*
─────────────────────────────

*1. RESPECT & KINDNESS* 🙏
• No hate speech of any kind
• Respect all genders & identities
• Be kind in all interactions

*2. AUTHENTICITY & SAFETY* 🔒
• Gender selection is PERMANENT
• No fake profiles in blind connections
• Voice messages are anonymized with Rubber Band
• Never share personal information

*3. APPROPRIATE CONTENT* ✅
• Confessions should be respectful
• No explicit or harmful content
• Voice confessions max 2 minutes

*4. COMMENT SYSTEM* 💬
• Comments are anonymous
• Keep comments respectful
• No harassment in comments
• Use reactions to show support

*5. BLIND CONNECTION ETIQUETTE* 💝
• Complete profile honestly with username
• Opposite gender matching only
• Respect gender preferences
• Report fake profiles immediately
• End chats respectfully

─────────────────────────────
*CONSEQUENCES OF VIOLATIONS:*
1️⃣ First offense: Warning
2️⃣ Second offense: Temporary ban
3️⃣ Third offense: Permanent ban
🔴 Fake profiles: Immediate ban

─────────────────────────────
*OUR MISSION:*
Create a safe, anonymous space for
expression, connection, and community
building with minimal, professional design.

*THANK YOU FOR BEING AMAZING!* ✨🤫`

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, rulesText, mainMenuKeyboard)
}

func sendFeedbackMessage(chatID int64) {
	feedbackText := `⭐ *FEEDBACK & RATING*
─────────────────────────────

✨ *We value your experience!*

*How are we doing?*
• Frosted mirror design
• Features & functionality
• Response time
• Community safety
• Overall experience

*📊 Rate this bot:*
1️⃣ ⭐ - Needs improvement
2️⃣ ⭐⭐ - Okay
3️⃣ ⭐⭐⭐ - Good
4️⃣ ⭐⭐⭐⭐ - Very good
5️⃣ ⭐⭐⭐⭐⭐ - Excellent!

*💭 Suggestions welcome:*
Use /contact_admin to share detailed feedback.

*🤝 Share with friends:*
Help grow our community!

─────────────────────────────
*THANK YOU FOR USING OUR BOT!* 🤫✨`

	mainMenuKeyboard := createMainMenuKeyboard()
	activeKeyboards[chatID] = mainMenuKeyboard
	sendMessageWithKeyboard(chatID, feedbackText, mainMenuKeyboard)
}
