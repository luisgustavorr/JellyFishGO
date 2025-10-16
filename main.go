package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"io"
	"jellyFish/modules"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"syscall"
	"unicode"

	"mime/multipart"
	"sync"
	"time"

	"math/rand"

	_ "net/http/pprof"

	_ "github.com/go-sql-driver/mysql"
	json "github.com/goccy/go-json"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/cors"
	"github.com/gofiber/fiber/v2/middleware/pprof"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/google/uuid"
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
	"golang.org/x/net/proxy"
	"golang.org/x/time/rate"
	"google.golang.org/protobuf/proto"
)

var (
	clientMap    = make(map[string]*whatsmeow.Client)
	clientsMutex sync.RWMutex // Mutex simples
)
var focusedMessagesKeys = []string{}
var _ = godotenv.Load()
var jsonFast = jsoniter.ConfigCompatibleWithStandardLibrary
var groupPicCache sync.Map // Thread-safe
// Pega foto de perfil do grupo
func getGroupProfilePicture(client *whatsmeow.Client, groupJID types.JID) *types.ProfilePictureInfo {
	if cached, ok := groupPicCache.Load(groupJID); ok {
		return cached.(*types.ProfilePictureInfo)
	}
	pictureInfo, err := client.GetProfilePictureInfo(groupJID, nil)
	if err != nil {
		fmt.Printf("Erro ao obter foto do grupo: %v \n", err)
		return nil
	}
	groupPicCache.Store(groupJID, pictureInfo)
	time.AfterFunc(30*time.Minute, func() {
		groupPicCache.Delete(groupJID)
	})
	return pictureInfo
}

type SeenMessage struct {
	Chat      string `json:"chat"`
	IdMessage string `json:"idMessage"`
	Lida      bool   `json:"lida"`
}

type SeenMessagesQueue struct {
	messageBuffer  map[string][]SeenMessage
	messageTimeout map[string]*time.Timer
}

// Cria nova fila de mensagens vistas
func NewSeenQueue() *SeenMessagesQueue {
	return &SeenMessagesQueue{
		messageBuffer:  make(map[string][]SeenMessage, 3),
		messageTimeout: make(map[string]*time.Timer, 1),
	}
}

type MessagesQueue struct {
	bufferLock     sync.Mutex
	messageBuffer  map[string][]Envelope
	messageTimeout map[string]*time.Timer
}

// Cria nova fila de mensagens recebidas
func NewQueue() *MessagesQueue {
	return &MessagesQueue{
		messageBuffer:  make(map[string][]Envelope, 5),
		messageTimeout: make(map[string]*time.Timer, 1),
	}
}

// Adicionar mensagem para fila de envio
func (c *MessagesQueue) AddMessage(clientID string, message Envelope, number string) {
	c.bufferLock.Lock()
	defer c.bufferLock.Unlock()
	compositeKey := clientID + "_" + number
	if _, exists := c.messageBuffer[compositeKey]; !exists {
		c.messageBuffer[compositeKey] = []Envelope{}
	}
	c.messageBuffer[compositeKey] = append(c.messageBuffer[compositeKey], message)
	if timer, exists := c.messageTimeout[compositeKey]; exists {
		timer.Stop()
	}
	messageCount := len(c.messageBuffer[compositeKey])
	if len(c.messageBuffer[compositeKey]) >= 5 {
		go c.ProcessMessages(clientID, number)
		fmt.Printf("⏳ -> ENVIANDO %d MENSAGENS ANTES DO TIMER DO CLIENTE %s \n", messageCount, clientID)
		return
	}
	timerBetweenMessage := -0.15*float64(messageCount)*float64(messageCount) + 0.5*float64(messageCount) + 7
	if timerBetweenMessage < 0 {
		timerBetweenMessage = 0.001
	}
	fmt.Printf("⏳ -> ESPERANDO %.3f SEGUNDOS PARA %d MENSAGENS DO CLIENTE %s \n", timerBetweenMessage, messageCount, clientID)

	timerDuration := time.Duration(timerBetweenMessage * float64(time.Second))
	c.messageTimeout[compositeKey] = time.AfterFunc(timerDuration, func(currentClientID string) func() {
		return func() {
			c.ProcessMessages(currentClientID, number)
		}
	}(clientID)) // <--- clientID é capturado como valor aqui!
}

// Processar mensagens recebida
func (c *MessagesQueue) ProcessMessages(clientID string, number string) {
	c.bufferLock.Lock()
	defer c.bufferLock.Unlock()
	compositeKey := clientID + "_" + number
	messages := c.messageBuffer[compositeKey]
	if messages == nil {
		return
	}
	fmt.Printf("📦 -> ENVIANDO LOTES DE %d MENSAGENS DO %s\n", len(messages), clientID)
	lastIndex := strings.LastIndex(clientID, "_")
	sufixo := clientID[lastIndex+1:]
	baseURL := strings.Split(modules.MapOficial[sufixo], "chatbot")[0]
	if strings.Contains(baseURL, "disparo") {
		baseURL = strings.Split(modules.MapOficial[sufixo], "disparo")[0]
	}
	data := EnvelopePayload{
		Evento:   "MENSAGEM_RECEBIDA",
		Sender:   2,
		ClientID: clientID,
		Data:     messages,
	}
	sendEnvelopeToEndPoint(data, baseURL+"chatbot/chat/mensagens/novas-mensagens/", "")

	if timer, exists := c.messageTimeout[compositeKey]; exists {
		timer.Stop()
		delete(c.messageTimeout, compositeKey)
	}
	delete(c.messageBuffer, compositeKey)
	modules.LogMemUsage()
}

// Carregar configuração inicial

// Pegar Token CSRFT

// Enviar envelope de mensagens para o end point
var retryEnvelope = map[string]int{}

