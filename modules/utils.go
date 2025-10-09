package modules

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/h2non/filetype"
	"github.com/joho/godotenv"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

func GetCSRFToken() string {
	// Gera um token CSRF aleatório
	rand.Seed(time.Now().UnixNano())
	randomToken := fmt.Sprintf("%x", rand.Int63())
	return randomToken
}

var _ = godotenv.Load()

var MODO_DESENVOLVIMENTO = os.Getenv("MODO_DESENVOLVIMENTO")
var Desenvolvimento = MODO_DESENVOLVIMENTO == "1"
var MapOficial = LoadConfigInicial(os.Getenv("STRING_CONN"))

func LoadConfigInicial(dsn string) map[string]string {
	fmt.Println(dsn)
	// Conectar ao banco de dados
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		log.Println(err)
	}
	defer db.Close()

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
	fmt.Println("MODO DESENVOLVIMENTO", Desenvolvimento)
	if Desenvolvimento {
		return mapDesenvolvimento
	}
	return mapProducao
}
func RandomBetween(min, max int) int {
	rand.Seed(time.Now().UnixNano()) // Garante que os números aleatórios mudem a cada execução
	return rand.Intn(max-min+1) + min
}
func Difference(a, b []string) (diff []string) {
	m := make(map[string]bool)
	for _, item := range b {
		m[item] = true
	}
	for _, item := range a {
		if _, ok := m[item]; !ok {
			diff = append(diff, item)
		}
	}
	return
}
func SetStatus(client *whatsmeow.Client, status string, JID types.JID) {
	if status == "conectado" {
		typePresence := types.PresenceAvailable
		client.SendPresence(typePresence)
		return
	}
	if status == "desconectado" {
		typePresence := types.PresenceUnavailable
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
	err = jpeg.Encode(outFile, img, &jpeg.Options{Quality: 80})
	if err != nil {
		fmt.Println("Erro ao converter para JPEG:", err)
		return err
	}
	return nil
}

var bufferPool = sync.Pool{
	New: func() interface{} {
		s := make([]byte, 0, 2<<20) // 2mb
		return &s                   // Retorna PONTEIRO para slice
	},
}

func converterParaOgg(inputPath string) (string, error) {
	ext := strings.ToLower(filepath.Ext(inputPath))
	if ext != ".mp3" && ext != ".wav" && ext != ".m4a" && ext != ".aac" {
		// Não precisa converter
		return inputPath, nil
	}

	base := strings.TrimSuffix(filepath.Base(inputPath), ext)
	dir := filepath.Dir(inputPath)
	outputPath := filepath.Join(dir, base+".ogg")
	cmd := exec.Command(
		"ffmpeg",
		"-loglevel", "quiet",
		"-y",
		"-i", inputPath,
		"-af", "asetpts=PTS-STARTPTS", // 👈 Zera timestamps de áudio
		"-c:a", "libopus",
		"-b:a", "16k",
		"-vbr", "on",
		"-compression_level", "10",
		"-ar", "16000",
		"-ac", "1",
		"-f", "ogg",
		"-avoid_negative_ts", "make_zero",
		outputPath,
	)

	if err := cmd.Run(); err != nil {
		return inputPath, fmt.Errorf("ffmpeg failed: %w", err)
	}

	os.Remove(inputPath)

	return outputPath, nil
}
func getAudioDuration(path string) (float64, error) {
	cmd := exec.Command(
		"ffprobe",
		"-v", "quiet",
		"-print_format", "json",
		"-show_entries", "format=duration",
		"-i", path,
	)

	var out bytes.Buffer
	cmd.Stdout = &out

	if err := cmd.Run(); err != nil {
		return 0, fmt.Errorf("erro ao executar ffprobe: %w", err)
	}

	var result struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}

	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		return 0, fmt.Errorf("erro ao interpretar resposta do ffprobe: %w", err)
	}

	seconds, err := strconv.ParseFloat(result.Format.Duration, 64)
	if err != nil {
		return 0, fmt.Errorf("erro ao converter duração: %w", err)
	}

	return seconds, nil
}

type SingleMessageInfo struct {
	JID         types.JID
	ClientId    string
	Text        string
	IdMensagem  string
	Focus       string
	Number      string
	LastError   error
	Context     context.Context
	MessageInfo *waE2E.Message
	Client      *whatsmeow.Client
	Attempts    int32
}

