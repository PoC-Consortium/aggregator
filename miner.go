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
	Port      string `json:"port"`
	MinerName string `json:"minerName"`
	sync.Mutex
}

var clients *cache.Cache

// UpdateClient refreshed Miner data
func UpdateClient(ip string, port string, minerName string, alias string, capacity int64) {
	cd := clientData{Id: clientID{IP: ip, Port: port, MinerName: minerName}, Capacity: capacity}
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
		sb.WriteString(fmt.Sprintf("Miner: %s %s %s %s TiB\n", key, miner.Alias, miner.Id.MinerName, strconv.FormatFloat(float64(miner.Capacity)/1024.0, 'f', 5, 64)))
		miner.Unlock()
		count++
	}
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
