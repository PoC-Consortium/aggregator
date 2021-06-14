package main

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"

	cache "github.com/patrickmn/go-cache"
)

// ClientData stores all miner info
type clientData struct {
	Id       clientID `json:"id"`
	Capacity int64    `json:"capacity"`
	Alias    string   `json:"alias"`
	sync.Mutex
}

type clientID struct {
	IP        string `json:"ip"`
	MinerName string `json:"minerName"`
	Xpu       string `json:"xpu"`
	sync.Mutex
}

var clients *cache.Cache

// UpdateClient refreshed Miner data
func UpdateClient(ip string, minerName string, alias string, xpu string, capacity int64) {
	cd := clientData{
		Id:       clientID{IP: ip, MinerName: minerName, Xpu: xpu},
		Capacity: capacity,
		Alias:    alias,
		Mutex:    sync.Mutex{},
	}
	key := hash(&cd.Id)
	clients.SetDefault(key, &cd)
}

func hash(cd *clientID) string {
	cd.Lock()
	req, _ := jsonx.MarshalToString(cd)
	cd.Unlock()
	hash := md5.Sum([]byte(req))
	hashString := hex.EncodeToString(hash[:])
	return hashString
}

// DisplayMiners shows all miners
func DisplayMiners() {
	var count = 0
	if clients.ItemCount() == 0 {
		return
	}
	miners := clients.Items()
	for key, value := range miners {
		miner := value.Object.(*clientData)
		miner.Lock()
		log.Println("Miner:", key, miner.Id.IP, miner.Id.MinerName, strconv.FormatFloat(float64(miner.Capacity)/1024.0, 'f', 5, 64), "TiB")
		miner.Unlock()
		count++
	}
	log.Println("Total Capacity:", strconv.FormatFloat(float64(TotalCapacity())/1024.0, 'f', 5, 64), "TiB")
}

// DisplayMiners shows all miners
func PrintMiners() string {
	var sb strings.Builder
	var count = 0
	if clients.ItemCount() == 0 {
		return ""
	}
	miners := clients.Items()
	for key, value := range miners {
		miner := value.Object.(*clientData)
		miner.Lock()
		hashrate := float64(miner.Capacity) / 240 / 1000 / 1000 * 8192 * 4 * 1024

		sb.WriteString(fmt.Sprintf("Miner: %s %s %s %sMH/s %sGiB %s\n", key, miner.Alias, miner.Id.MinerName, strconv.FormatFloat(hashrate, 'f', 2, 64), strconv.FormatFloat(float64(miner.Capacity), 'f', 2, 64), miner.Id.Xpu))
		miner.Unlock()
		count++
	}
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("Total Capacity: %s TiB", strconv.FormatFloat(float64(TotalCapacity())/1024.0, 'f', 5, 64)))
	return sb.String()
}

// TotalCapacity outputs total capacity
func TotalCapacity() int64 {
	var capa int64
	miners := clients.Items()
	for _, value := range miners {
		miner := value.Object.(*clientData)
		miner.Lock()
		defer miner.Unlock()
		capa += miner.Capacity
	}
	return capa
}
