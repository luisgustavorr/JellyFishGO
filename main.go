package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode"

	"sync"
	"time"
	"unicode/utf8"

	"mime/multipart"

	"math/rand"

	_ "net/http/pprof"

	_ "github.com/go-sql-driver/mysql"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/pprof"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/google/uuid"
	"github.com/h2non/filetype"
	"github.com/joho/godotenv"
	jsoniter "github.com/json-iterator/go"
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
	_ "golang.org/x/image/webp" // Importa suporte para WebP
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"
)

var json = jsoniter.ConfigFastest
var (
	clientMap    = make(map[string]*whatsmeow.Client)
	clientsMutex sync.Mutex // Mutex simples
)
var mapOficial, _ = loadConfigInicial("spacemid_luis:G4l01313@tcp(pro107.dnspro.com.br:3306)/spacemid_sistem_adm")
var messagesToSend = make(map[string][]*waE2E.Message)
var focusedMessagesKeys = []string{}
var processedMessages = make(map[string]bool)
var _ = godotenv.Load()

var MODO_DESENVOLVIMENTO = os.Getenv("MODO_DESENVOLVIMENTO")
var desenvolvilemto = MODO_DESENVOLVIMENTO == "1"

type MessagesQueue struct {
	bufferLock     sync.Mutex
	messageBuffer  map[string][]interface{}
	messageTimeout map[string]*time.Timer
}

var groupPicCache sync.Map // Thread-safe
var dbPool *sql.DB

