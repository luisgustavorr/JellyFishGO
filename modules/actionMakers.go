package modules

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	jsoniter "github.com/json-iterator/go"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"

	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

var jsonFast = jsoniter.ConfigCompatibleWithStandardLibrary

func testShark(data EnvelopePayload, url string, retryToken string) (bool, int, string) {
	buf := JsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer JsonBufferPool.Put(buf)
	err := jsonFast.NewEncoder(buf).Encode(data)
	if err != nil {
		fmt.Printf("Erro ao criar marshal: %v", err)

		return false, 500, fmt.Sprintf("Erro ao criar marshal: %v", err)
	}
	// fmt.Println("Envelope sendo envidada :", string(jsonData))
	if url == "" {
		fmt.Printf("URL %s vazia", url)
		return false, 500, fmt.Sprintf("URL %s vazia", url)
	}
	req, err := http.NewRequest("POST", url, buf)
	if err != nil {
		fmt.Printf("Erro ao criar a requisição: %v", err)
		return false, 500, fmt.Sprintf("URL %s vazia", url)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", os.Getenv("STRING_AUTH"))
	req.Header.Set("X-CSRFToken", GetCSRFToken())
	teste, _ := jsoniter.MarshalIndent(data, "", "")
	fmt.Println(string(teste))
	client := &http.Client{}
	res, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return false, 500, fmt.Sprintln(err)
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		fmt.Println(err)
		return false, res.StatusCode, fmt.Sprintln(err)
	}
	if res.StatusCode != 200 {
		fmt.Println(string(body))
		return false, res.StatusCode, string(body)

	}

	return true, res.StatusCode, ""
}
func GetActionPermission(client_id string, action string) bool {
	if DB == nil {
		DB, _ = ConnectPsql()
	}
	granted := false
	DB.QueryRow(fmt.Sprintf(`SELECT %s_permission from action_makers WHERE client_id = $1 LIMIT 1`, action), client_id).Scan(&granted)
	return granted
}
func emoji(stats bool) string {
	if stats {
		return "✅"
	} else {
		return "❌"

	}
}
func GetStatus(client_id string, client *whatsmeow.Client, requester types.JID, messageID types.MessageID, pushname string) (string, error) {
	allowed := GetActionPermission(client_id, "get_status")
	if !allowed {
		return "", fmt.Errorf("client_id sem permissao")
	}

	mensagem := MessagePayload{
		ID:        messageID,
		Sender:    pushname,
		Number:    requester.User,
		Text:      "get_status",
		Timestamp: time.Now().Unix(),
		Attrs:     MessageAttrs{},
	}
	envelope := []Envelope{
		{

			Mensagem: mensagem,
		},
	}
	payload := EnvelopePayload{
		Evento:   "MENSAGEM_RECEBIDA",
		Sender:   2,
		ClientID: client_id,
		Data:     envelope,
	}
	isSharkRunning, statusCodeShark, errMessage := testShark(payload, "https://sharkbusiness.com.br/chatbot/chat/mensagens/novas-mensagens/", "")
	isJellyFishRunning := true

	return fmt.Sprintf(`Ok, o status das aplicações são : 
%s (%s) Shark Business 
%s
%s (200) JellyFish 
`, emoji(isSharkRunning), strconv.Itoa(statusCodeShark), errMessage, emoji(isJellyFishRunning)), nil
}
func VerifyStatus(requester types.JID, client_id string, client *whatsmeow.Client, pushname string) {
	messageID := client.GenerateMessageID()

	text, err := GetStatus(client_id, client, requester, messageID, pushname)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(text)
	ctx := context.Background()

	message := waE2E.Message{

		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
		},
	}

	ctxTm, cancel := context.WithTimeout(ctx, 15000*time.Millisecond)
	defer cancel()
	_, err = client.SendMessage(ctxTm, requester, &message, whatsmeow.SendRequestExtra{ID: messageID})
	if err != nil {
		fmt.Println("Erro mensagem", err)
		return
	}
}
