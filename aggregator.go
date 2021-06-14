package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-querystring/query"
	jsoniter "github.com/json-iterator/go"
	cache "github.com/patrickmn/go-cache"
	"github.com/spf13/viper"
	"github.com/throttled/throttled"
	"github.com/throttled/throttled/store/memstore"
	"github.com/valyala/fasthttp"
)

const (
	version                = "1.2.3"
	defaultCacheExpiration = 15 * time.Minute
	minerCacheExpiration   = 60 * time.Second
	exceededMinersPerIP    = 0
	notUpdated             = 1
	updated                = 2
	remoteErr              = 3
	wrongHeight            = 4
)

// modules
var jsonx = jsoniter.ConfigCompatibleWithStandardLibrary
var client *fasthttp.Client
var websocketClient *websocketAPI

// config
var listenAddr string
var statsListenAddr string
var displayMiners bool
var primarySubmitURL string
var primTDL uint64
var primBest uint64
var primaryPassphrase string
var primaryIPForwarding bool
var primaryIgnoreWorseDeadlines bool
var primaryAccountKey string
var primaryws bool
var secondarySubmitURL string
var secTDL uint64
var secBest uint64
var secondaryPassphrase string
var secondaryIPForwarding bool
var secondaryIgnoreWorseDeadlines bool
var secondaryAccountKey string
var secondaryws bool
var minerName string
var minerAlias string

var fileLogging bool

var scanTime int64
var rateLimit int
var burstRate int
var minersPerIP int
var lieDetector bool

// state variables
var currentPrimChain atomicBool
var currentHeight uint64
var currentBaseTarget uint64 = 1
var curPrimaryMiningInfo atomic.Value

// last state variables
var lastPrimChain atomicBool
var lastHeight uint64
var lastBaseTarget uint64 = 1
var curSecondaryMiningInfo atomic.Value

// caches
var primc *cache.Cache
var secc *cache.Cache
var liarsCache *cache.Cache

// errors
var errSubmissionWrongFormatDeadline = errors.New("deadline submission has wrong format")
var errSubmissionWrongFormatNonce = errors.New("nonce submission has wrong format")
var errSubmissionWrongFormatBlockHeight = errors.New("blockheight submission has wrong format")
var errSubmissionWrongFormatAccountID = errors.New("account id submission has wrong format")
var errTooManySubmissionsDifferentMiners = errors.New("too many submissions from different account ids by same ip")
var errUnknownRequestType = errors.New("unknown request type")

type minerRound struct {
	AccountID  uint64 `url:"accountId"`
	Height     uint64 `url:"blockheight"`
	Deadline   uint64 `url:"deadline"`
	Nonce      uint64 `url:"nonce"`
	Passphrase string `url:"secretPhrase"`
	Adjusted   bool
}

type miningInfo struct {
	Height         FlexUInt64 `json:"height"`
	BaseTarget     FlexUInt64 `json:"baseTarget"`
	TargetDeadline FlexUInt64 `json:"targetDeadline"`
	GenSig         string     `json:"generationSignature"`
	bytes          []byte
	StartTime      time.Time
}

type submitResponse struct {
	Deadline FlexUInt64 `json:"deadline"`
}

type ipData struct {
	accountIDtoRound map[uint64]*minerRound
	sync.Mutex
}

