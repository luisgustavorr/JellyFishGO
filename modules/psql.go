package modules

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	_ "github.com/lib/pq"

	"golang.org/x/crypto/ssh"
)

func proxyConnection(local, remote net.Conn) {
	go func() {
		defer local.Close()
		defer remote.Close()
		_, _ = io.Copy(local, remote)
	}()
	go func() {
		defer local.Close()
		defer remote.Close()
		_, _ = io.Copy(remote, local)
	}()
}

var DB *sql.DB
var DBMutex sync.Mutex

func ConnectPsql() (*sql.DB, error) {
	DBMutex.Lock()
	defer DBMutex.Unlock()
	if DB != nil {
		return DB, nil
	}
	deployMode := os.Getenv("DEPLOYMODE") // Verifica o modo de deploy

	// Variáveis comuns para ambos os modos
	dbUser := os.Getenv("USER_DB")
	dbPassword := os.Getenv("PASSWORD_DB")
	dbName := os.Getenv("DB_NAME")
	dbHost := os.Getenv("HOST_DB") // Host do banco (público ou interno)
	dbPort := os.Getenv("DB_PORT") // Porta do banco de dados
	if dbPort == "" {
		dbPort = "5432" // Porta padrão se não definida
	}
	var psqlInfo string
	if deployMode == "production" {
		fmt.Println("🚀🔗->> CONEXÃO DIRETA SEM SSH")
		// Modo direto (sem SSH)
		psqlInfo = fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
			dbHost, dbPort, dbUser, dbPassword, dbName)
	} else {
		fmt.Println("🚀🔗->> CONEXÃO USANDO SSH")
		// Modo com SSH
		sshHost := os.Getenv("SSH_DB_HOST")
		sshPort := os.Getenv("SSH_DB_PORT")
		sshUser := os.Getenv("SSH_DB_USER")
		sshPassword := os.Getenv("SSH_DB_PASSWORD")
		localPort := os.Getenv("LOCAL_PORT")
		if localPort == "" {
			localPort = "15438" // Porta local padrão para o túnel
		}

		// Configuração do cliente SSH
		sshConfig := &ssh.ClientConfig{
			User: sshUser,
			Auth: []ssh.AuthMethod{
				ssh.Password(sshPassword),
			},
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         10 * time.Second,
		}

		// Conectar via SSH
		sshConn, err := ssh.Dial("tcp", sshHost+":"+sshPort, sshConfig)
		if err != nil {
			return nil, fmt.Errorf("falha na conexão SSH: %v", err)
		}

		// Criar túnel
		listener, err := net.Listen("tcp", "localhost:"+localPort)
		if err != nil {
			return nil, fmt.Errorf("erro ao criar listener local: %v", err)
		}

		go func() {
			for {
				localConn, err := listener.Accept()
				if err != nil {
					continue
				}

				// Conectar ao banco via SSH (host interno)
				remoteConn, err := sshConn.Dial("tcp", dbHost+":"+dbPort)
				if err != nil {
					localConn.Close()
					continue
				}

				go proxyConnection(localConn, remoteConn)
			}
		}()

		time.Sleep(1 * time.Second) // Espera estabilização do túnel

		// String de conexão via túnel
		psqlInfo = fmt.Sprintf("host=localhost port=%s user=%s password=%s dbname=%s sslmode=disable",
			localPort, dbUser, dbPassword, dbName)
	}

	// Conexão com o banco (comum para ambos os modos)
	db, err := sql.Open("postgres", psqlInfo)
	if err != nil {
		return nil, fmt.Errorf("erro ao abrir conexão com PostgreSQL: %v", err)
	}

	// Testar conexão
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("ping ao banco de dados falhou: %v", err)
	}
	// Configurações do pool de conexões
	db.SetMaxOpenConns(75)
	// db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)
	DB = db
	return DB, nil
}
