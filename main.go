package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	dnscache "go.mercari.io/go-dnscache"
	"go.uber.org/zap"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var (
	client    *http.Client
	upstreams map[string]string
)

func proxy(resp http.ResponseWriter, req *http.Request) {
	rc := make(chan *http.Response)
	ec := make(chan error)
	// Read in the whole body, we'll need a new reader for each upstream
	b, e := io.ReadAll(req.Body)
	if e != nil {
		resp.WriteHeader(500)
		resp.Write([]byte(e.Error()))
		return
	}

	for name, callback := range upstreams {
		go func(name, callback string) {
			req2 := req.Clone(req.Context())
			req2.RequestURI = "" // Isn't allowed to be set on client requests
			req2.Body = io.NopCloser(bytes.NewReader(b))
			// Re-target it to the upstream
			u, err := url.Parse(callback)
			if err != nil {
				ec <- err
				return
			}
			req2.URL.Host = u.Host
			req2.URL.Scheme = u.Scheme
			log.Printf("Forwarding request %s to upstream %s at %s", req2.URL.Path, name, callback)
			resp2, err := client.Do(req2)

			if err != nil {
				log.Printf("Error forwarding request %s to upstream %s at %s: %v", req2.URL.Path, name, callback, err)
				ec <- err
			} else if resp2.StatusCode < 200 || resp2.StatusCode > 399 {
				ec <- fmt.Errorf("Got unexpected status code %d from upstream", resp2.StatusCode)
			} else {
				log.Printf("Success forwarding request %s to upstream %s at %s: %v", req2.URL.Path, name, callback, resp2.StatusCode)
				rc <- resp2
			}
		}(name, callback)
	}
	if len(upstreams) < 1 {
		resp.WriteHeader(400)
		resp.Write([]byte("No upstreams registered"))
		return
	}
	var latest *http.Response
	dc := req.Context().Done()
	for _, _ = range upstreams {
		select {
		case latest = <-rc:
		case e := <-ec:
			resp.WriteHeader(500)
			resp.Write([]byte(e.Error()))
			return
		case _ = <-dc:
			resp.WriteHeader(500)
			resp.Write([]byte(req.Context().Err().Error()))
			return
		}
	}
	resp.WriteHeader(latest.StatusCode)
	latest.Write(resp)
}

type upstream struct {
	Name     string `json:"name"`
	Callback string `json:"callback"`
}

func register(resp http.ResponseWriter, req *http.Request) {
	var q upstream
	err := json.NewDecoder(req.Body).Decode(&q)
	if err != nil {
		resp.WriteHeader(400)
		resp.Write([]byte(err.Error()))
		return
	}
	log.Printf("Adding upstream %v", q)
	upstreams[q.Name] = q.Callback
	resp.WriteHeader(204)
}

func mustParseDuration(text string) time.Duration {
	dur, err := time.ParseDuration(text)
	if err != nil {
		log.Fatal(err)
	}
	return dur
}

func main() {
	hostPtr := flag.String("host", "0.0.0.0", "The host to bind to")
	portPtr := flag.Int("port", 9876, "The port to bind to")
	srtPtr := flag.String("server-read-timeout", "1s", "server read timeout")
	swtPtr := flag.String("server-write-timeout", "40s", "server write timeout")
	chtPtr := flag.String("client-http-timeout", "40s", "client timeout (for upstreams)")
	cdtPtr := flag.String("client-dial-timeout", "1s", "client dialer timeout")
	ckaiPtr := flag.String("client-keep-alive-interval", "-1s", "client keep-alive interval")
	cmicPtr := flag.Int64("client-max-idle-conns", 1, "client max idle connections (for connection pooling)")
	cmitPtr := flag.String("client-max-idle-timeout", "1s", "client idle connection timeout (for connection pooling)")
	useDnsCachePtr := flag.Bool("use-dns-cache", true, "use an internal DNS cache")
	dnsCacheRefresh := flag.String("dns-cache-refresh", "100h", "interval for refrshing DNS cache")
	dnsLookupTimeout := flag.String("dns-lookup-timeout", "5s", "timeout for DNS lookups")
	flag.Parse()

	upstreams = make(map[string]string)

	dc := (&net.Dialer{
		Timeout:   mustParseDuration(*cdtPtr),
		KeepAlive: mustParseDuration(*ckaiPtr),
	}).DialContext

	// Use a caching DNS resolver
	// https://www.reddit.com/r/golang/comments/9wk812/go_package_for_caching_dns_lookup_results_in/
	if *useDnsCachePtr {
		resolver, err := dnscache.New(mustParseDuration(*dnsCacheRefresh), mustParseDuration(*dnsLookupTimeout), zap.NewNop())
		if err != nil {
			log.Fatal(err)
		}
		dc = dnscache.DialFunc(resolver, dc)
	}

	client = &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			DialContext:     dc,
			MaxIdleConns:    int(*cmicPtr),
			IdleConnTimeout: mustParseDuration(*cmitPtr),
		},
		Timeout: mustParseDuration(*chtPtr),
	}
	sm := http.NewServeMux()
	sm.HandleFunc("/register", register)
	sm.HandleFunc("/", proxy)
	srv := http.Server {
		Addr:         net.JoinHostPort(*hostPtr, strconv.Itoa(*portPtr)),
		Handler:      sm,
		ReadTimeout:  mustParseDuration(*srtPtr),
		WriteTimeout: mustParseDuration(*swtPtr),
	}
	log.Fatal(srv.ListenAndServe())
}
