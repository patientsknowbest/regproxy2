package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	dnscache "go.mercari.io/go-dnscache"
	"go.uber.org/zap"
)

var (
	client    *http.Client
	upstreams map[string]*url.URL
)

func isSuccess(r *http.Response) bool {
	return r.StatusCode >= 200 && r.StatusCode < 400
}

func badRequest(resp http.ResponseWriter, errMsg string) {
	resp.WriteHeader(400)
	resp.Write([]byte(errMsg))
}

func errResp(resp http.ResponseWriter, e error) {
	resp.WriteHeader(500)
	resp.Write([]byte(e.Error()))
}

func proxy(resp http.ResponseWriter, req *http.Request) {
	rc := make(chan *http.Response)
	ec := make(chan error)

	// Validate the request
	if len(upstreams) < 1 {
		badRequest(resp, "No upstreams registered")
		return
	}

	// Read in the whole body, we'll need a new reader for each upstream
	b, e := io.ReadAll(req.Body)
	if e != nil {
		errResp(resp, e)
		return
	}

	// Call upstreams in parallel
	for name, callback := range upstreams {
		go func(name string, callback *url.URL) {
			// Note although there is an existing
			// net/http/httputil.ReverseProxy implementation, it doesn't let us
			// forward to _multiple_ upstreams and choose a response based on header
			// so we can't use it here unfortunately
			req2 := req.Clone(req.Context())
			req2.RequestURI = "" // Isn't allowed to be set on client requests
			req2.Body = io.NopCloser(bytes.NewReader(b))
			req2.URL.Host = callback.Host
			req2.URL.Scheme = callback.Scheme
			log.Printf("Forwarding request %s to upstream %s at %s", req2.URL.Path, name, callback)
			resp2, err := client.Do(req2)

			if err != nil {
				log.Printf("Error forwarding request %s to upstream %s at %s: %v", req2.URL.Path, name, callback, err)
				ec <- err
			} else {
				log.Printf("Success forwarding request %s to upstream %s at %s: %v", req2.URL.Path, name, callback, resp2.StatusCode)
				rc <- resp2
			}
		}(name, callback)
	}
	var latestSuccess *http.Response
	var latestErr *http.Response
	dc := req.Context().Done()
	// Wait for _all_ the responses, it's interesting to know which ones succeeded and
	// which ones failed during a single call.
	for range upstreams {
		select {
		case latest := <-rc:
			if isSuccess(latest) {
				latestSuccess = latest
			} else {
				latestErr = latest
			}
		case e = <-ec:
		// If our own client cancelled, we should stop waiting
		case _ = <-dc:
			errResp(resp, req.Context().Err())
			return
		}
	}
	// Any errors, oopsie
	if e != nil {
		errResp(resp, e)
		return
	}
	// Prefer to return non-success responses
	var rr = latestSuccess
	if latestErr != nil {
		rr = latestErr
	}
	resp.WriteHeader(rr.StatusCode)
	rr.Write(resp)
}

type upstream struct {
	Name     string `json:"name"`
	Callback string `json:"callback"`
}

func register(resp http.ResponseWriter, req *http.Request) {
	var q upstream
	err := json.NewDecoder(req.Body).Decode(&q)
	if err != nil {
		badRequest(resp, err.Error())
		return
	}
	upstream, err := url.Parse(q.Callback)
	if err != nil {
		badRequest(resp, err.Error())
		return
	}
	log.Printf("Adding upstream %v", q)
	upstreams[q.Name] = upstream
	resp.WriteHeader(204)
}

func main() {
	hostPtr := flag.String("host", "0.0.0.0", "The host to bind to")
	portPtr := flag.Int("port", 9876, "The port to bind to")
	srtPtr := flag.Duration("server-read-timeout", 1*time.Second, "server read timeout")
	swtPtr := flag.Duration("server-write-timeout", 40*time.Second, "server write timeout")
	chtPtr := flag.Duration("client-http-timeout", 40*time.Second, "client timeout (for upstreams)")
	cdtPtr := flag.Duration("client-dial-timeout", 1*time.Second, "client dialer timeout")
	ckaiPtr := flag.Duration("client-keep-alive-interval", -1*time.Second, "client keep-alive interval")
	cmicPtr := flag.Int64("client-max-idle-conns", 1, "client max idle connections (for connection pooling)")
	cmitPtr := flag.Duration("client-max-idle-timeout", 1*time.Second, "client idle connection timeout (for connection pooling)")
	useDnsCachePtr := flag.Bool("use-dns-cache", true, "use an internal DNS cache")
	dnsCacheRefresh := flag.Duration("dns-cache-refresh", 100*time.Hour, "interval for refrshing DNS cache")
	dnsLookupTimeout := flag.Duration("dns-lookup-timeout", 5*time.Second, "timeout for DNS lookups")
	flag.Parse()

	log.Println("Starting regproxy with args")
	log.Println(os.Args)

	upstreams = make(map[string]*url.URL)

	dc := (&net.Dialer{
		Timeout:   *cdtPtr,
		KeepAlive: *ckaiPtr,
	}).DialContext

	// Use a caching DNS resolver
	// https://www.reddit.com/r/golang/comments/9wk812/go_package_for_caching_dns_lookup_results_in/
	if *useDnsCachePtr {
		logger, err := zap.NewDevelopment()
		if err != nil {
			log.Fatal(err)
		}
		resolver, err := dnscache.New(*dnsCacheRefresh, *dnsLookupTimeout, logger)
		if err != nil {
			log.Fatal(err)
		}
		dc = dnscache.DialFunc(resolver, dc)
		log.Printf("Using DNS cache")
	}

	client = &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			DialContext:     dc,
			MaxIdleConns:    int(*cmicPtr),
			IdleConnTimeout: *cmitPtr,
		},
		Timeout: *chtPtr,
	}
	sm := http.NewServeMux()
	sm.HandleFunc("/register", register)
	sm.HandleFunc("/", proxy)
	srv := http.Server{
		Addr:         net.JoinHostPort(*hostPtr, strconv.Itoa(*portPtr)),
		Handler:      sm,
		ReadTimeout:  *srtPtr,
		WriteTimeout: *swtPtr,
	}
	log.Fatal(srv.ListenAndServe())
}
