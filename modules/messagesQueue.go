package modules

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/goccy/go-json"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

var Re0to9 = regexp.MustCompile("[0-9]+")

type GenericPayload struct {
	Evento   string      `json:"evento,omitempty"`
	ClientID string      `json:"clientId,omitempty"`
	Data     interface{} `json:"data,omitempty"`
	Sender   int         `json:"sender,omitempty"`
}
type SentMessagePayload struct {
	Evento   string          `json:"evento,omitempty"`
	ClientID string          `json:"clientId,omitempty"`
	Data     SentMessageData `json:"data,omitempty"`
	Sender   int             `json:"sender,omitempty"`
}

type SendMessageInfo struct {
	Result           []MessageIndividual   `json:"result"`
	ClientIdLocal    string                `json:"clientIdLocal"`
	SendContact      *Send_contact         `json:"sendContact"`
	NoTimeout        string                `json:"noTimeout"`
	DataProgramada   int64                 `json:"dataProgramada"`
	IdBatch          string                `json:"idBatch"`
	Documento_padrao string                `json:"-"`
	files            *multipart.FileHeader `json:"-"`
}
type SentMessageData struct {
	NewID  string `json:"newID"`
	OldID  string `json:"oldID"`
	Sender string `json:"sender"`
}
type BasicActions struct {
	GetClient            func(clientId string) *whatsmeow.Client
	CheckNumberWithRetry func(client *whatsmeow.Client, number string, de_grupo bool, clientId string) (resp []types.IsOnWhatsAppResponse, err error)
	TryConnecting        func(clientId string) *whatsmeow.Client
}
type Send_contact struct {
	Contato string `json:"contato"`
	Nome    string `json:"nome"`
}
type Quoted_message struct {
	Quoted_message_id string `json:"messageID"`
	Quoted_sender     string `json:"sender"`
	Quoted_text       string `json:"messageQuoted"`
}
type MessageIndividual struct {
	Text              string          `json:"text"`
	Number            string          `json:"number"`
	Focus             string          `json:"focus"`
	UUID              string          `json:"idMensagem"`
	Id_grupo          string          `json:"id_grupo"`
	Quoted_message    *Quoted_message `json:"quotedMessage,omitempty"`
	Edited_id_message string          `json:"editedIDMessage"`
	Send_contact      *Send_contact   `json:"sendContact,omitempty"`
	Documento_padrao  string          `json:"documento_padrao"`
	Attempts          int32           `json:"attempts"`
}

var dbMensagensPendentes *Database
var actions BasicActions
var urlSendMessageEdpoint = map[string]string{}

// Worker pool and poller configuration
var (
	jobQueue             chan string
	pollStop             chan struct{}
	maxRetries           int32 = 3
	startPoolOnce        sync.Once
	workerCountDefault   = 50
	pollIntervalDefault  = 200 * time.Millisecond
	pollBatchSizeDefault = 200
)

func (q *Quoted_message) Scan(value interface{}) error {
	if value == nil {
		return nil
	}

	var b []byte
	switch v := value.(type) {
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("Quoted message: unsupported type %T", value)
	}

	return json.Unmarshal(b, q)
}
func (s *Send_contact) Scan(value interface{}) error {
	if value == nil {
		return nil
	}

	var data []byte
	switch v := value.(type) {
	case string:
		data = []byte(v)
	case []byte:
		data = v
	default:
		return fmt.Errorf("Send_contact: unsupported type %T", value)
	}

	return json.Unmarshal(data, s)
}

type Statements struct {
	GetPendingByUUID    *sql.Stmt
	DeletePendingByUUID *sql.Stmt
	InsertUUID          *sql.Stmt
	selectPendingStmt   *sql.Stmt
}

type Database struct {
	DB    *sql.DB
	Stmts Statements
}

var jsonBufferPool = sync.Pool{
	New: func() any {
		return new(bytes.Buffer)
	},
}

