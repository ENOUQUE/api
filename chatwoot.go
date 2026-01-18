package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/gorilla/mux"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog/log"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// ChatwootConfig stores Chatwoot integration configuration
type ChatwootConfig struct {
	Enabled    bool   `db:"chatwoot_enabled"`
	URL        string `db:"chatwoot_url"`
	Token      string `db:"chatwoot_token"`
	AccountID  string `db:"chatwoot_account_id"`
	InboxID    string `db:"chatwoot_inbox_id"`
	WebhookURL string `db:"chatwoot_webhook_url"`
}

// SendToChatwoot sends WhatsApp events to Chatwoot
func sendToChatwoot(postmap map[string]interface{}, userID string, token string, db *sqlx.DB) {
	// Check if Chatwoot is enabled for this user
	var config ChatwootConfig
	err := db.Get(&config, `
		SELECT chatwoot_enabled, chatwoot_url, chatwoot_token, chatwoot_account_id, chatwoot_inbox_id
		FROM users WHERE id=$1`, userID)
	if err != nil || !config.Enabled || config.URL == "" || config.Token == "" {
		log.Debug().
			Str("userID", userID).
			Bool("enabled", config.Enabled).
			Msg("Chatwoot integration disabled")
		return
	}

	// Transform event to Chatwoot format
	chatwootPayload, err := transformEventToChatwoot(postmap, userID, db)
	if err != nil || chatwootPayload == nil {
		log.Debug().
			Str("userID", userID).
			Err(err).
			Msg("Chatwoot integration skipped - payload is nil (unsupported event type or filtered)")
		return
	}

	// Send to Chatwoot using public API
	chatwootURL := strings.TrimSuffix(config.URL, "/")
	inboxID := config.InboxID
	if inboxID == "" {
		log.Error().Str("userID", userID).Msg("Chatwoot inbox_id not configured")
		return
	}

	apiURL := fmt.Sprintf("%s/public/api/v1/inboxes/%s/messages", chatwootURL, inboxID)

	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("api_access_token", config.Token)
	client.SetHeader("Content-Type", "application/json")

	resp, err := client.R().
		SetBody(chatwootPayload).
		Post(apiURL)

	if err != nil {
		log.Error().
			Err(err).
			Str("userID", userID).
			Str("url", apiURL).
			Msg("Failed to send message to Chatwoot")
		return
	}

	if resp.StatusCode() != http.StatusOK && resp.StatusCode() != http.StatusCreated {
		log.Error().
			Int("status", resp.StatusCode()).
			Str("response", resp.String()).
			Str("userID", userID).
			Msg("Chatwoot API returned error")
		return
	}

	log.Info().
		Int("status", resp.StatusCode()).
		Str("userID", userID).
		Msg("Message sent to Chatwoot successfully")
}

