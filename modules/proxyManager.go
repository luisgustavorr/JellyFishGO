package modules

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"
)

var mysqlConnection *sql.DB

type ProxysServers struct {
	Name        string `json:"name"`
	ID          int    `json:"id"`
	User        string `json:"user"`
	Password    string `json:"password"`
	URL         string `json:"url"`
	MaxConns    int8   `json:"max_conns"`
	ActiveConns int8   `json:"active_conns"`
}

func connectMysql() *sql.DB {
	return nil
	if mysqlConnection != nil {
		return mysqlConnection
	}
	db, err := sql.Open("mysql", os.Getenv("STRING_CONN"))
	if err != nil {
		log.Println("failed to open MySQL:", err)

	}

	// Configure pooling
	db.SetMaxOpenConns(20)           // max open connections
	db.SetMaxIdleConns(10)           // max idle connections
	db.SetConnMaxLifetime(time.Hour) // recycle after 1h
	db.SetConnMaxIdleTime(10 * time.Minute)

	// Test connection
	if err := db.Ping(); err != nil {
		log.Println("failed to connect to MySQL:", err)
	}

	return db
}
func GetAvaiableServer(clientId string) (serverFound ProxysServers, found bool) {
	return ProxysServers{}, false

	if mysqlConnection == nil {
		mysqlConnection = connectMysql()
	}
	if err := mysqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		mysqlConnection = connectMysql()
	}
	rows, err := mysqlConnection.Query("SELECT * FROM proxy_servers WHERE max_conns > active_conns ORDER BY active_conns ASC LIMIT 1")
	if err != nil {
		log.Println(err)
	}
	defer rows.Close()
	var server = ProxysServers{}
	defer updateActiveConns()
	for rows.Next() {
		if err := rows.Scan(&server.ID, &server.Name, &server.User, &server.Password, &server.URL, &server.MaxConns, &server.ActiveConns); err != nil {
			log.Println(err)
			return ProxysServers{}, false
		}
		if server.ID != 0 {
			addProxyToClientId(clientId, server.ID)
			return server, true
		}

	}
	return server, false
}
func updateActiveConns() {
	if mysqlConnection == nil {
		mysqlConnection = connectMysql()
	}
	if err := mysqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		mysqlConnection = connectMysql()
	}
	_, err := mysqlConnection.Exec(`UPDATE proxy_servers ps
SET ps.active_conns = (
    SELECT COUNT(cp.id)
    FROM clients_in_proxy cp
    WHERE cp.proxy_id = ps.id
)
WHERE EXISTS (
    SELECT 1
    FROM clients_in_proxy cp
    WHERE cp.proxy_id = ps.id
);
`)
	if err != nil {
		log.Println(err)
	}
}
func addProxyToClientId(clientId string, proxyId int) bool {
	if mysqlConnection == nil {
		mysqlConnection = connectMysql()
	}
	if err := mysqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		mysqlConnection = connectMysql()
	}
	_, err := mysqlConnection.Exec(`INSERT INTO spacemid_sistem_adm.clients_in_proxy (proxy_id,client_id) VALUES (?,?);`, proxyId, clientId)
	if err != nil {
		log.Println(err)
	}

	return true
}
func RemoveProxyToClientId(clientId string) bool {
	if mysqlConnection == nil {
		mysqlConnection = connectMysql()
	}
	if err := mysqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		mysqlConnection = connectMysql()
	}
	_, err := mysqlConnection.Exec(`DELETE FROM spacemid_sistem_adm.clients_in_proxy WHERE client_id = ? `, clientId)
	if err != nil {
		log.Println(err)
	}
	fmt.Println("PROXYS REMOVIDAS !!!!")
	updateActiveConns()
	return true
}
func GetServerByClientId(clientId string) (serverFound ProxysServers, found bool) {
	if mysqlConnection == nil {
		mysqlConnection = connectMysql()
	}
	if err := mysqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		mysqlConnection = connectMysql()
	}
	rows, err := mysqlConnection.Query("SELECT ps.* FROM proxy_servers ps LEFT JOIN clients_in_proxy cp ON cp.proxy_id = ps.id WHERE cp.client_id = ? LIMIT 1", clientId)
	if err != nil {
		log.Println(err)
	}
	defer rows.Close()
	var server = ProxysServers{}

	for rows.Next() {
		if err := rows.Scan(&server.ID, &server.Name, &server.User, &server.Password, &server.URL, &server.MaxConns, &server.ActiveConns); err != nil {
			log.Println(err)
		}
		if server.ID != 0 {
			return server, true
		}

	}

	return GetAvaiableServer(clientId)
}