func convertPngToJpeg(pngPath string) (string, error) {
	f, err := os.Open(pngPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		return "", err
	}

	// Prepare output image (RGB)
	bounds := img.Bounds()
	outImg := image.NewRGBA(bounds)

	// Fill background with white
	draw.Draw(outImg, bounds, &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	// Draw original PNG on top
	draw.Draw(outImg, bounds, img, bounds.Min, draw.Over)

	outPath := pngPath[:len(pngPath)-len(".png")] + ".jpg"
	outFile, err := os.Create(outPath)
	if err != nil {
		return "", err
	}
	defer outFile.Close()

	opt := jpeg.Options{Quality: 90}
	if err := jpeg.Encode(outFile, outImg, &opt); err != nil {
		return "", err
	}
	os.Remove(pngPath)

	return outPath, nil
}
func PrepararMensagemArquivo(text string, message *waE2E.Message, chosedFile string, client *whatsmeow.Client, clientId string, msg SingleMessageInfo, UUID string) (messageBuilt *waE2E.Message, newPath string, shouldSendExtraMessage bool) {
	// Abrindo o arquivo
	var mensagem_ *waE2E.Message
	var extraMessageNeeded = false
	if message != nil {
		mensagem_ = proto.Clone(message).(*waE2E.Message)
	} else {
		mensagem_ = &waE2E.Message{}
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("🔥 panic in prepararMensagemArquivo: %v", r)
			debug.PrintStack()
		}
	}()
	if filepath.Ext(chosedFile) == ".mp3" || filepath.Ext(chosedFile) == ".ogg" {

		var err error
		chosedFile, err = converterParaOgg(chosedFile)
		if err != nil {
			log.Println("Falha ao converter:", err)
			return mensagem_, chosedFile, extraMessageNeeded
		}

	}
	if filepath.Ext(chosedFile) == ".png" {
		fmt.Println("📁🔄 -> Convertendo arquivo PNG para JPGEG")
		newFile, err := convertPngToJpeg(chosedFile)
		if err != nil {
			log.Println("Failed to convert PNG to JPEG:", err)
			return mensagem_, chosedFile, extraMessageNeeded

		}
		chosedFile = newFile
	}
	file, err := os.Open(chosedFile)
	if err != nil {
		fmt.Printf("Erro ao abrir o arquivo: %v", err)
		return mensagem_, chosedFile, extraMessageNeeded

	}
	defer file.Close()
	// Detectando o tipo MIME
	bufPtr := bufferPool.Get().(*[]byte)
	var buf []byte // outer buf variable

	if bufPtr != nil {
		buf = *bufPtr
		defer func() {
			*bufPtr = buf[:0]
			bufferPool.Put(bufPtr)
		}()
	} else {
		buf = make([]byte, 0) // fallback empty slice
	}
	defer func() {
		*bufPtr = buf[:0]
		bufferPool.Put(bufPtr)
	}()
	fileInfo, err := file.Stat()
	if err != nil {
		log.Printf("failed to stat file %s: %v", chosedFile, err)
		return mensagem_, chosedFile, extraMessageNeeded
	}
	fileSize := fileInfo.Size()
	if cap(buf) < int(fileSize) {
		buf = make([]byte, fileSize)
	} else {
		// Reutiliza capacidade existente
		buf = buf[:fileSize]
	}
	_, err = io.ReadFull(file, buf)
	if err != nil {
		fmt.Printf("Erro ao ler o arquivo: %v", err)
		return mensagem_, chosedFile, extraMessageNeeded

	}
	nomeArquivo := filepath.Base(chosedFile)
	nomeArquivo = strings.ReplaceAll(nomeArquivo, clientId, "")
	limit := 512
	if len(buf) < limit {
		limit = len(buf)
	}
	kind, err := filetype.Match(buf[:limit])
	if err != nil {
		fmt.Printf("Erro ao detectar tipo do arquivo: %v", err)
		return mensagem_, "", extraMessageNeeded
	}
	if kind == filetype.Unknown {
		fmt.Println("Tipo de arquivo desconhecido")
	}
	mimeType := ""
	ext := strings.ToLower(filepath.Ext(nomeArquivo))
	switch ext {
	case ".jpg", ".jpeg":
		mimeType = "image/jpeg"
	case ".png":
		mimeType = "image/png"
	case ".webp":
		mimeType = "image/webp"
	case ".mp3":
		mimeType = "audio/mpeg"
	case ".ogg":
		mimeType = "audio/ogg; codecs=opus" // VERY important for iOS
	default:
		mimeType = kind.MIME.Value // fallback
	}
	fmt.Println("📁 O arquivo é um :", mimeType, " com final ", filepath.Ext(nomeArquivo))

	mensagem_.ExtendedTextMessage = nil
	semExtensao := strings.TrimSuffix(nomeArquivo, filepath.Ext(nomeArquivo))
	explodedName := strings.Split(nomeArquivo, "_!-!_")
	semExtensao = explodedName[len(explodedName)-1]
	if filetype.IsAudio(buf) || filepath.Ext(nomeArquivo) == ".mp3" {
		fmt.Println("REsultado ogg", chosedFile)
		mimeType = "audio/ogg; codecs=opus"
		fmt.Println("Arquivo é um áudio")
		resp, err := client.Upload(context.Background(), buf, whatsmeow.MediaAudio)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		duration, err := getAudioDuration(chosedFile)
		if err != nil {
			log.Println("Erro ao pegar duração:", err)
			duration = 0
		}
		imageMsg := &waE2E.AudioMessage{
			Mimetype:          proto.String(mimeType),
			PTT:               proto.Bool(true),
			URL:               &resp.URL,
			DirectPath:        &resp.DirectPath,
			MediaKey:          resp.MediaKey,
			Seconds:           proto.Uint32(uint32(duration)),
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
			FileEncSHA256:     resp.FileEncSHA256,
			FileSHA256:        resp.FileSHA256,
			FileLength:        proto.Uint64(resp.FileLength),
		}
		extraMessageNeeded = true
		// if text != "" {
		// 	if mensagem_.ExtendedTextMessage == nil {
		// 		mensagem_.ExtendedTextMessage = &waE2E.ExtendedTextMessage{}
		// 	}
		// 	mensagem_.ExtendedTextMessage.Text = proto.String(text)
		// 	msg.MessageInfo = mensagem_
		// 	processarMensagem(msg, UUID)
		// }
		mensagem_.ExtendedTextMessage = nil
		mensagem_.AudioMessage = imageMsg

	} else if filetype.IsImage(buf) {
		resp, err := client.Upload(context.Background(), buf, whatsmeow.MediaImage)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		fmt.Println("O arquivo é uma imagem válida.")
		imageMsg := &waE2E.ImageMessage{
			Caption:           proto.String(text),
			Mimetype:          proto.String(mimeType),
			URL:               &resp.URL,
			DirectPath:        &resp.DirectPath,
			MediaKey:          resp.MediaKey,
			FileEncSHA256:     resp.FileEncSHA256,
			FileSHA256:        resp.FileSHA256,
			FileLength:        &resp.FileLength,
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),
		}
		mensagem_.ImageMessage = imageMsg
		mensagem_.ExtendedTextMessage = nil
	} else if filetype.IsVideo(buf) {
		resp, err := client.Upload(context.Background(), buf, whatsmeow.MediaVideo)
		if err != nil {
			fmt.Printf("Erro ao fazer upload da mídia: %v", err)
		}
		imageMsg := &waE2E.VideoMessage{
			Caption:           proto.String(text),
			Mimetype:          proto.String(mimeType),
			URL:               &resp.URL,
			DirectPath:        &resp.DirectPath,
			MediaKey:          resp.MediaKey,
			FileEncSHA256:     resp.FileEncSHA256,
			FileSHA256:        resp.FileSHA256,
			FileLength:        &resp.FileLength,
			GifPlayback:       proto.Bool(false),
			MediaKeyTimestamp: proto.Int64(time.Now().Unix()),

			//JPEGThumbnail: thumbnailBuf.Bytes(), // removed for this example
		}
		mensagem_.VideoMessage = imageMsg
	} else {
		resp, err := client.Upload(context.Background(), buf, whatsmeow.MediaDocument)
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
	fmt.Println(mensagem_)
	return mensagem_, chosedFile, extraMessageNeeded
}

func FormatPhoneNumber(phone string) string {
	// Remove caracteres não numéricos
	re := regexp.MustCompile(`\D`)
	phone = re.ReplaceAllString(phone, "")

	// Verifica se o número tem o código do país correto
	if len(phone) < 12 || len(phone) > 13 {
		return "Número inválido"
	}
	formatted := "+" + phone[:2] + " " + phone[2:4] + " "
	// Verifica se é um número de celular (tem o nono dígito)
	if len(phone) == 13 {
		formatted += phone[4:5] + " " + phone[5:9] + "-" + phone[9:]
	} else {
		formatted += phone[4:8] + "-" + phone[8:]
	}
	return formatted
}