// transformEventToChatwoot converts WhatsApp event to Chatwoot message format
func transformEventToChatwoot(postmap map[string]interface{}, userID string, db *sqlx.DB) (map[string]interface{}, error) {
	eventType, ok := postmap["type"].(string)
	if !ok {
		return nil, fmt.Errorf("event type is not a string")
	}

	// Only process Message events
	if eventType != "Message" {
		return nil, fmt.Errorf("unsupported event type: %s", eventType)
	}

	// Try to get the event as *events.Message first
	var evt *events.Message
	var err error
	
	if rawEvt, ok := postmap["event"]; ok {
		// Try type assertion to *events.Message
		if msgEvt, ok := rawEvt.(*events.Message); ok {
			evt = msgEvt
		} else {
			// If not, try to marshal/unmarshal to convert
			jsonData, marshalErr := json.Marshal(rawEvt)
			if marshalErr != nil {
				return nil, fmt.Errorf("failed to marshal event: %w", marshalErr)
			}
			// For now, we'll handle it as a map[string]interface{}
			var eventMap map[string]interface{}
			if err := json.Unmarshal(jsonData, &eventMap); err != nil {
				return nil, fmt.Errorf("failed to unmarshal event: %w", err)
			}
			
			// Extract from map
			info, ok := eventMap["Info"].(map[string]interface{})
			if !ok {
				return nil, fmt.Errorf("Info is not a map")
			}
			
			// Get sender information from map
			senderJID := ""
			chatJID := ""
			isFromMe := false
			
			if sender, ok := info["Sender"].(string); ok && sender != "" {
				senderJID = sender
			} else if senderMap, ok := info["Sender"].(map[string]interface{}); ok {
				if senderStr, ok := senderMap["String"].(string); ok {
					senderJID = senderStr
				}
			}
			
			if chat, ok := info["Chat"].(string); ok && chat != "" {
				chatJID = chat
			} else if chatMap, ok := info["Chat"].(map[string]interface{}); ok {
				if chatStr, ok := chatMap["String"].(string); ok {
					chatJID = chatStr
				}
			}
			
			if fromMe, ok := info["IsFromMe"].(bool); ok {
				isFromMe = fromMe
			}
			
			// Skip outgoing messages
			if isFromMe {
				return nil, fmt.Errorf("skipping outgoing message")
			}
			
			// Extract message content from map
			messageText := ""
			if message, ok := eventMap["Message"].(map[string]interface{}); ok {
				if textMsg, ok := message["Conversation"].(string); ok {
					messageText = textMsg
				} else if extendedText, ok := message["ExtendedTextMessage"].(map[string]interface{}); ok {
					if text, ok := extendedText["Text"].(string); ok {
						messageText = text
					}
				}
			}
			
			// Determine message type
			messageType := "text"
			if msgType, ok := info["Type"].(string); ok {
				messageType = msgType
			}
			
			// For media messages without text, use placeholder
			if messageText == "" {
				switch messageType {
				case "image":
					messageText = "[Imagem]"
				case "video":
					messageText = "[Vídeo]"
				case "audio":
					messageText = "[Áudio]"
				case "document":
					messageText = "[Documento]"
				case "sticker":
					messageText = "[Sticker]"
				default:
					messageText = "[Mensagem]"
				}
			}
			
			// Determine source_id
			sourceID := senderJID
			if strings.Contains(sourceID, "@") {
				sourceID = strings.Split(sourceID, "@")[0]
			}
			
			// For group messages, use group JID as source
			isGroup := false
			if group, ok := info["IsGroup"].(bool); ok {
				isGroup = group
			}
			if isGroup && chatJID != "" {
				if strings.Contains(chatJID, "@") {
					sourceID = strings.Split(chatJID, "@")[0]
				}
			}
			
			// Build Chatwoot payload
			payload := map[string]interface{}{
				"source_id":         sourceID,
				"message_type":      "incoming",
				"content":           messageText,
				"private":           false,
				"content_type":      "text",
				"content_attributes": map[string]interface{}{},
			}
			
			// Add phone number if available
			if sourceID != "" {
				payload["phone_number"] = sourceID
			}
			
			return payload, nil
		}
	} else {
		return nil, fmt.Errorf("event not found in postmap")
	}

	// Handle *events.Message directly
	if evt == nil {
		return nil, fmt.Errorf("failed to parse event")
	}

	// Get sender information
	senderJID := evt.Info.Sender.String()
	chatJID := evt.Info.Chat.String()
	isFromMe := evt.Info.IsFromMe

	// Skip outgoing messages (already handled by Chatwoot webhook)
	if isFromMe {
		return nil, fmt.Errorf("skipping outgoing message")
	}

	// Extract message content
	messageText := ""
	msg := evt.Message
	if msg.Conversation != nil {
		messageText = *msg.Conversation
	} else if msg.ExtendedTextMessage != nil && msg.ExtendedTextMessage.Text != nil {
		messageText = *msg.ExtendedTextMessage.Text
	}

	// Determine message type
	messageType := evt.Info.Type
	if messageType == "" {
		messageType = "text"
	}

	// For media messages without text, use placeholder
	if messageText == "" {
		switch messageType {
		case "image":
			messageText = "[Imagem]"
		case "video":
			messageText = "[Vídeo]"
		case "audio":
			messageText = "[Áudio]"
		case "document":
			messageText = "[Documento]"
		case "sticker":
			messageText = "[Sticker]"
		default:
			messageText = "[Mensagem]"
		}
	}

	// Determine source_id (phone number without @s.whatsapp.net)
	sourceID := senderJID
	if strings.Contains(sourceID, "@") {
		sourceID = strings.Split(sourceID, "@")[0]
	}

	// For group messages, use group JID as source
	isGroup := evt.Info.IsGroup
	if isGroup && chatJID != "" {
		if strings.Contains(chatJID, "@") {
			sourceID = strings.Split(chatJID, "@")[0]
		}
	}

	// Build Chatwoot payload
	payload := map[string]interface{}{
		"source_id":     sourceID,
		"message_type":  "incoming",
		"content":       messageText,
		"private":       false,
		"content_type":  "text",
		"content_attributes": map[string]interface{}{},
	}

	// Add phone number if available (for contact creation)
	if sourceID != "" {
		payload["phone_number"] = sourceID
	}

	return payload, nil
}

