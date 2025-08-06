package modules

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"mime/multipart"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type SendMessageInfo struct {
	Result           []MessageIndividual   `json:"result"`
	ClientIdLocal    string                `json:"clientIdLocal"`
	SendContact      string                `json:"sendContact"`
	NoTimeout        string                `json:"noTimeout"`
	DataProgramada   string                `json:"dataProgramada"`
	UUID             string                `json:"uuid"`
	documento_padrao *multipart.FileHeader `json:"-"`
	files            *multipart.FileHeader `json:"-"`
}

type BasicActions struct {
	GetClient            func(clientId string) *whatsmeow.Client
	CheckNumberWithRetry func(client *whatsmeow.Client, number string, de_grupo bool) (resp []types.IsOnWhatsAppResponse, err error)
}
type MessageIndividual struct {
	Text     string `json:"text"`
	Number   string `json:"number"`
	Focus    string `json:"focus"`
	Id_image string `json:"id_image"`
}

var dbMensagensPendentes *sql.DB
var dbMutex sync.Mutex
var actions BasicActions

func connectToMessagesQueueDB() *sql.DB {
	if dbMensagensPendentes != nil {
		return dbMensagensPendentes
	}
	var err error
	dbMensagensPendentes, err = sql.Open("sqlite3", "./schedules.db")
	if err != nil {
		log.Fatal(err)
	}
	dbMensagensPendentes.SetMaxOpenConns(5)
	dbMensagensPendentes.SetMaxIdleConns(5)
	dbMensagensPendentes.SetConnMaxLifetime(time.Hour)
	createTableSQL := `CREATE TABLE IF NOT EXISTS pendingMessages (
	uuid TEXT PRIMARY KEY,
	idBatch TEXT,
	clientId TEXT,
	text TEXT,
	number TEXT,
	focus TEXT,
	agendada BOOLEAN DEFAULT 0,
	data_desejada INTEGER
	);`

	_, err = dbMensagensPendentes.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	dbMensagensPendentes.Exec(`CREATE INDEX IF NOT EXISTS idx_data_desejada ON pendingMessages(data_desejada);`)
	dbMensagensPendentes.Exec("PRAGMA journal_mode = WAL;")

	return dbMensagensPendentes
}
func insertMessage(clientId string, idBatch string, pos int, msgInfo MessageIndividual, data_desejada int64) {
	db := connectToMessagesQueueDB()
	dbMutex.Lock()
	defer dbMutex.Unlock()
	_, err := db.Exec(`INSERT OR REPLACE INTO pendingMessages 
		(uuid,idBatch, clientId, text, number,data_desejada)
		VALUES (?,? ,?, ?, ?,?)`, idBatch+"_"+strconv.Itoa(pos), idBatch, clientId, msgInfo.Text, msgInfo.Number, data_desejada)
	if err != nil {
		log.Println("Erro ao remover mensagem:", err)
	}
}

