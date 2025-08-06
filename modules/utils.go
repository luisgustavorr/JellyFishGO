package modules

import (
	"math/rand"
	"time"
)

func RandomBetween(min, max int) int {
	rand.Seed(time.Now().UnixNano()) // Garante que os números aleatórios mudem a cada execução
	return rand.Intn(max-min+1) + min
}
