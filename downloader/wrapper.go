// Copyright 2016 laosj Author @songtianyi. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package downloader

import (
	"fmt"
	"github.com/songtianyi/laosj/storage"
	"github.com/songtianyi/rrframework/connector/redis"
	"github.com/songtianyi/rrframework/logs"
	"io/ioutil"
	"net"
	"net/http"
	"time"
)

const (
	URL_CACHE_KEY = "DATA:IMAGE:DOWNLOADED:URLS"
)

type Url struct {
	v string
}

type Downloader struct {
	// exported
	ConcurrencyLimit int    // number of goroutines to download
	RedisConnStr     string // redis connection string
	SourceQueue      string
	Store            storage.StorageWrapper
	UrlChannelFactor int

	// inner use
	sema chan struct{} // for concurrency-limiting
	flag chan struct{}
	urls chan Url
	rc   *rrredis.RedisClient
}

func (s *Downloader) Start() {
	// connect redis
	err, rc := rrredis.GetRedisClient(s.RedisConnStr)
	if err != nil {
		logs.Error("Start downloader fail %s", err)
		return
	}
	s.rc = rc

	// create channel
	s.sema = make(chan struct{}, s.ConcurrencyLimit)
	s.flag = make(chan struct{})
	s.urls = make(chan Url, s.ConcurrencyLimit*s.UrlChannelFactor)

	go func() {
	loop1:
		for {
			url, err := rc.LPop(s.SourceQueue)
			if err == rrredis.Nil {
				// empty queue, sleep
				time.Sleep(5 * time.Second)
				// continue the loop
				continue
			}
			if err != nil {
				logs.Error(err)
				// wait recovery
				time.Sleep(500 * time.Second)
				// continue the loop
				continue
			}
			select {
			case <-s.flag:
				// be stopped
				break loop1
			case s.urls <- Url{v: url}:
				// trying to push url to urls channel
			}
		}
	}()

	tick := time.Tick(2 * time.Second)

loop2:
	for {
		select {
		case <-s.flag:
			// be stopped
			for url := range s.urls {
				// push back to queue
				if _, err := rc.RPush(s.SourceQueue, url.v); err != nil {
					logs.Error(err)
				}
			}
			break loop2
		case s.sema <- struct{}{}:
			// s.sema not full
			url, ok := <-s.urls
			if !ok {
				// channel closed
				logs.Error("Channel s.urls may be closed")
				// TODO what's the right way to deal this situation?
				break loop2
			}
			go func() {
				if err := s.download(url.v, rc); err != nil {
					// download fail
					// push back to redis
					logs.Error("Download %s fail, %s", url.v, err)
					if _, err := rc.RPush(s.SourceQueue, url.v); err != nil {
						logs.Error("Push back to redis failed, %s", err)
					}
				} else {
					// download success
					// push downloaded url to cache
					if err := rc.HMSet(URL_CACHE_KEY, map[string]string{
						url.v: "1",
					}); err != nil {
						logs.Error("Push to cache failed,%s", err)
					}
				}
			}()
		case <-tick:
			logs.Info("In queue: %d, doing: %d", len(s.urls), len(s.sema))
		}
	}

}

func (s *Downloader) Stop() {
	close(s.flag)
}

func (s *Downloader) WaitCloser() {
loop:
	for {
		select {
		case <-time.After(1 * time.Second):
			// len
			if len(s.urls) > 0 || len(s.sema) > 1 {
				// TODO there is a chance that last url downloading process be interupted
				continue
			}
			if v, err := s.rc.LLen(s.SourceQueue); err != nil || v != 0 {
				continue
			}
			break loop
		}
	}
}

func (s *Downloader) download(url string, rc *rrredis.RedisClient) error {

	defer func() { <-s.sema }() // release

	// check if this url is downloaded
	exist, err := rc.HMExists(URL_CACHE_KEY, url)
	if err != nil {
		return err
	}
	if exist {
		// downloaded
		logs.Info("%s downloaded", url)
		return nil
	}

	logs.Info("Downloading %s", url)
	client := http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) { return net.DialTimeout(network, addr, 3*time.Second) },
		},
	}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("StatusCode %d", response.StatusCode)
	}

	// get binary
	b, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}
	// save
	if err := s.Store.Save(b); err != nil {
		return err
	}
	return nil
}