func AddMensagemPendente(sendInfo SendMessageInfo) {
	now := time.Now().Unix()
	for i, v := range sendInfo.Result {
		customDelay := RandomBetween(1, 4)
		fmt.Println(v)
		v.Text += "_" + strconv.Itoa(i)
		insertMessage(sendInfo.ClientIdLocal, sendInfo.UUID, i, v, now)
		now += int64(customDelay)
		fmt.Println("Adicionando mensagem", i, " Batch :", sendInfo.UUID, "UUID:", sendInfo.UUID+"_"+strconv.Itoa(i))

	}
	SearchNearMessages()
}
func InitMessagesQueue(basicActions BasicActions) {
	actions = basicActions
	db := connectToMessagesQueueDB()
	dbMutex.Lock()
	defer dbMutex.Unlock()
	_, err := db.Exec(`UPDATE pendingMessages
  SET agendada = 0 WHERE agendada != 0`)
	if err != nil {
		log.Println("Erro ao atualizar status agendada:", err)
	}
}
func SearchNearMessages() {
	fmt.Println("Procurando")
	db := connectToMessagesQueueDB()
	dbMutex.Lock()
	rows, err := db.Query(`UPDATE pendingMessages
  SET agendada = 1
  WHERE data_desejada <= ?
  AND agendada = 0
  RETURNING uuid, data_desejada`, time.Now().Add(15*time.Minute).Unix())
	dbMutex.Unlock()
	if err != nil {
		log.Println("Erro ao remover mensagem:", err)
	}
	defer rows.Close()

	var uuid string
	var ts int64
	for rows.Next() {
		fmt.Println("achou")

		err := rows.Scan(&uuid, &ts)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		delay := time.Until(time.Unix(ts, 0))
		if delay <= 0 {
			go enviarMensagem(uuid)
		} else {
			go prepareMessageTimer(uuid, delay)

		}
		fmt.Println("passou", delay)
	}
}
func prepareMessageTimer(uuid string, delay time.Duration) {
	_ = time.AfterFunc(delay, func() {
		go enviarMensagem(uuid)
	})
}

func enviarMensagem(uuid string) {

	db := connectToMessagesQueueDB()
	dbMutex.Lock()
	rows, err := db.Query(
		`SELECT clientId, text, number  FROM pendingMessages 
	 WHERE uuid = ?`,
		uuid,
	)
	dbMutex.Unlock()
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	var clientId string
	msgInfo := MessageIndividual{}
	for rows.Next() {
		err := rows.Scan(&clientId, &msgInfo.Text, &msgInfo.Number)
		if err != nil {
			log.Println("Erro no scan 2:", err)
			continue
		}
		msgInfo.Number = sanitizeNumber(msgInfo.Number)
		client := actions.GetClient(clientId)
		id_grupo := "" //todo PEGAR O ID DO GRUPO
		validNumber, err := actions.CheckNumberWithRetry(client, msgInfo.Number, id_grupo != "")
		var JID types.JID = types.JID{}
		message := &waE2E.Message{Conversation: &msgInfo.Text}
		if id_grupo != "" {
			JID = types.JID{User: strings.Replace(id_grupo, "@g.us", "", -1), Server: types.GroupServer}
		} else {
			if err != nil {
				fmt.Println(err, "ERRO ISONWHATSAPP")
				fmt.Println("⛔ -> Numero inválido Erro. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
				return
			}
			if len(validNumber) == 0 {
				fmt.Println("⛔ -> Numero inválido. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
				return
			}
			response := validNumber[0] // Acessa o primeiro item da slicet
			JID = response.JID
			IsIn := response.IsIn
			if !IsIn {
				fmt.Println("⛔ -> Numero not In WhatsApp. ClientId: ", clientId, " | Numero: ", msgInfo.Number, " | Mensagem :", msgInfo.Text, "| ID Grupo", id_grupo)
				return
			}
		}
		ctx := context.Background()
		_, err = client.SendMessage(ctx, JID, message)
		if err != nil {
			fmt.Println("Erro ao enviar mensagem", err)
		}
		go removeMensagemPendente(uuid)
	}
	// SEMPRE
	// fmt.Println("ENVIANDO MENSAGEM PEW PEW", clientId, msgInfo, JID, retornoEnvio)
}

func removeMensagemPendente(uuid string) {
	db := connectToMessagesQueueDB()
	dbMutex.Lock()
	defer dbMutex.Unlock()
	_, err := db.Exec("DELETE FROM pendingMessages WHERE uuid = ?", uuid)
	if err != nil {
		log.Println("Erro ao remover mensagem:", err)
	}
}

func sanitizeNumber(input string) string {
	re := regexp.MustCompile("[0-9]+")
	n := strings.Join(re.FindAllString(input, -1), "")
	if len(n) > 2 && !strings.HasPrefix(n, "55") {
		return "+55" + n
	}
	return n
}