func connectToMessagesQueueDB() *Database {
	if dbMensagensPendentes != nil {
		return dbMensagensPendentes
	}
	var err error
	db, err := ConnectPsql()

	if err != nil {
		log.Fatal(err)
	}
	selectStmt, err := db.Prepare(`
		SELECT clientId, text, number, documento_padrao, quoted_message, 
		       edited_id_message, focus, id_grupo, send_contact,attempts
		FROM pendingMessages
		WHERE uuid = $1 LIMIT 1`)
	if err != nil {
		log.Fatal(err)

	}
	selectPendingStmt, err := db.Prepare(`WITH cte AS (
  SELECT uuid
  FROM pendingMessages
  WHERE data_desejada <= $1
    AND agendada = false
    AND indev = $2
    AND attempts < $3
    AND should_try_again = true
  ORDER BY data_desejada ASC
  LIMIT $4
)
UPDATE pendingMessages p
SET agendada = true
FROM cte
WHERE p.uuid = cte.uuid
RETURNING p.uuid;`)
	if err != nil {
		log.Fatal(err)

	}
	insertStmt, err := db.Prepare(`
	INSERT INTO pendingMessages (
		uuid, idBatch, clientId, text, number, data_desejada, documento_padrao, 
		quoted_message, edited_id_message, focus, id_grupo, send_contact,indev,should_try_again,agendada,attempts
	)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,$13,true,false,0)
	ON CONFLICT (uuid) DO UPDATE SET
		idBatch = EXCLUDED.idBatch,
		clientId = EXCLUDED.clientId,
		text = EXCLUDED.text,
		number = EXCLUDED.number,
		data_desejada = EXCLUDED.data_desejada,
		documento_padrao = EXCLUDED.documento_padrao,
		quoted_message = EXCLUDED.quoted_message,
		edited_id_message = EXCLUDED.edited_id_message,
		focus = EXCLUDED.focus,
		id_grupo = EXCLUDED.id_grupo,
		send_contact = EXCLUDED.send_contact,
		indev = EXCLUDED.indev,
		should_try_again = EXCLUDED.should_try_again,
		agendada = EXCLUDED.agendada,
		attempts = EXCLUDED.attempts;
`)
	if err != nil {
		log.Fatal("Erro preparando insertStmt:", err)
	}
	deleteStmt, err := db.Prepare(`DELETE FROM pendingMessages WHERE uuid = $1`)
	if err != nil {
		log.Fatal(err)

	}
	db.SetMaxOpenConns(32) // allow parallel queries
	db.SetMaxIdleConns(8)  // keep a few idle ready
	db.SetConnMaxLifetime(time.Hour)

	dbMensagensPendentes = &Database{
		Stmts: Statements{},
	}
	dbMensagensPendentes.Stmts.GetPendingByUUID = selectStmt
	dbMensagensPendentes.Stmts.DeletePendingByUUID = deleteStmt
	dbMensagensPendentes.Stmts.InsertUUID = insertStmt
	dbMensagensPendentes.Stmts.selectPendingStmt = selectPendingStmt
	dbMensagensPendentes.DB = db
	return dbMensagensPendentes
}
func insertMessage(clientId string, idBatch string, idMessage string, msgInfo MessageIndividual, data_desejada int64, send_contact *Send_contact) {
	db := connectToMessagesQueueDB()

	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jsonBufferPool.Put(buf)
	var err error
	jsonStringOfQuotedMessage := []byte("{}")
	if msgInfo.Quoted_message != nil {
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(msgInfo.Quoted_message); err == nil {
			jsonStringOfQuotedMessage = buf.Bytes()
		}
	}

	jsonStringOfSendContact := []byte("{}")
	if send_contact != nil {
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(send_contact); err == nil {
			jsonStringOfSendContact = buf.Bytes()
		}
	}

	_, err = db.Stmts.InsertUUID.Exec(idMessage, idBatch, clientId, msgInfo.Text, msgInfo.Number, data_desejada, msgInfo.Documento_padrao, string(jsonStringOfQuotedMessage), msgInfo.Edited_id_message, msgInfo.Focus, msgInfo.Id_grupo, string(jsonStringOfSendContact), Desenvolvimento)
	if err != nil {
		log.Println("Erro ao adicionar mensagem:", err)
	}

}

