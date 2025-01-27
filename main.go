package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mime/multipart"

	"math/rand"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/h2non/filetype"
	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3" // Importação do driver SQLite
	"github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

var clientMap = make(map[string]*whatsmeow.Client)
var mapOficial, mapDesenvolvimento = loadConfigInicial("spacemid_luis:G4l01313@tcp(pro107.dnspro.com.br:3306)/spacemid_sistem_adm")
var messagesToSend = make(map[string][]*waE2E.Message)
var focusedMessagesKeys = []string{}

type MessagesQueue struct {
	messageBuffer  map[string][]interface{}
	messageTimeout map[string]*time.Timer
	bufferLock     sync.Mutex
}

func NewQueue() *MessagesQueue {
	return &MessagesQueue{
		messageBuffer:  make(map[string][]interface{}),
		messageTimeout: make(map[string]*time.Timer),
	}
}

func (c *MessagesQueue) AddMessage(clientID string, message map[string]interface{}, number string) {
	c.bufferLock.Lock()
	defer c.bufferLock.Unlock()

	// Inicializa o buffer para o cliente, se necessário
	if _, exists := c.messageBuffer[number]; !exists {
		c.messageBuffer[number] = []interface{}{}
	}

	// Adiciona a mensagem ao buffer
	c.messageBuffer[number] = append(c.messageBuffer[number], message)

	// Reinicia o timeout para o cliente
	if timer, exists := c.messageTimeout[number]; exists {
		timer.Stop()
	}

	// Calcula o tempo de espera com base no tamanho do buffer
	messageCount := len(c.messageBuffer[number])
	timerBetweenMessage := -0.15*float64(messageCount)*float64(messageCount) + 0.5*float64(messageCount) + 5
	if timerBetweenMessage < 0 {
		timerBetweenMessage = 0.001
	}
	timerDuration := time.Duration(timerBetweenMessage * float64(time.Second))
	fmt.Printf("ESPERANDO %.3f SEGUNDOS PARA %d MENSAGENS\n", timerBetweenMessage, messageCount)

	// Define um novo timer para processar as mensagens
	c.messageTimeout[number] = time.AfterFunc(timerDuration, func() {
		c.ProcessMessages(clientID, number)
	})
}
func (c *MessagesQueue) ProcessMessages(clientID string, number string) {
	c.bufferLock.Lock()
	defer c.bufferLock.Unlock()

	// Pega as mensagens do buffer
	messages := c.messageBuffer[number]
	c.messageBuffer[number] = nil // Limpa o buffer

	fmt.Printf("ENVIANDO LOTES DE %d MENSAGENS DO "+clientID+"\n", len(messages))
	lastIndex := strings.LastIndex(clientID, "_")
	sufixo := clientID[lastIndex+1:]
	baseURL := strings.Split(mapOficial[sufixo], "chatbot")[0]
	if strings.Contains(baseURL, "disparo") {
		baseURL = strings.Split(mapOficial[sufixo], "disparo")[0]
	}
	data := map[string]any{
		"evento":   "MENSAGEM_RECEBIDA",
		"sender":   2,
		"clientId": clientID,
		"data":     messages,
	}
	sendToEndPoint(data, clientID, baseURL+"chatbot/chat/mensagens/novas-mensagens/")
	// Remove o timer após o processamento
	if timer, exists := c.messageTimeout[clientID]; exists {
		timer.Stop()
		delete(c.messageTimeout, clientID)
	}
}

func salvarArquivoDeZip(zipFile *zip.File, idImage string) error {
	// Abrir o arquivo ZIP para leitura
	fileName := zipFile.Name
	if strings.Contains(fileName, "documento_"+idImage) {
		// Criar um arquivo local para salvar
		destFile, err := os.Create("./uploads/" + fileName)
		if err != nil {
			return fmt.Errorf("erro ao criar arquivo: %v", err)
		}
		defer destFile.Close()

		// Abrir o arquivo do ZIP
		zipFileReader, err := zipFile.Open()
		if err != nil {
			return fmt.Errorf("erro ao abrir arquivo do zip: %v", err)
		}
		defer zipFileReader.Close()

		// Copiar o conteúdo do arquivo do ZIP para o arquivo local
		_, err = io.Copy(destFile, zipFileReader)
		if err != nil {
			return fmt.Errorf("erro ao copiar conteúdo para o arquivo: %v", err)
		}

		fmt.Printf("Arquivo %s salvo em ./uploads/%s\n", fileName, fileName)
	}
	return nil
}