func tryUpdateRound(w *http.ResponseWriter, r *http.Request, ip string, round *minerRound) int {
	accountID := round.AccountID
	// check if submission is late (height mismatch) if chain wasn't switched.
	if round.Height != atomic.LoadUint64(&currentHeight) && currentPrimChain.Get() == lastPrimChain.Get() {
		log.Println("DL out-dated:", round.Height, round.AccountID, round.Nonce, "X"+strconv.FormatUint(round.Deadline, 10))
		return wrongHeight
	}

	// check if submission belong to previous block.
	if round.Height != atomic.LoadUint64(&currentHeight) && round.Height != atomic.LoadUint64(&lastHeight) {
		log.Println("DL out-dated:", round.Height, round.AccountID, round.Nonce, "X"+strconv.FormatUint(round.Deadline, 10))
		return wrongHeight
	}

	// you lie I lie
	_, exists := liarsCache.Get(ip)
	if exists {
		return notUpdated
	}

	// load relevant data
	var primChain = true
	var baseTarget uint64 = 1
	if round.Height == atomic.LoadUint64(&currentHeight) {
		primChain = currentPrimChain.Get()
		baseTarget = atomic.LoadUint64(&currentBaseTarget)
	} else {
		primChain = lastPrimChain.Get()
		baseTarget = atomic.LoadUint64(&lastBaseTarget)
	}
	deadline := round.Deadline
	if !round.Adjusted {
		deadline /= baseTarget
	}

	// deadlines filter
	if (primChain && (deadline > primTDL)) || (!primChain && (deadline > secTDL)) {
		log.Println("DL filtered:", round.Height, round.AccountID, round.Nonce, deadline)
		return notUpdated
	}
	if (primChain && (deadline > atomic.LoadUint64(&primBest)) && primaryIgnoreWorseDeadlines) || (!primChain && (deadline > atomic.LoadUint64(&secBest) && secondaryIgnoreWorseDeadlines)) {
		log.Println("DL discarded:", round.Height, round.AccountID, round.Nonce, deadline)
		return notUpdated
	}

	var ipDataV interface{}

	if primChain {
		ipDataV, exists = primc.Get(ip)
	} else {
		ipDataV, exists = secc.Get(ip)
	}

	if !exists {
		err := proxySubmitRound(w, r, ip, round, primChain, baseTarget)
		if err != nil {
			return remoteErr
		}
		if primChain {
			primc.SetDefault(ip, &ipData{
				accountIDtoRound: map[uint64]*minerRound{
					accountID: round,
				},
			})
		} else {
			secc.SetDefault(ip, &ipData{
				accountIDtoRound: map[uint64]*minerRound{
					accountID: round,
				},
			})
		}
		if primChain {
			atomic.StoreUint64(&primBest, deadline)
		} else {
			atomic.StoreUint64(&secBest, deadline)
		}
		log.Println("DL response:", round.Height, round.AccountID, round.Nonce, deadline)
		return updated
	}
	ipData := ipDataV.(*ipData)
	ipData.Lock()
	defer ipData.Unlock()
	existingRound, exists := ipData.accountIDtoRound[accountID]
	if !exists {
		minerCount := len(ipData.accountIDtoRound)
		if minerCount == minersPerIP {
			for _, otherRound := range ipData.accountIDtoRound {
				if otherRound.Height < round.Height {
					delete(ipData.accountIDtoRound, otherRound.AccountID)
					goto update
				}
			}
			log.Println("DL rejected:", round.Height, round.AccountID, round.Nonce, deadline)
			return exceededMinersPerIP
		}
	} else {
		existingDeadline := existingRound.Deadline
		if !existingRound.Adjusted {
			existingDeadline /= baseTarget
		}
		if existingRound.Height > round.Height || existingRound.Height == round.Height &&
			existingDeadline < deadline {
			log.Println("DL ignored:", round.Height, round.AccountID, round.Nonce, deadline)
			return notUpdated
		}
	}
update:
	if err := proxySubmitRound(w, r, ip, round, primChain, baseTarget); err != nil {
		return remoteErr
	}
	ipData.accountIDtoRound[accountID] = round
	if primChain {
		atomic.StoreUint64(&primBest, deadline)
	} else {
		atomic.StoreUint64(&secBest, deadline)
	}
	log.Println("DL response:", round.Height, round.AccountID, round.Nonce, deadline)
	return updated
}

func parseRound(r *http.Request) (*minerRound, error) {
	adjusted := false
	deadline, err := strconv.ParseUint((*r).FormValue("deadline"), 10, 64)
	if err != nil {
		// inefficient mining software detected :p
		deadline, err = strconv.ParseUint((*r).Header.Get("X-Deadline"), 10, 64)
		if err != nil {
			return nil, errSubmissionWrongFormatDeadline
		}
		adjusted = true
	}
	nonce, err := strconv.ParseUint((*r).FormValue("nonce"), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormatNonce
	}
	height, err := strconv.ParseUint((*r).FormValue("blockheight"), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormatBlockHeight
	}
	accountID, err := strconv.ParseUint((*r).FormValue("accountId"), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormatAccountID
	}

	passphrase := (*r).FormValue("secretPhrase")

	return &minerRound{
		Deadline:   deadline,
		Nonce:      nonce,
		Height:     height,
		AccountID:  accountID,
		Passphrase: passphrase,
		Adjusted:   adjusted,
	}, nil
}