func AddMensagemPendente(sendInfo SendMessageInfo) {
	now := time.Unix(int64(sendInfo.DataProgramada), 0).Unix()
	for i, v := range sendInfo.Result {
		v.Documento_padrao = sendInfo.Documento_padrao
		customDelay := RandomBetween(1, 4)
		// v.Text += "_" + strconv.Itoa(i)
		id_message := v.UUID
		if id_message == "" {
			id_message = sendInfo.IdBatch + strconv.Itoa(i)
		}
		insertMessage(sendInfo.ClientIdLocal, sendInfo.IdBatch, id_message, v, now, sendInfo.SendContact)
		if len(sendInfo.Result) > i+1 {
			nextItem := sendInfo.Result[i+1]
			totalDelay := RandomBetween(30, 45)
			text := nextItem.Text
			textLen := float64(len(text)) * 0.05
			if textLen > 20 {
				textLen = 20
			}
			totalDelay = int(textLen) + totalDelay
			fmt.Println("⏳ Tempo esperado para enviar a próxima mensagem:", totalDelay, "segundos...")
			LogMemUsage()
			customDelay = int(totalDelay)
			now += int64(customDelay)
		}
		// fmt.Println("Adicionando mensagem", v.Documento_padrao, i, " Batch :", sendInfo.IdBatch, "UUID:", id_message)
	}
	// SearchNearMessages()
}
func InitMessagesQueue(basicActions BasicActions) {
	actions = basicActions

	db := connectToMessagesQueueDB()

	_, err := db.DB.Exec(`UPDATE pendingMessages
  SET agendada = false WHERE agendada != false AND indev = $1`, Desenvolvimento)
	if err != nil {
		log.Println("Erro ao atualizar status agendada:", err)
	}
	startPoolOnce.Do(func() {
		StartMessageQueue(workerCountDefault, pollIntervalDefault, pollBatchSizeDefault)
	})
}

// func SearchNearMessages() {
// 	fmt.Println("Procurando mensagens próximas")
// 	db := connectToMessagesQueueDB()
// 	dbMutex.Lock()
// 	rows, err := db.DB.Query(`UPDATE pendingMessages
//   SET agendada = true
//   WHERE data_desejada <= $1
//   AND agendada = false
//   RETURNING uuid, data_desejada`, time.Now().Add(15*time.Minute).Unix())
// 	dbMutex.Unlock()
// 	if err != nil {
// 		log.Println("Erro ao atualizar mensagem:", err)
// 	}
// 	defer rows.Close()

//		var uuid string
//		var ts int64
//		for rows.Next() {
//			err := rows.Scan(&uuid, &ts)
//			if err != nil {
//				log.Println("Erro no scan:", err)
//				continue
//			}
//			delay := time.Until(time.Unix(ts, 0))
//			if delay <= 0 {
//				fmt.Println("Enviando AGORA")
//				enviarMensagem(uuid)
//			} else {
//				go prepareMessageTimer(uuid, delay)
//			}
//			fmt.Println("Faltam : ", delay)
//		}
//	}
func GetFilesToBeSent() []string {
	fmt.Println("Procurando Arquivos ")
	db := connectToMessagesQueueDB()
	rows, err := db.DB.Query(`SELECT documento_padrao FROM pendingMessages WHERE documento_padrao != '' AND data_desejada <= $1 AND indev = $2`, time.Now().Add(15*time.Minute).Unix(), Desenvolvimento)
	if err != nil {
		log.Println("Erro ao remover mensagem:", err)
	}
	defer rows.Close()

	var docPadrao string
	var docs = []string{}
	for rows.Next() {

		err := rows.Scan(&docPadrao)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		docs = append(docs, docPadrao)
	}
	return docs
}
func (d *Database) Close() {
	d.Stmts.GetPendingByUUID.Close()
	d.Stmts.DeletePendingByUUID.Close()
	d.DB.Close()
}

func SendToEndPoint(data SentMessagePayload, url string) {
	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jsonBufferPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return
	}

	// fmt.Println("Data sendo envidada :", string(jsonData))
	if url == "" {
		fmt.Printf("URL %s vazia", url)
		return
	}
	req, err := http.NewRequest("POST", url, buf)
	if err != nil {
		fmt.Printf("Erro ao criar a requisição: %v", err)
		return
	}
	defer buf.Reset()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", os.Getenv("STRING_AUTH"))
	req.Header.Set("X-CSRFToken", GetCSRFToken())
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Erro ao enviar a requisição: %v", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(url)
	fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | evento : ", data.Evento, " | clientId :", data.ClientID)
}
func SendGenericToEndPoint(data GenericPayload, url string) {
	buf := jsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer jsonBufferPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return
	}

	// fmt.Println("Data sendo envidada :", string(jsonData))
	if url == "" {
		fmt.Printf("URL %s vazia", url)
		return
	}
	req, err := http.NewRequest("POST", url, buf)
	if err != nil {
		fmt.Printf("Erro ao criar a requisição: %v", err)
		return
	}
	defer buf.Reset()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", os.Getenv("STRING_AUTH"))
	req.Header.Set("X-CSRFToken", GetCSRFToken())
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("Erro ao enviar a requisição: %v", err)
		return
	}
	defer resp.Body.Close()
	fmt.Println(url)
	fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | evento : ", data.Evento, " | clientId :", data.ClientID)
}