func sendEnvelopeToEndPoint(data EnvelopePayload, url string, retryToken string) {
	buf := modules.JsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer modules.JsonBufferPool.Put(buf)
	err := jsonFast.NewEncoder(buf).Encode(data)
	if err != nil {
		fmt.Printf("Erro ao criar marshal: %v", err)

		return
	}
	// fmt.Println("Envelope sendo envidada :", string(jsonData))
	if url == "" {
		fmt.Printf("URL %s vazia", url)
		return
	}
	req, err := http.NewRequest("POST", url, buf)
	if err != nil {
		fmt.Printf("Erro ao criar a requisição: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", os.Getenv("STRING_AUTH"))
	req.Header.Set("X-CSRFToken", modules.GetCSRFToken())
	client := &http.Client{}
	resp, err := client.Do(req)

	if err != nil {
		envelopeToken := retryToken
		if envelopeToken == "" {
			envelopeToken = uuid.New().String()
		}
		var dataToLog = EnvelopePayload{
			Sender:   data.Sender,
			Evento:   data.Evento,
			ClientID: data.ClientID,
		}
		for _, v := range data.Data {
			nonMediaData := v
			nonMediaData.Mensagem.Attrs.Media = "media_here"
			nonMediaData.Mensagem.Attrs.Audio = "audio_here"

			dataToLog.Data = append(dataToLog.Data, nonMediaData)
		}

		if retryEnvelope[envelopeToken] > 3 {
			b, _ := json.MarshalIndent(dataToLog, "", "  ")
			body := []byte{}
			if resp != nil {
				body, err = io.ReadAll(resp.Body)
				if err != nil {
					return
				}
			}

			fmt.Println("Body ERROR Response :", string(body))
			fmt.Println("Payload:", string(b))
			fmt.Printf("Erro ao enviar a requisição do '%s' , tentativa (%d/%d): %v\n", data.ClientID, retryEnvelope[envelopeToken]+1, 5, err)
			delete(retryEnvelope, envelopeToken)
		} else {
			retryEnvelope[envelopeToken] += 1
			time.Sleep(5 * time.Second)
			sendEnvelopeToEndPoint(data, url, envelopeToken)
		}
		return
	}
	defer resp.Body.Close()
	go func(data EnvelopePayload) {
		tempoParaVer := randomBetweenf(1, 4)
		fmt.Println("Esperando tempo para visualizar !! ", tempoParaVer, " segundos")
		whatsClient := getClient(data.ClientID)
		time.Sleep(time.Duration(tempoParaVer) * time.Second)
		validNumber, err := checkNumberWithRetry(whatsClient, data.Data[0].Mensagem.Number, data.Data[0].Mensagem.IDGrupo != "", data.ClientID)
		if err != nil {
			fmt.Println("RRO NO GET CLIENTE", err)
		} else {
			if len(validNumber) > 0 && validNumber[0].JID != types.EmptyJID {
				JID := validNumber[0].JID
				for _, messageToSee := range data.Data {
					var MessageID []types.MessageID = []types.MessageID{messageToSee.Mensagem.ID}
					whatsClient.MarkRead(MessageID, time.Now(), JID, JID, types.ReceiptTypeRead)
				}
			}

		}
	}(data)

	fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | evento : ", data.Evento, " | clientId :", data.ClientID)
	if resp.StatusCode != 200 {
		envelopeToken := retryToken
		if envelopeToken == "" {
			envelopeToken = uuid.New().String()
		}
		var dataToLog = EnvelopePayload{
			Sender:   data.Sender,
			Evento:   data.Evento,
			ClientID: data.ClientID,
		}
		for _, v := range data.Data {
			nonMediaData := v
			nonMediaData.Mensagem.Attrs.Media = "media_here"
			nonMediaData.Mensagem.Attrs.Audio = "audio_here"
			dataToLog.Data = append(dataToLog.Data, nonMediaData)
		}

		if retryEnvelope[envelopeToken] > 3 {
			b, _ := json.MarshalIndent(dataToLog, "", "  ")
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				return
			}
			fmt.Println("Body ERROR Response :", string(body))
			fmt.Println("Payload:", string(b))

			fmt.Printf("Erro ao enviar a requisição do '%s' , tentativa (%d/%d): %v\n", data.ClientID, retryEnvelope[envelopeToken]+1, 5, err)
			delete(retryEnvelope, envelopeToken)
		} else {
			retryEnvelope[envelopeToken] += 1
			time.Sleep(5 * time.Second)
			sendEnvelopeToEndPoint(data, url, envelopeToken)
		}
	}
}

// Enviar payload genérica

// Recupera texto da mensagem recebida
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
		text = message.ReactionMessage.GetText()
	}
	if text == "" {
		text = message.GetProtocolMessage().GetEditedMessage().GetConversation()
	}
	return text
}

var bufPool = sync.Pool{
	New: func() any {
		// 1MB de buffer inicial, ajusta conforme necessário
		s := make([]byte, 0, 300<<10) // 300kb
		return &s
	},
}

