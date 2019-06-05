package main

import (
	"sync"
	"log"
	"crypto/md5"
	"strconv"
	"encoding/hex"

	cache "github.com/patrickmn/go-cache"
)

// ClientData stores all miner info
type clientData struct {
	IP string `json:"ip"`
	MinerName string`json:"minerName"`
	Capacity int64 `json:"capacity"`
	sync.Mutex
}

var clients *cache.Cache

// UpdateClient refreshed Miner data
func UpdateClient(ip string, minerName string, capacity int64){
	cd := clientData{IP: ip, MinerName: minerName, Capacity: capacity}
	key := hash(&cd);
	clients.SetDefault(key, &cd)
}


func hash(cd *clientData) string {
	cd.Lock()
	req, _:= jsonx.MarshalToString(cd);
	cd.Unlock()
	hash := md5.Sum([]byte(req))
    hashString := hex.EncodeToString(hash[:])
    return hashString
}

// DisplayMiners shows all miners
func DisplayMiners(){
	var count = 0
	if clients.ItemCount() == 0 {
		return
	}
	miners := clients.Items()
	for key, value := range miners {
		miner := value.Object.(*clientData)
		miner.Lock()
		log.Println("Miner:", key, miner.IP, miner.MinerName, strconv.FormatFloat(float64(miner.Capacity)/1024.0, 'f', 5, 64),"TiB"  )
		miner.Unlock()
		count++
	}
	log.Println("Total Capacity:", strconv.FormatFloat(float64(TotalCapacity())/1024.0, 'f', 5, 64),"TiB"  )
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