// CreateOrGetChatwootInbox creates or gets a Chatwoot inbox
func createOrGetChatwootInbox(config ChatwootConfig) (string, error) {
	if config.URL == "" || config.Token == "" || config.AccountID == "" {
		return "", fmt.Errorf("chatwoot configuration incomplete")
	}

	chatwootURL := strings.TrimSuffix(config.URL, "/")
	accountID := config.AccountID

	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("api_access_token", config.Token)
	client.SetHeader("Content-Type", "application/json")

	// First, try to get existing inboxes
	apiURL := fmt.Sprintf("%s/api/v1/accounts/%s/inboxes", chatwootURL, accountID)
	resp, err := client.R().Get(apiURL)
	if err != nil {
		return "", fmt.Errorf("failed to list inboxes: %w", err)
	}

	if resp.StatusCode() == http.StatusOK {
		var result struct {
			Payload []struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(resp.Body(), &result); err == nil {
			// Look for existing WuzAPI inbox
			for _, inbox := range result.Payload {
				if strings.Contains(inbox.Name, "WuzAPI") {
					return fmt.Sprintf("%d", inbox.ID), nil
				}
			}
		}
	}

	// Create new inbox if not found
	createPayload := map[string]interface{}{
		"name": "WuzAPI",
		"channel": map[string]interface{}{
			"type": "api",
		},
	}

	createURL := fmt.Sprintf("%s/api/v1/accounts/%s/inboxes", chatwootURL, accountID)
	resp, err = client.R().
		SetBody(createPayload).
		Post(createURL)

	if err != nil {
		return "", fmt.Errorf("failed to create inbox: %w", err)
	}

	if resp.StatusCode() != http.StatusOK && resp.StatusCode() != http.StatusCreated {
		return "", fmt.Errorf("failed to create inbox: status %d, response: %s", resp.StatusCode(), resp.String())
	}

	var createResult struct {
		Payload struct {
			ID int `json:"id"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(resp.Body(), &createResult); err != nil {
		return "", fmt.Errorf("failed to parse create inbox response: %w", err)
	}

	return fmt.Sprintf("%d", createResult.Payload.ID), nil
}

// ConfigureChatwootWebhook configures webhook in Chatwoot inbox
func configureChatwootWebhook(config ChatwootConfig, webhookURL string) error {
	if config.URL == "" || config.Token == "" || config.AccountID == "" || config.InboxID == "" {
		return fmt.Errorf("chatwoot configuration incomplete")
	}

	chatwootURL := strings.TrimSuffix(config.URL, "/")
	updateURL := fmt.Sprintf("%s/api/v1/accounts/%s/inboxes/%s", chatwootURL, config.AccountID, config.InboxID)

	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("api_access_token", config.Token)
	client.SetHeader("Content-Type", "application/json")

	payload := map[string]interface{}{
		"webhook_url": webhookURL,
	}

	resp, err := client.R().
		SetBody(payload).
		Put(updateURL)

	if err != nil {
		return fmt.Errorf("failed to configure webhook: %w", err)
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("failed to configure webhook: status %d, response: %s", resp.StatusCode(), resp.String())
	}

	return nil
}

// GetChatwootConversation gets or creates a conversation in Chatwoot
func getChatwootConversation(config ChatwootConfig, sourceID string) (string, error) {
	if config.URL == "" || config.Token == "" || config.AccountID == "" || config.InboxID == "" {
		return "", fmt.Errorf("chatwoot configuration incomplete")
	}

	chatwootURL := strings.TrimSuffix(config.URL, "/")

	// First, try to find existing conversation
	searchURL := fmt.Sprintf("%s/api/v1/accounts/%s/conversations/search", chatwootURL, config.AccountID)
	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("api_access_token", config.Token)
	client.SetHeader("Content-Type", "application/json")

	searchPayload := map[string]interface{}{
		"q": sourceID,
	}

	resp, err := client.R().
		SetBody(searchPayload).
		Post(searchURL)

	if err == nil && resp.StatusCode() == http.StatusOK {
		var result struct {
			Payload []struct {
				ID int `json:"id"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(resp.Body(), &result); err == nil && len(result.Payload) > 0 {
			return fmt.Sprintf("%d", result.Payload[0].ID), nil
		}
	}

	// Create new conversation if not found
	createPayload := map[string]interface{}{
		"source_id": sourceID,
		"inbox_id":  config.InboxID,
	}

	createURL := fmt.Sprintf("%s/api/v1/accounts/%s/conversations", chatwootURL, config.AccountID)
	resp, err = client.R().
		SetBody(createPayload).
		Post(createURL)

	if err != nil {
		return "", fmt.Errorf("failed to create conversation: %w", err)
	}

	if resp.StatusCode() != http.StatusOK && resp.StatusCode() != http.StatusCreated {
		return "", fmt.Errorf("failed to create conversation: status %d, response: %s", resp.StatusCode(), resp.String())
	}

	var createResult struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(resp.Body(), &createResult); err != nil {
		return "", fmt.Errorf("failed to parse create conversation response: %w", err)
	}

	return fmt.Sprintf("%d", createResult.ID), nil
}

// SendChatwootMessage sends a message to a specific Chatwoot conversation
func sendChatwootMessage(config ChatwootConfig, conversationID string, content string) error {
	if config.URL == "" || config.Token == "" || config.AccountID == "" {
		return fmt.Errorf("chatwoot configuration incomplete")
	}

	chatwootURL := strings.TrimSuffix(config.URL, "/")
	apiURL := fmt.Sprintf("%s/api/v1/accounts/%s/conversations/%s/messages", chatwootURL, config.AccountID, conversationID)

	client := resty.New()
	client.SetTimeout(30 * time.Second)
	client.SetHeader("api_access_token", config.Token)
	client.SetHeader("Content-Type", "application/json")

	payload := map[string]interface{}{
		"content":      content,
		"message_type": "outgoing",
		"private":      false,
		"content_type": "text",
	}

	resp, err := client.R().
		SetBody(payload).
		Post(apiURL)

	if err != nil {
		return fmt.Errorf("failed to send message: %w", err)
	}

	if resp.StatusCode() != http.StatusOK && resp.StatusCode() != http.StatusCreated {
		return fmt.Errorf("failed to send message: status %d, response: %s", resp.StatusCode(), resp.String())
	}

	return nil
}

// HandleChatwootWebhook processes incoming webhook from Chatwoot
func handleChatwootWebhook(w http.ResponseWriter, r *http.Request, db *sqlx.DB, instanceName string) {
	// Get user by instance name
	var userID string
	err := db.Get(&userID, "SELECT id FROM users WHERE name=$1", instanceName)
	if err != nil {
		log.Error().Err(err).Str("instanceName", instanceName).Msg("Failed to get user by instance name")
		http.Error(w, "User not found", http.StatusNotFound)
		return
	}

	// Parse Chatwoot webhook payload
	var payload struct {
		Message struct {
			Content     string `json:"content"`
			MessageType string `json:"message_type"`
			Inbox       struct {
				ID int `json:"id"`
			} `json:"inbox"`
			Conversation struct {
				Contact struct {
					PhoneNumber string `json:"phone_number"`
				} `json:"contact"`
			} `json:"conversation"`
		} `json:"message"`
	}

	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		log.Error().Err(err).Msg("Failed to decode Chatwoot webhook payload")
		http.Error(w, "Invalid payload", http.StatusBadRequest)
		return
	}

	// Only process outgoing messages
	if payload.Message.MessageType != "outgoing" {
		http.Error(w, "Ignoring non-outgoing message", http.StatusOK)
		return
	}

	// Get user token and send message via WhatsApp
	myClient := clientManager.GetMyClient(userID)
	if myClient == nil || myClient.WAClient == nil {
		log.Error().Str("userID", userID).Msg("Client not found for user")
		http.Error(w, "Client not connected", http.StatusServiceUnavailable)
		return
	}

	phoneNumber := payload.Message.Conversation.Contact.PhoneNumber
	if phoneNumber == "" {
		http.Error(w, "Phone number not found", http.StatusBadRequest)
		return
	}

	// Parse JID
	jid, err := types.ParseJID(phoneNumber)
	if err != nil {
		// Try adding @s.whatsapp.net
		if !strings.Contains(phoneNumber, "@") {
			jid, err = types.ParseJID(phoneNumber + "@s.whatsapp.net")
		}
		if err != nil {
			log.Error().Err(err).Str("phoneNumber", phoneNumber).Msg("Failed to parse JID")
			http.Error(w, "Invalid phone number", http.StatusBadRequest)
			return
		}
	}

	// Send message
	ctx := context.Background()
	_, err = myClient.WAClient.SendMessage(ctx, jid, &waE2E.Message{
		Conversation: proto.String(payload.Message.Content),
	})
	if err != nil {
		log.Error().Err(err).Str("userID", userID).Str("phoneNumber", phoneNumber).Msg("Failed to send message to WhatsApp")
		http.Error(w, "Failed to send message", http.StatusInternalServerError)
		return
	}

	log.Info().
		Str("userID", userID).
		Str("phoneNumber", phoneNumber).
		Msg("Message sent from Chatwoot to WhatsApp successfully")

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "success"})
}
