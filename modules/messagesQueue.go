package modules

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
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

//OFICIAL

// OFICIAL
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
type RelatorioMensagensPayload struct {
	clientId   string
	OldID      string `json:"older_id"`
	Id_grupo   string `json:"slug_disparo"`
	Id_message string `json:"newer_id"`
	Number     string `json:"whatsapp"`
	Err        string `json:"erros"`
	Ok         string `json:"sucessos"`
	Data       string `json:"data_hora"`
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
	IdBatch           string          `json:"idbatch"`
	Quoted_message    *Quoted_message `json:"quotedMessage,omitempty"`
	Edited_id_message string          `json:"editedIDMessage"`
	Send_contact      *Send_contact   `json:"sendContact,omitempty"`
	Documento_padrao  string          `json:"documento_padrao"`
	Attempts          int32           `json:"attempts"`
	Typing_duration   int32           `json:"typing_duration"`
}

var dbMensagensPendentes *Database
var actions BasicActions
var urlSendMessageEdpoint = map[string]string{}
var sendMessageMutex = make(map[string]*sync.Mutex)
var sendMessageMutexLock sync.Mutex // protects access to the map itself

func getClientMutex(clientId string) *sync.Mutex {
	sendMessageMutexLock.Lock()
	defer sendMessageMutexLock.Unlock()

	m, ok := sendMessageMutex[clientId]
	if !ok {
		m = &sync.Mutex{}
		sendMessageMutex[clientId] = m
	}
	return m
}

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
	GetPendingByUUID         *sql.Stmt
	DeletePendingByUUID      *sql.Stmt
	InsertUUID               *sql.Stmt
	selectPendingStmt        *sql.Stmt
	selectPendingByGroupStmt *sql.Stmt
	selectTypingMessages     *sql.Stmt
}

type Database struct {
	DB    *sql.DB
	Stmts Statements
}