func proxySubmitRound(w *http.ResponseWriter, r *http.Request, ip string, round *minerRound, primary bool, baseTarget uint64) error {
	// websocket api handling
	if (primary && primaryws) || (!primary && secondaryws) {
		// fire submission
		websocketClient.submitNonce(round.AccountID, round.Height, round.Nonce, round.Deadline)
		log.Println("DL fired:", round.Height, round.AccountID, round.Nonce, round.Deadline)
		// fake answer
		var baseTarget = atomic.LoadUint64(&currentBaseTarget)
		if round.Height != atomic.LoadUint64(&currentHeight) {
			baseTarget = atomic.LoadUint64(&lastBaseTarget)
		}
		deadline := round.Deadline
		if !round.Adjusted {
			deadline /= baseTarget
		}
		(*w).Write([]byte(fmt.Sprintf("{\"deadline\":%d,\"result\":\"success\"}", deadline)))
		return nil
	}

	// passphrase overwrites
	if primary && primaryPassphrase != "" {
		round.Passphrase = primaryPassphrase
	}
	if !primary && secondaryPassphrase != "" {
		round.Passphrase = secondaryPassphrase
	}

	v, _ := query.Values(round)
	// treat unadj dl
	if round.Adjusted {
		v.Del("deadline")
	}

	// treat pool
	if round.Passphrase == "" {
		v.Del("secretPhrase")
	} else {
		v.Del("deadline")
	}

	v.Del("Adjusted")

	var submitURL string
	if primary {
		submitURL = primarySubmitURL
	} else {
		submitURL = secondarySubmitURL
	}

	req := fasthttp.AcquireRequest()
	req.URI().Update(submitURL + "/burst?requestType=submitNonce&" + v.Encode())

	var miner string
	if ua := r.Header.Get("User-Agent"); ua == "" {
		miner = r.Header.Get("X-Miner")
	} else {
		miner = ua
	}

	req.Header.Set("User-Agent", "Aggregator/"+version+"/"+miner)
	req.Header.Set("X-Miner", "Aggregator/"+version+"/"+miner)
	req.Header.Set("X-MinerAlias", minerAlias)
	req.Header.Set("X-Capacity", strconv.FormatInt(TotalCapacity(), 10))
	if primary {
		req.Header.Set("X-Account", primaryAccountKey)
	} else {
		req.Header.Set("X-Account", secondaryAccountKey)
	}

	// x-forwarded-for
	if (primary && primaryIPForwarding) || (!primary && secondaryIPForwarding) {
		ip, _, err := net.SplitHostPort(ip)
		if err == nil {
			req.Header.Set("X-Forwarded-For", ip)
		}
	}

	req.Header.SetMethodBytes([]byte("POST"))
	resp := fasthttp.AcquireResponse()
	err := client.Do(req, resp)

	if err != nil {
		(*w).Write(formatJSONError(3, "error reaching pool or wallet"))
		return err
	}

	// lie detector
	if lieDetector {
		var mi submitResponse
		if err := jsonx.Unmarshal(resp.Body(), &mi); err == nil {
			deadline := round.Deadline
			if !round.Adjusted {
				deadline /= baseTarget
			}
			if uint64(mi.Deadline) != deadline {
				var liar = true
				liarsCache.SetDefault(ip, &liar)
				log.Println("Liar detected:", round.Height, ip, mi.Deadline, deadline)
			}
		}
	}

	(*w).Write(resp.Body())
	return nil
}

