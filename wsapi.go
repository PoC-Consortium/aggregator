package main

import (
	"fmt"
	"encoding/json"
	"log"

	"time"
	"sync/atomic"

	"sync"
	"os"
	"github.com/mariuspass/recws"
	"strconv"
	"os/signal"
)

const (
	hdproxyVersion = "20190423"
	threshold      = 30
	frequency	   = 5
)

var currentMiningInfo atomic.Value
var lastHeartBeat atomic.Value
var available atomicBool

type websocketAPI struct {
	server     string
	accountKey string
	rc         *recws.RecConn
	ci         clientInfo
	sendMu     *sync.Mutex // Prevent "concurrent write to websocket connection"
	receiveMu  *sync.Mutex
}

type clientInfo struct {
	AccountKey string `json:"account_key"`
	MinerName  string `json:"miner_name"`
	MinerMark  string `json:"miner_mark"`
	Capacity   int64  `json:"capacity"`
}

type nonceSubmission struct {
	AccountKey string      `json:"account_key"`
	MinerName  string      `json:"miner_name"`
	MinerMark  string      `json:"miner_mark"`
	Capacity   int64       `json:"capacity"`
	Submit     []nonceData `json:"submit"`
}

type nonceData struct {
	AccountID uint64 `json:"accountId"`
	Height    uint64 `json:"height"`
	Nonce     string `json:"nonce"`
	Deadline  uint64 `json:"deadline"`
	Ts        int64  `json:"ts"`
}

type websocketMiningInfo struct {
	Cmd  string     `json:"cmd"`
	Para miningInfo `json:"para"`
}

type websocketHeartBeat struct {
	Cmd  string     `json:"cmd"`
	Para clientInfo `json:"para"`
}

type websocketMessage struct {
	Cmd  string      `json:"cmd"`
	Para interface{} `json:"para"`
}

func newWebsocketAPI(server string, accountKey string, minerName string, capacityGB int64) (c *websocketAPI) {
	ws := recws.RecConn{}
	ci := clientInfo{accountKey, minerName, minerName + ".hdproxy.exe." + hdproxyVersion, capacityGB}
	c = &websocketAPI{
		server,
		accountKey,
		&ws,
		ci,
		&sync.Mutex{},
		&sync.Mutex{}}
	return
}

func (c *websocketAPI) UpdateSize(totalSize int64){
	c.sendMu.Lock()
	c.ci.Capacity = totalSize
	c.sendMu.Unlock()
}

func (c *websocketAPI) Close() {
	c.rc.Close()
}

func (c *websocketAPI) Connect() {
	c.rc.SubscribeHandler = c.subscribe
	c.rc.Dial(c.server, nil)

	// message handler
	go func() {
		for {
			c.receiveMu.Lock()
			messageType, message, err := c.rc.ReadMessage()
			c.receiveMu.Unlock()
			if err != nil {
				continue
			}
			// handle all text messages
			switch messageType {
			case 1:
				onTextMessage(string(message))

			}
		}
	}()
}