var JsonBufferPool = sync.Pool{
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
		       edited_id_message, focus, id_grupo, send_contact,attempts,idbatch
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
	selectPendingUUIDByGroupStmt, err := db.Prepare(`
	SELECT coalesce( COUNT(uuid),0)
  FROM pendingMessages p
  WHERE 
  data_desejada >= $1
  and agendada = false
    AND indev = $2
    AND attempts < $3
    AND should_try_again = true
    AND p.idbatch = $4
group by p.idbatch
limit 1
`)
	if err != nil {
		log.Fatal(err)

	}
	selectTypingMessages, err := db.Prepare(`
	WITH cte AS (
SELECT clientId, number,typing_duration,uuid FROM pendingMessages
		WHERE data_desejada-typing_duration <= $1 AND indev = $2 AND typing = false and should_try_again = true AND attempts < $3
		  LIMIT $3
)
UPDATE pendingMessages p
SET typing = true,status = 'Typing...'
FROM cte
WHERE p.uuid = cte.uuid
RETURNING p.clientId, p.number,p.typing_duration,p.uuid,p.id_grupo;
`)
	if err != nil {
		log.Fatal(err)

	}
	insertStmt, err := db.Prepare(`
	INSERT INTO pendingMessages (
		uuid, idBatch, clientId, text, number, data_desejada, documento_padrao, 
		quoted_message, edited_id_message, focus, id_grupo, send_contact,indev,should_try_again,agendada,attempts,typing_duration)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,$13,true,false,0,$14)
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
		attempts = EXCLUDED.attempts,
		typing_duration = EXCLUDED.typing_duration;
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
	dbMensagensPendentes.Stmts.selectTypingMessages = selectTypingMessages
	dbMensagensPendentes.Stmts.GetPendingByUUID = selectStmt
	dbMensagensPendentes.Stmts.DeletePendingByUUID = deleteStmt
	dbMensagensPendentes.Stmts.InsertUUID = insertStmt
	dbMensagensPendentes.Stmts.selectPendingStmt = selectPendingStmt
	dbMensagensPendentes.Stmts.selectPendingByGroupStmt = selectPendingUUIDByGroupStmt
	dbMensagensPendentes.DB = db
	return dbMensagensPendentes
}
func insertMessage(clientId string, idBatch string, idMessage string, msgInfo MessageIndividual, data_desejada int64, send_contact *Send_contact) {
	db := connectToMessagesQueueDB()

	buf := JsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer JsonBufferPool.Put(buf)
	var err error
	jsonStringOfQuotedMessage := "{}"
	if msgInfo.Quoted_message != nil {
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(msgInfo.Quoted_message); err == nil {
			jsonStringOfQuotedMessage = strings.Clone(buf.String())
		}
	}
	jsonStringOfSendContact := "{}"
	if send_contact != nil {
		buf.Reset()
		if err := json.NewEncoder(buf).Encode(send_contact); err == nil {
			jsonStringOfSendContact = strings.Clone(buf.String())
		}
	}
	fmt.Println(jsonStringOfQuotedMessage)
	_, err = db.Stmts.InsertUUID.Exec(idMessage, idBatch, clientId, msgInfo.Text, msgInfo.Number, data_desejada, msgInfo.Documento_padrao, jsonStringOfQuotedMessage, msgInfo.Edited_id_message, msgInfo.Focus, msgInfo.Id_grupo, jsonStringOfSendContact, Desenvolvimento, msgInfo.Typing_duration)
	if err != nil {
		log.Println("Erro ao adicionar mensagem:", err)
	}

}
func GetTypingMessages() error {
	db := connectToMessagesQueueDB()
	now := time.Now().Unix()
	rows, err := db.Stmts.selectTypingMessages.Query(now, Desenvolvimento, pollBatchSizeDefault)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var clientId string
		var number string
		var typing_duration int32
		var uuid string
		var id_grupo string
		err := rows.Scan(&clientId, &number, &typing_duration, &uuid, &id_grupo)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		fmt.Println("Para digitar : ", clientId, number, typing_duration)
		client := actions.GetClient(clientId)
		validNumber, err := actions.CheckNumberWithRetry(client, number, id_grupo != "", clientId)
		var JID types.JID
		if err != nil {
			fmt.Println(err, "ERRO ISONWHATSAPP")
			fmt.Println("⛔ -> Número inválido Erro. ClientId: ", clientId, " | Número: ", number)
			return err
		}
		if len(validNumber) == 0 {
			err = fmt.Errorf("Número inválido\n")
			fmt.Println("⛔ -> Número inválido. ClientId: ", clientId, " | Número: ", number)
			return err
		}
		response := validNumber[0] // Acessa o primeiro item da slicet
		JID = response.JID
		IsIn := response.IsIn
		if !IsIn {
			err = fmt.Errorf("Número não está no WhatsApp\n")
			fmt.Println("⛔ -> Número not In WhatsApp. ClientId: ", clientId, " | Número: ", number)
			return err
		}
		SetStatus(client, "digitando", JID)
	}
	return nil
}
func AddMensagemPendente(sendInfo SendMessageInfo) {
	now := time.Unix(int64(sendInfo.DataProgramada), 0).Unix()
	var lastDuration int32 = 0

	for i, v := range sendInfo.Result {
		v.Documento_padrao = sendInfo.Documento_padrao
		customDelay := RandomBetween(1, 4)
		// v.Text += "_" + strconv.Itoa(i)
		id_message := v.UUID
		if id_message == "" {
			id_message = sendInfo.IdBatch + strconv.Itoa(i)
		}
		if lastDuration == 0 && len(sendInfo.Result) > 1 {
			text := v.Text
			textLen := float64(len(text)) * 0.05
			if textLen > 20 {
				textLen = 20
			} else if textLen < 5 {
				textLen = 2
			}
			lastDuration = int32(textLen)
			now += int64(textLen + 1)
		}
		v.Typing_duration = lastDuration
		insertMessage(sendInfo.ClientIdLocal, sendInfo.IdBatch, id_message, v, now, sendInfo.SendContact)
		if len(sendInfo.Result) > i+1 {
			nextItem := sendInfo.Result[i+1]
			totalDelay := RandomBetween(30, 45)
			text := nextItem.Text
			textLen := float64(len(text)) * 0.05
			if textLen > 20 {
				textLen = 20
			} else if textLen < 5 {
				textLen = 2
			}
			lastDuration = int32(textLen)
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
	buf := JsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer JsonBufferPool.Put(buf)

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
	if resp.StatusCode == 403 {
		body := []byte{}
		if resp != nil {
			body, err = io.ReadAll(resp.Body)
			if err != nil {
				return
			}
		}

		fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | message : ", string(body), " | evento : ", data.Evento, " | clientId :", data.ClientID)
		return
	}
	fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | evento : ", data.Evento, " | clientId :", data.ClientID)
}
func SendRelatorioToEndPoint(data RelatorioMensagensPayload, url string) {
	buf := JsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer JsonBufferPool.Put(buf)

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
	fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | evento : ", "Relatório enviado", " | clientId :", data.clientId)
}
func SendGenericToEndPoint(data GenericPayload, url string) {
	buf := JsonBufferPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer JsonBufferPool.Put(buf)

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
	if resp.StatusCode == 403 {
		body := []byte{}
		if resp != nil {
			body, err = io.ReadAll(resp.Body)
			if err != nil {
				return
			}
		}

		fmt.Println("🌐 -> Resposta Status: [", resp.Status, "] | message : ", string(body), " | evento : ", data.Evento, " | clientId :", data.ClientID)
		return
	}
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

	for range numWorkers {
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
				GetTypingMessages()

			case <-pollStop:
				return
			}
		}
	}()
	// Initial immediate poll
	pollDueMessages(batchSize)
	GetTypingMessages()

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
func GetRemainingMessages(idBatch string) int {
	db := connectToMessagesQueueDB()
	count := 0
	err := db.Stmts.selectPendingByGroupStmt.QueryRow(time.Now().Unix(), Desenvolvimento, maxRetries, idBatch).Scan(&count)
	if err != nil {
		log.Println("Erro ao buscar mensagens faltando:", err)
		return count
	}
	fmt.Println(count)

	return count
}
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
	if !shouldTryAgain {
		attempts = maxRetries
	}
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
  SET last_error = $1,status='Final Error, Will be deleted',attempts = $2,should_try_again=$3 WHERE uuid= $4`, lastError, attempts, false, uuid)
		if err != nil {
			fmt.Println(err)
			return err
		}
	}

	return nil
}
func changeFileName(idbatch string, fileName string) error {
	db := connectToMessagesQueueDB()
	_, err := db.DB.Exec(`UPDATE pendingMessages
  SET documento_padrao = $1 WHERE idbatch = $2`, fileName, idbatch)
	if err != nil {
		return err

	}
	return nil
}

