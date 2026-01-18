package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-resty/resty/v2"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog/log"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"google.golang.org/protobuf/proto"
)

/*
|--------------------------------------------------------------------------
| Chatwoot Config & Structs
|--------------------------------------------------------------------------
*/

type ChatwootConfig struct {
	Enabled    bool   `db:"chatwoot_enabled" json:"enabled"`
	URL        string `db:"chatwoot_url" json:"url"`
	Token      string `db:"chatwoot_token" json:"token"`
	AccountID  string `db:"chatwoot_account_id" json:"account_id"`
	InboxID    string `db:"chatwoot_inbox_id" json:"inbox_id"`
	WebhookURL string `db:"chatwoot_webhook_url" json:"webhook_url"`
}

type ChatwootWebhookPayload struct {
	Event        string `json:"event"`
	MessageType  string `json:"message_type"`
	Content      string `json:"content"`
	Conversation struct {
		Contact struct {
			PhoneNumber string `json:"phone_number"`
		} `json:"contact"`
	} `json:"conversation"`
	Message struct {
		Content     string `json:"content"`
		MessageType string `json:"message_type"`
	} `json:"message"`
}

/*
|--------------------------------------------------------------------------
| Fluxo: WhatsApp -> Chatwoot
|--------------------------------------------------------------------------
*/

// sendToChatwoot gerencia a criação de contato e envio de mensagem
func sendToChatwoot(postmap map[string]interface{}, userID string, db *sqlx.DB) {
	var config ChatwootConfig

	err := db.Get(&config, `
		SELECT chatwoot_enabled, chatwoot_url, chatwoot_token, chatwoot_account_id, chatwoot_inbox_id
		FROM users WHERE id=$1`, userID)

	if err != nil || !config.Enabled || config.URL == "" || config.Token == "" || config.InboxID == "" {
		return
	}

	// 1. Prepara os dados do evento
	rawEvt, ok := postmap["event"].(*events.Message)
	if !ok || rawEvt.Info.IsFromMe {
		return
	}

	sourceID := strings.Split(rawEvt.Info.Sender.String(), "@")[0]
	pushName := rawEvt.Info.PushName
	if pushName == "" {
		pushName = sourceID
	}

	// 2. Cria ou Atualiza o Contato no Chatwoot (Essencial para aparecer no painel)
	contactIdentifier := createChatwootContact(config, sourceID, pushName)
	if contactIdentifier == "" {
		contactIdentifier = sourceID
	}

	// 3. Transforma a mensagem
	payload, err := transformEventToChatwoot(postmap, contactIdentifier)
	if err != nil {
		return
	}

	// 4. Envia a mensagem via Public API
	apiURL := fmt.Sprintf(
		"%s/public/api/v1/inboxes/%s/messages",
		strings.TrimSuffix(config.URL, "/"),
		config.InboxID,
	)

	client := resty.New().SetTimeout(30 * time.Second).
		SetHeader("api_access_token", config.Token).
		SetHeader("Content-Type", "application/json")

	resp, err := client.R().SetBody(payload).Post(apiURL)

	if err != nil || (resp.StatusCode() != http.StatusOK && resp.StatusCode() != http.StatusCreated) {
		log.Error().Int("status", resp.StatusCode()).Msg("Erro ao enviar mensagem para Chatwoot")
	}
}

// createChatwootContact garante que o contato exista (Igual Evolution API)
func createChatwootContact(config ChatwootConfig, phone string, name string) string {
	apiURL := fmt.Sprintf(
		"%s/public/api/v1/inboxes/%s/contacts",
		strings.TrimSuffix(config.URL, "/"),
		config.InboxID,
	)

	client := resty.New().SetHeader("api_access_token", config.Token)
	
	payload := map[string]interface{}{
		"name":         name,
		"phone_number": "+" + phone,
		"identifier":   phone,
	}

	resp, err := client.R().SetBody(payload).Post(apiURL)
	if err != nil {
		return ""
	}

	var result map[string]interface{}
	json.Unmarshal(resp.Body(), &result)

	if identifier, ok := result["source_id"].(string); ok {
		return identifier
	}

	return phone
}

