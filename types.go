package main

import (
	"strconv"
	"sync/atomic"
)

// atomic boolean
type atomicBool struct {
	flag int32
}

func (b *atomicBool) Set(value bool) {
	var i int32
	if value {
		i = 1
	}
	atomic.StoreInt32(&(b.flag), i)
}

func (b *atomicBool) Get() bool {
	if atomic.LoadInt32(&(b.flag)) != 0 {
		return true
	}
	return false
}

// FlexUInt64 handling json type inconsistencies of pools and wallets. integers are sometimes sent as string
// https://engineering.bitnami.com/articles/dealing-with-json-with-non-homogeneous-types-in-go.html
type FlexUInt64 int

// UnmarshalJSON JSON unmarshaller
func (fi *FlexUInt64) UnmarshalJSON(b []byte) error {
	if b[0] != '"' {
		return jsonx.Unmarshal(b, (*int)(fi))
	}
	var s string
	if err := jsonx.Unmarshal(b, &s); err != nil {
		return err
	}
	i, err := strconv.Atoi(s)
	if err != nil {
		return err
	}
	*fi = FlexUInt64(i)
	return nil
}