func getGroupProfilePicture(client *whatsmeow.Client, groupJID types.JID) *types.ProfilePictureInfo {
	if cached, ok := groupPicCache.Load(groupJID); ok {
		return cached.(*types.ProfilePictureInfo)
	}
	pictureInfo, err := client.GetProfilePictureInfo(groupJID, nil)
	if err != nil {
		log.Printf("Erro ao obter foto do grupo: %v", err)
		return nil
	}
	groupPicCache.Store(groupJID, pictureInfo)
	time.AfterFunc(1*time.Hour, func() {
		groupPicCache.Delete(groupJID)
	})
	return pictureInfo
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
	compositeKey := clientID + "_" + number
	if _, exists := c.messageBuffer[compositeKey]; !exists {
		c.messageBuffer[compositeKey] = []interface{}{}
	}
	c.messageBuffer[compositeKey] = append(c.messageBuffer[compositeKey], message)
	if timer, exists := c.messageTimeout[compositeKey]; exists {
		timer.Stop()
	}
	messageCount := len(c.messageBuffer[compositeKey])
	timerBetweenMessage := -0.15*float64(messageCount)*float64(messageCount) + 0.5*float64(messageCount) + 7
	if timerBetweenMessage < 0 {
		timerBetweenMessage = 0.001
	}
	timerDuration := time.Duration(timerBetweenMessage * float64(time.Second))
	fmt.Printf("⏳ -> ESPERANDO %.3f SEGUNDOS PARA %d MENSAGENS DO CLIENTE %s \n", timerBetweenMessage, messageCount, clientID)
	c.messageTimeout[compositeKey] = time.AfterFunc(timerDuration, func(currentClientID string) func() {
		return func() {
			c.ProcessMessages(currentClientID, number)
		}
	}(clientID)) // <--- clientID é capturado como valor aqui!
}
func (c *MessagesQueue) ProcessMessages(clientID string, number string) {
	c.bufferLock.Lock()
	defer c.bufferLock.Unlock()
	compositeKey := clientID + "_" + number
	messages := c.messageBuffer[compositeKey]
	if messages == nil {
		return
	}
	c.messageBuffer[compositeKey] = nil
	fmt.Printf("📦 -> ENVIANDO LOTES DE %d MENSAGENS DO %s\n", len(messages), clientID)
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
	sendToEndPoint(data, baseURL+"chatbot/chat/mensagens/novas-mensagens/")
	if timer, exists := c.messageTimeout[compositeKey]; exists {
		timer.Stop()
		delete(c.messageTimeout, compositeKey)
	}
}
func gerarTarefasProgramadas() {
	fmt.Println("Pegando tarefas")
	var err error
	dbPool, err = sql.Open("sqlite3", "./manager.db")
	if err != nil {
		log.Println("ERRO AO ADD TAREFA DB", err)
	}
	dbPool.SetMaxOpenConns(10)
	db := dbPool
	createTableSQL := `CREATE TABLE IF NOT EXISTS tarefas_agendadas (
		clientId TEXT,
		data_desejada TEXT,
		infoObjects TEXT,
		documento_padrao TEXT,
		files TEXT
	);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	rows, err := db.Query("SELECT clientId, data_desejada,infoObjects,documento_padrao,files FROM tarefas_agendadas")
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	defer db.Close()
	for rows.Next() {
		var clientId string
		var data_desejada string
		var infoObjects string
		var documento_padrao string
		var files string
		if err := rows.Scan(&clientId, &data_desejada, &infoObjects, &documento_padrao, &files); err != nil {
			log.Fatal(err)
		}
		iniciarTarefaProgramada(clientId, data_desejada, infoObjects, documento_padrao, files)
	}
}
func sendFilesProgramados(clientId string, infoObjects string, documento_padrao string, files string) {
	url := "http://localhost:3000/sendFiles"
	method := "POST"

	payload := &bytes.Buffer{}
	writer := multipart.NewWriter(payload)
	_ = writer.WriteField("clientId", clientId)
	_ = writer.WriteField("infoObjects", infoObjects)
	if documento_padrao != "" {
		documento_padrao_filePath := documento_padrao
		file, _ := os.Open(documento_padrao_filePath)
		defer file.Close()
		part4, _ := writer.CreateFormFile("documento_padrao", filepath.Base(documento_padrao_filePath))
		_, errFile4 := io.Copy(part4, file)
		if errFile4 != nil {
			fmt.Println("Erro:", errFile4)
			return
		}
	}

	if files != "" {
		files_filePath := files
		file, _ := os.Open(files_filePath)
		defer file.Close()
		part4, _ := writer.CreateFormFile("file", filepath.Base(files_filePath))
		_, errFile4 := io.Copy(part4, file)
		if errFile4 != nil {
			fmt.Println("Erro:", errFile4)
			return
		}
	}

	err := writer.Close()
	if err != nil {
		fmt.Println("ERRO1", err)
		return
	}
	client := &http.Client{}
	req, err := http.NewRequest(method, url, payload)
	if err != nil {
		fmt.Println("erro Enviando sendFiles", err)
		return
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	res, err := client.Do(req)
	if err != nil {
		fmt.Println("erro Enviando sendFiles1", err)
		return
	}
	defer res.Body.Close()

	body, err := io.ReadAll(res.Body)
	if err != nil {
		fmt.Println("erro Enviando sendFiles2", err)

		return
	}
	fmt.Println("RETORNO", string(body))
}
func iniciarTarefaProgramada(clientId string, dataDesejada string, infoObjects string, documento_padrao string, files string) {
	dbDateStr := dataDesejada
	layout := "2006-01-02 15:04:05"
	loc, err := time.LoadLocation("America/Sao_Paulo")
	if err != nil {
		log.Fatal("Erro ao carregar fuso horário:", err)
	}
	scheduledTime, err := time.ParseInLocation(layout, dbDateStr, loc)
	if err != nil {
		log.Println("Erro ao converter data:", err)
		removerTarefaProgramadaDB(clientId, dataDesejada, documento_padrao, files)
		return
	}
	now := time.Now().In(loc)          // Ambos no mesmo fuso horário
	duration := scheduledTime.Sub(now) // Forma alternativa mais precisa
	if duration <= 0 {
		fmt.Println("----> O horário agendado já passou. <-----", duration, now, scheduledTime)
		sendFilesProgramados(clientId, infoObjects, documento_padrao, files)
		removerTarefaProgramadaDB(clientId, dataDesejada, documento_padrao, files)
		return
	}
	fmt.Printf("Agendando disparo do cliente %s para as %s", clientId, dataDesejada)
	// Agendar a execução da função após a duração calculada
	time.AfterFunc(duration+1, func() {
		sendFilesProgramados(clientId, infoObjects, documento_padrao, files)
		removerTarefaProgramadaDB(clientId, dataDesejada, documento_padrao, files)
	})
}
func removerTarefaProgramadaDB(clientId string, dataDesejada string, documento_padrao string, files string) {
	if documento_padrao != "" {
		os.Remove(documento_padrao)
	}
	if files != "" {
		os.Remove(files)
	}
	var err error
	dbPool, err = sql.Open("sqlite3", "./manager.db")

	if err != nil {
		log.Println("ERRO AO ADD TAREFA DB", err)
	}
	dbPool.SetMaxOpenConns(10) // Ajuste co
	newDB := dbPool
	createTableSQL := `CREATE TABLE IF NOT EXISTS tarefas_agendadas (
		clientId TEXT,
		data_desejada TEXT,
		infoObjects TEXT,
		documento_padrao TEXT,
		files TEXT
	);`
	_, err = newDB.Exec(createTableSQL)
	if err != nil {
		log.Println("Erro ao criar TABELA", err)
	}
	defer newDB.Close()
	// Usar a consulta parametrizada
	query := "DELETE FROM tarefas_agendadas WHERE data_desejada = ? AND clientId = ?"
	_, err = newDB.Exec(query, dataDesejada, clientId)
	if err != nil {
		log.Println("Erro ao executar a consulta:", err)
	}
}
func addTarefaProgramadaDB(clientId string, dataDesejada string, infoObjects string, documento_padrao string, files string) {
	newDB, err := sql.Open("sqlite3", "./manager.db")
	if err != nil {
		log.Println("ERRO AO ADD TAREFA DB", err)
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS tarefas_agendadas (
		clientId TEXT,
		data_desejada TEXT,
		infoObjects TEXT,
		documento_padrao TEXT,
		files TEXT
	);`
	_, err = newDB.Exec(createTableSQL)
	if err != nil {
		log.Println("Erro ao criar TABELA", err)
	}
	defer newDB.Close()
	// Usar a consulta parametrizada
	query := "INSERT INTO `tarefas_agendadas` ( `clientId`, `data_desejada`, `infoObjects`,`documento_padrao`,`files`) VALUES ( ?, ?, ?,?,?);"
	_, err = newDB.Exec(query, clientId, dataDesejada, infoObjects, documento_padrao, files)
	if err != nil {
		log.Println("Erro ao executar a consulta:", err)
	}
	iniciarTarefaProgramada(clientId, dataDesejada, infoObjects, documento_padrao, files)
}
func loadConfigInicial(dsn string) (map[string]string, map[string]string) {
	// Conectar ao banco de dados
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Println(err)
	}
	if err := db.Ping(); err != nil {
		log.Println(err)
	}
	rows, err := db.Query("SELECT sufixo, link_oficial,base_link_teste,link_teste FROM clientes")
	if err != nil {
		log.Println(err)
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
			log.Println(err)
		}
		mapProducao[sufixo] = link_oficial
		mapDesenvolvimento[sufixo] = base_link_teste + link_teste
	}
	if err != nil {
		log.Fatal("Erro ao carregar o arquivo .env")
	}

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
func sendToEndPoint(data map[string]any, url string) {
	jsonData, err := json.Marshal(data)
	if err != nil {
		fmt.Printf("Erro ao criar marshal: %v", err)
		return
	}
	if url == "" {
		fmt.Printf("URL %s vazia", url)
		return
	}
	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		fmt.Printf("Erro ao criar a requisição: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "jelly_fish_con_|7@625^4|7")
	req.Header.Set("X-CSRFToken", getCSRFToken())
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Erro ao enviar a requisição: %v", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(url)
	fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | evento : ", data["evento"], " | clientId :", data["clientId"])
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
	if client == nil {
		// Reconecta sob demanda
		client = tryConnecting(clientId)
		if client == nil {
			fmt.Println("cliente não disponível")
			return "", ""
		}
	}
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
	parts := strings.SplitN(senderNumber, "@", 2) // Mais eficiente que Split
	return parts[0]
}

var messagesQueue = NewQueue()
var (
	sentMessages = make(map[string]bool)
	pendingSync  = make(chan string, 1000)
)

func requestLogger(c *fiber.Ctx) error {
	start := time.Now()
	method := c.Method()
	path := c.Path()
	clientId := utils.CopyString(c.FormValue("clientId"))
	err := c.Next()
	duration := time.Since(start)
	log.Printf(" [%s] %s | Tempo: %v | ClientId: %s\n", method, path, duration, clientId)
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
	if fullInfoMessage == nil {
		log.Println("Mensagem recebida é nil")
		return false
	}
	if client == nil {
		log.Println("Cliente Whatsmeow é nil")
		return false
	}
	var channel bool = fullInfoMessage.SourceWebMsg.GetBroadcast()
	var statusMessage bool = strings.Contains(fullInfoMessage.Info.Chat.String(), "status")
	var LocationMessage bool = fullInfoMessage.Message.LocationMessage != nil
	var pollMessage bool = fullInfoMessage.Message.GetPollUpdateMessage() != nil || fullInfoMessage.Message.GetPollCreationMessage() != nil || fullInfoMessage.Message.GetPollCreationMessageV2() != nil || fullInfoMessage.Message.GetPollCreationMessageV3() != nil || fullInfoMessage.Message.GetPollCreationMessageV4() != nil || fullInfoMessage.Message.GetPollCreationMessageV5() != nil
	if statusMessage || pollMessage || LocationMessage || channel {
		fmt.Println("Mensagem de grupo ou status, ignorando...", fullInfoMessage.Info.Chat.String())
		return false
	}
	var contactMessage *waE2E.ContactMessage = fullInfoMessage.Message.GetContactMessage()
	message := fullInfoMessage.Message
	var groupMessage bool = strings.Contains(fullInfoMessage.Info.Chat.String(), "@g.us")
	var contextInfo = message.ExtendedTextMessage.GetContextInfo()
	var senderName string = fullInfoMessage.Info.PushName
	var text string = getText(message)
	var fromMe = fullInfoMessage.Info.IsFromMe
	var senderNumber string = getSender(fullInfoMessage.Info.Sender.User)
	if utf8.RuneCountInString(senderNumber) > 13 {
		fmt.Println("------> Sender Number Grande Demais <------")
		return false
	}
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
		return false
	}
	response := validNumber[0] // Acessa o primeiro item da slice
	JID := response.JID
	if fromMe {
		senderNumber = fullInfoMessage.Info.Chat.User
	}
	params := &whatsmeow.GetProfilePictureParams{}
	profilePic, _ := client.GetProfilePictureInfo(JID, params)
	if editedInfo != "" {
		edited = 1
		id_message = editedInfo
	}
	var contactObject map[string]interface{} = nil
	if contactMessage != nil {
		var name string = *contactMessage.DisplayName
		var vcard string = *contactMessage.Vcard
		startIndex := strings.Index(vcard, "waid=")
		var numero string
		if startIndex != -1 {
			startIndex += len("waid=") // Pular "waid="
			endIndex := startIndex
			for endIndex < len(vcard) && unicode.IsDigit(rune(vcard[endIndex])) {
				endIndex++
			}
			numero = vcard[startIndex:endIndex]
		} else {
			numero = "sem_whatsapp"
		}
		contactObject = map[string]interface{}{
			"contato": numero,
			"nome":    name,
		}
	}
	messageAttr := map[string]interface{}{
		"quotedMessage": quotedMessageID,
		"edited":        edited,
		"contact":       contactObject,
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
	if groupMessage {
		groupJID := fullInfoMessage.Info.Chat
		groupInfo, _ := client.GetGroupInfo(groupJID)
		groupImage := getGroupProfilePicture(client, groupJID)
		if groupImage != nil {
			mensagem["imagem_grupo"] = groupImage.URL
		}
		if groupInfo != nil {
			mensagem["nome_grupo"] = groupInfo.GroupName.Name
		}
		mensagem["id_grupo"] = strings.Replace(fullInfoMessage.Info.Chat.String(), "@g.us", "", -1)
	}
	var focus = getMessageFocus(focusedMessagesKeys, id_message)
	if focus != "" {
		if focus == "noreply" {
			fmt.Println("Mensagem não deve ser enviada, focus 'noreply'")
			return false
		}
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
			sendToEndPoint(data, baseURL+"chatbot/chat/mensagens/novas-mensagens/")
		}
	} else {

		var uniqueMessageID string = strings.Replace(id_message+"_"+senderNumber+"_"+clientId, " ", "", -1)
		if val, exists := sentMessages[uniqueMessageID]; exists && val && edited == 0 {
			fmt.Println("❌ -> Mensagem REPETIDA:", id_message, senderName, senderNumber, clientId, text)
			fmt.Println("!--------------------->MENSAGEM COM ID JÁ ENVIADO<---------------------!", sentMessages[uniqueMessageID])
			return false
		}
		var MessageID []types.MessageID = []types.MessageID{id_message}
		client.MarkRead(MessageID, time.Now(), JID, JID, types.ReceiptTypeRead)
		if media != "" || text != "" || contactMessage != nil {
			if messagesToSend[clientId] == nil {
				messagesToSend[clientId] = []*waE2E.Message{}
			}
			messagesToSend[clientId] = append(messagesToSend[clientId], message)
			messagesQueue.AddMessage(clientId, objetoMensagens, senderNumber)
			log.Printf("------------------ %s Receiving Message Event | By Group : %v ------------------------ \n\n", clientId, groupMessage)
			fmt.Println("📩 -> Mensagem RECEBIDA:", id_message, senderName, senderNumber, clientId, text, " | By Group:", groupMessage)
			fmt.Println("Mensagens registradas : ", len(sentMessages)+1)
			sentMessages[uniqueMessageID] = true
			pendingSync <- id_message + "_" + senderNumber + "_" + clientId
			processedMessages[clientId+"_"+senderNumber+"_"+id_message] = true
		}
	}
	return true
}
func safePanic(arguments ...any) {
	log.Println("Pânico controlado -> ", arguments)
	saveMessagesReceived()
	os.Exit(1)
}
func autoConnection() {
	dir := "./clients_db"
	files, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("Erro ao ler clientes: %v", err)
		return
	}
	// Fase 1: Coleta de IDs sem lock
	var clientIDs []string
	for _, file := range files {
		if !file.IsDir() {
			clientID := strings.TrimSuffix(file.Name(), ".db")
			clientIDs = append(clientIDs, clientID)
		}
	}
	// Fase 2: Processamento individual com lock curto
	for _, clientID := range clientIDs {
		clientsMutex.Lock()
		_, exists := clientMap[clientID]
		clientsMutex.Unlock()
		if !exists {
			// Reconecta sem lock
			newClient := tryConnecting(clientID)
			clientsMutex.Lock()
			if newClient != nil {
				clientMap[clientID] = newClient
			} else {
				removeClientDB(clientID, nil)
			}
			clientsMutex.Unlock()
		}
	}
}
func isOnWhatsAppSafe(client *whatsmeow.Client, numbers []string) ([]types.IsOnWhatsAppResponse, error) {
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return nil, fmt.Errorf("cliente não conectado")
	}
	return client.IsOnWhatsApp(numbers)
}
func checkNumberWithRetry(client *whatsmeow.Client, number string, de_grupo bool) (resp []types.IsOnWhatsAppResponse, err error) {
	maxRetries := 3
	backoff := 1 * time.Second

	for i := 0; i < maxRetries; i++ {
		responses, err := isOnWhatsAppSafe(client, []string{number})
		if err == nil && len(responses) > 0 {
			response := responses[0]
			fmt.Println("Resposta Recebida", responses)
			if response.IsIn || de_grupo {
				return responses, nil
			}
		}
		time.Sleep(backoff)
	}

	if len(number) < 5 {
		return []types.IsOnWhatsAppResponse{}, fmt.Errorf("error : número pequeno demais, inválido para segunda comparação")
	}
	numberWith9 := number[:4] + "9" + number[4:]
	fmt.Println("Tentando com 9 adicional", numberWith9)

	backoff = 1 * time.Second
	for i := 0; i < maxRetries; i++ {
		responses, err := isOnWhatsAppSafe(client, []string{numberWith9})
		if err == nil && len(responses) > 0 {
			return responses, nil
		}
		time.Sleep(backoff)
		backoff *= 2
	}
	return []types.IsOnWhatsAppResponse{}, fmt.Errorf("falha após %d tentativas: %v", maxRetries, err)
}
func tryConnecting(clientId string) *whatsmeow.Client {
	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New("sqlite3", "file:./clients_db/"+clientId+".db?_foreign_keys=on", dbLog)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	deviceStore, err := container.GetFirstDevice()
	if err != nil {
		fmt.Println("erro pegandoDevice", err)
		return nil
	}
	clientLog := waLog.Stdout("Client", "ERROR", true)
	client := whatsmeow.NewClient(deviceStore, clientLog)
	client.EnableAutoReconnect = true
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Connected:
			clientsMutex.Lock()
			clientMap[clientId] = client
			clientsMutex.Unlock()
			fmt.Println("🎉 -> CLIENTE CONECTADO", clientId)
		case *events.Receipt:
			if strings.Contains(clientId, "chat") {
				handleSeenMessage(v, clientId)
			}
		case *events.Disconnected:
			log.Printf("🔄 -> RECONECTANDO CLIENTE %s", clientId)
		case *events.LoggedOut:
			desconctarCliente(clientId)
			fmt.Println("Cliente " + clientId + " deslogou do WhatsApp!")
		case *events.Message:
			if strings.Contains(clientId, "chat") {
				handleMessage(v, clientId, client)
			}
		}
	})
	if client.Store.ID == nil {
		// removeClientDB(clientId, container)
		return nil
	} else {
		err = client.Connect()
		clientsMutex.Lock()
		defer clientsMutex.Unlock()
		clientMap[clientId] = client
		if strings.Contains(clientId, "chat") {
			setStatus(client, "conectado", types.JID{})
		}
		if err != nil {
			fmt.Println("erro pegandoDevice", err)
		}
		return client

	}
}
func removeClientDB(clientId string, container *sqlstore.Container) {
	if container != nil {
		container.Close()
	}
	err := os.Remove("./clients_db/" + clientId + ".db")
	if err != nil {
		fmt.Println("---- Erro excluindo arquivo de sessão :", err)
	}
}
func getClient(clientId string) *whatsmeow.Client {
	clientsMutex.Lock()
	defer clientsMutex.Unlock()
	if clientMap[clientId] == nil || !clientMap[clientId].IsConnected() || !clientMap[clientId].IsLoggedIn() {
		return nil
	}
	return clientMap[clientId] // Retorna nil se não existir
}
func randomBetween(min, max int) int {
	rand.Seed(time.Now().UnixNano()) // Garante que os números aleatórios mudem a cada execução
	return rand.Intn(max-min+1) + min
}

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