func transformEventToChatwoot(postmap map[string]interface{}, sourceID string) (map[string]interface{}, error) {
	rawEvt, ok := postmap["event"].(*events.Message)
	if !ok {
		return nil, fmt.Errorf("invalid event")
	}

	text := ""
	if rawEvt.Message.Conversation != nil {
		text = *rawEvt.Message.Conversation
	} else if rawEvt.Message.ExtendedTextMessage != nil {
		text = *rawEvt.Message.ExtendedTextMessage.Text
	} else {
		text = "[Mídia recebida]"
	}

	return map[string]interface{}{
		"source_id":    sourceID,
		"message_type": "incoming",
		"content":      text,
		"content_type": "text",
	}, nil
}

/*
|--------------------------------------------------------------------------
| Fluxo: Chatwoot -> WhatsApp (Webhook)
|--------------------------------------------------------------------------
*/

func handleChatwootWebhook(w http.ResponseWriter, r *http.Request, db *sqlx.DB, instanceName string) {
	var userID string
	err := db.Get(&userID, "SELECT id FROM users WHERE name=$1", instanceName)
	if err != nil {
		log.Error().Err(err).Str("instance", instanceName).Msg("Instância não encontrada")
		w.WriteHeader(http.StatusNotFound)
		return
	}

	var payload ChatwootWebhookPayload
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Normalização do conteúdo da mensagem
	msgContent := payload.Message.Content
	if msgContent == "" { msgContent = payload.Content }

	msgType := payload.Message.MessageType
	if msgType == "" { msgType = payload.MessageType }

	// Filtro: Apenas mensagens enviadas por agentes
	if msgType != "outgoing" {
		w.WriteHeader(http.StatusOK)
		return
	}

	myClient := clientManager.GetMyClient(userID)
	if myClient == nil || myClient.WAClient == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	phone := payload.Conversation.Contact.PhoneNumber
	phone = strings.ReplaceAll(phone, "+", "")
	
	jid, err := types.ParseJID(phone)
	if err != nil || jid.Server != "s.whatsapp.net" {
		jid, _ = types.ParseJID(phone + "@s.whatsapp.net")
	}

	_, err = myClient.WAClient.SendMessage(context.Background(), jid, &waE2E.Message{
		Conversation: proto.String(msgContent),
	})

	if err != nil {
		log.Error().Err(err).Msg("Erro ao enviar mensagem via whatsmeow")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

/*
|--------------------------------------------------------------------------
| Funções Administrativas (Configuração do Inbox)
|--------------------------------------------------------------------------
*/

func createOrGetChatwootInbox(config ChatwootConfig) (string, error) {
	chatwootURL := strings.TrimSuffix(config.URL, "/")
	apiURL := fmt.Sprintf("%s/api/v1/accounts/%s/inboxes", chatwootURL, config.AccountID)

	client := resty.New().
		SetTimeout(30 * time.Second).
		SetHeader("api_access_token", config.Token)

	// Tenta criar um novo Inbox do tipo API
	resp, err := client.R().
		SetBody(map[string]interface{}{
			"name": "WuzAPI - WhatsApp",
			"channel": map[string]interface{}{
				"type": "api",
			},
		}).Post(apiURL)

	if err != nil {
		return "", err
	}

	var result map[string]interface{}
	json.Unmarshal(resp.Body(), &result)

	// Se já existir ou criar, retorna o ID (ajuste conforme a resposta da sua versão do Chatwoot)
	if id, ok := result["id"].(float64); ok {
		return fmt.Sprintf("%.0f", id), nil
	}

	return "", fmt.Errorf("não foi possível criar o inbox")
}

func configureChatwootWebhook(config ChatwootConfig, webhookURL string) error {
	chatwootURL := strings.TrimSuffix(config.URL, "/")
	apiURL := fmt.Sprintf("%s/api/v1/accounts/%s/inboxes/%s", chatwootURL, config.AccountID, config.InboxID)

	client := resty.New().
		SetTimeout(30 * time.Second).
		SetHeader("api_access_token", config.Token)

	payload := map[string]interface{}{
		"webhook_url": webhookURL,
	}

	resp, err := client.R().SetBody(payload).Patch(apiURL)
	if err != nil {
		return err
	}

	if resp.StatusCode() != http.StatusOK {
		return fmt.Errorf("erro ao configurar webhook: %s", resp.String())
	}

	return nil
}
