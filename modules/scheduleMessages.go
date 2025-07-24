package modules

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type MessageInfos struct {
	Key             string `json:"key"`
	ClientID        string `json:"clientid"`
	InfoObjects     string `json:"InfoObjects"`
	DocumentoPadrao string `json:"documento_padrao"`
	Files           string `json:"files"`
}

type Schedule struct {
	Key  string    `json:"key"`
	When time.Time `json:"when"`
}

var schedulesDB *sql.DB
var messagesDB *sql.DB
var (
	scheduleRegistry = make(map[string]*Schedule)
	scheduleMutex    sync.Mutex
)

func connectToSchedulesDB() *sql.DB {
	var err error
	if schedulesDB != nil {
		return schedulesDB
	}
	schedulesDB, err = sql.Open("sqlite3", "./schedules.db")
	if err != nil {
		log.Fatal(err)
	}
	schedulesDB.SetMaxOpenConns(5) // Limite de conexões
	schedulesDB.SetMaxIdleConns(5)
	schedulesDB.SetConnMaxLifetime(time.Hour)
	createTableSQL := `CREATE TABLE IF NOT EXISTS schedules_keys (
		key TEXT,
		data_desejada INTEGER
	);`
	_, err = schedulesDB.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	_, err = schedulesDB.Exec(`CREATE INDEX IF NOT EXISTS idx_schedules_data ON schedules_keys(data_desejada);`)
	if err != nil {
		log.Fatal("Erro ao criar índice", err)
	}
	return schedulesDB
}
func connectToMessagesDB() *sql.DB {
	fmt.Println("CRIANDO DB MESSAGES")
	var err error
	if messagesDB != nil {
		return messagesDB
	}
	messagesDB, err = sql.Open("sqlite3", "./schedules.db")
	if err != nil {
		log.Fatal(err)
	}
	messagesDB.SetMaxOpenConns(5)
	messagesDB.SetMaxIdleConns(5)
	messagesDB.SetConnMaxLifetime(time.Hour)
	createTableSQL := `CREATE TABLE IF NOT EXISTS schedules_messages (
	key TEXT,
		clientId TEXT,
		infoObjects TEXT,
		documento_padrao TEXT,
		files TEXT
	);`
	_, err = messagesDB.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	_, err = messagesDB.Exec(`CREATE INDEX IF NOT EXISTS idx_schedules_messages_key ON schedules_messages(key);`)
	if err != nil {
		log.Fatal("Erro ao criar índice", err)
	}
	return messagesDB
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
	_, err = io.ReadAll(res.Body)
	if err != nil {
		fmt.Println("erro Enviando sendFiles2", err)

		return
	}
}
func executeSchedule(target_key string) {
	log.Printf("Executando tarefa: %s\n", target_key)
	connectToMessagesDB()
	rows, err := messagesDB.Query(
		`SELECT * FROM schedules_messages 
	 WHERE key = ?`,
		target_key,
	)
	if err != nil {
		log.Fatal(err)
	}
	var key string
	var clientId string
	var infoObjects string
	var documento_padrao string
	var files string
	for rows.Next() {
		err := rows.Scan(&key, &clientId, &infoObjects, &documento_padrao, &files)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		fmt.Printf(`
		Enviar mensagem com informações:
		clientId : %s,
		infoObjects: %s,
		\n`, clientId, infoObjects)
		sendFilesProgramados(clientId, infoObjects, documento_padrao, files)
	}
	deleteSchedule(key)
}
func saveScheduleInMemory(task *Schedule) {
	fmt.Println("Adicionando Schedule para :", task.When.Format(time.RFC1123Z))
	scheduleMutex.Lock()
	defer scheduleMutex.Unlock()

	// já existe? evita duplicata
	if _, exists := scheduleRegistry[task.Key]; exists {
		return
	}
	// calcula quanto tempo até a execução
	delay := time.Until(task.When)
	if delay <= 0 {
		// já passou, executa agora
		go executeSchedule(task.Key)
		return
	}
	// cria o timer
	_ = time.AfterFunc(delay, func() {
		executeSchedule(task.Key)
		// remove do registry após executar
		scheduleMutex.Lock()
		delete(scheduleRegistry, task.Key)
		scheduleMutex.Unlock()
	})
	// salva no mapa
	scheduleRegistry[task.Key] = task
	// opcional: se quiser guardar o timer, precisaria de outro campo
}
func deleteSchedule(key string) {
	var err error
	connectToSchedulesDB()
	_, err = schedulesDB.Exec("DELETE FROM schedules_keys WHERE key = ?", key)
	if err != nil {
		log.Fatal(err)
	}
	_, err = schedulesDB.Exec("DELETE FROM schedules_messages WHERE key = ?", key)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("Removendo Schedule", key)
}

func CreateSchedule(key string, when time.Time, message_info MessageInfos) {
	var err error
	connectToSchedulesDB()
	_, err = schedulesDB.Exec("INSERT INTO schedules_keys (key,data_desejada) VALUES (?,?)", key, when.Unix())
	if err != nil {
		log.Fatal(err)
	}
	addMessageToDB(key, message_info)
}
func addMessageToDB(key string, message_info MessageInfos) {
	var err error
	connectToMessagesDB()
	_, err = schedulesDB.Exec(`INSERT INTO schedules_messages (key ,clientId ,
		infoObjects ,
		documento_padrao ,
		files 	) VALUES (?,?,?,?,?)`, key, message_info.ClientID, message_info.InfoObjects, message_info.DocumentoPadrao, message_info.Files)
	if err != nil {
		log.Fatal(err)
	}
}
func CheckCloseSchedules() {
	connectToSchedulesDB()
	limite := time.Now().Add(30 * time.Minute).Unix()
	rows, err := schedulesDB.Query(
		`SELECT key, data_desejada FROM schedules_keys 
	 WHERE data_desejada <= ?`,
		limite,
	)
	if err != nil {
		log.Fatal(err)
	}
	var key string
	var ts int64
	for rows.Next() {
		err := rows.Scan(&key, &ts)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		schedule := &Schedule{
			Key:  key,
			When: time.Unix(ts, 0),
		}
		saveScheduleInMemory(schedule)
	}
}
