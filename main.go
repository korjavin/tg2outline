package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

// mediaGroupFlushDelay is how long to wait after the last message in a media
// group before treating the group as complete. Telegram delivers album items
// as separate updates ~instantly, so a short delay is enough.
const mediaGroupFlushDelay = 2 * time.Second

func main() {
	botToken := os.Getenv("BOT_TOKEN")
	outlineToken := os.Getenv("OUTLINE_API_TOKEN")
	outlineURL := os.Getenv("OUTLINE_URL")
	collectionID := os.Getenv("OUTLINE_COLLECTION_ID")
	tgUserIDStr := os.Getenv("TG_USER_ID")

	if botToken == "" || outlineToken == "" || outlineURL == "" || collectionID == "" || tgUserIDStr == "" {
		log.Fatal("BOT_TOKEN, OUTLINE_API_TOKEN, OUTLINE_URL, OUTLINE_COLLECTION_ID, and TG_USER_ID environment variables must be set")
	}

	tgUserID, err := strconv.ParseInt(tgUserIDStr, 10, 64)
	if err != nil {
		log.Fatalf("Invalid TG_USER_ID: %v", err)
	}

	outline := NewOutlineClient(outlineURL, outlineToken)

	bot, err := tgbotapi.NewBotAPI(botToken)
	if err != nil {
		log.Fatalf("Failed to initialize Telegram bot: %v", err)
	}

	log.Printf("Authorized on account %s", bot.Self.UserName)
	log.Printf("Outline target: %s (collection %s)", outlineURL, collectionID)

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = 60

	updates := bot.GetUpdatesChan(updateConfig)

	groups := newMediaGroupBuffer(func(msgs []*tgbotapi.Message) {
		processMessages(bot, msgs, outline, collectionID)
	})

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.From.ID != tgUserID {
			log.Printf("Unauthorized message from user ID: %d", update.Message.From.ID)
			continue
		}
		if update.Message.MediaGroupID != "" {
			groups.add(update.Message)
			continue
		}
		processMessages(bot, []*tgbotapi.Message{update.Message}, outline, collectionID)
	}
}

// processMessages collapses one or more Telegram messages (typically a media
// group / album) into a single Outline document.
func processMessages(bot *tgbotapi.BotAPI, messages []*tgbotapi.Message, outline *OutlineClient, collectionID string) {
	if len(messages) == 0 {
		return
	}
	// Stable order across the album.
	sort.SliceStable(messages, func(i, j int) bool {
		return messages[i].MessageID < messages[j].MessageID
	})
	primary := messages[0]

	var rawTextParts, textParts, mediaParts []string
	var forwardInfo string

	for _, m := range messages {
		if forwardInfo == "" {
			forwardInfo = forwardHeader(m)
		}
		if m.Text != "" {
			rawTextParts = append(rawTextParts, m.Text)
			textParts = append(textParts, entitiesToMarkdown(m.Text, m.Entities))
		}
		if m.Caption != "" {
			rawTextParts = append(rawTextParts, m.Caption)
			textParts = append(textParts, entitiesToMarkdown(m.Caption, m.CaptionEntities))
		}
		if md := uploadMedia(bot, m, outline); md != "" {
			mediaParts = append(mediaParts, md)
		}
	}

	rawText := strings.Join(rawTextParts, "\n\n")
	messageText := strings.Join(textParts, "\n\n")

	if messageText == "" && len(mediaParts) == 0 && forwardInfo == "" {
		reply := tgbotapi.NewMessage(primary.Chat.ID, "Cannot add empty message to Outline.")
		reply.ReplyToMessageID = primary.MessageID
		bot.Send(reply)
		return
	}

	var bodyParts []string
	if forwardInfo != "" {
		bodyParts = append(bodyParts, "> "+forwardInfo)
	}
	if messageText != "" {
		bodyParts = append(bodyParts, messageText)
	}
	bodyParts = append(bodyParts, mediaParts...)
	body := strings.Join(bodyParts, "\n\n")

	title := generateTitle(rawText, len(mediaParts) > 0)

	if _, err := outline.CreateDocument(collectionID, title, body); err != nil {
		log.Printf("Failed to create document in Outline: %v", err)
		reply := tgbotapi.NewMessage(primary.Chat.ID, fmt.Sprintf("Error adding to Outline: %v", err))
		reply.ReplyToMessageID = primary.MessageID
		bot.Send(reply)
		return
	}

	confirmation := "Added to Outline"
	switch {
	case len(messages) > 1:
		confirmation = fmt.Sprintf("Added to Outline (%d items)", len(messages))
	case len(mediaParts) > 0:
		confirmation += " with media"
	}
	reply := tgbotapi.NewMessage(primary.Chat.ID, confirmation)
	reply.ReplyToMessageID = primary.MessageID
	bot.Send(reply)
}