func refreshMiningInfo() error {
	// primary chain
	var mi miningInfo
	var errchain1 error
	if primaryws {
		if available.Get() {
			mi = *currentMiningInfo.Load().(*miningInfo)
		} else {
			// initial mining info missing
			errchain1 = fmt.Errorf("primary chain: initial mining info missing")
		}
	} else {
		req := fasthttp.AcquireRequest()
		req.URI().Update(primarySubmitURL + "/burst?requestType=getMiningInfo")
		req.Header.Set("User-Agent", "Aggregator/"+version)
		req.Header.Set("X-Miner", "Aggregator/"+version)
		req.Header.Set("X-Capacity", strconv.FormatInt(TotalCapacity(), 10))
		req.Header.SetMethodBytes([]byte("GET"))
		resp := fasthttp.AcquireResponse()
		err1 := client.Do(req, resp)
		errchain1 = err1
		if errchain1 == nil {
			if err := jsonx.Unmarshal(resp.Body(), &mi); err != nil {
				return err
			}
		}
	}

	var curPrimMi *miningInfo
	if curPrimMiV := curPrimaryMiningInfo.Load(); curPrimMiV != nil {
		curPrimMi = curPrimMiV.(*miningInfo)
	}

	var curSecMi *miningInfo
	if curSecMiV := curSecondaryMiningInfo.Load(); curSecMiV != nil {
		curSecMi = curSecMiV.(*miningInfo)
	}

	var lastPrimaryStart = time.Time{}
	if curPrimMi != nil {
		lastPrimaryStart = curPrimMi.StartTime
	}

	var lastSecondaryStart = time.Time{}
	if curSecMi != nil {
		lastSecondaryStart = curSecMi.StartTime
	}
	if errchain1 == nil {
		switch {
		case curPrimMi == nil || curPrimMi.Height < mi.Height:
			log.Println("New Block", mi.Height, mi.BaseTarget, mi.TargetDeadline, mi.GenSig)
			if displayMiners {
				DisplayMiners()
			}
			mi.bytes, _ = json.Marshal(map[string]string{
				"height":              fmt.Sprintf("%d", mi.Height),
				"baseTarget":          fmt.Sprintf("%d", mi.BaseTarget),
				"generationSignature": mi.GenSig})
			mi.StartTime = time.Now()
			curPrimaryMiningInfo.Store(&mi)
			if !currentPrimChain.Get() {
				atomic.StoreUint64(&lastBaseTarget, atomic.LoadUint64(&currentBaseTarget))
				atomic.StoreUint64(&lastHeight, atomic.LoadUint64(&currentHeight))
				lastPrimChain.Set(false)
			}
			atomic.StoreUint64(&currentBaseTarget, uint64(mi.BaseTarget))
			atomic.StoreUint64(&currentHeight, uint64(mi.Height))
			currentPrimChain.Set(true)
			atomic.StoreUint64(&primBest, ^uint64(0))
			// reschedule secondary chain on interrupt
			if int64(time.Now().Sub(lastSecondaryStart).Seconds()) < scanTime {
				reset := miningInfo{0, 0, 0, "", []byte{0}, time.Time{}}
				curSecondaryMiningInfo.Store(&reset)
			}
			return nil
		case curPrimMi.Height > mi.Height: // fork handling
			log.Println("New Block", mi.Height, mi.BaseTarget, mi.TargetDeadline, mi.GenSig)
			if displayMiners {
				DisplayMiners()
			}
			mi.bytes, _ = json.Marshal(map[string]string{
				"height":              fmt.Sprintf("%d", mi.Height),
				"baseTarget":          fmt.Sprintf("%d", mi.BaseTarget),
				"generationSignature": mi.GenSig})
			mi.StartTime = time.Now()
			curPrimaryMiningInfo.Store(&mi)
			primc.Flush()
			if !currentPrimChain.Get() {
				atomic.StoreUint64(&lastBaseTarget, atomic.LoadUint64(&currentBaseTarget))
				atomic.StoreUint64(&lastHeight, atomic.LoadUint64(&currentHeight))
				lastPrimChain.Set(false)
			}
			atomic.StoreUint64(&currentBaseTarget, uint64(mi.BaseTarget))
			atomic.StoreUint64(&currentHeight, uint64(mi.Height))
			currentPrimChain.Set(true)
			atomic.StoreUint64(&primBest, ^uint64(0))
			// reschedule secondary chain on interrupt
			if int64(time.Now().Sub(lastSecondaryStart).Seconds()) < scanTime {
				reset := miningInfo{0, 0, 0, "", []byte{0}, time.Time{}}
				curSecondaryMiningInfo.Store(&reset)
			}
			return nil
		case curPrimMi.BaseTarget != mi.BaseTarget: // fork handling
			log.Println("New Block", mi.Height, mi.BaseTarget, mi.TargetDeadline, mi.GenSig)
			if displayMiners {
				DisplayMiners()
			}
			mi.bytes, _ = json.Marshal(map[string]string{
				"height":              fmt.Sprintf("%d", mi.Height),
				"baseTarget":          fmt.Sprintf("%d", mi.BaseTarget),
				"generationSignature": mi.GenSig})
			mi.StartTime = time.Now()
			curPrimaryMiningInfo.Store(&mi)
			primc.Flush()
			if !currentPrimChain.Get() {
				atomic.StoreUint64(&lastBaseTarget, atomic.LoadUint64(&currentBaseTarget))
				atomic.StoreUint64(&lastHeight, atomic.LoadUint64(&currentHeight))
				lastPrimChain.Set(false)
			}
			atomic.StoreUint64(&currentBaseTarget, uint64(mi.BaseTarget))
			atomic.StoreUint64(&currentHeight, uint64(mi.Height))
			currentPrimChain.Set(true)
			atomic.StoreUint64(&primBest, ^uint64(0))
			// reschedule secondary chain on interrupt
			if int64(time.Now().Sub(lastSecondaryStart).Seconds()) < scanTime {
				reset := miningInfo{0, 0, 0, "", []byte{0}, time.Time{}}
				curSecondaryMiningInfo.Store(&reset)
			}
			return nil
		}
	}

	// single chain
	if secondarySubmitURL == "" {
		return nil
	}
	// skip secondary if primary is scanning
	if int64(time.Now().Sub(lastPrimaryStart).Seconds()) < scanTime {
		return nil
	}

	// secondary chain
	var errchain2 error
	if secondaryws {
		if available.Get() {
			mi = *currentMiningInfo.Load().(*miningInfo)
		} else {
			// initial mining info missing
			errchain2 = fmt.Errorf("secondary chain: initial mining info missing")
			return errchain2
		}
	} else {
		req := fasthttp.AcquireRequest()
		req.URI().Update(secondarySubmitURL + "/burst?requestType=getMiningInfo")
		req.Header.Set("User-Agent", "Aggregator/"+version)
		req.Header.Set("X-Miner", "Aggregator/"+version)
		req.Header.Set("X-Capacity", strconv.FormatInt(TotalCapacity(), 10))
		req.Header.SetMethodBytes([]byte("GET"))
		resp := fasthttp.AcquireResponse()
		err2 := client.Do(req, resp)
		errchain2 = err2
		if errchain2 == nil {
			if err := jsonx.Unmarshal(resp.Body(), &mi); err != nil {
				return err
			}
		} else {
			return errchain2
		}
	}

	switch {
	case curSecMi == nil || curSecMi.Height < mi.Height:
		log.Println("New Block", mi.Height, mi.BaseTarget, mi.TargetDeadline, mi.GenSig)
		if displayMiners {
			DisplayMiners()
		}
		mi.bytes, _ = json.Marshal(map[string]string{
			"height":              fmt.Sprintf("%d", mi.Height),
			"baseTarget":          fmt.Sprintf("%d", mi.BaseTarget),
			"generationSignature": mi.GenSig})
		mi.StartTime = time.Now()
		curSecondaryMiningInfo.Store(&mi)

		if currentPrimChain.Get() {
			atomic.StoreUint64(&lastBaseTarget, atomic.LoadUint64(&currentBaseTarget))
			atomic.StoreUint64(&lastHeight, atomic.LoadUint64(&currentHeight))
			lastPrimChain.Set(true)
		}
		atomic.StoreUint64(&currentBaseTarget, uint64(mi.BaseTarget))
		atomic.StoreUint64(&currentHeight, uint64(mi.Height))
		currentPrimChain.Set(false)
		atomic.StoreUint64(&secBest, ^uint64(0))
		return nil
	case curSecMi.Height > mi.Height: // fork handling
		log.Println("New Block", mi.Height, mi.BaseTarget, mi.TargetDeadline, mi.GenSig)
		if displayMiners {
			DisplayMiners()
		}
		mi.bytes, _ = json.Marshal(map[string]string{
			"height":              fmt.Sprintf("%d", mi.Height),
			"baseTarget":          fmt.Sprintf("%d", mi.BaseTarget),
			"generationSignature": mi.GenSig})
		mi.StartTime = time.Now()
		curSecondaryMiningInfo.Store(&mi)
		secc.Flush()
		if currentPrimChain.Get() {
			atomic.StoreUint64(&lastBaseTarget, atomic.LoadUint64(&currentBaseTarget))
			atomic.StoreUint64(&lastHeight, atomic.LoadUint64(&currentHeight))
			lastPrimChain.Set(true)
		}
		atomic.StoreUint64(&currentBaseTarget, uint64(mi.BaseTarget))
		atomic.StoreUint64(&currentHeight, uint64(mi.Height))
		currentPrimChain.Set(true)
		atomic.StoreUint64(&secBest, ^uint64(0))
		return nil
	case curSecMi.BaseTarget != mi.BaseTarget: // fork handling
		log.Println("New Block", mi.Height, mi.BaseTarget, mi.TargetDeadline, mi.GenSig)
		if displayMiners {
			DisplayMiners()
		}
		mi.bytes, _ = json.Marshal(map[string]string{
			"height":              fmt.Sprintf("%d", mi.Height),
			"baseTarget":          fmt.Sprintf("%d", mi.BaseTarget),
			"generationSignature": mi.GenSig})
		mi.StartTime = time.Now()
		curSecondaryMiningInfo.Store(&mi)
		secc.Flush()
		if currentPrimChain.Get() {
			atomic.StoreUint64(&lastBaseTarget, atomic.LoadUint64(&currentBaseTarget))
			atomic.StoreUint64(&lastHeight, atomic.LoadUint64(&currentHeight))
			lastPrimChain.Set(true)
		}
		atomic.StoreUint64(&currentBaseTarget, uint64(mi.BaseTarget))
		atomic.StoreUint64(&currentHeight, uint64(mi.Height))
		currentPrimChain.Set(false)
		atomic.StoreUint64(&secBest, ^uint64(0))
		return nil
	}
	return nil
}

