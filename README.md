<img align="right" src="https://i.imgur.com/YdcmVnS.png" height="200">

# Aggregator - Burstminer Proxy

### Requirements
- go >= 1.11

### Compile

``` shell
go build
```

## Configure

``` yaml
# address on which aggregator serves mining info and accepts nonce submissions
listenAddress: 127.0.0.1:6655
# allowed miner account ids per ip
minersPerIP: 5
# pool/wallet to proxy to
proxyUrl: http://wallet.dev.burst-test.net:6876
# certificate for tls
certFile: ""
# key file for tls
keyFile: ""
```

### Run

``` shell
./aggregator
```
