package modules

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

var verifiedNumbers *sql.DB

func criarDBCache() *sql.DB {
	var err error
	if verifiedNumbers != nil {
		return verifiedNumbers
	}
	verifiedNumbers, err = sql.Open("sqlite3", "./verifiedNumbers.db")
	if err != nil {
		log.Fatal(err)
	}
	verifiedNumbers.SetMaxOpenConns(5) // Limite de conexões
	verifiedNumbers.SetMaxIdleConns(5)
	verifiedNumbers.SetConnMaxLifetime(time.Hour)
	createTableSQL := `CREATE TABLE IF NOT EXISTS verifiedNumbers (
		raw_number TEXT UNIQUE,
		found_number TEXT,
		found_server TEXT,
		clientId TEXT,
		expires_in INTEGER
	);`
	_, err = verifiedNumbers.Exec(createTableSQL)
	if err != nil {
		log.Fatal("Erro ao criar TABELA", err)
	}
	_, err = verifiedNumbers.Exec(`CREATE INDEX IF NOT EXISTS raw_number_x ON verifiedNumbers(raw_number);`)
	if err != nil {
		log.Fatal("Erro ao criar índice ", err)
	}
	return verifiedNumbers
}

func FindNumberInCache(raw_number string) (found bool, number_found string, server_found string) {
	criarDBCache()
	rows, err := verifiedNumbers.Query(
		`SELECT found_number,found_server FROM verifiedNumbers 
	 WHERE raw_number = ? LIMIT 1`,
		raw_number,
	)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer rows.Close()

	var found_number string
	var found_server string
	for rows.Next() {
		err := rows.Scan(&found_number, &found_server)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		fmt.Println("Número em cache achado : ", found_number)
		if found_number == "" && found_server == "" {
			return true, found_number, found_server

		}
		return true, found_number, found_server

	}
	return false, "", found_server
}
func FindRandomNumberInCache(client_id string) (found bool, number_found string, server_found string) {
	criarDBCache()
	rows, err := verifiedNumbers.Query(
		`SELECT found_number,found_server FROM verifiedNumbers 
	 WHERE clientId = ? AND found_number != '' AND found_server != ''   ORDER BY RANDOM() LIMIT 1;`,
		client_id,
	)
	if err != nil {
		log.Fatal(err)
		return
	}
	defer rows.Close()

	var found_number string
	var found_server string
	for rows.Next() {
		err := rows.Scan(&found_number, &found_server)
		if err != nil {
			log.Println("Erro no scan:", err)
			continue
		}
		fmt.Println("Número em cache achado : ", found_number)
		return true, found_number, found_server

	}
	return false, "", found_server
}
func SaveNumberInCache(raw_number string, found_number string, server string, client_id string, isOnWhatsapp bool) {
	criarDBCache()
	// limite := time.Now().Unix()
	if !isOnWhatsapp {
		found_number = ""
		server = ""
	}
	limite := time.Now().Add((30 * 24) * time.Hour).Unix()
	_, err := verifiedNumbers.Exec(`INSERT INTO verifiedNumbers (raw_number ,found_number ,found_server,
		clientId ,
		expires_in 	) VALUES (?,?,?,?,?) ON CONFLICT (raw_number) DO UPDATE SET
		expires_in = EXCLUDED.expires_in
	`, raw_number, found_number, server, client_id, limite)
	if err != nil {
		fmt.Println("Erro no save number : ", err)
	}
	fmt.Println("Números adicionados:")
}
func RemoveExpiredCaches() {
	criarDBCache()

	_, err := verifiedNumbers.Exec(
		`DELETE FROM verifiedNumbers 
	 WHERE expires_in <= ?`,
		time.Now().Unix(),
	)
	if err != nil {
		fmt.Println("Erro no save number : ", err)
	}
}