var (
	repeats               sync.Map // clientId -> *int32 (contador atômico)
	stoppedQrCodeRequests sync.Map // clientId -> *int32 (0 = false, 1 = true)
)

func normalizeFileName(filename string) string {
	normalizedFileName := filename
	normalizedFileName = strings.ReplaceAll(normalizedFileName, " ", "_") // Substitui espaços por underline
	normalizedFileName = strings.ReplaceAll(normalizedFileName, ":", "_")
	return normalizedFileName
}

var (
	seenMessagesQueue      = NewQueue()
	seenMessagesQueueMutex sync.Mutex
)

func sendSeenMessages(clientId string) {
	seenMessagesQueueMutex.Lock()
	defer seenMessagesQueueMutex.Unlock()
	data := map[string]any{
		"evento":   "MENSAGEM_LIDA",
		"clientId": clientId,
		"data":     seenMessagesQueue.messageBuffer[clientId],
	}
	lastIndex := strings.LastIndex(clientId, "_")
	sufixo := clientId[lastIndex+1:]
	baseURL := strings.Split(mapOficial[sufixo], "chatbot")[0]
	if strings.Contains(baseURL, "disparo") {
		baseURL = strings.Split(mapOficial[sufixo], "disparo")[0]
	}
	fmt.Println("✅ -> MENSAGEM LIDA DO CLIENTE ", clientId)
	sendToEndPoint(data, baseURL+"chatbot/chat/mensagens/read/")
	seenMessagesQueue.messageBuffer[clientId] = nil
	if timer, exists := seenMessagesQueue.messageTimeout[clientId]; exists {
		timer.Stop()
		delete(seenMessagesQueue.messageTimeout, clientId)
	}
}
func addSeenMessageToQueue(message interface{}, clientId string) bool {
	seenMessagesQueueMutex.Lock()
	defer seenMessagesQueueMutex.Unlock()
	if _, exists := seenMessagesQueue.messageBuffer[clientId]; !exists {
		seenMessagesQueue.messageBuffer[clientId] = []interface{}{}
	}
	seenMessagesQueue.messageBuffer[clientId] = append(seenMessagesQueue.messageBuffer[clientId], message)
	if timer, exists := seenMessagesQueue.messageTimeout[clientId]; exists {
		timer.Stop()
	}
	messageCount := len(seenMessagesQueue.messageBuffer[clientId])
	timerBetweenMessage := -0.15*float64(messageCount)*float64(messageCount) + 0.5*float64(messageCount) + 2
	if timerBetweenMessage < 0 {
		timerBetweenMessage = 0.001
	}
	timerDuration := time.Duration(timerBetweenMessage * float64(time.Second))
	seenMessagesQueue.messageTimeout[clientId] = time.AfterFunc(timerDuration, func(currentClientID string) func() {
		return func() {
			sendSeenMessages(currentClientID)
		}
	}(clientId))
	return true
}
func handleSeenMessage(event *events.Receipt, clientId string) {

	if event.Type == "read" && !event.IsFromMe {
		for i := 0; i < len(event.MessageIDs); i++ {
			var seenMessage = make(map[string]interface{})
			seenMessage["idMessage"] = event.MessageIDs[i]
			seenMessage["chat"] = event.Chat.User
			seenMessage["lida"] = true
			addSeenMessageToQueue(seenMessage, clientId)
		}
	}
}
func saveMessagesReceived() {
	fmt.Println("Deve salvar mensagens")
	go func() {
		batch := make([]string, 0, 100)
		for msgID := range pendingSync {
			batch = append(batch, msgID)
			if len(batch) >= 20 {
				fmt.Println("💾 -> SALVANDO MENSAGENS NO DB ")
				placeholders := make([]string, len(batch))
				values := make([]interface{}, len(batch))
				for i, msgID := range batch {
					placeholders[i] = "(?)"
					values[i] = msgID
				}
				var err error
				db, err := sql.Open("sqlite3", "./messages.db")
				db.SetMaxOpenConns(10) // Ajuste co
				if err != nil {
					fmt.Println("ERRO AO ADD TAREFA DB", err)
					return
				}
				createTableSQL := `CREATE TABLE IF NOT EXISTS sent_messages (
                    id TEXT
                );`
				_, err = db.Exec(createTableSQL)
				if err != nil {
					log.Fatal("Erro ao criar TABELA", err)
				}
				query := "INSERT INTO sent_messages (id) VALUES " + strings.Join(placeholders, ",")
				_, _ = db.Exec(query, values...)
				batch = batch[:0]
			}
		}
	}()
}
func loadMessagesReceiveds() {
	var err error
	db, err := sql.Open("sqlite3", "./messages.db")
	db.SetMaxOpenConns(10) // Ajuste co
	if err != nil {
		safePanic("ERRO AO ADD TAREFA DB", err)
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS sent_messages (
                    id TEXT
                );`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	query := "SELECT * FROM sent_messages"
	result, _ := db.Query(query)
	defer result.Close()
	if result != nil {
		fmt.Println(result)
		for result.Next() {
			var id string
			if err := result.Scan(&id); err != nil {
				log.Println(err)
			}
			sentMessages[id] = true
		}

	}
}

type sendMessageInfo struct {
	ClientIdLocal    string                   `json:"clientIdLocal"`
	Result           []map[string]interface{} `json:"result"`
	documento_padrao *multipart.FileHeader    `json:"-"`
	files            *multipart.FileHeader    `json:"-"`
	SendContact      string                   `json:"sendContact"`
	NoTimeout        string                   `json:"noTimeout"`
	DataProgramada   string                   `json:"dataProgramada"`
	InfoObjects      string                   `json:"infoObjects"`
	Counter          int32                    `json:"counter"`
	UUID             string                   `json:"uuid"`
}

var dbMensagensPendentes *sql.DB
var mensagensPendentes sync.Map

func updateDBMensagensPendentes(uuid string, processed bool) bool {
	db := dbMensagensPendentes
	var err error
	if db == nil {
		db, err = sql.Open("sqlite3", "./pendingMessages.db")
		if err != nil {
			log.Println("ERRO AO ADD TAREFA DB", err)
		}
		dbMensagensPendentes = db
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS pendingMessages (
		uuid TEXT PRIMARY KEY,
		sendInfo TEXT
	);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	if processed {
		// fmt.Println("Tentando excluir Mensagem ----")
		deleteSQL := `DELETE FROM pendingMessages WHERE uuid = ?`
		_, err = db.Exec(deleteSQL, uuid)
		if err != nil {
			fmt.Println("Tentando excluir Mensagem ----", err)
			return false
		}
	} else {
		sendInfo, ok := mensagensPendentes.Load(uuid)
		if !ok {
			fmt.Println("Erro ao carregar Mensagens pendentes no update")
			return false
		}
		insertSQL := `INSERT OR REPLACE INTO pendingMessages (uuid, sendInfo) VALUES (?, ?)`
		_, err = db.Exec(insertSQL, uuid, sendInfo)
		if err != nil {
			return false
		}
	}
	return true
}
func addMensagemPendente(uuid string, sendInfo sendMessageInfo) {
	data, err := json.Marshal(sendInfo)
	if err != nil {
		return
	}
	mensagensPendentes.Store(uuid, string(data))
	updateDBMensagensPendentes(uuid, false)
}
func removeGrupoMensagemPendente(uuid string) {
	updateDBMensagensPendentes(uuid, true)
	mensagensPendentes.Delete(uuid)
}
func removeMensagemPendente(uuid string, text string, number string) {
	sendInfoStr, ok := mensagensPendentes.Load(uuid)
	if !ok {
		fmt.Println("Erro ao carregar Mensagens pendentes no removeMensagemPendente")
		return
	}
	var sendInfo sendMessageInfo
	err := json.Unmarshal([]byte(sendInfoStr.(string)), &sendInfo)
	if err != nil {
		panic(err)
	}
	re := regexp.MustCompile("[0-9]+")
	number = strings.Join(re.FindAllString(number, -1), "")
	var newResult []map[string]interface{}
	for _, msgInfo := range sendInfo.Result {
		fmt.Println(msgInfo, text, number, msgInfo["number"] != number)
		if msgInfo["text"] != text && msgInfo["number"] != number {
			newResult = append(newResult, msgInfo)
		}
	}
	fmt.Println(newResult)
	if len(newResult) == 0 {
		// removeGrupoMensagemPendente(uuid)
	} else {
		fmt.Println(sendInfo)
		sendInfo.Result = newResult
		fmt.Println(sendInfo)
		// addMensagemPendente(uuid, sendInfo)
	}
	// updateDBMensagensPendentes(uuid, true)
	// mensagensPendentes.Delete(uuid)
}
func loadMensagensPendentesFromDB() {
	db := dbMensagensPendentes
	var err error
	if db == nil {
		db, err = sql.Open("sqlite3", "./pendingMessages.db")
		if err != nil {
			log.Println("ERRO AO ADD TAREFA DB", err)
		}
		dbMensagensPendentes = db
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS pendingMessages (
		uuid TEXT PRIMARY KEY,
		sendInfo TEXT
	);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}

	rows, _ := db.Query("SELECT uuid,sendInfo FROM pendingMessages")
	for rows.Next() {
		var sendInfoStr string
		var uuid string
		if err := rows.Scan(&uuid, &sendInfoStr); err != nil {
			log.Fatal(err)
		}
		var sendInfo sendMessageInfo
		err := json.Unmarshal([]byte(sendInfoStr), &sendInfo)
		if err != nil {
			panic(err)
		}
		processarGrupoMensagens(sendInfo)
	}
}

type singleMessageInfo struct {
	client      *whatsmeow.Client
	clientId    string
	context     context.Context
	text        string
	idMensagem  string
	focus       string
	number      string
	JID         types.JID
	messageInfo *waE2E.Message
	Attempts    int32
	LastError   error
}

func processarGrupoMensagens(sendInfoMain sendMessageInfo) {
	fmt.Println(sendInfoMain.UUID)
	workers := make(chan struct{}, 10)
	limiter := rate.NewLimiter(rate.Every(2*time.Second), 1)
	var wg sync.WaitGroup
	fmt.Printf("Processando grupo de %v mensagens para %s \n", len(sendInfoMain.Result), sendInfoMain.ClientIdLocal)
	var leitorZip *zip.Reader = nil
	if sendInfoMain.files != nil {
		zipFile, err := sendInfoMain.files.Open()
		if err != nil {
			log.Fatal("Erro abrindo ZIP", err)
		}
		defer zipFile.Close()
		zipReader, err := zip.NewReader(zipFile, sendInfoMain.files.Size)
		if err != nil {
			log.Fatal(err)
		}
		leitorZip = zipReader
	}
	counter := sendInfoMain.Counter
	for i := range sendInfoMain.Result {
		wg.Add(1)
		workers <- struct{}{}
		go func(index int, sendInfo sendMessageInfo) {
			documento_padrao := sendInfo.documento_padrao
			sendContact := sendInfo.SendContact
			item := sendInfo.Result[i]
			currentClientID, _ := item["clientId"].(string)
			defer func() {
				<-workers
				wg.Done()
			}()
			currentCount := atomic.AddInt32(&counter, 1)
			limiter.Wait(context.Background())
			log.Printf("------------------ %s Inside Go Func Inside FOR (%v,%v)------------------------ \n\n", currentClientID, currentCount, len(sendInfo.Result))
			focus, _ := item["focus"].(string)
			text, ok := item["text"].(string)
			if !ok {
				text = ""
			}
			id_grupo, ok := item["id_grupo"].(string)
			if !ok {
				id_grupo = ""
			}
			idMensagem, ok := item["idMensagem"].(string)
			if !ok {
				idMensagem = "" // ou outro valor padrão
			}
			re := regexp.MustCompile("[0-9]+")
			numberWithOnlyNumbers := strings.Join(re.FindAllString(item["number"].(string), -1), "")
			if len(numberWithOnlyNumbers) > 2 && id_grupo == "" {
				if numberWithOnlyNumbers[:2] != "55" {
					numberWithOnlyNumbers = "+55" + numberWithOnlyNumbers
				}
			} else {
				numberWithOnlyNumbers = ""
			}

			number := numberWithOnlyNumbers
			client := getClient(currentClientID)
			if client == nil {
				client = tryConnecting(currentClientID)
				if client == nil {
					fmt.Println("⛔ -> Cliente não disponível, ClientId: ", currentClientID, " | Numero: ", number, " | Mensagem :", text, "| ID Grupo", id_grupo)
					return
				}
			}
			if client == nil {
				return
			}
			msg := singleMessageInfo{
				clientId:    currentClientID,
				client:      client,
				context:     context.Background(),
				messageInfo: nil,
				Attempts:    0,
				LastError:   nil,
				focus:       focus,
				text:        text,
				number:      number,
				idMensagem:  idMensagem,
			}
			currentClientID = msg.clientId
			client = msg.client
			text = msg.text
			number = msg.number
			fmt.Println("Cliente recuperado :", currentClientID)
			// fmt.Println("Doc padrao recuperado :", documento_padrao)
			// fmt.Println("Files recuperado :", sendInfo.files)
			var idImage string
			switch v := item["id_image"].(type) {
			case string:
				idImage = v
			case int, int64, float64:
				idImage = fmt.Sprintf("%v", v) // Converte para string
			default:
				idImage = "UNDEFINED"
			}
			quotedMessage, _ := item["quotedMessage"].(map[string]interface{})
			paymentMessage, _ := item["paymentMessage"].(map[string]interface{})
			editedIDMessage, ok := item["editedIDMessage"].(string)
			if !ok {
				editedIDMessage = "" // ou outro valor padrão
			}
			validNumber, err := checkNumberWithRetry(client, number, id_grupo != "")
			var JID types.JID = types.JID{}
			if id_grupo != "" {
				JID = types.JID{User: strings.Replace(id_grupo, "@g.us", "", -1), Server: types.GroupServer}
			} else {
				if err != nil {
					fmt.Println(err, "ERRO ISONWHATSAPP")
					fmt.Println("⛔ -> Numero inválido Erro. ClientId: ", currentClientID, " | Numero: ", number, " | Mensagem :", text, "| ID Grupo", id_grupo)
					return
				}
				if len(validNumber) == 0 {
					fmt.Println("⛔ -> Numero inválido. ClientId: ", currentClientID, " | Numero: ", number, " | Mensagem :", text, "| ID Grupo", id_grupo)
					return
				}
				response := validNumber[0] // Acessa o primeiro item da slicet
				JID = response.JID
				IsIn := response.IsIn
				if !IsIn {
					fmt.Println("⛔ -> Numero not In WhatsApp. ClientId: ", currentClientID, " | Numero: ", number, " | Mensagem :", text, "| ID Grupo", id_grupo)
					return
				}
			}
			msg.JID = JID
			setStatus(client, "digitando", JID)
			message := &waE2E.Message{Conversation: &text}
			if sendContact != "" {
				var sendContactMap map[string]string
				// Deserializando o JSON corretamente para o map
				err = json.Unmarshal([]byte(sendContact), &sendContactMap)
				if err != nil {
					fmt.Println("Erro ao desserializar JSON:", err)
					return
				}
				validNumber, err := client.IsOnWhatsApp([]string{sendContactMap["contato"]})
				if err != nil {
					fmt.Println(err, "ERRO IS ONWHATSAPP")
				}
				response := validNumber[0]
				cell := response.JID.User
				formatedNumber := formatPhoneNumber(cell)
				if formatedNumber != "" {
					contactMessage := &waE2E.Message{
						ContactMessage: &waE2E.ContactMessage{
							DisplayName: proto.String(sendContactMap["nome"]),
							Vcard:       proto.String("BEGIN:VCARD\nVERSION:3.0\nN:;" + sendContactMap["nome"] + ";;;\nFN:" + sendContactMap["nome"] + "\nitem1.TEL;waid=" + cell + ":" + formatedNumber + "\nitem1.X-ABLabel:Celular\nEND:VCARD"),
						}}
					msg.messageInfo = contactMessage
					processarMensagem(msg, sendInfo.UUID)
					// client.SendMessage(context.Background(), JID, contactMessage)
				} else {
					fmt.Println("FORMATADO ->", err)
				}
			}
			if text == "" {
				return
			}
			if leitorZip != nil {
				for _, arquivo := range leitorZip.File {
					if strings.Contains(arquivo.Name, "documento_"+idImage) {
						// Criar um arquivo local para salvar
						fileName := strings.Replace(arquivo.Name, "documento_"+idImage+"_", "", -1)
						destFile, err := os.Create("./uploads/" + currentClientID + fileName)
						if err != nil {
							fmt.Printf("erro ao criar arquivo: %v", err)
						}
						defer destFile.Close()
						// Abrir o arquivo do ZIP
						zipFileReader, err := arquivo.Open()
						if err != nil {
							fmt.Printf("erro ao abrir arquivo do zip: %v", err)
						}
						defer zipFileReader.Close()
						// Copiar o conteúdo do arquivo do ZIP para o arquivo local
						_, err = io.Copy(destFile, zipFileReader)
						if err != nil {
							fmt.Printf("erro ao copiar conteúdo para o arquivo: %v", err)
						}
						uniqueFileText := text
						if documento_padrao != nil {
							uniqueFileText = ""
						}
						tempMessage := prepararMensagemArquivo(uniqueFileText, message, "./uploads/"+currentClientID+fileName, client, currentClientID)
						if documento_padrao != nil {
							msg.messageInfo = tempMessage
							processarMensagem(msg, sendInfo.UUID)
						} else {
							message = tempMessage
						}
					}
				}
			}
			if documento_padrao != nil {
				message = prepararMensagemArquivo(text, message, "./uploads/"+currentClientID+documento_padrao.Filename, client, currentClientID)
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
					fmt.Println(err, "ERRO IS ONWHATSAPP pgm")
					return
				}
				response := validNumber[0]
				senderJID := response.JID
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
			msg.messageInfo = message
			processarMensagem(msg, sendInfo.UUID)
		}(i, sendInfoMain)
		totalDelay := time.Duration(randomBetween(30, 45)) * time.Second
		fmt.Println("⏳ Tempo esperado para enviar a próxima mensagem:", totalDelay, "segundos...")
		time.Sleep(totalDelay) //-\\ é o que separa as mensagens de lote
	}
	wg.Wait()
}
func autoCleanup() {
	ticker := time.NewTicker(1 * time.Hour)
	for range ticker.C {
		clientsMutex.Lock()
		for id, client := range clientMap {
			if !client.IsConnected() {
				delete(clientMap, id)
			}
		}
		clientsMutex.Unlock()
	}
}
func processarMensagem(msg singleMessageInfo, uuid string) {
	if err := enviarMensagem(msg, uuid); err != nil {
		msg.Attempts++
		msg.LastError = err
		requeueMessage(msg, uuid) // Adicione a mensagem a uma fila de retentativas
	}
}
func requeueMessage(msg singleMessageInfo, uuid string) {
	newAttempts := atomic.AddInt32(&msg.Attempts, 1)
	if newAttempts >= 3 {
		println("Mensagem passou quantidade de tentativas máximas")
		return
	}
	processarMensagem(msg, uuid)
}

var focusedMessagesKeysMutex sync.Mutex

func adicionarFocusedMessage(key string) {
	focusedMessagesKeysMutex.Lock()
	defer focusedMessagesKeysMutex.Unlock()
	focusedMessagesKeys = append(focusedMessagesKeys, key)
}
func enviarMensagem(msg singleMessageInfo, uuid string) error {
	clientId := msg.clientId
	client := msg.client
	JID := msg.JID
	context := msg.context
	text := msg.text
	focus := msg.focus
	idMensagem := msg.idMensagem
	number := msg.number
	retornoEnvio, err := client.SendMessage(context, JID, msg.messageInfo)
	fmt.Println(JID)
	// fmt.Printf("📦 -> MENSAGEM [ID:%s, clientID:%s, mensagem:%s, numero:%s] ENVIADA \n", JID, clientId, text, number)
	fmt.Printf("📦 -> MENSAGEM [ID:%s, clientID:%s, mensagem:%s, numero:%s] ENVIADA \n", retornoEnvio.ID, clientId, text, number)
	// removeMensagemPendente(uuid, text, number)
	if err != nil {
		fmt.Println("Erro ao enviar mensagem", err)
	}
	if focus != "" {
		if focusedMessagesKeys == nil {
			focusedMessagesKeys = []string{}
		}
		adicionarFocusedMessage(focus + "_" + retornoEnvio.ID)
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
		sendToEndPoint(data, baseURL+"chatbot/chat/mensagens/novo-id/")
	}
	return nil
}
func cleanup() {
	saveMessagesReceived()
}
func main() {

	go autoCleanup()
	// loadMensagensPendentesFromDB()
	defer cleanup() // Fecha conexões, salva estado, etc.
	// Captura sinais de interrupção (Ctrl+C)
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalCh
		cleanup()
		os.Exit(0)
	}()
	loadMessagesReceiveds()
	saveMessagesReceived()
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Panic detectado:", r)
			// Cleanup de emergência
		}
	}()
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
	var a string

	r.Use(cors.New())
	r.Use(pprof.New())
	r.Use(requestLogger)
	// r.LoadHTMLGlob("templates/*.html")
	r.Get("/:a", func(c *fiber.Ctx) error {
		if a == "" {
			a = utils.CopyString(c.Params("a"))
		}
		log.Println(c.Params("a"), a)
		return c.SendString(a)
	})
	r.Post("/stopRequest", func(c *fiber.Ctx) error {
		clientId := utils.CopyString(c.FormValue("clientId"))
		stoppedQrCodeRequests.Store(clientId, int32(1))
		return c.Status(200).JSON(fiber.Map{
			"message": "Cliente Pausado",
		})
	})
	r.Post("/verifyConnection", func(c *fiber.Ctx) error {
		clientId := utils.CopyString(c.FormValue("clientId"))
		client := getClient(clientId)
		if client == nil {
			client = tryConnecting(clientId)
			if client == nil {
				// removeClientDB(clientId, nil)
				return c.Status(500).JSON(fiber.Map{
					"message": "Cliente não conectado",
				})
			}
		}
		if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
			return c.Status(500).JSON(fiber.Map{
				"message": "Cliente não conectado",
			})
		}
		return c.Status(200).JSON(fiber.Map{
			"message": "Cliente conectado",
		})
	})
	r.Post("/deleteMessage", func(c *fiber.Ctx) error {
		clientId := utils.CopyString(c.FormValue("clientId"))
		messageID := c.FormValue("messageID")
		receiverNumber := c.FormValue("receiverNumber")
		client := getClient(clientId)
		if client == nil {
			client = tryConnecting(clientId)
			if client == nil {
				return fmt.Errorf("cliente não disponível")
			}
		}
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
		clientId := utils.CopyString(c.FormValue("clientId"))
		desconctarCliente(clientId)
		return c.Status(200).JSON(fiber.Map{
			"message": "Cliente desconectado",
		})
	})
	r.Get("/", func(c *fiber.Ctx) error {
		return c.SendFile("./templates/index.html")
	})
	r.Post("/sendFiles", func(c *fiber.Ctx) error {
		clientId := utils.CopyString(c.FormValue("clientId"))
		log.Printf("------------------ %s Send Files Request ------------------------ \n\n", clientId)
		client := getClient(clientId)
		if client == nil {
			client = tryConnecting(clientId)
			if client == nil {
				return c.Status(500).JSON(fiber.Map{
					"message": "Cliente não conectado",
				})
			}
		}
		if client == nil {
			return c.Status(500).JSON(fiber.Map{
				"message": "Cliente não conectado",
			})
		}
		noTimeout := utils.CopyString(c.FormValue("noTimeout"))
		sendContact := utils.CopyString(c.FormValue("contact"))
		infoObjects := utils.CopyString(c.FormValue("infoObjects"))
		dataProgramada := utils.CopyString(c.FormValue("dataProgramada"))
		if dataProgramada != "" {
			layout := "2006-01-02 15:04:05"
			t, err := time.Parse(layout, dataProgramada)
			if err != nil {
				fmt.Println("Erro ao converter data:", t, err)
				return c.Status(500).JSON(fiber.Map{
					"message": "Data Inválida",
				})
			}
		}
		documento_padrao_filePath := ""
		files_filePath := ""
		var documento_padrao *multipart.FileHeader = nil
		documento_padrao, err = c.FormFile("documento_padrao")
		if err != nil {
			fmt.Println("Nenhum documento padrão enviado.", err)
		}
		if documento_padrao != nil {
			savePath := "./uploads/" + clientId + documento_padrao.Filename
			if dataProgramada != "" {
				savePath = normalizeFileName("./arquivos_disparos_programados/padrao_" + dataProgramada + clientId + documento_padrao.Filename)
				documento_padrao_filePath = savePath
			}
			if err := c.SaveFile(documento_padrao, savePath); err != nil {
				fmt.Printf("Erro ao salvar o arquivo: %v", err)
			}
			if strings.Contains(savePath, ".webp") {
				errorconvert := convertWebPToJPEG(savePath, strings.Replace(savePath, ".webp", ".jpeg", -1))
				if errorconvert == nil {
					defer os.Remove("./uploads/" + clientId + documento_padrao.Filename)
					documento_padrao.Filename = strings.Replace(documento_padrao.Filename, ".webp", ".jpeg", -1)
				}
			}
		}
		var files *multipart.FileHeader = nil
		files, _ = c.FormFile("file")
		var result []map[string]interface{}
		// Deserializando o JSON para o map
		err = json.Unmarshal([]byte(infoObjects), &result)
		if err != nil {
			fmt.Printf("Erro ao converter JSON: %v", err)
		}
		for i := range result {
			result[i]["clientId"] = clientId
		}
		if dataProgramada != "" {
			if files != nil {
				savePath := normalizeFileName("./arquivos_disparos_programados/zip_" + dataProgramada + clientId + files.Filename)
				files_filePath = savePath
				if err := c.SaveFile(files, savePath); err != nil {
					fmt.Printf("Erro ao salvar o arquivo files para disparo Futuros: %v", err)
				}
			}
			addTarefaProgramadaDB(clientId, dataProgramada, infoObjects, documento_padrao_filePath, files_filePath)
			return c.Status(200).JSON(fiber.Map{
				"message": "Disparo agendado com sucesso",
			})
		}
		log.Printf("ClientId antes da goroutine: %s", clientId)
		go processarGrupoMensagens(sendMessageInfo{clientId,
			result,
			documento_padrao,
			files,
			sendContact,
			noTimeout,
			dataProgramada,
			infoObjects, 0, clientId + uuid.New().String()})
		log.Printf("ClientId dentro da goroutine: %s", clientId)
		return c.Status(200).JSON(fiber.Map{
			"message": "Arquivo recebido e enviado no WhatsApp.",
		})
	})
	r.Post("/getQRCode", func(c *fiber.Ctx) error {

		// Recupera o corpo da requisição e faz a bind para a estrutura de dados
		sendEmail := utils.CopyString(c.FormValue("notifyEmail"))
		clientId := utils.CopyString(c.FormValue("clientId"))
		stoppedQrCodeRequests.Store(clientId, int32(0))
		repeats.Store(clientId, int32(0))
		fmt.Printf("Gerando QR Code para o cliente '%s'\n", clientId)

		clientsMutex.Lock()
		if clientMap[clientId] != nil && clientMap[clientId].IsConnected() && clientMap[clientId].IsLoggedIn() {
			clientsMutex.Unlock() // Libera o mutex antes de retornar a resposta
			return c.Status(200).JSON(fiber.Map{
				"message": "Cliente já autenticado",
			})
		}
		clientsMutex.Unlock() // Libera o mutex após verificar o clientId
		qrCode := c.FormValue("qrCode") == "true"
		dbLog := waLog.Stdout("Database", "INFO", true)
		container, err := sqlstore.New("sqlite3", "file:./clients_db/"+clientId+".db?_foreign_keys=on", dbLog)
		if err != nil {
			fmt.Println(err)
		}
		deviceStore, err := container.GetFirstDevice()
		if err != nil {
			fmt.Println(err)
		}
		if strings.Contains(clientId, "_chat") {
			store.DeviceProps = &waCompanionReg.DeviceProps{Os: proto.String("Shark Business(ChatBot)")}
		} else if strings.Contains(clientId, "_shark") {
			store.DeviceProps = &waCompanionReg.DeviceProps{Os: proto.String("Shark Business")}
		}
		clientLog := waLog.Stdout("Client", "ERROR", true)
		client := whatsmeow.NewClient(deviceStore, clientLog)
		client.EnableAutoReconnect = true
		client.AddEventHandler(func(evt interface{}) {
			switch v := evt.(type) {
			case *events.Connected:
				stoppedQrCodeRequests.Store(clientId, int32(1))
				clientsMutex.Lock()
				clientMap[clientId] = client
				clientsMutex.Unlock()
				fmt.Println("🎉 -> CLIENTE CONECTADO", clientId)
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
				if sendEmail != "" {
					getOrSetEmails("INSERT INTO emails_conexoes ( `clientId`, `email`) VALUES (?,?);", []any{clientId, "luisgustavo20061@gmail.com"})
				}
				sendToEndPoint(data, baseURL)
			case *events.Disconnected:
				log.Printf("🔄 -> RECONECTANDO CLIENTE %s", clientId)
			case *events.Receipt:
				if strings.Contains(clientId, "chat") {
					handleSeenMessage(v, clientId)
				}
			case *events.LoggedOut:
				desconctarCliente(clientId)
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
			go func(clientIdCopy string) {
				actual, _ := repeats.LoadOrStore(clientIdCopy, new(int32))
				counterPtr := actual.(int32)
				atomic.StoreInt32(&counterPtr, int32(1)) // Define o valor inicial como 1
				for evt := range qrChan {
					stoppedActual, _ := stoppedQrCodeRequests.LoadOrStore(clientIdCopy, new(int32))
					stoppedPtr := stoppedActual.(int32)
					if atomic.LoadInt32(&stoppedPtr) == 1 {
						repeats.Store(clientIdCopy, int32(5))
						fmt.Printf("Cliente %s pausado", clientIdCopy)
						return
					}
					if evt.Event == "code" {
						// Gerar o QR Code como imagem PNG
						fmt.Println("GERANDO QRCODE --------")
						// Retorne a imagem PNG gerada como resposta
						if qrCode {
							png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256) // Tamanho: 256x256 pixels
							if err != nil {
								fmt.Printf("Erro ao gerar QR Code: %v", err)
							}
							// Converter para Base64
							base64Img := base64.StdEncoding.EncodeToString(png)

							// Criar o data URL
							dataURL := fmt.Sprintf("data:image/png;base64,%s", base64Img)

							data := map[string]any{
								"evento":   evento,
								"clientId": clientIdCopy,
								"data":     dataURL,
							}
							lastIndex := strings.LastIndex(clientIdCopy, "_")
							sufixo := clientIdCopy[lastIndex+1:]
							baseURL := mapOficial[sufixo]
							sendToEndPoint(data, baseURL)

						} else {
							data := map[string]any{
								"evento":   evento,
								"clientId": clientIdCopy,
								"data":     evt.Code,
							}
							lastIndex := strings.LastIndex(clientIdCopy, "_")
							sufixo := clientIdCopy[lastIndex+1:]
							baseURL := mapOficial[sufixo]
							sendToEndPoint(data, baseURL)
						}
						currentRepeat := atomic.AddInt32(&counterPtr, int32(1))
						if currentRepeat >= 5 {
							// desconectar
							fmt.Println("Tentativas de login excedidas")
							desconctarCliente(clientIdCopy)
							return
						}
						fmt.Printf("Tentativa %d de 5 do cliente %s\n", currentRepeat, clientIdCopy)

					} else if evt.Event == "success" {
						fmt.Println("-------------------AUTENTICADO")
						return
					}
				}
			}(clientId)

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
				sendToEndPoint(data, baseURL)
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
				sendToEndPoint(data, baseURL)
				return c.Status(200).JSON(fiber.Map{
					"qrCode": firstQRCode.Code,
				})
			}
		} else {
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
	r.Hooks().OnListen(func(listenData fiber.ListenData) error {
		gerarTarefasProgramadas()
		return nil
	})
	fmt.Println("⏳ Iniciando servidor...", PORT)
	r.Listen(":" + PORT)
}
func getOrSetEmails(query string, args []any) *sql.Rows {
	db, err := sql.Open("sqlite3", "./manager.db")
	if err != nil {
		log.Println("ERRO AO ADD TAREFA DB", err)
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS emails_conexoes (
		clientId TEXT,
		email TEXT
	);`
	_, err = db.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	if strings.Contains(query, "INSERT") {
		db.Exec(query, args...)
		rows := &sql.Rows{}
		return rows
	}
	rows, _ := db.Query(query, args...)
	// for rows.Next() {
	// 	var clientId string
	// 	var email string
	// 	if err := rows.Scan(&clientId, &email); err != nil {
	// 		log.Fatal(err)
	// 	}
	// 	fmt.Println("AAAA ->", email, clientId)
	// }
	defer db.Close()
	return rows
}
func sendToEmail(target string, text string) {
	fmt.Println("Enviando email de desconexão para", target)
	from := "oisharkbusiness@gmail.com"
	pass := "ucjj iway qetc ftvv "
	to := target

	msg := "From: " + from + "\n" +
		"To: " + to + "\n" +
		"Subject: Conexão Perdida !\n\n" +
		text

	err := smtp.SendMail("smtp.gmail.com:587",
		smtp.PlainAuth("", from, pass, "smtp.gmail.com"),
		from, []string{to}, []byte(msg))

	if err != nil {
		log.Printf("smtp error: %s", err)
		return
	}

	log.Print("sent, visit http://foobarbazz.mailinator.com")
}
func sendEmailDisconnection(clientId string) {
	rows := getOrSetEmails("SELECT * FROM emails_conexoes WHERE clientId = ?", []any{clientId})
	fmt.Println()
	defer rows.Close()
	for rows.Next() {
		var clientId string
		var email string
		if err := rows.Scan(&clientId, &email); err != nil {
			log.Fatal(err)
		}
		sendToEmail(email, "Sua conexão no whatsapp foi perdida, acesse o Shark Business e verifique ")
	}
}
func desconctarCliente(clientId string) bool {
	fmt.Println("⛔ -> CLIENTE DESCONECTADO", clientId)
	sendEmailDisconnection(clientId)
	client := getClient(clientId)
	if client == nil {
		// Reconecta sob demanda
		client = tryConnecting(clientId)
		if client == nil {
			fmt.Println("cliente não disponível")
			return false
		}
	}
	if client != nil {
		clientsMutex.Lock()
		defer clientsMutex.Unlock()
		client.Logout()
	}
	data := map[string]any{
		"evento":   "CLIENTE_DESCONECTADO",
		"clientId": clientId,
		"data":     "CLIENTE_DESCONECTADO",
	}
	delete(clientMap, clientId)
	lastIndex := strings.LastIndex(clientId, "_")
	sufixo := clientId[lastIndex+1:]
	baseURL := mapOficial[sufixo]
	sendToEndPoint(data, baseURL)
	return true
}
func convertWebPToJPEG(inputPath, outputPath string) error {
	// Abre o arquivo WebP
	file, err := os.Open(inputPath)
	if err != nil {
		fmt.Println("Erro abrindo a imagem:", err)
		return err
	}
	defer file.Close()
	// Decodifica a imagem WebP
	img, _, err := image.Decode(file)
	if err != nil {
		fmt.Println("Erro ao decodificar imagem WebP:", err)
		return err
	}
	// Cria o arquivo de saída JPEG
	outFile, err := os.Create(outputPath)
	if err != nil {
		fmt.Println("Erro criando arquivo de saída:", err)
		return err
	}
	defer outFile.Close()
	// Codifica a imagem como JPEG e salva no arquivo
	err = jpeg.Encode(outFile, img, &jpeg.Options{Quality: 90})
	if err != nil {
		fmt.Println("Erro ao converter para JPEG:", err)
		return err
	}
	return nil
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
	file.Seek(0, 0)
	contentBuf := bytes.NewBuffer(nil)
	if _, err := contentBuf.ReadFrom(file); err != nil {
		fmt.Printf("Erro ao ler o arquivo completo: %v", err)
	}
	mensagem_ := proto.Clone(message).(*waE2E.Message)
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
	return mensagem_
}
func formatPhoneNumber(phone string) string {
	// Remove caracteres não numéricos
	re := regexp.MustCompile(`\D`)
	phone = re.ReplaceAllString(phone, "")

	// Verifica se o número tem o código do país correto
	if len(phone) < 12 || len(phone) > 13 {
		return "Número inválido"
	}

	// Adiciona o "+"
	formatted := "+" + phone[:2] + " " + phone[2:4] + " "

	// Verifica se é um número de celular (tem o nono dígito)
	if len(phone) == 13 {
		formatted += phone[4:5] + " " + phone[5:9] + "-" + phone[9:]
	} else {
		formatted += phone[4:8] + "-" + phone[8:]
	}

	return formatted
}