func forwardHeader(m *tgbotapi.Message) string {
	switch {
	case m.ForwardFrom != nil:
		return fmt.Sprintf("Forwarded from: %s %s (@%s)",
			m.ForwardFrom.FirstName,
			m.ForwardFrom.LastName,
			m.ForwardFrom.UserName)
	case m.ForwardFromChat != nil:
		s := fmt.Sprintf("Forwarded from: %s", m.ForwardFromChat.Title)
		if m.ForwardFromChat.UserName != "" {
			s += fmt.Sprintf(" (@%s)", m.ForwardFromChat.UserName)
		}
		return s
	}
	return ""
}

// uploadMedia uploads any photo/video/animation/document/audio/voice attached
// to the message and returns the markdown to embed it. Returns "" if there's
// no recognized media or the upload failed (errors are logged).
func uploadMedia(bot *tgbotapi.BotAPI, m *tgbotapi.Message, outline *OutlineClient) string {
	var fileID, name, contentType string
	isImage := false

	switch {
	case m.Photo != nil:
		ph := m.Photo[len(m.Photo)-1]
		fileID = ph.FileID
		name = fmt.Sprintf("telegram-%d.jpg", m.MessageID)
		contentType = "image/jpeg"
		isImage = true
	case m.Video != nil:
		fileID = m.Video.FileID
		contentType = firstNonEmpty(m.Video.MimeType, "video/mp4")
		name = mediaFileName(m.Video.FileName, m.MessageID, contentType, ".mp4")
	case m.Animation != nil:
		fileID = m.Animation.FileID
		contentType = firstNonEmpty(m.Animation.MimeType, "video/mp4")
		name = mediaFileName(m.Animation.FileName, m.MessageID, contentType, ".mp4")
	case m.Document != nil:
		fileID = m.Document.FileID
		contentType = firstNonEmpty(m.Document.MimeType, "application/octet-stream")
		name = mediaFileName(m.Document.FileName, m.MessageID, contentType, "")
		isImage = strings.HasPrefix(contentType, "image/")
	case m.Audio != nil:
		fileID = m.Audio.FileID
		contentType = firstNonEmpty(m.Audio.MimeType, "audio/mpeg")
		name = mediaFileName(m.Audio.FileName, m.MessageID, contentType, ".mp3")
	case m.Voice != nil:
		fileID = m.Voice.FileID
		contentType = firstNonEmpty(m.Voice.MimeType, "audio/ogg")
		name = fmt.Sprintf("telegram-voice-%d.ogg", m.MessageID)
	default:
		return ""
	}

	fileURL, err := bot.GetFileDirectURL(fileID)
	if err != nil {
		log.Printf("Failed to get file URL for message %d: %v", m.MessageID, err)
		return ""
	}
	data, err := DownloadFile(fileURL)
	if err != nil {
		log.Printf("Failed to download %s: %v", name, err)
		return ""
	}
	attachmentURL, err := outline.UploadAttachment(name, contentType, data)
	if err != nil {
		log.Printf("Failed to upload %s to Outline: %v", name, err)
		return ""
	}
	log.Printf("Uploaded %s to Outline: %s", name, attachmentURL)
	if isImage {
		return fmt.Sprintf("![](%s)", attachmentURL)
	}
	return fmt.Sprintf("[%s](%s)", name, attachmentURL)
}

