package main

import (
	"errors"
	"fmt"
	"log"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/go-querystring/query"
	jsoniter "github.com/json-iterator/go"
	cache "github.com/patrickmn/go-cache"
	"github.com/spf13/viper"
	"github.com/valyala/fasthttp"
)

const (
	defaultMinersPerIP     = 3
	defaultRequestsPerSec  = 3
	defaultCacheExpiration = 15 * time.Minute
	defaultListenAddr      = "127.0.0.1:6655"

	exceededMinersPerIP = 0
	notUpdated          = 1
	updated             = 2
	remoteErr           = 3
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

type aggregator struct {
	listenAddress string
	proxyURL      string
	cache         *cache.Cache
	curMiningInfo atomic.Value
	client        *fasthttp.Client
	minersPerIP   int
}

type Aggregator interface{}

var cfg Config

type Config struct {
	MinersPerIP   int
	ListenAddress string
	ProxyURL      string
	CertFile      string
	KeyFile       string
}

// var requestsPerSec int

var errSubmissionWrongFormat = errors.New("submission has wrong format")
var errTooManySubmissionsDifferenMiners = errors.New("too many submissions from different account ids by same ip")
var errUnknownRequestType = errors.New("unknown request type")

type minerRound struct {
	AccountID  uint64 `url:"accountId"`
	Height     uint64 `url:"blockheight"`
	Deadline   uint64 `url:"deadline"`
	Nonce      uint64 `url:"nonce"`
	Passphrase string `url:"secretPhrase,omitempty"`
}

// handling json type inconsistencies of pools and wallets. integers are sometimes sent as string
// https://engineering.bitnami.com/articles/dealing-with-json-with-non-homogeneous-types-in-go.html
type FlexUInt64 int

func (fi *FlexUInt64) UnmarshalJSON(b []byte) error {
	if b[0] != '"' {
		return json.Unmarshal(b, (*int)(fi))
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*fi = FlexUInt64(i)
	return nil
}

type miningInfo struct {
	Height         FlexUInt64 `json:"height"`
	BaseTarget     FlexUInt64 `json:"baseTarget"`
	TargetDeadline FlexUInt64 `json:"targetDeadline"`
	GenSig         string     `json:"generationSignature"`
	bytes          []byte
}

type ipData struct {
	accountIDtoRound map[uint64]*minerRound
	sync.Mutex
}

func (a *aggregator) tryUpdateRound(ctx *fasthttp.RequestCtx, ip string, round *minerRound) int {
	accountID := round.AccountID
	ipDataV, exists := a.cache.Get(ip)
	if !exists {
		err := a.proxySubmitRound(ctx, round)
		if err != nil {
			return remoteErr
		}
		a.cache.SetDefault(ip, &ipData{
			accountIDtoRound: map[uint64]*minerRound{
				accountID: round,
			},
		})
		return updated
	}
	ipData := ipDataV.(*ipData)
	ipData.Lock()
	defer ipData.Unlock()
	existingRound, exists := ipData.accountIDtoRound[accountID]
	if !exists {
		minerCount := len(ipData.accountIDtoRound)
		if minerCount == a.minersPerIP {
			for _, otherRound := range ipData.accountIDtoRound {
				if otherRound.Height < round.Height {
					delete(ipData.accountIDtoRound, otherRound.AccountID)
					goto update
				}
			}
			return exceededMinersPerIP
		}
	} else if existingRound.Height > round.Height || existingRound.Height == round.Height &&
		existingRound.Deadline < round.Deadline {
		return notUpdated
	}
update:
	if err := a.proxySubmitRound(ctx, round); err != nil {
		return remoteErr
	}
	ipData.accountIDtoRound[accountID] = round
	return updated
}

func parseRound(ctx *fasthttp.RequestCtx) (*minerRound, error) {
	deadline, err := strconv.ParseUint(string(ctx.FormValue("deadline")), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormat
	}
	nonce, err := strconv.ParseUint(string(ctx.FormValue("nonce")), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormat
	}
	height, err := strconv.ParseUint(string(ctx.FormValue("blockheight")), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormat
	}
	accountID, err := strconv.ParseUint(string(ctx.FormValue("accountId")), 10, 64)
	if err != nil {
		return nil, errSubmissionWrongFormat
	}
	passphrase := string(ctx.FormValue("secretPhrase"))
	return &minerRound{
		Deadline:   deadline,
		Nonce:      nonce,
		Height:     height,
		AccountID:  accountID,
		Passphrase: passphrase,
	}, nil
}

func (a *aggregator) proxySubmitRound(ctx *fasthttp.RequestCtx, round *minerRound) error {
	v, _ := query.Values(round)
	// pool mode if no passphrase is present, else wallet mode
	if round.Passphrase != "" {
		v.Del("deadline")
	}

	_, respBody, err := a.client.Post(nil, a.proxyURL+"/burst?requestType=submitNonce&"+v.Encode(), nil)
	if err != nil {
		ctx.SetBody(errBytesFor(3, "error reaching pool or wallet"))
		return err
	}
	ctx.SetBody(respBody)
	return nil
}

func errBytesFor(code int, msg string) []byte {
	return []byte(fmt.Sprintf("{\"error\":{\"code\":%d,\"message\":\"%v\"}}", code, msg))
}

func (a *aggregator) refreshMiningInfo() error {
	_, respBody, err := a.client.Get(nil, a.proxyURL+"/burst?requestType=getMiningInfo")
	if err != nil {
		return err
	}
	var mi miningInfo
	if err := json.Unmarshal(respBody, &mi); err != nil {
		return err
	}
	var curMi *miningInfo
	if curMiV := a.curMiningInfo.Load(); curMiV != nil {
		curMi = curMiV.(*miningInfo)
	}
	switch {
	case curMi == nil || curMi.Height < mi.Height:
		mi.bytes = respBody
		a.curMiningInfo.Store(&mi)
	case curMi.Height > mi.Height: // fork handling
		mi.bytes = respBody
		a.curMiningInfo.Store(&mi)
		a.cache.Flush()
	case curMi.BaseTarget != mi.BaseTarget: // fork handling
		mi.bytes = respBody
		a.curMiningInfo.Store(&mi)
		a.cache.Flush()
	}
	return nil
}

func (a *aggregator) requestHandler(ctx *fasthttp.RequestCtx) {
	ip := ctx.RemoteIP()
	switch reqType := string(ctx.FormValue("requestType")); reqType {
	case "getMiningInfo":
		ctx.SetBody(a.curMiningInfo.Load().(*miningInfo).bytes)
	case "submitNonce":
		round, err := parseRound(ctx)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBody(errBytesFor(1, err.Error()))
			return
		}
		switch res := a.tryUpdateRound(ctx, ip.String(), round); res {
		case updated:
		case notUpdated:
			ctx.SetBody([]byte(
				fmt.Sprintf(
					"{\"deadline\":%d,\"result\":\"success\"}",
					// stupid miners send unadjusted deadlines, but expect adjusted :-(
					round.Deadline/uint64(a.curMiningInfo.Load().(*miningInfo).BaseTarget),
				)))
		case exceededMinersPerIP:
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBody(errBytesFor(2, errTooManySubmissionsDifferenMiners.Error()))
		}
	default:
		ctx.SetStatusCode(fasthttp.StatusBadRequest)
		ctx.SetBody(errBytesFor(4, errUnknownRequestType.Error()))
	}
}

func (a *aggregator) run(listenAddress, certFile, keyFile string) {
	if err := a.refreshMiningInfo(); err != nil {
		log.Fatalln("get initial mining info: ", err)
	}
	go func() {
		t := time.NewTicker(1 * time.Second)
		for {
			for range t.C {
				_ = a.refreshMiningInfo()
			}
		}
	}()

	var err error
	if certFile == "" {
		err = fasthttp.ListenAndServe(listenAddress, a.requestHandler)
	} else {
		err = fasthttp.ListenAndServeTLS(listenAddress, certFile, keyFile, a.requestHandler)
	}
	if err != nil {
		log.Fatalf("listen and serve: %s", err)
	}
}

func newAggregator(minersPerIP int, proxyURL string) *aggregator {
	return &aggregator{
		client:      &fasthttp.Client{},
		minersPerIP: minersPerIP,
		proxyURL:    proxyURL,
		cache:       cache.New(defaultCacheExpiration, defaultCacheExpiration),
	}
}

func init() {
	viper.SetDefault("MinersPerIP", 5)
	viper.SetDefault("ListenAddress", "127.0.0.1:6655")

	viper.SetConfigFile("config.yml")
	viper.ReadInConfig()

	if err := viper.Unmarshal(&cfg); err != nil {
		panic(err)
	}
}

func main() {
	a := newAggregator(cfg.MinersPerIP, cfg.ProxyURL)
	a.run(cfg.ListenAddress, cfg.CertFile, cfg.KeyFile)
}