// StartMessageQueue initializes workers and a periodic poller that claims due messages.
func StartMessageQueue(numWorkers int, interval time.Duration, batchSize int) {
	if jobQueue == nil {
		jobQueue = make(chan string, batchSize*2)
	}
	if pollStop == nil {
		pollStop = make(chan struct{})
	}
	// Start workers
	for i := 0; i < numWorkers; i++ {
		go func() {
			for uuid := range jobQueue {
				enviarMensagem(uuid)
			}
		}()
	}
	// Start poller
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				pollDueMessages(batchSize)
			case <-pollStop:
				return
			}
		}
	}()
	// Initial immediate poll
	pollDueMessages(batchSize)
}

// triggerImmediatePoll triggers a one-off poll to enqueue newly due messages.
func triggerImmediatePoll() {
	go pollDueMessages(pollBatchSizeDefault)
}
func DeleteOlderMessages() error {
	db := connectToMessagesQueueDB()
	timestampPassado := time.Now().Add(-20 * time.Minute).Unix()
	fmt.Println(timestampPassado)
	_, err := db.DB.Exec(`DELETE from pendingMessages WHERE data_desejada <= $1 and attempts >= $2  and should_try_again = false`, timestampPassado, maxRetries)
	if err != nil {
		return err
	}
	return nil
}
func CancelMessages(id string, batch bool) error {
	if batch {
		db := connectToMessagesQueueDB()
		_, err := db.DB.Exec(`delete from pendingMessages WHERE idbatch = $1`, id)
		if err != nil {
			return err
		}
	} else {
		db := connectToMessagesQueueDB()
		_, err := db.DB.Exec(`delete from pendingMessages WHERE uuid = $1`, id)
		if err != nil {
			return err
		}
	}

	return nil
}