func mediaFileName(provided string, messageID int, contentType, fallbackExt string) string {
	if provided != "" {
		return provided
	}
	ext := fallbackExt
	if slash := strings.IndexByte(contentType, '/'); slash >= 0 {
		guess := "." + contentType[slash+1:]
		// Strip any media-type parameters (e.g. "video/mp4; codecs=...").
		if semi := strings.IndexByte(guess, ';'); semi >= 0 {
			guess = strings.TrimSpace(guess[:semi])
		}
		if guess != "." {
			ext = guess
		}
	}
	return fmt.Sprintf("telegram-%d%s", messageID, ext)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// mediaGroupBuffer accumulates messages sharing a MediaGroupID and flushes
// them as a single batch shortly after the last item arrives. Album items are
// delivered as separate updates by Telegram.
type mediaGroupBuffer struct {
	mu     sync.Mutex
	groups map[string]*pendingGroup
	flush  func([]*tgbotapi.Message)
}

type pendingGroup struct {
	messages []*tgbotapi.Message
	timer    *time.Timer
}

func newMediaGroupBuffer(flush func([]*tgbotapi.Message)) *mediaGroupBuffer {
	return &mediaGroupBuffer{
		groups: make(map[string]*pendingGroup),
		flush:  flush,
	}
}

func (b *mediaGroupBuffer) add(msg *tgbotapi.Message) {
	id := msg.MediaGroupID
	b.mu.Lock()
	defer b.mu.Unlock()

	g, ok := b.groups[id]
	if !ok {
		g = &pendingGroup{}
		b.groups[id] = g
	} else if g.timer != nil {
		g.timer.Stop()
	}
	g.messages = append(g.messages, msg)
	g.timer = time.AfterFunc(mediaGroupFlushDelay, func() {
		b.mu.Lock()
		pending, ok := b.groups[id]
		if !ok {
			b.mu.Unlock()
			return
		}
		delete(b.groups, id)
		msgs := pending.messages
		b.mu.Unlock()
		b.flush(msgs)
	})
}

// entitiesToMarkdown overlays Telegram message entities onto the raw text and
// returns markdown — preserving links, bold/italic/code, etc. that would
// otherwise be lost. Entity offsets are in UTF-16 code units (per Telegram's
// Bot API), so we convert to a UTF-16 buffer before slicing.
func entitiesToMarkdown(text string, entities []tgbotapi.MessageEntity) string {
	if text == "" || len(entities) == 0 {
		return text
	}
	units := utf16.Encode([]rune(text))

	es := make([]tgbotapi.MessageEntity, len(entities))
	copy(es, entities)
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].Offset != es[j].Offset {
			return es[i].Offset < es[j].Offset
		}
		return es[i].Length > es[j].Length
	})

	var b strings.Builder
	cursor := 0
	for _, e := range es {
		if e.Offset < cursor || e.Offset > len(units) {
			// Overlapping/nested or out-of-range entity — skip.
			continue
		}
		end := e.Offset + e.Length
		if end > len(units) {
			end = len(units)
		}
		b.WriteString(string(utf16.Decode(units[cursor:e.Offset])))
		inner := string(utf16.Decode(units[e.Offset:end]))
		b.WriteString(applyEntity(e, inner))
		cursor = end
	}
	if cursor < len(units) {
		b.WriteString(string(utf16.Decode(units[cursor:])))
	}
	return b.String()
}

func applyEntity(e tgbotapi.MessageEntity, inner string) string {
	switch e.Type {
	case "text_link":
		return fmt.Sprintf("[%s](%s)", inner, e.URL)
	case "bold":
		return "**" + inner + "**"
	case "italic":
		return "_" + inner + "_"
	case "code":
		return "`" + inner + "`"
	case "pre":
		lang := e.Language
		return "\n```" + lang + "\n" + inner + "\n```\n"
	case "strikethrough":
		return "~~" + inner + "~~"
	default:
		// url, mention, hashtag, email, phone_number, etc. — text already
		// contains the visible value, so leave it untouched.
		return inner
	}
}

// generateTitle picks the first non-empty line of the message, trimmed to 80 chars.
func generateTitle(text string, hasMedia bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		if hasMedia {
			return "Telegram media"
		}
		return "Telegram message"
	}
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if line == "" {
		return "Telegram message"
	}
	if len(line) > 80 {
		line = line[:80] + "..."
	}
	return line
}
