package modules

import (
	"database/sql"
	"fmt"
	"log"
)

var psqlConnection *sql.DB

type ProxysServers struct {
	Name        string `json:"name"`
	ID          int    `json:"id"`
	User        string `json:"user"`
	Password    string `json:"password"`
	URL         string `json:"url"`
	MaxConns    int8   `json:"max_conns"`
	ActiveConns int8   `json:"active_conns"`
}

func GetAvaiableServer(clientId string) (serverFound ProxysServers, found bool) {

	if psqlConnection == nil {
		psqlConnection, _ = ConnectPsql()
	}
	if err := psqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		psqlConnection, _ = ConnectPsql()
	}
	rows, err := psqlConnection.Query("SELECT * FROM public.proxy_servers WHERE max_conns > active_conns ORDER BY active_conns ASC LIMIT 1")
	if err != nil {
		log.Println("Erro get avaiable server", err)
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
	if psqlConnection == nil {
		psqlConnection, _ = ConnectPsql()
	}
	if err := psqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		psqlConnection, _ = ConnectPsql()
	}
	_, err := psqlConnection.Exec(`UPDATE proxy_servers
SET active_conns = (
    SELECT COUNT(cp.id)
    FROM clients_in_proxy cp
    WHERE cp.proxy_id = proxy_servers.id
)
WHERE EXISTS (
    SELECT 1
    FROM clients_in_proxy cp
    WHERE cp.proxy_id = id
);
`)
	if err != nil {
		log.Println("Erro update active", err)
	}
}
func addProxyToClientId(clientId string, proxyId int) bool {
	if psqlConnection == nil {
		psqlConnection, _ = ConnectPsql()
	}
	if err := psqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		psqlConnection, _ = ConnectPsql()
	}
	_, err := psqlConnection.Exec(`INSERT INTO clients_in_proxy (proxy_id,client_id) VALUES ($1,$2);`, proxyId, clientId)
	if err != nil {
		log.Println(err)
	}

	return true
}
func RemoveProxyToClientId(clientId string) bool {
	if psqlConnection == nil {
		psqlConnection, _ = ConnectPsql()
	}
	if err := psqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		psqlConnection, _ = ConnectPsql()
	}
	_, err := psqlConnection.Exec(`DELETE FROM clients_in_proxy WHERE client_id = $1 `, clientId)
	if err != nil {
		log.Println(err)
	}
	fmt.Println("PROXYS REMOVIDAS !!!!")
	updateActiveConns()
	return true
}
func GetServerByClientId(clientId string) (serverFound ProxysServers, found bool) {
	if psqlConnection == nil {
		psqlConnection, _ = ConnectPsql()
	}
	if err := psqlConnection.Ping(); err != nil {
		log.Println("MySQL ping failed, reconnecting:", err)
		psqlConnection, _ = ConnectPsql()
	}
	query := `
SELECT ps.*
FROM public.proxy_servers ps
LEFT JOIN public.clients_in_proxy cp
  ON cp.proxy_id = ps.id
WHERE cp.client_id = $1
LIMIT 1;

`
	rows, err := psqlConnection.Query(query, clientId)
	if err != nil {
		log.Println("Erro pegando proxys", err)
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
