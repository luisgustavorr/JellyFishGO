package modules

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func testShark() (bool, int) {
	url := "https://sharkbusiness.com.br"
	method := "GET"

	client := &http.Client{}
	req, err := http.NewRequest(method, url, nil)

	if err != nil {
		fmt.Println(err)
		return false, 500
	}
	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", "insomnia/2023.5.8")

	res, err := client.Do(req)
	if err != nil {
		fmt.Println(err)
		return false, 500
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		fmt.Println(err)
		return false, res.StatusCode
	}
	if res.StatusCode != 200 {
		fmt.Println(string(body))
	}

	return true, res.StatusCode
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
func GetStatus(client_id string) (string, error) {
	allowed := GetActionPermission(client_id, "get_status")
	if !allowed {
		return "", fmt.Errorf("client_id sem permissao")
	}
	isSharkRunning, statusCodeShark := testShark()
	isJellyFishRunning := true

	return fmt.Sprintf(`Ok, o status das aplicações são : 
%s (%s) Shark Business 
%s (200) JellyFish
`, emoji(isSharkRunning), strconv.Itoa(statusCodeShark), emoji(isJellyFishRunning)), nil
}
func VerifyStatus(requester types.JID, client_id string, client *whatsmeow.Client) {
	text, err := GetStatus(client_id)
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(text)
	message := waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(text),
		},
	}
	ctx := context.Background()
	ctxTm, cancel := context.WithTimeout(ctx, 15000*time.Millisecond)
	defer cancel()
	_, err = client.SendMessage(ctxTm, requester, &message)
	if err != nil {
		fmt.Println("Erro mensagem", err)
		return
	}
}
