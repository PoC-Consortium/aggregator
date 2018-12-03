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
	"github.com/jessevdk/go-flags"
	"github.com/json-iterator/go"
	"github.com/patrickmn/go-cache"
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

var submitURL string
var c *cache.Cache
var curMiningInfo atomic.Value
var client *fasthttp.Client
var minersPerIP int

// var requestsPerSec int

var errSubmissionWrongFormat = errors.New("submission has wrong format")
var errTooManySubmissionsDifferenMiners = errors.New("too many submissions from different account ids by same ip")
var errUnknownRequestType = errors.New("unknown request type")

var opts struct {
	MinersPerIP int `short:"m" long:"miners-per-ip" description:"miners allowed per ip"`
	// ReqsPerSec  uint   `short:"r" long:"allowed-requests-per-sec" description:"allowed requests per second per ip"`
	SubmitURL  string `short:"u" long:"submit-url" description:"url to forward nonces to (pool, wallet)" required:"true"`
	ListenAddr string `short:"l" long:"listen-address" description:"address proxy listens on"`

	CertFile string `short:"c" long:"cert-file" description:"certificate file for tls"`
	KeyFile  string `short:"k" long:"key-file" description:"key file for tls"`
}

type minerRound struct {
	AccountID uint64 `url:"accountId"`
	Height    uint64 `url:"blockheight"`
	Deadline  uint64 `url:"deadline"`
	Nonce     uint64 `url:"nonce"`
}

type miningInfo struct {
	Height         uint64 `json:"height"`
	BaseTarget     uint64 `json:"baseTarget"`
	TargetDeadline uint64 `json:"targetDeadline"`
	GenSig         string `json:"generationSignature"`
	bytes          []byte
}

type ipData struct {
	accountIDtoRound map[uint64]*minerRound
	sync.Mutex
}

func tryUpdateRound(ctx *fasthttp.RequestCtx, ip string, round *minerRound) int {
	accountID := round.AccountID
	ipDataV, exists := c.Get(ip)
	if !exists {
		err := proxySubmitRound(ctx, round)
		if err != nil {
			return remoteErr
		}
		c.SetDefault(ip, &ipData{
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
		if minerCount == minersPerIP {
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
	if err := proxySubmitRound(ctx, round); err != nil {
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
	return &minerRound{
		Deadline:  deadline,
		Nonce:     nonce,
		Height:    height,
		AccountID: accountID,
	}, nil
}

func proxySubmitRound(ctx *fasthttp.RequestCtx, round *minerRound) error {
	v, _ := query.Values(round)
	_, respBody, err := client.Post(nil, submitURL+"/burst?requestType=submitNonce&"+v.Encode(), nil)
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

func refreshMiningInfo() error {
	_, respBody, err := client.Get(nil, submitURL+"/burst?requestType=getMiningInfo")
	if err != nil {
		return err
	}
	var mi miningInfo
	if err := json.Unmarshal(respBody, &mi); err != nil {
		return err
	}
	var curMi *miningInfo
	if curMiV := curMiningInfo.Load(); curMiV != nil {
		curMi = curMiV.(*miningInfo)
	}
	switch {
	case curMi == nil || curMi.Height < mi.Height:
		mi.bytes = respBody
		curMiningInfo.Store(&mi)
	case curMi.Height > mi.Height: // fork handling
		mi.bytes = respBody
		curMiningInfo.Store(&mi)
		c.Flush()
	case curMi.BaseTarget != mi.BaseTarget: // fork handling
		mi.bytes = respBody
		curMiningInfo.Store(&mi)
		c.Flush()
	}
	return nil
}

func requestHandler(ctx *fasthttp.RequestCtx) {
	ip := ctx.RemoteIP()
	switch reqType := string(ctx.FormValue("requestType")); reqType {
	case "getMiningInfo":
		ctx.SetBody(curMiningInfo.Load().(*miningInfo).bytes)
	case "submitNonce":
		round, err := parseRound(ctx)
		if err != nil {
			ctx.SetStatusCode(fasthttp.StatusBadRequest)
			ctx.SetBody(errBytesFor(1, err.Error()))
			return
		}
		switch res := tryUpdateRound(ctx, ip.String(), round); res {
		case updated:
		case notUpdated:
			ctx.SetBody([]byte(
				fmt.Sprintf(
					"{\"deadline\":%d,\"result\":\"success\"}",
					// stupid miners send unadjusted deadlines, but expect adjusted :-(
					round.Deadline/curMiningInfo.Load().(*miningInfo).BaseTarget,
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

func main() {
	if _, err := flags.Parse(&opts); err != nil {
		log.Fatal(err)
	}
	if opts.ListenAddr == "" {
		opts.ListenAddr = defaultListenAddr
	}
	if opts.MinersPerIP == 0 {
		minersPerIP = defaultMinersPerIP
	} else {
		minersPerIP = opts.MinersPerIP
	}
	submitURL = opts.SubmitURL

	client = &fasthttp.Client{}

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

	c = cache.New(defaultCacheExpiration, defaultCacheExpiration)

	var err error
	if opts.CertFile == "" {
		err = fasthttp.ListenAndServe(opts.ListenAddr, requestHandler)
	} else {
		err = fasthttp.ListenAndServeTLS(opts.ListenAddr, opts.CertFile, opts.KeyFile, requestHandler)
	}
	if err != nil {
		log.Fatalf("listen and serve: %s", err)
	}
}