func (c *websocketAPI) subscribe() error {
	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt)

	// request initial mining info
	c.sendMu.Lock()
	if err := c.rc.WriteMessage(1, []byte("{\"cmd\":\"mining_info\",\"para\":{}}")); err != nil {
		log.Printf("Error: WriteMessage %s", c.rc.GetURL())
		return err
	}
	c.sendMu.Unlock()
	// subscribe for future mining infos
	channelName := "poolmgr.mining_info"
	subscribeObject := getSubscribeEventObject(channelName, 0)
	subscribeData := serializeDataIntoString(subscribeObject)
	c.sendMu.Lock()
	if err := c.rc.WriteMessage(1, []byte(subscribeData)); err != nil {
		log.Printf("Error: WriteMessage %s", c.rc.GetURL())
		return err
	}
	c.sendMu.Unlock()
	// subscribe to heartbeat
	// cancel existing
	// create new
	ct := time.Now()
	lastHeartBeat.Store(ct)
	ticker := time.NewTicker(time.Duration(frequency) * time.Second)
	go func() {
		for {
		select {
			case <- ticker.C:
				// check last heartbeatACK
				ht := lastHeartBeat.Load()
				if int64(time.Now().Sub(ht.(time.Time)).Seconds()) > threshold {
					// attempt reconnect
					// stop heartbeat, will be restarted after connect
					ticker.Stop();
					log.Println("websocket api: heartbeat lost, trying to reconnect...")
					ct := time.Now()
					lastHeartBeat.Store(ct)
					c.Close()					
				}
				c.sendMu.Lock()
				ci := clientInfo{c.accountKey,c.ci.MinerName,c.ci.MinerName+".hdproxy.exe."+hdproxyVersion, c.ci.Capacity}
				hb := websocketMessage{"poolmgr.heartbeat",ci}
				req, err := jsonx.MarshalToString(&hb);
				if err != nil {
					return
				}
				// debug
				// log.Println(req)
				c.rc.WriteMessage(1, []byte(req))
				c.sendMu.Unlock()
			case <- interrupt:
				ticker.Stop()
				os.Exit(0)
				return
			}
		}
	}()
	return nil
}

func onTextMessage(message string) {
	// debug log.Println("recv (text):", message)
	var hi websocketMessage
			if err := jsonx.UnmarshalFromString(message, &hi); err != nil {
				return
			}
		switch hi.Cmd{
		case "poolmgr.heartbeat": 
			ct := time.Now()
			lastHeartBeat.Store(ct)
			//log.Println("websocket api: heartbeat");
		case "poolmgr.mining_info":
			var mi websocketMiningInfo
			if err := jsonx.UnmarshalFromString(message, &mi); err != nil {
				return
			}
			mi.Para.bytes, _ = json.Marshal(map[string]string{
				"height":              fmt.Sprintf("%d", mi.Para.Height),
				"baseTarget":          fmt.Sprintf("%d", mi.Para.BaseTarget),
				"generationSignature": mi.Para.GenSig})
			mi.Para.StartTime = time.Now()
			currentMiningInfo.Store(&mi.Para)
			available.Set(true)
			log.Println("websocket api: new mining info received");
		case "mining_info":
			var mi websocketMiningInfo
			mi.Para.StartTime = time.Now()
			if err := jsonx.UnmarshalFromString(message, &mi); err != nil {
				return
			}
			mi.Para.bytes, _ = json.Marshal(map[string]string{
				"height":              fmt.Sprintf("%d", mi.Para.Height),
				"baseTarget":          fmt.Sprintf("%d", mi.Para.BaseTarget),
				"generationSignature": mi.Para.GenSig})
			mi.Para.StartTime = time.Now()
			currentMiningInfo.Store(&mi.Para)
			available.Set(true)
			log.Println("websocket api: initial mining info received.");
		return
		}
}

func getSubscribeEventObject(channelName string, messageID int) emitEvent {
	return emitEvent{
		Event: "#subscribe",
		Data:  channel{Channel: channelName},
		Cid:   messageID,
	}
}

type emitEvent struct {
	Event string      `json:"event"`
	Data  interface{} `json:"data"`
	Cid   int         `json:"cid"`
}

type channel struct {
	Channel string      `json:"channel"`
	Data    interface{} `json:"data,omitempty"`
}

func serializeDataIntoString(data interface{}) string {
	b, _ := json.Marshal(data)
	return string(b)
}

func (c *websocketAPI) submitNonce(accountID uint64, height uint64, nonce uint64, deadline uint64){
	c.sendMu.Lock()
	nd := nonceData{accountID, height, strconv.FormatUint(nonce,10), deadline, time.Now().Unix()}
	ns := nonceSubmission{c.ci.AccountKey,c.ci.MinerName,"",c.ci.Capacity,[]nonceData{nd}}
	hb := websocketMessage{"poolmgr.submit_nonce",ns}
	req, err := jsonx.MarshalToString(&hb);
	// debug
	// log.Println(req)
	if err != nil {
		return
	}
	c.rc.WriteMessage(1, []byte(req))
	c.sendMu.Unlock()
}