func loadConfigInicial(dsn string) (map[string]string, map[string]string) {
	// Conectar ao banco de dados
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Verificar se a conexão com o banco está ok
	if err := db.Ping(); err != nil {
		log.Fatal(err)
	}

	// Executar uma consulta simples
	rows, err := db.Query("SELECT sufixo, link_oficial,base_link_teste,link_teste FROM clientes")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	mapProducao := map[string]string{}
	mapDesenvolvimento := map[string]string{}
	for rows.Next() {
		var sufixo string
		var link_oficial string
		var base_link_teste string
		var link_teste string
		if err := rows.Scan(&sufixo, &link_oficial, &base_link_teste, &link_teste); err != nil {
			log.Fatal(err)
		}
		mapProducao[sufixo] = link_oficial
		mapDesenvolvimento[sufixo] = base_link_teste + link_teste
	}
	err = godotenv.Load()
	if err != nil {
		log.Fatal("Erro ao carregar o arquivo .env")
	}
	MODO_DESENVOLVIMENTO := os.Getenv("MODO_DESENVOLVIMENTO")
	var desenvolvilemto = MODO_DESENVOLVIMENTO == "1"
	fmt.Println("MODO DESENVOLVIMENTO", desenvolvilemto)

	if desenvolvilemto {
		return mapDesenvolvimento, mapDesenvolvimento
	}
	return mapProducao, mapDesenvolvimento
}
func getCSRFToken() string {
	// Gera um token CSRF aleatório
	rand.Seed(time.Now().UnixNano())
	randomToken := fmt.Sprintf("%x", rand.Int63())
	return randomToken
}
func sendToEndPoint(data any, clientId string, url string) {

	jsonData, err := json.Marshal(data)
	if err != nil {
		fmt.Printf("Erro ao criar marshal: %v", err)
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("Erro ao criar a requisição: %v", err)
	}

	// Definindo os cabeçalhos
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "jelly_fish_con_|7@625^4|7")
	req.Header.Set("X-CSRFToken", getCSRFToken())

	// Enviando a requisição com http.Client
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Erro ao enviar a requisição: %v", err)
	}
	defer resp.Body.Close()
	// Verificando o status da resposta
	fmt.Println("Resposta Status:", resp.Status)

}
func getText(message *waE2E.Message) string {
	var text string = message.GetConversation()
	if text == "" {
		text = message.ExtendedTextMessage.GetText()
	}
	if text == "" {
		text = message.ImageMessage.GetCaption()
	}
	if text == "" {
		text = message.VideoMessage.GetCaption()
	}
	if text == "" {
		text = message.DocumentMessage.GetCaption()
	}
	if text == "" {
		text = message.GetProtocolMessage().GetEditedMessage().GetConversation()
	}
	return text
}
func getMedia(evt *events.Message, clientId string) (string, string) {
	client := getClient(clientId)
	var mimeType string = ""
	if imgMsg := evt.Message.GetImageMessage(); imgMsg != nil {
		mimeType = imgMsg.GetMimetype()
		mediaMessage := imgMsg
		data, err := client.Download(mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a mídia: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(data)
		return base64Data, mimeType
	}
	if vidMsg := evt.Message.GetVideoMessage(); vidMsg != nil {
		mimeType = vidMsg.GetMimetype()
		mediaMessage := vidMsg
		data, err := client.Download(mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a mídia: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(data)
		return base64Data, mimeType
	}
	if audioMsg := evt.Message.GetAudioMessage(); audioMsg != nil {
		mimeType = audioMsg.GetMimetype()
		mediaMessage := audioMsg
		data, err := client.Download(mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a mídia: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(data)
		return base64Data, mimeType
	}
	if stickerMsg := evt.Message.GetStickerMessage(); stickerMsg != nil {
		mimeType = stickerMsg.GetMimetype()
		mediaMessage := stickerMsg
		data, err := client.Download(mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a mídia: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(data)
		return base64Data, mimeType
	}
	if docMsg := evt.Message.GetDocumentMessage(); docMsg != nil {
		mimeType = docMsg.GetMimetype()
		mediaMessage := docMsg
		data, err := client.Download(mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a mídia: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(data)
		return base64Data, mimeType
	} else {
		return "", ""

	}

}
func getSender(senderNumber string) string {
	parts := strings.Split(senderNumber, "@")
	if len(parts) > 0 {
		number := parts[0]
		return number
	} else {
		return ""
	}
}

var messagesQueue = NewQueue()

func requestLogger(c *fiber.Ctx) error {
	start := time.Now()
	method := c.Method()
	path := c.Path()
	// ip := c.IP()
	clientId := c.FormValue("clientId")
	err := c.Next()
	duration := time.Since(start)
	log.Printf("[%s] %s | Tempo: %v | ClientId: %s\n", method, path, duration, clientId)
	return err
}
func getMessageFocus(arr []string, id_message string) string {
	for _, v := range arr {
		if strings.Contains(v, "_"+id_message) {
			return strings.Split(v, "_")[0]
		}
	}
	return ""
}
func removeString(slice []string, value string) []string {
	filtered := []string{}
	for _, v := range slice {
		if v != value { // Mantém apenas os valores diferentes
			filtered = append(filtered, v)
		}
	}
	return filtered
}
func handleMessage(fullInfoMessage *events.Message, clientId string, client *whatsmeow.Client) bool {
	var groupMessage bool = strings.Contains(fullInfoMessage.Info.Chat.String(), "@g.us")
	var channel bool = fullInfoMessage.SourceWebMsg.GetBroadcast()
	var statusMessage bool = strings.Contains(fullInfoMessage.Info.Chat.String(), "status")
	var contactMessage bool = fullInfoMessage.Message.GetContactMessage() != nil
	var LocationMessage bool = fullInfoMessage.Message.LocationMessage != nil
	var pollMessage bool = fullInfoMessage.Message.GetPollUpdateMessage() != nil || fullInfoMessage.Message.GetPollCreationMessage() != nil || fullInfoMessage.Message.GetPollCreationMessageV2() != nil || fullInfoMessage.Message.GetPollCreationMessageV3() != nil || fullInfoMessage.Message.GetPollCreationMessageV4() != nil || fullInfoMessage.Message.GetPollCreationMessageV5() != nil
	if groupMessage || statusMessage || pollMessage || contactMessage || LocationMessage || channel {
		fmt.Println("Mensagem de grupo ou status, ignorando...", fullInfoMessage.Info.Chat.String())
		return false
	}
	message := fullInfoMessage.Message
	var contextInfo = message.ExtendedTextMessage.GetContextInfo()
	var senderName string = fullInfoMessage.Info.PushName
	var text string = getText(message)
	var fromMe = fullInfoMessage.Info.IsFromMe
	var senderNumber string = getSender(fullInfoMessage.Info.Sender.User)
	var id_message string = fullInfoMessage.Info.ID
	var datetime string = fullInfoMessage.Info.Timestamp.String()
	var editedInfo = message.GetProtocolMessage().GetKey().GetId()
	layout := "2006-01-02 15:04:05"
	// Parse da string para o tipo time.Time
	trimmedDate := strings.Split(datetime, " -")[0]
	t, err := time.Parse(layout, trimmedDate)
	if err != nil {
		fmt.Println("Erro ao converter data:", err)
	}
	// Convertendo para o timestamp (seconds desde a época Unix)
	timestamp := t.Unix()

	var quotedMessageID string = contextInfo.GetStanzaID()
	media, fileType := getMedia(fullInfoMessage, clientId)
	edited := 0
	validNumber, err := client.IsOnWhatsApp([]string{senderNumber})
	if err != nil {
		fmt.Println(err, "ERRO IS ONWHATSAPP")
	}
	response := validNumber[0] // Acessa o primeiro item da slice
	JID := response.JID
	if fromMe {
		senderNumber = fullInfoMessage.Info.Chat.User
	}
	params := &whatsmeow.GetProfilePictureParams{}
	profilePic, err := client.GetProfilePictureInfo(JID, params)
	if err != nil {
		fmt.Println("sem perfil")
	}
	if editedInfo != "" {
		edited = 1
		id_message = editedInfo
	}
	messageAttr := map[string]interface{}{
		"quotedMessage": quotedMessageID,
		"edited":        edited,
	}

	if media != "" {
		messageAttr["file_type"] = fileType
		if strings.Contains(fileType, "audio") {
			messageAttr["audio"] = media
		} else {
			messageAttr["media"] = media
		}
	}

	mensagem := map[string]interface{}{
		"id":        id_message,
		"sender":    senderName,
		"number":    senderNumber,
		"text":      text,
		"attrs":     messageAttr,
		"timestamp": timestamp,
	}
	var focus = getMessageFocus(focusedMessagesKeys, id_message)
	if focus != "" {
		fmt.Println("MENSAGEM FOCADA", focus)
		mensagem["focus"] = focus
		focusedMessagesKeys = removeString(focusedMessagesKeys, focus+"_"+id_message)
		fmt.Println(focusedMessagesKeys)
	}
	if profilePic != nil {
		mensagem["perfil_image"] = profilePic.URL
	}
	objetoMensagens := map[string]interface{}{
		"mensagem": mensagem,
	}
	if fromMe {
		if media != "" || text != "" {

			listaMensagens := []map[string]interface{}{}
			fmt.Println("-> Mensagem ENVIADA PELO WHATSAPP:", id_message, senderName, senderNumber, text)
			listaMensagens = append(listaMensagens, objetoMensagens)

			data := map[string]any{
				"evento":   "MENSAGEM_RECEBIDA",
				"sender":   1,
				"clientId": clientId,
				"data":     listaMensagens,
			}
			lastIndex := strings.LastIndex(clientId, "_")
			sufixo := clientId[lastIndex+1:]
			baseURL := strings.Split(mapOficial[sufixo], "chatbot")[0]
			if strings.Contains(baseURL, "disparo") {
				baseURL = strings.Split(mapOficial[sufixo], "disparo")[0]
			}
			sendToEndPoint(data, clientId, baseURL+"chatbot/chat/mensagens/novas-mensagens/")
		}
	} else {
		if media != "" || text != "" {
			if messagesToSend[clientId] == nil {
				messagesToSend[clientId] = []*waE2E.Message{}
			}
			messagesToSend[clientId] = append(messagesToSend[clientId], message)
			messagesQueue.AddMessage(clientId, objetoMensagens, senderNumber)
			fmt.Println("<- Mensagem RECEBIDA:", id_message, senderName, senderNumber, text)
			var MessageID []types.MessageID = []types.MessageID{id_message}
			client.MarkRead(MessageID, time.Now(), JID, JID, types.ReceiptTypeRead)
		}
	}
	return true
}

func autoConnection() {
	dir := "./clients_db" // Substitua pelo caminho da sua pasta
	fmt.Println("---------RODANDO")
	// Listar arquivos na pasta
	files, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("Erro ao ler a pasta: %v", err)
	}

	// Criar uma fatia para armazenar os nomes dos arquivos
	var fileNames []string

	// Iterar pelos arquivos e adicionar os nomes na fatia
	for _, file := range files {
		// Adiciona o nome do arquivo se for um arquivo regular
		if !file.IsDir() {
			fileNames = append(fileNames, file.Name())
		}
	}

	for _, fileName := range fileNames {
		cleanClientId := strings.Replace(fileName, ".db", "", -1)
		fmt.Println(cleanClientId)
		getClient(cleanClientId)

	}
}
func tryConnecting(clientId string) bool {
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New("sqlite3", "file:./clients_db/"+clientId+".db?_foreign_keys=on", dbLog)
	if err != nil {
		fmt.Println(err)
	}
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		fmt.Println("erro pegandoDevice", err)
	}
	clientLog := waLog.Stdout("Client", "ERROR", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.EnableAutoReconnect = true

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Connected:
			clientMap[clientId] = client
			fmt.Println("Cliente conectado ao WhatsApp!")
		case *events.Disconnected:
			fmt.Println("Cliente " + clientId + "desconectou do WhatsApp!")
		case *events.LoggedOut:

			desconctarCliente(clientId, container)
			fmt.Println("Cliente " + clientId + " deslogou do WhatsApp!")
		case *events.Message:
			if strings.Contains(clientId, "chat") {
				handleMessage(v, clientId, client)
			}
		}
	})
	if client.Store.ID == nil {
		// removeClientDB(clientId, container)
		return false
	} else {
		err = client.Connect()
		clientMap[clientId] = client
		if strings.Contains(clientId, "chat") {
			setStatus(client, "conectado", types.JID{})
		}
		if err != nil {
			fmt.Println("erro pegandoDevice", err)
		}
		return true

	}
}

//	func removeClientDB(clientId string, container *sqlstore.Container) {
//		container.Close()
//		err := os.Remove("./clients_db/" + clientId + ".db")
//		if err != nil {
//			fmt.Println("---- Erro excluindo arquivo de sessão :", err)
//		}
//		return
//	}
func getClient(clientId string) *whatsmeow.Client {
	if clientMap[clientId] == nil {
		tryConnecting(clientId)
		time.Sleep(500)
	}
	return clientMap[clientId]
}

func randomBetween(min, max int) int {
	rand.Seed(time.Now().UnixNano()) // Garante que os números aleatórios mudem a cada execução
	return rand.Intn(max-min+1) + min
}

var repeats map[string]int = make(map[string]int)

func setStatus(client *whatsmeow.Client, status string, JID types.JID) {
	if status == "conectado" {
		typePresence := types.PresenceAvailable
		client.SendPresence(typePresence)
		return
	}
	if status == "digitando" {
		client.SendChatPresence(JID, types.ChatPresenceComposing, "")
		return
	}
	if status == "gravando" {
		client.SendChatPresence(JID, types.ChatPresence(types.ChatPresenceMediaAudio), "")
		return
	}

}
func main() {
	autoConnection()
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Erro ao carregar o arquivo .env")
	}
	PORT := os.Getenv("PORT_JELLYFISH_GOLANG")
	r := fiber.New(fiber.Config{
		ReadTimeout:       10 * time.Minute, // Ajuste o tempo limite de leitura conforme necessário
		WriteTimeout:      10 * time.Minute,
		StreamRequestBody: true,
		BodyLimit:         20 * 1024 * 1024,
	})
	r.Use(cors.New())

	r.Use(requestLogger)
	// r.LoadHTMLGlob("templates/*.html")
	r.Post("/verifyConnection", func(c *fiber.Ctx) error {
		clientId := c.FormValue("clientId")
		client := getClient(clientId)
		if client == nil {

			return c.Status(500).JSON(fiber.Map{
				"message": "Cliente não conectado",
			})
		}
		return c.Status(200).JSON(fiber.Map{
			"message": "Cliente conectado",
		})
	})
	r.Post("/deleteMessage", func(c *fiber.Ctx) error {
		clientId := c.FormValue("clientId")
		messageID := c.FormValue("messageID")
		receiverNumber := c.FormValue("receiverNumber")
		client := getClient(clientId)
		if client == nil {
			return c.Status(500).JSON(fiber.Map{
				"message": "Cliente não conectado",
			})
		}
		validNumber, err := client.IsOnWhatsApp([]string{receiverNumber})
		if err != nil {
			fmt.Println(err, "ERRO IS ONWHATSAPP")
		}
		response := validNumber[0]
		JID := response.JID
		messageKey := client.BuildMessageKey(JID, *client.Store.ID, messageID)
		client.SendMessage(context.Background(), JID, &waE2E.Message{
			ProtocolMessage: &waE2E.ProtocolMessage{
				Key:  messageKey,
				Type: waE2E.ProtocolMessage_REVOKE.Enum(),
			},
		})
		return c.Status(200).JSON(fiber.Map{
			"messageID": messageID,
			"message":   "excluída",
		})
	})
	r.Post("/destroySession", func(c *fiber.Ctx) error {
		clientId := c.FormValue("clientId")
		client := getClient(clientId)
		client.Logout()
		clientMap[clientId] = nil
		tryConnecting(clientId)
		fmt.Println("Desconectando")

		return c.Status(200).JSON(fiber.Map{
			"message": "Cliente desconectado",
		})
	})
	r.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("./templates/index.html")
	})
	r.Post("/sendFiles", func(c *fiber.Ctx) error {
		clientId := c.FormValue("clientId")
		client := getClient(clientId)
		if client == nil {
			return c.Status(500).JSON(fiber.Map{
				"message": "Cliente não conectado",
			})
		}

		// c.Status(200).JSON(fiber.Map{
		// 	"message": "Arquivo recebido e enviado no WhatsApp.",
		// })
		infoObjects := c.FormValue("infoObjects")
		var documento_padrao *multipart.FileHeader = nil
		documento_padrao, err = c.FormFile("documento_padrao")

		if err != nil {
			fmt.Println("Nenhum arquivo enviado.")
		}
		if documento_padrao != nil {
			savePath := "./uploads/" + clientId + documento_padrao.Filename
			// Salvar o arquivo no caminho especificado
			if err := c.SaveFile(documento_padrao, savePath); err != nil {
				fmt.Printf("Erro ao salvar o arquivo: %v", err)
			} else {
				fmt.Println("Arquivo salvo com sucesso!")
			}
		}
		var files *multipart.FileHeader = nil
		files, err = c.FormFile("file")
		if err != nil {
			fmt.Println("Nenhum arquivo enviado.")
		}
		var result []map[string]interface{}
		fmt.Println(infoObjects)
		// Deserializando o JSON para o map
		err = json.Unmarshal([]byte(infoObjects), &result)
		if err != nil {
			fmt.Printf("Erro ao converter JSON: %v", err)
		}

		// Exibindo o resultado
		go func() {
			var leitorZip *zip.Reader = nil
			if files != nil {
				zipFile, err := files.Open()
				if err != nil {
					log.Fatal("Erro abrindo ZIP", err)
				}
				defer zipFile.Close()
				// Lendo o arquivo ZIP
				zipReader, err := zip.NewReader(zipFile, files.Size)
				if err != nil {
					log.Fatal(err)
				}
				leitorZip = zipReader
			}
			for _, item := range result {
				fmt.Println(item["number"]) // Exibe cada item do JSON como um map[string]interface{}
				number := "+" + item["number"].(string)
				idImage, ok := item["id_image"].(string)
				if !ok {
					log.Println("Erro ao obter id_image")
					idImage = "UNDEFINED"
				}
				focus, ok := item["focus"].(string)
				if !ok {
					log.Println("Erro ao obter focus")
				}
				quotedMessage, ok := item["quotedMessage"].(map[string]interface{})
				if !ok {
					log.Println("quotedMessage não é um mapa.")
				}
				fmt.Println(item["paymentMessage"])
				paymentMessage, ok := item["paymentMessage"].(map[string]interface{})
				if !ok {
					log.Println("paymentMessage não é um mapa.")
				}

				editedIDMessage, ok := item["editedIDMessage"].(string)
				if !ok {
					// Defina o valor padrão ou apenas ignore a chave
					editedIDMessage = "" // ou outro valor padrão
				}
				idMensagem, ok := item["idMensagem"].(string)
				if !ok {
					// Defina o valor padrão ou apenas ignore a chave
					idMensagem = "" // ou outro valor padrão
				}
				fmt.Println("editedIDMessage", editedIDMessage)
				text := item["text"].(string)
				numbers := []string{number}
				validNumber, err := client.IsOnWhatsApp(numbers)
				if err != nil {
					fmt.Println(err, "ERRO ISONWHATSAPP")
				}
				if len(validNumber) == 0 {
					fmt.Println("Nenhum número está no WhatsApp.")
					continue
				}

				response := validNumber[0] // Acessa o primeiro item da slice
				JID := response.JID

				IsIn := response.IsIn
				// Query := response.Query
				// VerifiedName := response.VerifiedName

				if !IsIn {
					fmt.Println("NUMERO INVALIDO")
					continue
				}
				setStatus(client, "digitando", JID)

				message := &waE2E.Message{Conversation: &text}
				if leitorZip != nil {
					for _, arquivo := range leitorZip.File {
						fmt.Println(arquivo, idImage)
						if strings.Contains(arquivo.Name, "documento_"+idImage) {
							fmt.Println("Arquivos encontrados no ZIP:", leitorZip.File)

							// Criar um arquivo local para salvar
							fileName := strings.Replace(arquivo.Name, "documento_"+idImage+"_", "", -1)
							destFile, err := os.Create("./uploads/" + clientId + fileName)
							if err != nil {
								fmt.Errorf("erro ao criar arquivo: %v", err)
							}
							defer destFile.Close()

							// Abrir o arquivo do ZIP
							zipFileReader, err := arquivo.Open()
							if err != nil {
								fmt.Errorf("erro ao abrir arquivo do zip: %v", err)
							}
							defer zipFileReader.Close()
							// Copiar o conteúdo do arquivo do ZIP para o arquivo local
							_, err = io.Copy(destFile, zipFileReader)
							if err != nil {
								fmt.Errorf("erro ao copiar conteúdo para o arquivo: %v", err)
							}
							fmt.Printf("Arquivo %s salvo em ./uploads/%s\n", fileName, fileName)
							uniqueFileText := text
							if documento_padrao != nil {
								uniqueFileText = ""
							}

							if documento_padrao != nil {
								client.SendMessage(context.Background(), JID, prepararMensagemArquivo(uniqueFileText, message, "./uploads/"+clientId+fileName, client, clientId))
								fmt.Println("NOVAMENTE MENSAGEM", message)
							}
						}
					}
				}
				if documento_padrao != nil {

					message = prepararMensagemArquivo(text, message, "./uploads/"+clientId+documento_padrao.Filename, client, clientId)
				}
				if quotedMessage != nil {
					messageID, ok := quotedMessage["messageID"].(string)
					if !ok {
						log.Println("messageID não é uma string.")
					}
					sender, ok := quotedMessage["sender"].(string)
					if !ok {
						log.Println("sender não é uma string.")
					}
					messageQuoted, ok := quotedMessage["messageQuoted"].(string)
					if !ok {
						log.Println("messageQuoted não é uma string.")
					}
					validNumber, err := client.IsOnWhatsApp([]string{sender})
					if err != nil {
						fmt.Println(err, "ERRO IS ONWHATSAPP")
					}
					response := validNumber[0]
					senderJID := response.JID
					fmt.Println("JID da mensagem", messageID, senderJID, JID)

					var msg_quote *waE2E.Message = &waE2E.Message{
						ExtendedTextMessage: &waE2E.ExtendedTextMessage{
							Text: proto.String(messageQuoted),
						},
					}
					message.Conversation = nil
					message.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
						Text: proto.String(text),
						ContextInfo: &waE2E.ContextInfo{
							StanzaID:      &messageID,
							Participant:   proto.String(senderJID.String()),
							QuotedMessage: msg_quote,
						},
					}
				}
				if editedIDMessage != "" {
					message = client.BuildEdit(JID, editedIDMessage, message)
				}
				if paymentMessage != nil {

				}

				retornoEnvio, err := client.SendMessage(context.Background(), JID, message)
				if err != nil {
					fmt.Println("Erro ao enviar mensagem", err)
				}
				fmt.Println("-> Mensagem ENVIADA:", retornoEnvio.ID, clientId, text)

				if focus != "" {
					if focusedMessagesKeys == nil {
						focusedMessagesKeys = []string{}
					}
					focusedMessagesKeys = append(focusedMessagesKeys, focus+"_"+retornoEnvio.ID)

				}
				if idMensagem != "" {

					data := map[string]any{
						"evento":   "MENSAGEM_ENVIADA",
						"clientId": clientId,
						"data": map[string]string{
							"newID":  retornoEnvio.ID,
							"oldID":  idMensagem,
							"sender": strings.Replace(number, "+", "", -1),
						},
					}
					lastIndex := strings.LastIndex(clientId, "_")
					sufixo := clientId[lastIndex+1:]
					baseURL := strings.Split(mapOficial[sufixo], "chatbot")[0]
					if strings.Contains(baseURL, "disparo") {
						baseURL = strings.Split(mapOficial[sufixo], "disparo")[0]
					}
					sendToEndPoint(data, clientId, baseURL+"chatbot/chat/mensagens/novo-id/")
				}

				var tempoEsperado int = randomBetween(30, 45)
				if len(result) > 1 {
					fmt.Println("Tempo esperado para enviar a próxima mensagem:", tempoEsperado, "segundos...")
					time.Sleep(time.Duration(tempoEsperado) * time.Second)
				}
			}
			if documento_padrao != nil {
				err = os.Remove("./uploads/" + clientId + documento_padrao.Filename)
				if err != nil {
					fmt.Println("Erro ao excluir arquivo", err)
				}
			}

		}()
		return c.Status(200).JSON(fiber.Map{
			"message": "Arquivo recebido e enviado no WhatsApp.",
		})
	})
	r.Post("/getQRCode", func(c *fiber.Ctx) error {
		// Recupera o corpo da requisição e faz a bind para a estrutura de dados
		clientId := c.FormValue("clientId")
		fmt.Printf("Gerando QR Code para o cliente '%s'\n", clientId)
		if strings.Contains(clientId, "_chat") {
			store.DeviceProps = &waCompanionReg.DeviceProps{Os: proto.String("Shark Business(ChatBot)")}
		} else if strings.Contains(clientId, "_shark") {
			store.DeviceProps = &waCompanionReg.DeviceProps{Os: proto.String("Shark Business")}

		}
		if clientMap[clientId] != nil {
			return c.Status(200).JSON(fiber.Map{
				"message": "Cliente já autenticado",
			})

		}
		qrCode := c.FormValue("qrCode") == "true"
		// Obtenha o dispositivo
		dbLog := waLog.Stdout("Database", "INFO", true)

		container, err := sqlstore.New("sqlite3", "file:./clients_db/"+clientId+".db?_foreign_keys=on", dbLog)
		if err != nil {
			fmt.Println(err)
		}
		deviceStore, err := container.GetFirstDevice()
		if err != nil {
			fmt.Println(err)
		}
		// Crie o cliente WhatsApp
		clientLog := waLog.Stdout("Client", "ERROR", true)
		client := whatsmeow.NewClient(deviceStore, clientLog)
		client.EnableAutoReconnect = true
		client.AddEventHandler(func(evt interface{}) {
			switch v := evt.(type) {
			case *events.Connected:
				clientMap[clientId] = client
				fmt.Println("Cliente conectado ao WhatsApp, enviando evento")
				data := map[string]any{
					"evento":   "CLIENTE_CONECTADO",
					"clientId": clientId,
					"data": map[string]string{
						"numero_conectado": client.Store.ID.User,
						"status":           "conectado",
						"data_conexao":     "2025-01-20",
					},
				}
				lastIndex := strings.LastIndex(clientId, "_")
				sufixo := clientId[lastIndex+1:]
				baseURL := mapOficial[sufixo]
				sendToEndPoint(data, clientId, baseURL)
			case *events.Disconnected:
				fmt.Println("Cliente " + clientId + "desconectou do WhatsApp!")
			case *events.LoggedOut:
				clientMap[clientId] = nil
				desconctarCliente(clientId, container)

				fmt.Println("Cliente " + clientId + " deslogou do WhatsApp!")
			case *events.Message:
				if strings.Contains(clientId, "chat") {
					handleMessage(v, clientId, client)

				}
			}
		})
		if client.Store.ID == nil {
			// Não há ID armazenado, novo login
			qrChan, _ := client.GetQRChannel(context.Background())
			// Conecte o cliente
			err = client.Connect()
			if err != nil {
				fmt.Println(err)
			}
			// Aqui, aguardamos pelo QR Code gerado
			var evento string = "QRCODE_ATUALIZADO"

			go func() {
				repeats[clientId] = 1
				for evt := range qrChan {
					if evt.Event == "code" {
						// Gerar o QR Code como imagem PNG
						fmt.Println("GERANDO QRCODE --------")
						// Retorne a imagem PNG gerada como resposta
						if qrCode {
							png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256) // Tamanho: 256x256 pixels
							if err != nil {
								fmt.Println("Erro ao gerar QR Code: %v", err)
							}
							// Converter para Base64
							base64Img := base64.StdEncoding.EncodeToString(png)

							// Criar o data URL
							dataURL := fmt.Sprintf("data:image/png;base64,%s", base64Img)
							// SENDQRCODE
							data := map[string]any{
								"evento":   evento,
								"clientId": clientId,
								"data":     dataURL,
							}
							lastIndex := strings.LastIndex(clientId, "_")
							sufixo := clientId[lastIndex+1:]
							baseURL := mapOficial[sufixo]
							sendToEndPoint(data, clientId, baseURL)

						} else {
							data := map[string]any{
								"evento":   evento,
								"clientId": clientId,
								"data":     evt.Code,
							}
							lastIndex := strings.LastIndex(clientId, "_")
							sufixo := clientId[lastIndex+1:]
							baseURL := mapOficial[sufixo]
							sendToEndPoint(data, clientId, baseURL)
						}
						repeats[clientId] = repeats[clientId] + 1
						if repeats[clientId] >= 5 {
							// desconectar
							fmt.Println("Tentativas de login excedidas")
							desconctarCliente(clientId, container)
							return
						}
						fmt.Printf("Tentativa %d de 5 do cliente %s\n", repeats[clientId], clientId)

					} else if evt.Event == "success" {
						fmt.Println("-------------------AUTENTICADO")
						return
					}
				}
			}()

			firstQRCode := <-qrChan
			if qrCode {
				png, err := qrcode.Encode(firstQRCode.Code, qrcode.Medium, 256) // Tamanho: 256x256 pixels
				if err != nil {
					fmt.Printf("Erro ao gerar QR Code: %v", err)
				}
				// Converter para Base64
				base64Img := base64.StdEncoding.EncodeToString(png)
				// Criar o data URL
				dataURL := fmt.Sprintf("data:image/png;base64,%s", base64Img)

				data := map[string]any{
					"evento":   evento,
					"clientId": clientId,
					"data":     dataURL,
				}
				lastIndex := strings.LastIndex(clientId, "_")
				sufixo := clientId[lastIndex+1:]
				baseURL := mapOficial[sufixo]
				sendToEndPoint(data, clientId, baseURL)
				return c.Status(200).JSON(fiber.Map{
					"qrCode": dataURL,
				})
			} else {

				data := map[string]any{
					"evento":   evento,
					"clientId": clientId,
					"data":     firstQRCode.Code,
				}
				lastIndex := strings.LastIndex(clientId, "_")
				sufixo := clientId[lastIndex+1:]
				baseURL := mapOficial[sufixo]
				sendToEndPoint(data, clientId, baseURL)
				return c.Status(200).JSON(fiber.Map{
					"qrCode": firstQRCode.Code,
				})
			}
		} else {
			// Caso já tenha ID armazenado, não é necessário gerar QR Code

			// Conecta o cliente
			err = client.Connect()
			if err != nil {
				fmt.Println(err)
			}
			return c.Status(200).JSON(fiber.Map{
				"message": "Cliente já autenticado",
			})
		}
	})
	fmt.Println("Rodando na porta " + PORT)
	r.Listen(":" + PORT) // Escutando na porta 8080
}
func desconctarCliente(clientId string, container *sqlstore.Container) bool {
	fmt.Println("Desconectando " + clientId + " ...")
	client := getClient(clientId)
	if client != nil {
		clientMap[clientId] = nil
		client.Logout()
	}
	data := map[string]any{
		"evento":   "CLIENTE_DESCONECTADO",
		"clientId": clientId,
		"data":     "CLIENTE_DESCONECTADO",
	}
	lastIndex := strings.LastIndex(clientId, "_")
	sufixo := clientId[lastIndex+1:]
	baseURL := mapOficial[sufixo]
	sendToEndPoint(data, clientId, baseURL)
	return true
}
func prepararMensagemArquivo(text string, message *waE2E.Message, chosedFile string, client *whatsmeow.Client, clientId string) *waE2E.Message {
	// Abrindo o arquivo
	file, err := os.Open(chosedFile)
	if err != nil {
		fmt.Printf("Erro ao abrir o arquivo: %v", err)
	}
	defer file.Close()

	// Detectando o tipo MIME
	buf := make([]byte, 512) // O pacote mime usa os primeiros 512 bytes para detectar o tipo MIME
	_, err = file.Read(buf)
	if err != nil {
		fmt.Printf("Erro ao ler o arquivo: %v", err)
	}
	nomeArquivo := strings.Replace(strings.Split(chosedFile, "/")[len(strings.Split(chosedFile, "/"))-1], clientId, "", -1)

	kind, _ := filetype.Match(buf)
	if kind == filetype.Unknown {
		fmt.Println("Unknown file type")
	}

	mimeType := kind.MIME.Value
	if strings.Contains(nomeArquivo, ".mp3") {
		mimeType = "audio/mpeg"
	}
	fmt.Println("mimeType ", mimeType)

	// Resetando o ponteiro do arquivo
	file.Seek(0, 0)
	// Lendo o conteúdo do arquivo completo
	contentBuf := bytes.NewBuffer(nil)
	if _, err := contentBuf.ReadFrom(file); err != nil {
		fmt.Printf("Erro ao ler o arquivo completo: %v", err)
	}
	// Fazendo o upload da mídia

	// Criando a mensagem de imagem
	// Atribuindo a mensagem de imagem
	mensagem_ := *message
	mensagem_.Conversation = nil
	semExtensao := strings.TrimSuffix(nomeArquivo, filepath.Ext(nomeArquivo))

	if strings.Contains(nomeArquivo, ".mp3") {
		resp, err := client.Upload(context.Background(), contentBuf.Bytes(), whatsmeow.MediaAudio)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		imageMsg := &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		mensagem_.Conversation = nil
		mensagem_.AudioMessage = imageMsg
	} else if filetype.IsImage(buf) {
		resp, err := client.Upload(context.Background(), contentBuf.Bytes(), whatsmeow.MediaImage)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		fmt.Println("O arquivo é uma imagem válida.")

		imageMsg := &waE2E.ImageMessage{
			Caption:       proto.String(text),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		mensagem_.ImageMessage = imageMsg

	} else if filetype.IsVideo(buf) {
		resp, err := client.Upload(context.Background(), contentBuf.Bytes(), whatsmeow.MediaVideo)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		imageMsg := &waE2E.VideoMessage{
			Caption:       proto.String(text),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
			GifPlayback:   proto.Bool(false),

			//JPEGThumbnail: thumbnailBuf.Bytes(), // removed for this example
		}
		mensagem_.VideoMessage = imageMsg
	} else if filetype.IsAudio(buf) {
		resp, err := client.Upload(context.Background(), contentBuf.Bytes(), whatsmeow.MediaAudio)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		imageMsg := &waE2E.AudioMessage{
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		mensagem_.Conversation = nil
		mensagem_.AudioMessage = imageMsg
	} else {
		resp, err := client.Upload(context.Background(), contentBuf.Bytes(), whatsmeow.MediaDocument)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		var isDocx bool = strings.Contains(nomeArquivo, ".docx")
		if isDocx {
			mimeType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
		}
		var isXlsx bool = strings.Contains(nomeArquivo, ".xlsx")
		if isXlsx {
			mimeType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		}
		documentMsg := &waE2E.DocumentMessage{
			Title:         proto.String(semExtensao),
			Caption:       proto.String(text),
			Mimetype:      proto.String(mimeType),
			URL:           &resp.URL,
			DirectPath:    &resp.DirectPath,
			MediaKey:      resp.MediaKey,
			FileEncSHA256: resp.FileEncSHA256,
			FileSHA256:    resp.FileSHA256,
			FileLength:    &resp.FileLength,
		}
		mensagem_.DocumentMessage = documentMsg
	}
	return &mensagem_
}