func CleanUploads() { // limpar arquivos do uploads
	dir := "./uploads/"
	arquivosAindaSalvos := GetFilesToBeSent()
	arquivosNaPasta, _ := GetContents(dir)
	diff := Difference(arquivosNaPasta, arquivosAindaSalvos)
	fmt.Println(arquivosNaPasta, arquivosAindaSalvos, diff)
	for _, name := range diff {
		err := os.RemoveAll(filepath.Join(dir, name))
		if err != nil {
		}
	}
	// RemoveContents("./uploads/")
}
func enviarMensagem(uuid string) {

	db := connectToMessagesQueueDB()
	mainCtx := context.Background()
	// ctx := context.Background()
	// 01-mes 02-dia 2006-ano 03-hora 04-min 05-seg
	ctxWT, cancel := context.WithTimeout(mainCtx, 12*time.Second)
	defer cancel()
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
		&msgInfo.IdBatch,
	)

	if err != nil {
		if err == sql.ErrNoRows {
			log.Println("UUID not found:", uuid)
			return
		}
		log.Println("Scan error:", err)
		return
	}
	clientMutex := getClientMutex(clientId)
	clientMutex.Lock()
	defer clientMutex.Unlock()
	msgInfo.Number = SanitizeNumber(msgInfo.Number)
	client := actions.GetClient(clientId)
	defer SetStatus(client, "conectado", types.JID{})
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
		fmt.Println("[", uuid, "]-> Arquivo : ", msgInfo.Documento_padrao)
		message, newNameFile, extraMessage = PrepararMensagemArquivo(msgInfo.Text, message, msgInfo.Documento_padrao, client, clientId, msg, uuid)

	}
	var retornoEnvio = whatsmeow.SendResponse{}
	if msgInfo.Focus == "disparo_mensagens" {
		defer func() {
			fmt.Println("Devolver relatório do disparo", err)
			errMessage := ""
			okMessage := "Mensagem enviada com sucesso !"
			if err != nil {
				errMessage = err.Error()
				okMessage = ""
			}
			data := RelatorioMensagensPayload{
				clientId:   clientId,
				Id_grupo:   msgInfo.IdBatch,
				OldID:      uuid,
				Id_message: retornoEnvio.ID,
				Number:     strings.ReplaceAll(msgInfo.Number, "+", ""),
				Ok:         okMessage,
				Err:        errMessage,
				Data:       time.Now().Format("2006-01-02T03:04:05"),
			}
			lastIndex := strings.LastIndex(clientId, "_")
			sufixo := clientId[lastIndex+1:]
			urlToSend := ""
			baseURL := strings.Split(MapOficial[sufixo], "chatbot")[0]
			if strings.Contains(baseURL, "disparo") {
				baseURL = strings.Split(MapOficial[sufixo], "disparo")[0]
			}
			urlToSend = baseURL + "disparo/v2/retorno-disparo/"
			fmt.Println("ULR Q TA MANDANDO : ", urlToSend, sufixo)
			removeMensagemPendente(uuid)

			SendRelatorioToEndPoint(data, urlToSend)
		}()
	}
	if id_grupo != "" {
		JID = types.JID{User: strings.ReplaceAll(id_grupo, "@g.us", ""), Server: types.GroupServer}
	} else {
		if err != nil {
			fmt.Println(err, "ERRO ISONWHATSAPP")
			fmt.Println("⛔ -> Número inválido Erro. ClientId: ", clientId, " | Número: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
			updateLastError(uuid, true, msgInfo.Attempts, fmt.Sprintln("⛔ -> Número inválido Erro: ", err))
			return
		}
		if len(validNumber) == 0 {
			err = fmt.Errorf("Número inválido\n")
			fmt.Println("⛔ -> Número inválido. ClientId: ", clientId, " | Número: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
			updateLastError(uuid, false, msgInfo.Attempts, fmt.Sprintln("⛔ -> Número inválido. ClientId: ", clientId, " | Número: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo))
			return
		}
		response := validNumber[0] // Acessa o primeiro item da slicet
		JID = response.JID
		IsIn := response.IsIn
		if !IsIn {
			err = fmt.Errorf("Número não está no WhatsApp\n")
			fmt.Println("⛔ -> Número not In WhatsApp. ClientId: ", clientId, " | Número: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
			updateLastError(uuid, false, msgInfo.Attempts, fmt.Sprintln("⛔ -> Número not In WhatsApp. ClientId: ", clientId, " | Número: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo))

			return
		}
	}

	SetStatus(client, "digitando", JID)
	defer SetStatus(client, "conectado", JID)
	if msgInfo.Quoted_message != nil && msgInfo.Quoted_message.Quoted_sender != "" && msgInfo.Quoted_message.Quoted_message_id != "" {
		validNumber, err := actions.CheckNumberWithRetry(client, SanitizeNumber(msgInfo.Quoted_message.Quoted_sender), id_grupo != "", clientId)

		if err == nil && len(validNumber) > 0 && validNumber[0].JID != types.EmptyJID {
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
		} else {
			// err = fmt.Errorf("Número da mensagem marcada inválido! Erro :%s", err.Error())
		}
	}
	if msgInfo.Edited_id_message != "" {
		message = client.BuildEdit(JID, msgInfo.Edited_id_message, message)
	}
	sendContact := msgInfo.Send_contact
	if sendContact != nil && sendContact.Contato != "" && sendContact.Nome != "" {
		// Deserializando o JSON corretamente para o map
		validNumber, err := actions.CheckNumberWithRetry(client, SanitizeNumber(sendContact.Contato), id_grupo != "", clientId)
		if err == nil && len(validNumber) > 0 && validNumber[0].JID != types.EmptyJID {
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
		} else {
			err = fmt.Errorf("Número do contato inválido ! Erro :%s", err.Error())
		}
	}
	retornoEnvio, err = client.SendMessage(ctxWT, JID, message)
	if err != nil {
		updateLastError(uuid, true, msgInfo.Attempts, fmt.Sprintln("Erro ao enviar mensagem 1°: ", err))

		fmt.Println("Erro ao enviar mensagem", err)
		return
	} else {
		if newNameFile != "" {
			changeFileName(msgInfo.IdBatch, newNameFile)
			defer func(idbatch string) {
				if GetRemainingMessages(idbatch) == 0 {
					os.Remove(newNameFile)

				}

			}(msgInfo.IdBatch)
		}

		//6PF365PCL6MN5UUFBAAORJUPQ_chat3d0e59e6-ccef-43ee-ab2b-084dbeb0878e_!-!_Captura de tela de 2025-09-01 20-49-57
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
	fmt.Println("[", uuid, "]📦 -> MENSAGEM [ID:%s, clientID:%s, mensagem:%s, numero:%s, JID:%s] ENVIADA \n", retornoEnvio.ID, clientId, msgInfo.Text, msgInfo.Number, JID.User)
	if msgInfo.Focus == "noreply" {
		fmt.Println("Mensagem não deve ser enviada, focus 'noreply'")
		removeMensagemPendente(uuid)

		return
	}
	if msgInfo.Focus == "disparo_mensagens" {
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