func statsRequestHandler(w http.ResponseWriter, r *http.Request) {
	fmt.Fprintf(w, "Bencher Stats\n\n")
	fmt.Fprintf(w, PrintMiners())
}

func requestHandler(w http.ResponseWriter, r *http.Request) {
	ipport := r.RemoteAddr
	ip, port, _ := net.SplitHostPort(ipport)
	switch reqType := string(r.FormValue("requestType")); reqType {
	case "getMiningInfo":
		if currentPrimChain.Get() {
			w.Write(curPrimaryMiningInfo.Load().(*miningInfo).bytes)
		} else {
			w.Write(curSecondaryMiningInfo.Load().(*miningInfo).bytes)
		}
		// log client
		var miner string
		if ua := r.Header.Get("User-Agent"); ua == "" {
			miner = r.Header.Get("X-Miner")
		} else {
			miner = ua
		}
		alias := r.Header.Get("X-MinerAlias")
		xpu := r.Header.Get("X-Xpu")

		size, _ := strconv.ParseInt(r.Header.Get("X-Capacity"), 10, 64)
		UpdateClient(ip, port, miner, alias, xpu, size)
		if primaryws || secondaryws {
			websocketClient.UpdateSize(TotalCapacity())
		}

	case "submitNonce":
		round, err := parseRound(r)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write(formatJSONError(1, err.Error()))
			return
		}
		switch res := tryUpdateRound(&w, r, ip, round); res {
		case updated:
		case notUpdated:
			var baseTarget = atomic.LoadUint64(&currentBaseTarget)
			if round.Height != atomic.LoadUint64(&currentHeight) {
				baseTarget = atomic.LoadUint64(&lastBaseTarget)
			}
			deadline := round.Deadline
			if !round.Adjusted {
				deadline /= baseTarget
			}
			w.Write([]byte(fmt.Sprintf("{\"deadline\":%d,\"result\":\"success\"}", deadline)))
		case wrongHeight:
			w.WriteHeader(http.StatusBadRequest)
			w.Write(formatJSONError(1005, "Submitted on wrong height"))
		case exceededMinersPerIP:
			w.WriteHeader(http.StatusBadRequest)
			w.Write(formatJSONError(2, errTooManySubmissionsDifferentMiners.Error()))
		}
	default:
		w.WriteHeader(http.StatusBadRequest)
		w.Write(formatJSONError(4, errUnknownRequestType.Error()))
	}
}