// Recupera arquivos recebidos pela mensagem
func getMedia(ctx context.Context, evt *events.Message, clientId string) (string, string) {
	bufPtr := bufPool.Get().(*[]byte)
	buf := *bufPtr
	defer func() {
		*bufPtr = buf[:0]
		bufPool.Put(bufPtr)
	}()
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
	var err error
	if imgMsg := evt.Message.GetImageMessage(); imgMsg != nil {
		mimeType = imgMsg.GetMimetype()
		mediaMessage := imgMsg

		buf, err = client.Download(ctx, mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a imagem: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(buf)
		return base64Data, mimeType
	}
	if vidMsg := evt.Message.GetVideoMessage(); vidMsg != nil {
		mimeType = vidMsg.GetMimetype()
		mediaMessage := vidMsg
		buf, err := client.Download(ctx, mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a video: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(buf)
		return base64Data, mimeType
	}
	if audioMsg := evt.Message.GetAudioMessage(); audioMsg != nil {
		mimeType = audioMsg.GetMimetype()
		mediaMessage := audioMsg
		buf, err := client.Download(ctx, mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a audio: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(buf)
		return base64Data, mimeType
	}
	if stickerMsg := evt.Message.GetStickerMessage(); stickerMsg != nil {
		mimeType = stickerMsg.GetMimetype()
		mediaMessage := stickerMsg
		buf, err := client.Download(ctx, mediaMessage)
		if err != nil {
			fmt.Printf("Erro ao baixar a sticker: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(buf)
		return base64Data, mimeType
	}
	if docMsg := evt.Message.GetDocumentMessage(); docMsg != nil {
		mimeType = docMsg.GetMimetype()

		if docMsg.FileName != nil && strings.Contains(*docMsg.FileName, ".cdr") {
			mimeType = "application/vnd.corel-draw"
			docMsg.Mimetype = &mimeType
		}

		if docMsg.FileName != nil {
			fmt.Printf("Document title: %s\n", *docMsg.FileName)
		}

		buf, err := client.Download(ctx, docMsg)
		if err != nil {
			fmt.Printf("Erro ao baixar a doc: %v", err)
		}
		base64Data := base64.StdEncoding.EncodeToString(buf)
		return base64Data, mimeType
	} else {
		return "", ""
	}
}

// Recupera quem enviou a mensagem
func getSender(senderNumber string) string {
	parts := strings.SplitN(senderNumber, "@", 2) // Mais eficiente que Split
	return parts[0]
}

var messagesQueue = NewQueue()

// Loga as informações de requisições
func requestLogger(c *fiber.Ctx) error {
	start := time.Now()
	method := c.Method()
	path := c.Path()
	clientId := utils.CopyString(c.FormValue("clientId"))
	err := c.Next()
	duration := time.Since(start)
	fmt.Printf(" [%s] %s | Tempo: %v | ClientId: %s\n", method, path, duration, clientId)
	return err
}

// Recupera focus da mensagem
func getMessageFocus(arr []string, id_message string) string {
	for _, v := range arr {
		if strings.Contains(v, "_"+id_message) {
			return strings.Split(v, "_")[0]
		}
	}
	return ""
}

// Remove string
func removeString(slice []string, value string) []string {
	filtered := []string{}
	for _, v := range slice {
		if v != value { // Mantém apenas os valores diferentes
			filtered = append(filtered, v)
		}
	}
	return filtered
}

type EnvelopePayload struct {
	Data     []Envelope `json:"data,omitempty"`
	Evento   string     `json:"evento,omitempty"`
	ClientID string     `json:"clientId,omitempty"`
	Sender   int        `json:"sender,omitempty"`
}
type MessageAttrs struct {
	FileType      string         `json:"file_type,omitempty"`
	FileName      string         `json:"file_name,omitempty"`
	Media         string         `json:"media,omitempty"`
	Audio         string         `json:"audio,omitempty"`
	Edited        int            `json:"edited,omitempty"`
	QuotedMessage *QuotedMessage `json:"quotedMessage,omitempty"`
	Contact       *ContactInfo   `json:"contact,omitempty"`
}
type QuotedMessage struct {
	SenderName    string `json:"senderName"`
	MessageQuoted string `json:"messageQuoted"`
	MessageID     string `json:"messageID"`
	Sender        int    `json:"sender"`
}

type ContactInfo struct {
	Contato string `json:"contato"`
	Nome    string `json:"nome"`
}

type MessagePayload struct {
	Attrs     MessageAttrs `json:"attrs"`
	ID        string       `json:"id"`
	Sender    string       `json:"sender"`
	Number    string       `json:"number"`
	Text      string       `json:"text"`
	Focus     string       `json:"focus,omitempty"`
	IDGrupo   string       `json:"id_grupo,omitempty"`
	NomeGrupo string       `json:"nome_grupo,omitempty"`
	ImgGrupo  string       `json:"imagem_grupo,omitempty"`
	PerfilImg string       `json:"perfil_image,omitempty"`
	Timestamp int64        `json:"timestamp"`
}

type Envelope struct {
	Mensagem MessagePayload `json:"mensagem"`
}

// Lida com mensagens recebidas
func handleMessage(fullInfoMessage *events.Message, clientId string, client *whatsmeow.Client) bool {
	if fullInfoMessage == nil {
		log.Println("Mensagem recebida é nil")
		return false
	}

	if client == nil {
		log.Println("Cliente Whatsmeow é nil")
		return false
	}
	// infoINJSON, _ := json.Marshal(fullInfoMessage)
	// fmt.Println("INFOS RECEBIDAS", string(infoINJSON))
	// Broadcast (linha de transmissão)
	var isBroadcast bool = fullInfoMessage.SourceWebMsg.GetBroadcast()
	chatID := fullInfoMessage.Info.Chat.String()
	// Status (mensagem no status)
	var isStatus bool = strings.Contains(chatID, "status")

	// Enquete (vários tipos)
	var isPoll bool = fullInfoMessage.Message.GetPollUpdateMessage() != nil ||
		fullInfoMessage.Message.GetPollCreationMessage() != nil ||
		fullInfoMessage.Message.GetPollCreationMessageV2() != nil ||
		fullInfoMessage.Message.GetPollCreationMessageV3() != nil ||
		fullInfoMessage.Message.GetPollCreationMessageV4() != nil ||
		fullInfoMessage.Message.GetPollCreationMessageV5() != nil

	// Localização
	var isLocation bool = fullInfoMessage.Message.LocationMessage != nil
	// Comunidade (geralmente terminam com "@g.us" e possuem 'IsCommunityAnnounceMsg')
	// 120363167775174375@newsletter

	var isNewsLetter bool = strings.HasSuffix(chatID, "@newsletter") || fullInfoMessage.NewsletterMeta != nil
	var isCommunityAnnounce bool = fullInfoMessage.Info.Multicast && strings.HasSuffix(chatID, "@g.us")
	// Mensagem de protocolo (ex: deletada, chamada, etc.)
	var isIgnoredProtocolMsg bool

	if fullInfoMessage.Message.ProtocolMessage != nil {
		switch fullInfoMessage.Message.ProtocolMessage.GetType() {
		case waE2E.ProtocolMessage_REVOKE, waE2E.ProtocolMessage_EPHEMERAL_SETTING:
			isIgnoredProtocolMsg = true
		default:
			isIgnoredProtocolMsg = false
		}
	}

	// Outro tipo inesperado? Mensagem sem corpo (?)
	// var isEmptyMessage bool = fullInfoMessage.Message == nil

	if isBroadcast || isStatus || isPoll || isLocation || isCommunityAnnounce || isIgnoredProtocolMsg || isNewsLetter {
		fmt.Printf("Ignorando mensagem chatID : '%s' do tipo especial: isBroadcast ? %t | isStatus ? %t | isPoll ? %t | isLocation ? %t | isCommunityAnnounce ? %t | isProtocolMsg ? %t | isNewsLetter ? %t\n", chatID, isBroadcast, isStatus, isPoll, isLocation, isCommunityAnnounce, isIgnoredProtocolMsg, isNewsLetter)
		return false
	}
	var contactMessage *waE2E.ContactMessage = fullInfoMessage.Message.GetContactMessage()
	var contactMessageArray *waE2E.ContactsArrayMessage = fullInfoMessage.Message.GetContactsArrayMessage()
	message := fullInfoMessage.Message

	var groupMessage bool = strings.Contains(fullInfoMessage.Info.Chat.String(), "@g.us")
	var contextInfo = message.ExtendedTextMessage.GetContextInfo()
	var text string = getText(message)

	var senderName string = fullInfoMessage.Info.PushName
	var fromMe = fullInfoMessage.Info.IsFromMe
	ctx := context.Background()
	sender := fullInfoMessage.Info.Sender
	var senderNumber string = getSender(sender.User)

	if !fromMe {

		if sender.Server == "lid" {

			pn, err := client.Store.LIDs.GetPNForLID(ctx, sender)
			if err != nil {
				client.Log.Warnf("Failed to get LID for %s: %v", sender, err)
			} else if !pn.IsEmpty() {
				fullInfoMessage.Info.Sender = pn
				senderNumber = getSender(pn.User)
			}
		}
		// fmt.Println("📩 -> Mensagem RECEBIDA TEMPORARIO LOG:", senderName, senderNumber, clientId, text, fullInfoMessage.Info.Sender, " | By Group:", groupMessage)
	}
	var fileName = ""
	if message.DocumentMessage != nil {
		fileName = *message.DocumentMessage.FileName
		fmt.Println("File name encontrado : ", fileName)

	}
	var id_message string = fullInfoMessage.Info.ID
	var editedInfo = message.GetProtocolMessage().GetKey().GetID()
	timestamp := fullInfoMessage.Info.Timestamp.Unix()
	var quotedMessageID string = contextInfo.GetStanzaID()
	media, fileType := getMedia(ctx, fullInfoMessage, clientId)
	edited := 0

	JID := sender.ToNonAD()
	if fromMe {
		JID = client.Store.GetJID().ToNonAD()
		senderNumber = fullInfoMessage.Info.Chat.User
	}
	params := &whatsmeow.GetProfilePictureParams{}
	profilePic, _ := client.GetProfilePictureInfo(JID, params)
	if editedInfo != "" {
		edited = 1
		id_message = editedInfo
	}
	var contactObject ContactInfo
	if contactMessage != nil {
		var name string = *contactMessage.DisplayName
		var vcard string = contactMessage.GetVcard()
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
			startIndex = strings.Index(vcard, "type=VOICE:")
			if startIndex != -1 {
				startIndex += len("type=VOICE:") // Pular "waid="
				endIndex := startIndex
				for endIndex < len(vcard) && unicode.IsDigit(rune(vcard[endIndex])) {
					endIndex++
				}
				name += " (" + vcard[startIndex:endIndex] + ")"
			}
			numero = "sem_whatsapp"
		}
		contactObject = ContactInfo{
			Contato: numero,
			Nome:    name,
		}
		fmt.Println(contactObject)
	}

	messageAttr := MessageAttrs{
		Edited: edited,
	}
	if quotedMessageID != "" {
		fmt.Println("ADICIONANDO QUOTE")
		var quoted string
		if message.ExtendedTextMessage != nil &&
			message.ExtendedTextMessage.ContextInfo != nil &&
			message.ExtendedTextMessage.ContextInfo.QuotedMessage != nil &&
			message.ExtendedTextMessage.ContextInfo.QuotedMessage.Conversation != nil {
			quoted = *message.ExtendedTextMessage.ContextInfo.QuotedMessage.Conversation
		} else {
			quoted = "" // ou outra lógica, como ignorar ou registrar
		}
		var quotedMessage = &QuotedMessage{
			Sender:        2,
			SenderName:    senderName,
			MessageID:     quotedMessageID,
			MessageQuoted: quoted,
		}
		messageAttr.QuotedMessage = quotedMessage
	}
	if contactObject.Nome != "" && contactObject.Contato != "" {
		messageAttr.Contact = &contactObject
	}
	if media != "" {
		messageAttr.FileType = fileType
		if strings.Contains(fileType, "audio") {
			messageAttr.Audio = media
		} else {
			if fileName != "" {
				messageAttr.FileName = fileName
			}
			messageAttr.Media = media
		}
	}
	mensagem := MessagePayload{
		ID:        id_message,
		Sender:    senderName,
		Number:    senderNumber,
		Text:      text,
		Timestamp: timestamp,
		Attrs:     messageAttr,
	}
	if groupMessage {
		groupJID := fullInfoMessage.Info.Chat
		groupInfo, _ := client.GetGroupInfo(groupJID)
		groupImage := getGroupProfilePicture(client, groupJID)
		if groupImage != nil {
			mensagem.ImgGrupo = groupImage.URL
		}
		if groupInfo != nil {
			mensagem.NomeGrupo = groupInfo.GroupName.Name
		}
		mensagem.IDGrupo = strings.Replace(fullInfoMessage.Info.Chat.String(), "@g.us", "", -1)
	}
	var focus = getMessageFocus(focusedMessagesKeys, id_message)
	if focus != "" {
		if focus == "noreply" {
			fmt.Println("Mensagem não deve ser enviada, focus 'noreply'")
			return false
		}
		fmt.Println("MENSAGEM FOCADA", focus)
		mensagem.Focus = focus
		focusedMessagesKeys = removeString(focusedMessagesKeys, focus+"_"+id_message)
	}
	if profilePic != nil {
		mensagem.PerfilImg = profilePic.URL
	}
	objetoMensagens := Envelope{Mensagem: mensagem}
	if fromMe {
		if media != "" || text != "" {
			listaMensagens := []Envelope{}
			fmt.Println("-> Mensagem ENVIADA PELO WHATSAPP:", id_message, senderName, senderNumber, text)
			listaMensagens = append(listaMensagens, objetoMensagens)
			data := EnvelopePayload{
				Evento:   "MENSAGEM_RECEBIDA",
				Sender:   1,
				ClientID: clientId,
				Data:     listaMensagens,
			}
			lastIndex := strings.LastIndex(clientId, "_")
			sufixo := clientId[lastIndex+1:]
			baseURL := strings.Split(modules.MapOficial[sufixo], "chatbot")[0]
			if strings.Contains(baseURL, "disparo") {
				baseURL = strings.Split(modules.MapOficial[sufixo], "disparo")[0]
			}

			sendEnvelopeToEndPoint(data, baseURL+"chatbot/chat/mensagens/novas-mensagens/", "")
		}
	} else {
		//save aqui
		modules.SaveNumberInCache(modules.SanitizeNumber(sender.User), sender.User, sender.Server, clientId, true)

		if media != "" || text != "" || contactMessage != nil || contactMessageArray != nil {
			if contactMessageArray != nil {
				for _, contactMessage := range contactMessageArray.Contacts {

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
						startIndex = strings.Index(vcard, "type=VOICE:")
						if startIndex != -1 {
							startIndex += len("type=VOICE:") // Pular "waid="
							endIndex := startIndex
							for endIndex < len(vcard) && unicode.IsDigit(rune(vcard[endIndex])) {
								endIndex++
							}
							name += " (" + vcard[startIndex:endIndex] + ")"

						}
						numero = "sem_whatsapp" + name
					}
					var uniqueMessageID string = strings.Replace(id_message+"_"+string(numero)+"_"+senderNumber+"_"+clientId, " ", "", -1)
					if idMessageJaEnviado(uniqueMessageID) && edited == 0 {
						fmt.Println("❌ -> Mensagem REPETIDA:", string(numero), id_message, senderName, senderNumber, clientId, text)
						fmt.Println("!--------------------->MENSAGEM COM ID JÁ ENVIADO<---------------------!")
						return false
					}
					if name != "" && numero != "" {
						fmt.Println("Adicionando ", name, numero)
						objetoMensagens.Mensagem.Attrs.Contact = &ContactInfo{
							Contato: numero,
							Nome:    name,
						}
					}

					novoObjetoMensagens := objetoMensagens
					novoObjetoMensagens.Mensagem.ID = novoObjetoMensagens.Mensagem.ID + "_" + numero
					fmt.Println(novoObjetoMensagens.Mensagem.Attrs)
					messagesQueue.AddMessage(clientId, novoObjetoMensagens, senderNumber)
					fmt.Printf("------------------ %s Receiving Message Event | By Group : %v | Is Media : %v ------------------------ \n\n", clientId, groupMessage, media != "")
					fmt.Println("📩 -> Mensagem RECEBIDA:", id_message, senderName, senderNumber, clientId, text, " | By Group:", groupMessage, "| Is Media :", media != "")
					saveIdEnviado(uniqueMessageID)
				}
			} else {
				var uniqueMessageID string = strings.Replace(id_message+"_"+senderNumber+"_"+clientId, " ", "", -1)
				if idMessageJaEnviado(uniqueMessageID) && edited == 0 {
					fmt.Println("❌ -> Mensagem REPETIDA:", id_message, senderName, senderNumber, clientId, text)
					fmt.Println("!--------------------->MENSAGEM COM ID JÁ ENVIADO<---------------------!")
					return false
				}
				// teste, _ := json.MarshalIndent(objetoMensagens.Mensagem, " ", "")
				// fmt.Println(string(teste))
				// var MessageID []types.MessageID = []types.MessageID{id_message}
				// go func(MessageID []types.MessageID) {
				// 	tempoParaVer := randomBetweenf(3, 15)
				// 	fmt.Println("Esperando tempo para visualizar !! ", tempoParaVer, " segundos")
				// 	time.Sleep(time.Duration(tempoParaVer) * time.Second)
				// 	client.MarkRead(MessageID, time.Now(), JID, JID, types.ReceiptTypeRead)
				// }(MessageID)

				messagesQueue.AddMessage(clientId, objetoMensagens, senderNumber)
				fmt.Printf("------------------ %s Receiving Message Event | By Group : %v | Is Media : %v ------------------------ \n\n", clientId, groupMessage, media != "")
				fmt.Println("📩 -> Mensagem RECEBIDA:", id_message, senderName, senderNumber, clientId, text, " | By Group:", groupMessage, "| Is Media :", media != "")
				saveIdEnviado(uniqueMessageID)
			}

		}
	}
	return true
}

// Safe panic
func safePanic(arguments ...any) {
	log.Println("Pânico controlado -> ", arguments)
	os.Exit(1)
}

// Conectar automaticamente os clientes pelo db liteSQL
func autoConnection() {
	dir := "./clients_db"
	files, err := os.ReadDir(dir)
	if err != nil {
		fmt.Printf("Erro ao ler clientes: %v", err)
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
	// modules.SearchNearMessages(modules.BasicActions{
	// 	GetClient: getClient, CheckNumberWithRetry: checkNumberWithRetry,
	// })
}

// Verifica com error Handling se o número está no WhatsApp
func isOnWhatsAppSafe(client *whatsmeow.Client, numbers []string) ([]types.IsOnWhatsAppResponse, error) {
	if client == nil || !client.IsConnected() || !client.IsLoggedIn() {
		return nil, fmt.Errorf("cliente não conectado")
	}
	return client.IsOnWhatsApp(numbers)
}

// Verifica se o número está no WhatsApp com tentativas
func checkNumberWithRetry(client *whatsmeow.Client, number string, de_grupo bool, clientId string) (resp []types.IsOnWhatsAppResponse, err error) {
	maxRetries := 3
	backoff := 1 * time.Second
	found, found_number, found_server := modules.FindNumberInCache(number)
	if found {
		fmt.Printf("# ✅ -> Número '%s' (asked as : '%s') achado no cache !! \n", found_number, number)
		if found_number == "" && found_server == "" {
			return []types.IsOnWhatsAppResponse{}, nil

		}
		return []types.IsOnWhatsAppResponse{
			{
				Query: "",
				IsIn:  true,
				JID:   types.JID{User: found_number, Server: found_server},
			},
		}, nil

	}
	fmt.Println("Resultado em cache :", found, number)
	for i := 0; i < maxRetries; i++ {
		responses, err := isOnWhatsAppSafe(client, []string{number})
		if err == nil && len(responses) > 0 {
			response := responses[0]
			if response.IsIn || de_grupo {
				modules.SaveNumberInCache(number, response.JID.User, response.JID.Server, clientId, true)

				return responses, nil
			}
		} else {
			modules.SaveNumberInCache(number, "", "", clientId, false)
			return []types.IsOnWhatsAppResponse{}, err

		}
		time.Sleep(backoff)
	}
	fmt.Println(len(number))
	if len(number) < 5 || len(number) == 13 {
		modules.SaveNumberInCache(number, "", "", clientId, false)
		return []types.IsOnWhatsAppResponse{}, fmt.Errorf("error : número pequeno/grande demais, inválido para segunda comparação")
	}
	numberWith9 := number[:4] + "9" + number[4:]
	fmt.Println("Tentando com 9 adicional", numberWith9)
	backoff = 1 * time.Second
	for i := 0; i < maxRetries; i++ {
		responses, err := isOnWhatsAppSafe(client, []string{numberWith9})
		if err == nil && len(responses) > 0 && responses[0].IsIn {
			modules.SaveNumberInCache(number, responses[0].JID.User, responses[0].JID.Server, clientId, true)
			return responses, nil
		} else {
			time.Sleep(backoff)
			backoff *= 2
			modules.SaveNumberInCache(number, "", "", clientId, false)

			return []types.IsOnWhatsAppResponse{}, err

		}

	}
	return []types.IsOnWhatsAppResponse{}, nil
}

// Tenta reconectar o WhatsApp do cliente
func tryConnecting(clientId string) *whatsmeow.Client {
	context := context.Background()

	dbLog := waLog.Stdout("Database", "INFO", true)
	container, err := sqlstore.New(context, "sqlite3", "file:./clients_db/"+clientId+".db?_foreign_keys=on", dbLog)
	if err != nil {
		fmt.Println(err)
		return nil
	}
	deviceStore, err := container.GetFirstDevice(context)
	if err != nil {
		fmt.Println("erro pegandoDevice", err)
		return nil
	}
	clientLog := waLog.Stdout("Client", "ERROR", true)

	client := whatsmeow.NewClient(deviceStore, clientLog)

	avaiableProxyServer, found := modules.GetServerByClientId(clientId)
	if !found {
		fmt.Printf("[PROXY] -> Não existem servidores disponíveis para o cliente : %s \n", clientId)
	} else {
		fmt.Printf("[PROXY] -> '%s' Conectado em '%s' (%d/%d) \n", clientId, avaiableProxyServer.Name, avaiableProxyServer.ActiveConns, avaiableProxyServer.MaxConns)
		dialer, err := proxy.SOCKS5("tcp", avaiableProxyServer.URL, &proxy.Auth{
			User:     avaiableProxyServer.User,
			Password: avaiableProxyServer.Password,
		}, proxy.Direct)
		if err != nil {
			panic(err)
		}
		conn, err := dialer.Dial("tcp", "api.ipify.org:443")
		if err != nil {
			fmt.Println("[PROXY] -> Proxy failed:", err)
		} else {
			fmt.Println("[PROXY] -> Proxy working, conn:", conn.RemoteAddr())
			conn.Close()
			client.SetSOCKSProxy(dialer, whatsmeow.SetProxyOptions{
				NoWebsocket: false,
				NoMedia:     false,
			})
		}

	}

	client.EnableAutoReconnect = true

	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Connected:
			clientsMutex.Lock()
			clientMap[clientId] = client
			clientsMutex.Unlock()
			fmt.Println("🎉 -> CLIENTE CONECTADO", clientId)
			UpdateMemoryLimit(len(clientMap))
			if strings.Contains(clientId, "chat") {
				modules.SetStatus(client, "conectado", types.JID{})
			}
		case *events.Receipt:
			if strings.Contains(clientId, "chat") {
				handleSeenMessage(v, clientId)
			}
		case *events.Disconnected:
			fmt.Printf("🔄 -> RECONECTANDO CLIENTE %s", clientId)
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
		if err != nil {
			fmt.Println("erro pegandoDevice", err)
		}
		return client

	}
}

// remove o DB do cliente
func removeClientDB(clientId string, container *sqlstore.Container) {
	if container != nil {
		container.Close()
	}
	err := os.Remove("./clients_db/" + clientId + ".db")
	if err != nil {
		fmt.Println("---- Erro excluindo arquivo de sessão :", err)
	}
}

// Recupera client pelo clientId
func getClient(clientId string) *whatsmeow.Client {
	clientsMutex.Lock()

	defer func() {

		clientsMutex.Unlock()
	}()

	if clientMap[clientId] == nil || !clientMap[clientId].IsConnected() || !clientMap[clientId].IsLoggedIn() {
		return nil
	}

	return clientMap[clientId] // Retorna nil se não existir
}

var (
	repeats               sync.Map // clientId -> *int32 (contador atômico)
	stoppedQrCodeRequests sync.Map // clientId -> *int32 (0 = false, 1 = true)
)

var (
	seenMessagesQueue      = NewSeenQueue()
	seenMessagesQueueMutex sync.Mutex
)

func sendSeenMessages(clientId string) {
	seenMessagesQueueMutex.Lock()
	defer seenMessagesQueueMutex.Unlock()
	data := modules.GenericPayload{
		Evento:   "MENSAGEM_LIDA",
		ClientID: clientId,
		Data:     seenMessagesQueue.messageBuffer[clientId],
	}
	lastIndex := strings.LastIndex(clientId, "_")
	sufixo := clientId[lastIndex+1:]
	baseURL := strings.Split(modules.MapOficial[sufixo], "chatbot")[0]
	if strings.Contains(baseURL, "disparo") {
		baseURL = strings.Split(modules.MapOficial[sufixo], "disparo")[0]
	}
	fmt.Println("✅ -> MENSAGEM LIDA DO CLIENTE ", clientId)
	modules.SendGenericToEndPoint(data, baseURL+"chatbot/chat/mensagens/read/")
	seenMessagesQueue.messageBuffer[clientId] = nil
	if timer, exists := seenMessagesQueue.messageTimeout[clientId]; exists {
		timer.Stop()
		delete(seenMessagesQueue.messageTimeout, clientId)
	}
}
func addSeenMessageToQueue(message SeenMessage, clientId string) bool {
	seenMessagesQueueMutex.Lock()
	defer seenMessagesQueueMutex.Unlock()
	if _, exists := seenMessagesQueue.messageBuffer[clientId]; !exists {
		seenMessagesQueue.messageBuffer[clientId] = []SeenMessage{}
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
			var seenMessage = SeenMessage{}
			seenMessage.IdMessage = event.MessageIDs[i]
			seenMessage.Chat = event.Chat.User
			seenMessage.Lida = true
			addSeenMessageToQueue(seenMessage, clientId)
		}
	}
}

var messagesDB *sql.DB

func connectMessagesDB() bool {
	if messagesDB != nil {
		return true
	}
	var err error
	messagesDB, err = sql.Open("sqlite3", "./messages.db")
	if err != nil {
		safePanic("ERRO AO ADD TAREFA DB", err)
		return false
	}
	createTableSQL := `CREATE TABLE IF NOT EXISTS sent_messages (
                    id TEXT
                );`
	_, err = messagesDB.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	cleanup := `
DELETE FROM sent_messages
WHERE rowid NOT IN (
  SELECT MIN(rowid)
  FROM sent_messages
  GROUP BY id
);
`
	_, err = messagesDB.Exec(cleanup)
	if err != nil {
		log.Fatal("Erro ao limpar duplicatas:", err)
	}
	createIndex := `CREATE UNIQUE INDEX IF NOT EXISTS idx_sent_messages_id ON sent_messages(id);`
	_, err = messagesDB.Exec(createIndex)
	if err != nil {
		log.Fatal("Erro ao criar INDEX", err)
	}
	return true
}

func idMessageJaEnviado(id string) bool {
	if messagesDB == nil {
		if !connectMessagesDB() {
			fmt.Println("CONEXÃO COM DB DE MENSAGENS FALHOU")
			return false
		}
	}
	query := "SELECT 1 FROM sent_messages WHERE id = ? LIMIT 1"
	var dummy int
	err := messagesDB.QueryRow(query, id).Scan(&dummy)
	if err == sql.ErrNoRows {
		return false // não encontrado
	}
	if err != nil {
		log.Println("Erro ao checar mensagem enviada:", err)
		return false // falha na consulta, melhor não considerar como enviado
	}
	return true // encontrado
}
func saveIdEnviado(id string) error {
	if messagesDB == nil {
		if !connectMessagesDB() {
			return fmt.Errorf("CONEXÃO COM DB DE MENSAGENS FALHOU")
		}
	}
	query := "INSERT OR IGNORE INTO sent_messages (id) VALUES (?)"
	_, err := messagesDB.Exec(query, id)
	return err
}

type sendMessageInfo struct {
	Result           []map[string]interface{} `json:"result"`
	ClientIdLocal    string                   `json:"clientIdLocal"`
	SendContact      string                   `json:"sendContact"`
	NoTimeout        string                   `json:"noTimeout"`
	DataProgramada   string                   `json:"dataProgramada"`
	InfoObjects      string                   `json:"infoObjects"`
	UUID             string                   `json:"uuid"`
	documento_padrao *multipart.FileHeader    `json:"-"`
	files            *multipart.FileHeader    `json:"-"`
	Counter          int32                    `json:"counter"`
}

type Mood struct {
	Name            string
	EventsFrequency int     // in minutes
	OnlineChance    float64 // probability of appearing online
	OfflineChance   float64 // probability of disconnecting
	TypingChance    float64 // probability of showing typing...
	AudioChance     float64 // probability of recording audio
	ActiveHours     [2]int  // hours of day when activity is higher
}

var moods = map[string]Mood{
	"active": {
		Name:            "Active",
		OnlineChance:    0.3,
		OfflineChance:   0.1,
		TypingChance:    0.3,
		AudioChance:     0.4, // loves audio
		ActiveHours:     [2]int{8, 23},
		EventsFrequency: 16,
	},
	"sleepy": {
		Name:            "Sleepy",
		OnlineChance:    0.2,
		OfflineChance:   0.6,
		TypingChance:    0.1,
		AudioChance:     0.05,
		ActiveHours:     [2]int{10, 22},
		EventsFrequency: 30,
	},
	"workaholic": {
		Name:            "Workaholic",
		OnlineChance:    0.8,
		OfflineChance:   0.05,
		TypingChance:    0.3,
		AudioChance:     0.1,
		ActiveHours:     [2]int{9, 18},
		EventsFrequency: 18,
	},
	"casual": {
		Name:            "Casual",
		OnlineChance:    0.5,
		OfflineChance:   0.2,
		TypingChance:    0.2,
		AudioChance:     0.1,
		ActiveHours:     [2]int{9, 23},
		EventsFrequency: 24,
	},
	"shy": {
		Name:            "Shy",
		OnlineChance:    0.3,
		OfflineChance:   0.3,
		TypingChance:    0.2,
		AudioChance:     0.01, // almost never records
		ActiveHours:     [2]int{10, 22},
		EventsFrequency: 27,
	},
}

var clientLimiters sync.Map // clientId -> *rate.Limiter
var lastAction sync.Map     // clientId -> time.Time
var rng = rand.New(rand.NewSource(time.Now().UnixNano()))

func getClientLimiter(clientId string) *rate.Limiter {
	if v, ok := clientLimiters.Load(clientId); ok {
		return v.(*rate.Limiter)
	}
	l := rate.NewLimiter(rate.Every(30*time.Second), 1) // 1 action per ~30s by default
	clientLimiters.Store(clientId, l)
	return l
}

func randomBetweenf(min, max float64) float64 {
	return min + rng.Float64()*(max-min)
}

func humanTypingDuration(text string) time.Duration {
	// realistic: 200 chars/min => 0.3s/char; add pauses and jitter
	chars := float64(len(text))
	baseSeconds := chars * 0.3
	jitter := randomBetweenf(-0.25, 0.4) // -25% .. +40%
	dur := baseSeconds * (1 + jitter)
	if dur < 1.0 {
		dur = 1.0
	}
	// clamp to reasonable max
	if dur > 60 {
		dur = 60
	}
	return time.Duration(dur) * time.Second
}

func simulateEvents(clientId string, mood Mood) {
	limiter := getClientLimiter(clientId)
	if !limiter.Allow() {
		return
	}

	if last, ok := lastAction.Load(clientId); ok {
		if t, _ := last.(time.Time); time.Since(t) < time.Duration(mood.EventsFrequency)*time.Minute/2 {
			return
		}
	}

	client := getClient(clientId)
	if client == nil {
		return
	}

	r := rng.Float64()
	pOnline := mood.OnlineChance
	pOffline := mood.OfflineChance
	pTyping := mood.TypingChance
	// pAudio := mood.AudioChance

	pOnline *= 1 + randomBetweenf(-0.15, 0.15)
	pTyping *= 1 + randomBetweenf(-0.15, 0.15)

	switch {
	case r < pOnline:
		modules.SetStatus(client, "conectado", types.JID{})
		fmt.Printf("[%s] -> online (mood %s)\n", clientId, mood.Name)
	case r < pOnline+pOffline:
		modules.SetStatus(client, "desconectado", types.JID{})
		fmt.Printf("[%s] -> offline (mood %s)\n", clientId, mood.Name)
	case r < pOnline+pOffline+pTyping:
		found, number, server := modules.FindRandomNumberInCache(clientId)
		if !found {
			return
		}
		JID := types.JID{User: number, Server: server}
		modules.SetStatus(client, "digitando", JID)
		dummyText := "Bom dia, tudo bem ?"
		duration := humanTypingDuration(dummyText)
		time.Sleep(duration)
		modules.SetStatus(client, "conectado", types.JID{})
		fmt.Printf("[%s] -> typing for %v (to %s)\n", clientId, duration, number)
	// case r < pOnline+pOffline+pTyping+pAudio:
	// 	// prefer NOT to fake audio recording unless you actually send audio sometimes.
	// 	// instead, emulate 'holding mic' with a short typing-like event
	// 	found, number, server := modules.FindRandomNumberInCache(clientId)
	// 	if !found {
	// 		return
	// 	}
	// 	JID := types.JID{User: number, Server: server}
	// 	modules.SetStatus(client, "gravando", JID)
	// 	time.Sleep(time.Duration(2+rng.Intn(6)) * time.Second) // 2-8s
	// 	modules.SetStatus(client, "conectado", types.JID{})
	// 	fmt.Printf("[%s] -> recorded-sim (short) to %s\n", clientId, number)
	default:
		// nothing — leave it alone
	}

	lastAction.Store(clientId, time.Now())
}

func autoCleanup() {
	// _15minTicker := time.NewTicker(2 * time.Minute)
	// for range _15minTicker.C {
	// 	fmt.Println("Procurando novas mensagens")
	// 	modules.LogMemUsage()
	// 	// modules.SearchNearMessages()
	// }
	_20minticker := time.NewTicker(20 * time.Minute)
	for range _20minticker.C {
		modules.DeleteOlderMessages()
	}
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

func cleanUploads() { // limpar arquivos do uploads
	dir := "./uploads/"
	arquivosAindaSalvos := modules.GetFilesToBeSent()
	arquivosNaPasta, _ := GetContents(dir)
	diff := modules.Difference(arquivosNaPasta, arquivosAindaSalvos)
	fmt.Println(arquivosNaPasta, arquivosAindaSalvos, diff)
	for _, name := range diff {
		err := os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
		}
	}
	// RemoveContents("./uploads/")
}
func GetContents(dir string) ([]string, error) {
	d, err := os.Open(dir)
	if err != nil {
		return []string{}, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	return names, err
}

var lastLimit int64 = -1

func UpdateMemoryLimit(activeConnections int) {
	const base = 30 << 20
	const perConn = 3 << 20
	limit := int64(base + (perConn * activeConnections))

	if limit != lastLimit {
		debug.SetMemoryLimit(limit)
		lastLimit = limit
		fmt.Printf("🔧 Mem limit ajustado: %.2f MiB para %d conexões\n",
			float64(limit)/1024/1024,
			activeConnections)
	}
}

func main() {
	//TANTO NO TESTE SEM E NO TESTE COM 4MB, NO ÚLTIMO TESTE HOUVE UM FLUXO MAIOR DE MENSAGENS, POR ISSO O CONSUMO ELEVADO
	//1° OBS (2 TESTES FEITOS) : COM O SOFT CAP O GC ESTÁ LIMPANDO MAIS RÁPIDO A HEAP, FAZENDO O CONSUMO FICAR ESTÁTICO EM ~4.5(caindo as vezes para 4.3) JÁ SEM O SOFT CAP ELE DEIXA ACUMULAR MAIS
	//DESSA FORMA FICAVA SUBINDO. EX : 4->4.18->5->5.05->5.28
	//TESTAR COLOCAR UM LIMIT MAIOR PARA EVITAR O GC aR VÁRIAS VEZES DESNECESSÁRIAMENTE (20MB)
	//2° OBS (3 TESTES FEITOS) : COM 20 MB ELE NÃO VAI LIMPAR PQ N CHEGA PERTO DO LIMIT, VOU REDUZIR PARA UNS 10MB
	//TESTAR COLOCAR UM LIMIT MAIOR PARA EVITAR O GC RODAR VÁRIAS VEZES DESNECESSÁRIAMENTE (20MB)
	//3° OBS (4 TESTES FEITOS) : FICOU ESTÁVEL EM ~4.48 (max 4.51) bem semelhante ao com 4MB
	// SEM SOFT CAP : 4MB - 4.18MB - 5MB
	// SOFT CAP 4MB : 3.57MB - 3.58MB - 4.5MB
	// SOFT CAP 20MB : 4.02MB - 4.29MB - 4.42MB
	// SOFT CAP 10MB : 3.57MB - 3.59MB - 4.48MB
	// simulateEvents("teste_disparo_shark", moods["active"])
	// found, number := modules.FindNumberInCache("5537984103402", "teste_disparo_shark")

	go cleanUploads()
	go autoCleanup()

	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signalCh
		os.Exit(0)
	}()
	defer func() {
		if r := recover(); r != nil {
			fmt.Println("Panic detectado:", r)
			// Cleanup de emergência
		}
	}()
	go func() {
		autoConnection()
		modules.DeleteOlderMessages()
		modules.InitMessagesQueue(modules.BasicActions{
			GetClient: getClient, CheckNumberWithRetry: checkNumberWithRetry, TryConnecting: tryConnecting,
		})
	}()

	modules.RemoveExpiredCaches()
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
	r.Post("/checknumber", func(c *fiber.Ctx) error {
		clientId := utils.CopyString(c.FormValue("clientId"))
		number := utils.CopyString(c.FormValue("numero"))
		re := regexp.MustCompile("[0-9]+")
		numberWithOnlyNumbers := strings.Join(re.FindAllString(number, -1), "")
		if numberWithOnlyNumbers[:2] != "55" {
			numberWithOnlyNumbers = "+55" + numberWithOnlyNumbers
		}
		client := getClient(clientId)
		// lidJID := types.NewJID(number, "lid")
		// ctx := context.Background()
		// pn, err := client.Store.LIDs.GetPNForLID(ctx, lidJID)
		// if err != nil {
		// 	log.Println("Erro ao obter número de telefone:", err)
		// } else if pn == types.EmptyJID {
		// 	log.Println("Nenhum número associado a esse LID foi encontrado ainda.")
		// } else {
		// 	fmt.Println("Número de telefone vinculado ao LID:", pn.)
		// }
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

		validNumber, err := checkNumberWithRetry(client, numberWithOnlyNumbers, false, clientId)
		if err != nil {
			fmt.Println("Erro check", err)
			return c.Status(500).JSON(fiber.Map{
				"message": "Numero inválido ERRO",
			})
		}
		fmt.Println("Numero Válido", validNumber)
		if len(validNumber) == 0 {
			fmt.Println("⛔ -> Numero inválido. ClientId: ", clientId, " | Numero: ", number)
			return c.Status(500).JSON(fiber.Map{
				"message": "Numero inválido",
			})
		}
		response := validNumber[0] // Acessa o primeiro item da slicet
		IsIn := response.IsIn
		if !IsIn {
			fmt.Println("⛔ -> Numero not In WhatsApp. ClientId: ", clientId, " | Numero: ", number)
			return c.Status(500).JSON(fiber.Map{
				"message": "Numero inválido",
			})
		}

		return c.Status(200).JSON(fiber.Map{
			"message": "Número Válido",
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
		slug_disparo := utils.CopyString(c.FormValue("slug_disparo"))
		UUID := slug_disparo
		if UUID == "" {
			UUID = clientId + uuid.New().String()
		}
		fmt.Printf("------------------ %s Send Files Request ------------------------ \n\n", clientId)

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
		timestamp := time.Now().Unix()
		if dataProgramada != "" {
			layout := "2006-01-02 15:04:05"
			t, err := time.Parse(layout, strings.TrimSpace(dataProgramada))
			if err != nil {
				fmt.Println("Erro ao converter data:", t, err)
				return c.Status(500).JSON(fiber.Map{
					"message": "Data Inválida",
				})
			}
			timestamp = t.Add(3 * time.Hour).Unix()

		}
		documento_padrao_filePath := ""
		// files_filePath := ""
		var documento_padrao *multipart.FileHeader = nil
		documento_padrao, err = c.FormFile("documento_padrao")
		if err != nil {
			fmt.Println("📁 ❌-> Nenhum documento padrão enviado.", err)
		}
		if documento_padrao != nil {
			savePath := "./uploads/" + UUID + "_!-!_" + documento_padrao.Filename
			if err := c.SaveFile(documento_padrao, savePath); err != nil {
				fmt.Printf("Erro ao salvar o arquivo: %v", err)
			}
			if strings.Contains(savePath, ".webp") {
				errorconvert := modules.ConvertWebPToJPEG(savePath, strings.Replace(savePath, ".webp", ".jpeg", -1))
				if errorconvert == nil {
					defer os.Remove("./uploads/" + clientId + documento_padrao.Filename)
					documento_padrao.Filename = strings.Replace(documento_padrao.Filename, ".webp", ".jpeg", -1)
					savePath = strings.Replace(savePath, ".webp", ".jpeg", -1)
				}
			}
			documento_padrao_filePath = savePath
		}
		var resultStructed []modules.MessageIndividual
		// Deserializando o JSON para o map
		err = json.Unmarshal([]byte(infoObjects), &resultStructed)
		if err != nil {
			fmt.Printf("Erro ao converter JSON structed: %v", err)
		}
		// fmt.Println(resultStructed)
		sendContactStructed := modules.Send_contact{}
		if sendContact != "" {
			err = json.Unmarshal([]byte(sendContact), &sendContactStructed)
			if err != nil {
				fmt.Printf("Erro ao converter JSON structed: %v", err)
			}
		}
		// go processarGrupoMensagens(sendMessageInfo{ClientIdLocal: clientId,
		// 	Result:           result,
		// 	documento_padrao: documento_padrao,
		// 	files:            files,
		// 	SendContact:      sendContact,
		// 	NoTimeout:        noTimeout,
		// 	DataProgramada:   dataProgramada,
		// 	InfoObjects:      infoObjects, Counter: 0, UUID: UUID})
		sendMessageInfo := modules.SendMessageInfo{ClientIdLocal: clientId,
			Result:           resultStructed,
			Documento_padrao: documento_padrao_filePath,
			// files:            files,
			SendContact:    &sendContactStructed,
			NoTimeout:      noTimeout,
			DataProgramada: timestamp, IdBatch: UUID}
		// sendMessageInfoInJSON, _ := json.MarshalIndent(sendMessageInfo, " ", "")
		// fmt.Println("[ENVIO_MENSAGEM] -> ", string(sendMessageInfoInJSON))
		go modules.AddMensagemPendente(sendMessageInfo)
		if dataProgramada != "" {
			return c.Status(200).JSON(fiber.Map{
				"message": "Disparo agendado com sucesso",
			})
		}
		return c.Status(200).JSON(fiber.Map{
			"message": "Arquivo recebido e enviado no WhatsApp.",
		})
	})
	r.Post("/getQRCode", func(c *fiber.Ctx) error {
		ctx := context.Background()

		// Recupera o corpo da requisição e faz a bind para a estrutura de dados
		sendEmail := utils.CopyString(c.FormValue("notifyEmail"))
		clientId := utils.CopyString(c.FormValue("clientId"))
		pairphone := utils.CopyString(c.FormValue("pairphone"))
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

		container, err := sqlstore.New(ctx, "sqlite3", "file:./clients_db/"+clientId+".db?_foreign_keys=on", dbLog)
		if err != nil {
			fmt.Println(err)
		}
		deviceStore, err := container.GetFirstDevice(ctx)
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
		if pairphone != "" {
			client.Connect()
			code, err := client.PairPhone(ctx, pairphone, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
			if err != nil {
				fmt.Println("Erro no pairphone", err)
			}

			return c.Status(200).JSON(fiber.Map{
				"pairCOde": code,
			})
		}
		avaiableProxyServer, found := modules.GetServerByClientId(clientId)
		if !found {
			fmt.Printf("[PROXY] -> Não existem servidores disponíveis para o cliente : %s \n", clientId)
		} else {
			fmt.Printf("[PROXY] -> '%s' Conectado em '%s' (%d/%d) \n", clientId, avaiableProxyServer.Name, avaiableProxyServer.ActiveConns, avaiableProxyServer.MaxConns)
			dialer, err := proxy.SOCKS5("tcp", avaiableProxyServer.URL, &proxy.Auth{
				User:     avaiableProxyServer.User,
				Password: avaiableProxyServer.Password,
			}, proxy.Direct)
			if err != nil {
				panic(err)
			}
			conn, err := dialer.Dial("tcp", "api.ipify.org:443")
			if err != nil {
				fmt.Println("[PROXY] -> Proxy failed:", err)
			} else {
				fmt.Println("[PROXY] -> Proxy working, conn:", conn.RemoteAddr())
				conn.Close()
				client.SetSOCKSProxy(dialer, whatsmeow.SetProxyOptions{
					NoWebsocket: false,
					NoMedia:     false,
				})
			}

		}
		client.AddEventHandler(func(evt interface{}) {
			switch v := evt.(type) {
			case *events.Connected:
				stoppedQrCodeRequests.Store(clientId, int32(1))
				clientsMutex.Lock()
				clientMap[clientId] = client
				clientsMutex.Unlock()
				fmt.Println("🎉 -> CLIENTE CONECTADO", clientId)
				if strings.Contains(clientId, "chat") {
					modules.SetStatus(client, "conectado", types.JID{})
				}
				UpdateMemoryLimit(len(clientMap))
				data := modules.GenericPayload{
					Evento:   "CLIENTE_CONECTADO",
					ClientID: clientId,
					Data: map[string]string{
						"numero_conectado": client.Store.ID.User,
						"status":           "conectado",
						"data_conexao":     "2025-01-20",
					},
				}
				lastIndex := strings.LastIndex(clientId, "_")
				sufixo := clientId[lastIndex+1:]
				baseURL := modules.MapOficial[sufixo]
				if sendEmail != "" {
					getOrSetEmails("INSERT INTO emails_conexoes ( `clientId`, `email`) VALUES (?,?);", []any{clientId, "luisgustavo20061@gmail.com"})
				}
				modules.SendGenericToEndPoint(data, baseURL)
			case *events.Disconnected:
				fmt.Printf("🔄 -> RECONECTANDO CLIENTE %s", clientId)
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
			qrChan, _ := client.GetQRChannel(ctx)

			err = client.Connect()
			if err != nil {
				fmt.Println(err)
			}
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

							data := modules.GenericPayload{
								Evento:   evento,
								ClientID: clientIdCopy,
								Data:     dataURL,
							}
							lastIndex := strings.LastIndex(clientIdCopy, "_")
							sufixo := clientIdCopy[lastIndex+1:]
							baseURL := modules.MapOficial[sufixo]
							modules.SendGenericToEndPoint(data, baseURL)

						} else {
							data := modules.GenericPayload{
								Evento:   evento,
								ClientID: clientIdCopy,
								Data:     evt.Code,
							}
							lastIndex := strings.LastIndex(clientIdCopy, "_")
							sufixo := clientIdCopy[lastIndex+1:]
							baseURL := modules.MapOficial[sufixo]
							modules.SendGenericToEndPoint(data, baseURL)
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

				data := modules.GenericPayload{
					Evento:   evento,
					ClientID: clientId,
					Data:     dataURL,
				}
				lastIndex := strings.LastIndex(clientId, "_")
				sufixo := clientId[lastIndex+1:]
				baseURL := modules.MapOficial[sufixo]
				modules.SendGenericToEndPoint(data, baseURL)
				return c.Status(200).JSON(fiber.Map{
					"qrCode": dataURL,
				})
			} else {
				data := modules.GenericPayload{
					Evento:   evento,
					ClientID: clientId,
					Data:     firstQRCode.Code,
				}
				lastIndex := strings.LastIndex(clientId, "_")
				sufixo := clientId[lastIndex+1:]
				baseURL := modules.MapOficial[sufixo]
				modules.SendGenericToEndPoint(data, baseURL)
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
		return nil
	})
	fmt.Println("⏳ Iniciando servidor...", PORT)
	// modules.SearchNearMessages()

	r.Listen(":" + PORT)
}
func getOrSetEmails(query string, args []any) *sql.Rows {
	db, err := sql.Open("sqlite3", "./manager.db")
	if err != nil {
		log.Println("ERRO AO ADD TAREFA DB", err)
	}
	defer db.Close()

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
	return rows
}

// func sendToEmail(target string, text string) {
// 	fmt.Println("Enviando email de desconexão para", target)
// 	from := "oisharkbusiness@gmail.com"
// 	pass := os.Getenv("EMAIL_PASS")
// 	to := target

// 	msg := "From: " + from + "\n" +
// 		"To: " + to + "\n" +
// 		"Subject: Conexão Perdida !\n\n" +
// 		text

//		err := smtp.SendMail("74.125.142.108:587", // IPv4 direto
//			smtp.PlainAuth("", from, pass, "smtp.gmail.com"), // mantenha o domínio aqui
//			from, []string{to}, []byte(msg))
//		if err != nil {
//			fmt.Printf("smtp error: %s", err)
//			return
//		}
//		fmt.Println("sent, visit http://foobarbazz.mailinator.com")
//	}
//
//	func sendEmailDisconnection(clientId string) {
//		rows := getOrSetEmails("SELECT * FROM emails_conexoes WHERE clientId = ?", []any{clientId})
//		fmt.Println()
//		defer rows.Close()
//		for rows.Next() {
//			var clientId string
//			var email string
//			if err := rows.Scan(&clientId, &email); err != nil {
//				log.Fatal(err)
//			}
//			// sendToEmail(email, "Sua conexão no whatsapp foi perdida, acesse o Shark Business e verifique ")
//		}
//	}
func desconctarCliente(clientId string) bool {
	ctx := context.Background()
	fmt.Println("⛔ -> CLIENTE DESCONECTADO", clientId)
	// sendEmailDisconnection(clientId)
	client := getClient(clientId)
	data := modules.GenericPayload{
		Evento:   "CLIENTE_DESCONECTADO",
		ClientID: clientId,
		Data:     "CLIENTE_DESCONECTADO",
	}
	delete(clientMap, clientId)
	lastIndex := strings.LastIndex(clientId, "_")
	sufixo := clientId[lastIndex+1:]
	baseURL := modules.MapOficial[sufixo]
	modules.SendGenericToEndPoint(data, baseURL)
	defer modules.RemoveProxyToClientId(clientId)
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
		client.Logout(ctx)
	}

	return true
}

var bufferPool = sync.Pool{
	New: func() interface{} {
		s := make([]byte, 0, 2<<20) // 2mb
		return &s                   // Retorna PONTEIRO para slice
	},
}
