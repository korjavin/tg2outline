package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

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

	for update := range updates {
		if update.Message == nil {
			continue
		}
		if update.Message.From.ID != tgUserID {
			log.Printf("Unauthorized message from user ID: %d", update.Message.From.ID)
			continue
		}
		processMessage(bot, update.Message, outline, collectionID)
	}
}

// processMessage turns a Telegram message into a single Outline document.
func processMessage(bot *tgbotapi.BotAPI, message *tgbotapi.Message, outline *OutlineClient, collectionID string) {
	// rawText is plain text (used for the title); messageText carries
	// entity-derived markdown (links, bold, etc.) for the document body.
	rawText := message.Text
	messageText := entitiesToMarkdown(message.Text, message.Entities)
	var forwardInfo string
	var imageMarkdown string

	if message.ForwardFrom != nil {
		forwardInfo = fmt.Sprintf("Forwarded from: %s %s (@%s)",
			message.ForwardFrom.FirstName,
			message.ForwardFrom.LastName,
			message.ForwardFrom.UserName)
	} else if message.ForwardFromChat != nil {
		forwardInfo = fmt.Sprintf("Forwarded from: %s", message.ForwardFromChat.Title)
		if message.ForwardFromChat.UserName != "" {
			forwardInfo += fmt.Sprintf(" (@%s)", message.ForwardFromChat.UserName)
		}
	}

	if message.Photo != nil {
		// Largest size is last in the slice.
		photoSize := message.Photo[len(message.Photo)-1]
		fileURL, err := bot.GetFileDirectURL(photoSize.FileID)
		if err != nil {
			log.Printf("Failed to get photo URL: %v", err)
		} else {
			fileData, err := DownloadFile(fileURL)
			if err != nil {
				log.Printf("Failed to download photo: %v", err)
			} else {
				name := fmt.Sprintf("telegram-%d.jpg", message.MessageID)
				attachmentURL, err := outline.UploadAttachment(name, "image/jpeg", fileData)
				if err != nil {
					log.Printf("Failed to upload photo to Outline: %v", err)
				} else {
					log.Printf("Photo uploaded to Outline: %s", attachmentURL)
					imageMarkdown = fmt.Sprintf("![](%s)", attachmentURL)
				}
			}
		}
	}

	if message.Caption != "" {
		if messageText != "" {
			messageText += "\n\n"
		}
		messageText += entitiesToMarkdown(message.Caption, message.CaptionEntities)
		if rawText != "" {
			rawText += "\n\n"
		}
		rawText += message.Caption
	}

	if messageText == "" && imageMarkdown == "" && forwardInfo == "" {
		reply := tgbotapi.NewMessage(message.Chat.ID, "Cannot add empty message to Outline.")
		reply.ReplyToMessageID = message.MessageID
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
	if imageMarkdown != "" {
		bodyParts = append(bodyParts, imageMarkdown)
	}
	body := strings.Join(bodyParts, "\n\n")

	title := generateTitle(rawText, imageMarkdown != "")

	if _, err := outline.CreateDocument(collectionID, title, body); err != nil {
		log.Printf("Failed to create document in Outline: %v", err)
		reply := tgbotapi.NewMessage(message.Chat.ID, fmt.Sprintf("Error adding to Outline: %v", err))
		reply.ReplyToMessageID = message.MessageID
		bot.Send(reply)
		return
	}

	confirmation := "Added to Outline"
	if imageMarkdown != "" {
		confirmation += " with image"
	}
	reply := tgbotapi.NewMessage(message.Chat.ID, confirmation)
	reply.ReplyToMessageID = message.MessageID
	bot.Send(reply)
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
func generateTitle(text string, hasImage bool) string {
	text = strings.TrimSpace(text)
	if text == "" {
		if hasImage {
			return "Telegram photo"
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