func main() {
	log.Println("Aggregator v." + version)

	viper.SetConfigName("config")
	viper.AddConfigPath(".")
	err := viper.ReadInConfig()
	if err != nil {
		panic(fmt.Errorf("fatal error config file: %s", err))
	}

	client = &fasthttp.Client{NoDefaultUserAgentHeader: true}
	client.MaxIdleConnDuration = 0 * time.Millisecond

	listenAddr = viper.GetString("listenAddr")
	statsListenAddr = viper.GetString("statslistenAddr")
	displayMiners = viper.GetBool("displayMiners")
	log.Println("Proxy address:", listenAddr)
	minersPerIP = viper.GetInt("minersPerIP")
	primarySubmitURL = viper.GetString("primarySubmitURL")
	primaryPassphrase = viper.GetString("primaryPassphrase")
	primaryIPForwarding = viper.GetBool("primaryIpForwarding")
	primaryIgnoreWorseDeadlines = viper.GetBool("primaryIgnoreWorseDeadlines")
	primaryAccountKey = viper.GetString("primaryAccountKey")
	primTDL = uint64(viper.GetInt64("primaryTargetDeadline"))
	secondarySubmitURL = viper.GetString("secondarySubmitURL")
	secondaryPassphrase = viper.GetString("secondaryPassphrase")
	secondaryIPForwarding = viper.GetBool("secondaryIpForwarding")
	secondaryIgnoreWorseDeadlines = viper.GetBool("secondaryIgnoreWorseDeadlines")
	secondaryAccountKey = viper.GetString("secondaryAccountKey")
	secTDL = uint64(viper.GetInt64("secondaryTargetDeadline"))
	fileLogging = viper.GetBool("fileLogging")

	scanTime = viper.GetInt64("scanTime")
	rateLimit = viper.GetInt("rateLimit")
	burstRate = viper.GetInt("burstRate")
	lieDetector = viper.GetBool("lieDetector")
	log.Println("Primary chain:", primarySubmitURL)
	log.Println("Secondary chain:", secondarySubmitURL)
	log.Println("Rate Limiter:", "limit="+strconv.Itoa(rateLimit), "per second, burstrate="+strconv.Itoa(burstRate))
	minerName = viper.GetString("minerName")
	minerAlias = viper.GetString("minerAlias")

	// todo check exactly one url is wss
	primaryws = strings.HasPrefix(primarySubmitURL, "wss")
	secondaryws = strings.HasPrefix(secondarySubmitURL, "wss")

	if primaryws && secondaryws {
		panic("can only have a single websocket upstream")
	}

	// launch api
	if primaryws {
		websocketClient = newWebsocketAPI(primarySubmitURL, primaryAccountKey, minerName, 0)
		websocketClient.Connect()
	}

	if secondaryws {
		websocketClient = newWebsocketAPI(secondarySubmitURL, secondaryAccountKey, minerName, 0)
		websocketClient.Connect()
	}
	// amend submit & getMiningInfo

	if fileLogging {
		logFile, err := os.OpenFile("log.txt", os.O_CREATE|os.O_APPEND|os.O_RDWR, 0666)
		if err != nil {
			panic(err)
		}

		mw := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(mw)
	}

	clients = cache.New(minerCacheExpiration, minerCacheExpiration)

	if err := refreshMiningInfo(); err != nil {
		log.Fatalln("get initial mining info: ", err)
	}
	go func() {
		t := time.NewTicker(1 * time.Second)
		for {
			for range t.C {
				_ = refreshMiningInfo()
			}
		}
	}()

	primc = cache.New(defaultCacheExpiration, defaultCacheExpiration)
	secc = cache.New(defaultCacheExpiration, defaultCacheExpiration)
	liarsCache = cache.New(defaultCacheExpiration, defaultCacheExpiration)

	store, err := memstore.New(65536)
	if err != nil {
		log.Fatal(err)
	}

	quota := throttled.RateQuota{MaxRate: throttled.PerSec(rateLimit), MaxBurst: burstRate}
	rateLimiter, err := throttled.NewGCRARateLimiter(store, quota)
	if err != nil {
		log.Fatal(err)
	}

	httpRateLimiter := throttled.HTTPRateLimiter{
		RateLimiter: rateLimiter,
		VaryBy:      &throttled.VaryBy{Path: true},
	}

	h := http.HandlerFunc(requestHandler)
	i := http.HandlerFunc(statsRequestHandler)

	go func() {
		err = fasthttp.ListenAndServe(statsListenAddr, NewFastHTTPHandler(httpRateLimiter.RateLimit(i)))
		if err != nil {
			log.Fatalf("listen and serve: %s", err)
		}
	}()

	err = fasthttp.ListenAndServe(listenAddr, NewFastHTTPHandler(httpRateLimiter.RateLimit(h)))
	if err != nil {
		log.Fatalf("listen and serve: %s", err)
	}
}

func formatJSONError(errorCode int64, errorMsg string) []uint8 {
	bytes, _ := json.Marshal(map[string]string{
		"errorCode":        strconv.FormatInt(errorCode, 10),
		"errorDescription": errorMsg})
	return bytes
}