// pollDueMessages claims due messages and enqueues their UUIDs for workers.
func pollDueMessages(batchSize int) {
	db := connectToMessagesQueueDB()
	rows, err := db.Stmts.selectPendingStmt.Query(time.Now().Unix(), Desenvolvimento, maxRetries, batchSize)
	if err != nil {
		log.Println("Erro ao buscar mensagens devidas:", err)
		return
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			log.Println("Erro no scan (poll):", err)
			continue
		}
		jobQueue <- u
		count++

		if count >= batchSize {
			break
		}
	}
}
func updateLastError(uuid string, shouldTryAgain bool, attempts int32, lastError string) error {
	db := connectToMessagesQueueDB()
	fmt.Printf("[ERROR_STATUS] -> Atualizando status de erro da mensagem %s, com %d tentativas, deve tentar novamente ? %v \n", uuid, attempts, shouldTryAgain)
	backoff := 30 * attempts
	if attempts < maxRetries-1 {
		_, err := db.DB.Exec(`UPDATE pendingMessages
  SET last_error = $1,status='Error, Will Try Again',attempts = attempts +1 ,data_desejada = data_desejada+$2,agendada = false,should_try_again=$3 WHERE uuid= $4`, lastError, backoff, shouldTryAgain, uuid)
		if err != nil {
			fmt.Println(err)
			return err
		}
	} else {
		_, err := db.DB.Exec(`UPDATE pendingMessages
  SET last_error = $1,status='Final Error, Will be deleted',attempts = attempts +1,should_try_again=$2 WHERE uuid= $3`, lastError, false, uuid)
		if err != nil {
			fmt.Println(err)
			return err
		}
	}

	return nil
}
func enviarMensagem(uuid string) {

	db := connectToMessagesQueueDB()
	mainCtx := context.Background()
	// ctx := context.Background()

	ctxWT, cancel := context.WithTimeout(mainCtx, 15000*time.Millisecond)
	defer cancel()
	// rows := db.DB.QueryRowContext(ctx,
	// 	`SELECT clientId, text, number,documento_padrao,quoted_message,edited_id_message,focus,id_grupo,send_contact  FROM pendingMessages
	//  WHERE uuid = ? LIMIT 1`,
	// 	uuid,
	// )

	var clientId string
	msgInfo := MessageIndividual{}
	err := db.Stmts.GetPendingByUUID.QueryRowContext(ctxWT, uuid).Scan(
		&clientId,
		&msgInfo.Text,
		&msgInfo.Number,
		&msgInfo.Documento_padrao,
		&msgInfo.Quoted_message,
		&msgInfo.Edited_id_message,
		&msgInfo.Focus,
		&msgInfo.Id_grupo,
		&msgInfo.Send_contact,
		&msgInfo.Attempts,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			log.Println("UUID not found:", uuid)
			return
		}
		log.Println("Scan error:", err)
		return
	}

	msgInfo.Number = sanitizeNumber(msgInfo.Number)
	client := actions.GetClient(clientId)
	if client == nil {
		client = actions.TryConnecting(clientId)
		if client == nil {
			// removeClientDB(clientId, nil)
			return
		}
	}
	id_grupo := msgInfo.Id_grupo //todo PEGAR O ID DO GRUPO
	validNumber, err := actions.CheckNumberWithRetry(client, msgInfo.Number, id_grupo != "", clientId)
	var JID types.JID = types.JID{}
	message := &waE2E.Message{
		ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(msgInfo.Text),
		},
	}
	msg := SingleMessageInfo{
		ClientId:    clientId,
		Client:      client,
		Context:     context.Background(),
		MessageInfo: nil,
		Attempts:    0,
		LastError:   nil,
		Focus:       msgInfo.Focus,
		Text:        msgInfo.Text,
		Number:      msgInfo.Number,
		IdMensagem:  uuid,
	}

	newNameFile := ""
	extraMessage := false
	if msgInfo.Documento_padrao != "" {
		message, newNameFile, extraMessage = PrepararMensagemArquivo(msgInfo.Text, message, msgInfo.Documento_padrao, client, clientId, msg, uuid)

	}

	if id_grupo != "" {
		JID = types.JID{User: strings.Replace(id_grupo, "@g.us", "", -1), Server: types.GroupServer}
	} else {
		if err != nil {
			fmt.Println(err, "ERRO ISONWHATSAPP")
			fmt.Println("⛔ -> Numero inválido Erro. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
			updateLastError(uuid, true, msgInfo.Attempts, fmt.Sprintln("⛔ -> Numero inválido Erro: ", err))
			return
		}
		if len(validNumber) == 0 {
			fmt.Println("⛔ -> Numero inválido. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
			updateLastError(uuid, false, msgInfo.Attempts, fmt.Sprintln("⛔ -> Numero inválido. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo))

			return
		}
		response := validNumber[0] // Acessa o primeiro item da slicet
		JID = response.JID
		IsIn := response.IsIn
		if !IsIn {
			fmt.Println("⛔ -> Numero not In WhatsApp. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
			updateLastError(uuid, false, msgInfo.Attempts, fmt.Sprintln("⛔ -> Numero not In WhatsApp. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo))

			return
		}
	}

	SetStatus(client, "digitando", JID)
	defer SetStatus(client, "conectado", JID)
	if msgInfo.Quoted_message != nil && msgInfo.Quoted_message.Quoted_sender != "" && msgInfo.Quoted_message.Quoted_message_id != "" {
		validNumber, err := actions.CheckNumberWithRetry(client, sanitizeNumber(msgInfo.Quoted_message.Quoted_sender), id_grupo != "", clientId)
		if err == nil {
			var msg_quote *waE2E.Message = &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(msgInfo.Quoted_message.Quoted_text),
				},
			}
			message.ExtendedTextMessage = &waE2E.ExtendedTextMessage{
				Text: proto.String(msgInfo.Text),
				ContextInfo: &waE2E.ContextInfo{
					StanzaID:      proto.String(msgInfo.Quoted_message.Quoted_message_id),
					Participant:   proto.String(validNumber[0].JID.String()),
					QuotedMessage: msg_quote,
				},
			}
		}

	}
	if msgInfo.Edited_id_message != "" {
		message = client.BuildEdit(JID, msgInfo.Edited_id_message, message)
	}
	sendContact := msgInfo.Send_contact
	if sendContact != nil && sendContact.Contato != "" && sendContact.Nome != "" {
		// Deserializando o JSON corretamente para o map
		validNumber, err := actions.CheckNumberWithRetry(client, sanitizeNumber(sendContact.Contato), id_grupo != "", clientId)
		if err == nil {
			fmt.Println(err, "ERRO IS ONWHATSAPP")
			response := validNumber[0]
			cell := response.JID.User
			formatedNumber := FormatPhoneNumber(cell)
			if formatedNumber != "" {
				contactMessage := &waE2E.Message{
					ContactMessage: &waE2E.ContactMessage{
						DisplayName: proto.String(sendContact.Nome),
						Vcard:       proto.String("BEGIN:VCARD\nVERSION:3.0\nN:;" + sendContact.Nome + ";;;\nFN:" + sendContact.Nome + "\nitem1.TEL;waid=" + cell + ":" + formatedNumber + "\nitem1.X-ABLabel:Celular\nEND:VCARD"),
					}}
				msg.MessageInfo = contactMessage
				message = contactMessage
				extraMessage = true
				// client.SendMessage(context.Background(), JID, contactMessage)
			} else {
				fmt.Println("FORMATADO ->", err)
			}
		}
	}
	retornoEnvio, err := client.SendMessage(ctxWT, JID, message)
	if err != nil {
		updateLastError(uuid, true, msgInfo.Attempts, fmt.Sprintln("Erro ao enviar mensagem 1°: ", err))

		fmt.Println("Erro ao enviar mensagem", err)
		return
	} else {
		if newNameFile != "" {
			os.Remove(newNameFile)
		}
		if extraMessage {
			retornoEnvio2, err := client.SendMessage(ctxWT, JID, &waE2E.Message{
				ExtendedTextMessage: &waE2E.ExtendedTextMessage{
					Text: proto.String(msgInfo.Text),
				},
			})
			if err != nil {
				updateLastError(uuid, true, msgInfo.Attempts, fmt.Sprintln("Erro ao enviar mensagem extra de texto° : ", err))

				fmt.Println("Erro ao enviar mensagem extra de texto", err)
				return
			}
			if msgInfo.Focus == "noreply" {
				fmt.Println("Mensagem não deve ser enviada, focus 'noreply'")
				removeMensagemPendente(uuid)
				return
			}
			data := SentMessagePayload{
				Evento:   "MENSAGEM_ENVIADA",
				ClientID: clientId,
				Data: SentMessageData{
					NewID:  retornoEnvio2.ID,
					OldID:  uuid,
					Sender: strings.ReplaceAll(msgInfo.Number, "+", ""),
				},
			}
			lastIndex := strings.LastIndex(clientId, "_")
			sufixo := clientId[lastIndex+1:]
			if urlSendMessageEdpoint[sufixo] == "" {
				baseURL := strings.Split(MapOficial[sufixo], "chatbot")[0]
				if strings.Contains(baseURL, "disparo") {
					baseURL = strings.Split(MapOficial[sufixo], "disparo")[0]
				}
				urlSendMessageEdpoint[sufixo] = baseURL + "chatbot/chat/mensagens/novo-id/"
			}
			SendToEndPoint(data, urlSendMessageEdpoint[sufixo])
		}

	}
	if msgInfo.Focus == "noreply" {
		fmt.Println("Mensagem não deve ser enviada, focus 'noreply'")
		removeMensagemPendente(uuid)

		return
	}
	data := SentMessagePayload{
		Evento:   "MENSAGEM_ENVIADA",
		ClientID: clientId,
		Data: SentMessageData{
			NewID:  retornoEnvio.ID,
			OldID:  uuid,
			Sender: strings.ReplaceAll(msgInfo.Number, "+", ""),
		},
	}
	lastIndex := strings.LastIndex(clientId, "_")
	sufixo := clientId[lastIndex+1:]
	if urlSendMessageEdpoint[sufixo] == "" {
		baseURL := strings.Split(MapOficial[sufixo], "chatbot")[0]
		if strings.Contains(baseURL, "disparo") {
			baseURL = strings.Split(MapOficial[sufixo], "disparo")[0]
		}
		urlSendMessageEdpoint[sufixo] = baseURL + "chatbot/chat/mensagens/novo-id/"
	}

	fmt.Printf("📦 -> MENSAGEM [ID:%s, clientID:%s, mensagem:%s, numero:%s, JID:%s] ENVIADA \n", retornoEnvio.ID, clientId, msgInfo.Text, msgInfo.Number, JID.User)
	removeMensagemPendente(uuid)

	SendToEndPoint(data, urlSendMessageEdpoint[sufixo])

	// SEMPRE
	// fmt.Println("ENVIANDO MENSAGEM PEW PEW", clientId, msgInfo, JID, retornoEnvio)
}

func removeMensagemPendente(uuid string) {
	db := connectToMessagesQueueDB()

	_, err := db.Stmts.DeletePendingByUUID.Exec(uuid)
	if err != nil {
		log.Println("Erro ao remover mensagem 1:", err)
	}
}

func sanitizeNumber(input string) string {
	n := strings.Join(Re0to9.FindAllString(input, -1), "")
	if len(n) > 2 && !strings.HasPrefix(n, "55") {
		return "+55" + n
	}
	return n
}
