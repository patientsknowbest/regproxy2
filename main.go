package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.mercari.io/go-dnscache"
	"go.uber.org/zap"
)

func isSuccess(r *http.Response) bool {
	return r.StatusCode >= 200 && r.StatusCode < 400
}

func badRequest(resp http.ResponseWriter, errMsg string) {
	resp.WriteHeader(400)
	_, _ = resp.Write([]byte(errMsg))
}

func errResp(resp http.ResponseWriter, e error) {
	resp.WriteHeader(500)
	_, _ = resp.Write([]byte(e.Error()))
}

type RegStorage interface {
	Put(string, *url.URL) error
	Remove(string) error
	All() (map[string]*url.URL, error)
}

type RegStorageMemory struct {
	upstreams map[string]*url.URL
}

func (m *RegStorageMemory) Put(name string, url *url.URL) error {
	m.upstreams[name] = url
	return nil
}
func (m *RegStorageMemory) Remove(name string) error {
	delete(m.upstreams, name)
	return nil
}
func (m *RegStorageMemory) All() (map[string]*url.URL, error) {
	return maps.Clone(m.upstreams), nil
}

type RegStorageFile struct {
	fileName string
}

func NewRegStorageFile(fileName string) (*RegStorageFile, error) {
	content, err := os.ReadFile(fileName)
	if os.IsNotExist(err) {
		if err := os.WriteFile(fileName, []byte{}, os.ModePerm); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	upstreams, err := parseMap(content)
	if err != nil {
		return nil, err
	}
	for name, u := range upstreams {
		ups := registerRequest{
			Name:     name,
			Callback: u.String(),
		}
		log.Printf("Adding upstream from file %v", ups)
	}
	return &RegStorageFile{fileName: fileName}, nil
}

func (m *RegStorageFile) Put(name string, url *url.URL) error {
	mm, err := m.All()
	if err != nil {
		return err
	}
	mm[name] = url
	return m.write(mm)
}
func (m *RegStorageFile) Remove(name string) error {
	mm, err := m.All()
	if err != nil {
		return err
	}
	delete(mm, name)
	return m.write(mm)
}
func (m *RegStorageFile) write(content map[string]*url.URL) error {
	sb := strings.Builder{}
	for name, u := range content {
		sb.WriteString(name)
		sb.WriteString("=")
		sb.WriteString(u.String())
		sb.WriteString("\n")
	}
	return os.WriteFile(m.fileName, []byte(sb.String()), os.ModePerm)
}
func (m *RegStorageFile) All() (map[string]*url.URL, error) {
	file, err := os.ReadFile(m.fileName)
	if err != nil {
		return nil, err
	}
	return parseMap(file)
}

func parseMap(file []byte) (map[string]*url.URL, error) {
	scan := bufio.NewScanner(bytes.NewReader(file))
	res := make(map[string]*url.URL)
	for scan.Scan() {
		strs := strings.Split(scan.Text(), "=")
		if len(strs) != 2 {
			return nil, errors.New(fmt.Sprintf("corrupt storage, read invalid line [%s]", scan.Text()))
		}
		u, err := url.Parse(strs[1])
		if err != nil {
			return nil, err
		}
		res[strs[0]] = u
	}
	return res, nil
}

type RegProxy struct {
	client    *http.Client
	storage   RegStorage
	writeLock sync.Mutex
	handler   http.Handler
}

func (p *RegProxy) proxy(resp http.ResponseWriter, req *http.Request) {
	rc := make(chan *http.Response)
	ec := make(chan error)

	// Validate the request
	upstreams, err := p.storage.All()
	if err != nil {
		errResp(resp, err)
		return
	}
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
			requestId := uuid.New()
			req2 := req.Clone(req.Context())
			req2.RequestURI = "" // Isn't allowed to be set on client requests
			req2.Body = io.NopCloser(bytes.NewReader(b))
			req2.URL.Host = callback.Host
			req2.URL.Scheme = callback.Scheme
			log.Printf("Forwarding request %s (ID %v) to upstream %s at %s", req2.URL.Path, requestId, name, callback)
			requestStart := time.Now()
			resp2, err := p.client.Do(req2)

			requestDuration := time.Now().Sub(requestStart)

			if err != nil {
				log.Printf("Error forwarding request %s (ID %v) to upstream %s at %s: %v", req2.URL.Path, requestId, name, callback, err)
				ec <- err
			} else if !isSuccess(resp2) {
				log.Printf("Error forwarding request %s (ID %v) to upstream %s at %s: %v", req2.URL.Path, requestId, name, callback, resp2.StatusCode)
				ec <- fmt.Errorf("unsuccessful status code for request %s (ID %v): %v", req2.URL.Path, requestId, resp2.StatusCode)
			} else {
				log.Printf("Success forwarding request %s (ID %v) to upstream %s at %s in %v: %v", req2.URL.Path, requestId, name, callback, requestDuration, resp2.StatusCode)
				rc <- resp2
			}
		}(name, callback)
	}
	var latestSuccess *http.Response
	dc := req.Context().Done()
	// Wait for _all_ the responses, it's interesting to know which ones succeeded and
	// which ones failed during a single call.
	for range upstreams {
		select {
		case latest := <-rc:
			latestSuccess = latest
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

	// Otherwise write latest success
	resp.WriteHeader(latestSuccess.StatusCode)
	_ = latestSuccess.Write(resp)
}

type registerRequest struct {
	Name     string `json:"name"`
	Callback string `json:"callback"`
}

func (p *RegProxy) register(resp http.ResponseWriter, req *http.Request) {
	var q registerRequest
	err := json.NewDecoder(req.Body).Decode(&q)
	if err != nil {
		badRequest(resp, err.Error())
		return
	}
	upstream, err := url.Parse(q.Callback)
	if err != nil {
		log.Println("Failed to parse URL")
		badRequest(resp, err.Error())
		return
	}

	p.writeLock.Lock()
	defer p.writeLock.Unlock()
	log.Printf("Adding upstream %v", q)
	if err := p.storage.Put(q.Name, upstream); err != nil {
		errResp(resp, err)
		return
	}
	resp.WriteHeader(204)
}

type deregisterRequest struct {
	Name string `json:"name"`
}

func (p *RegProxy) deregister(resp http.ResponseWriter, req *http.Request) {
	var q deregisterRequest
	err := json.NewDecoder(req.Body).Decode(&q)
	if err != nil {
		badRequest(resp, err.Error())
		return
	}
	log.Printf("Removing upstream %v", q)
	if err := p.storage.Remove(q.Name); err != nil {
		errResp(resp, err)
		return
	}
	resp.WriteHeader(204)
}

type upstream struct {
	Name     string `json:"name"`
	Callback string `json:"callback"`
}

func (p *RegProxy) list(resp http.ResponseWriter, req *http.Request) {
	all, err := p.storage.All()
	if err != nil {
		errResp(resp, err)
		return
	}
	var res []upstream
	for k, v := range all {
		res = append(res, upstream{Name: k, Callback: v.String()})
	}
	resp.WriteHeader(200)
	err = json.NewEncoder(resp).Encode(res)
	if err != nil {
		log.Printf("error writing response %v", err)
	}
}

func (p *RegProxy) health(resp http.ResponseWriter, _ *http.Request) {
	// https://inadarei.github.io/rfc-healthcheck/
	resp.WriteHeader(200)
	resp.Header().Add("Content-Type", "application/health+json")
	_, _ = resp.Write([]byte(`{"status": "pass"}`))
}

func NewRegProxy(
	clientHttpTimeout, clientDialTimeout, clientKeepAliveInterval, dnsCacheRefresh, dnsLookupTimeout, clientMaxIdleTimeout *time.Duration,
	clientMaxIdleConnections *int64,
	useDnsCachePtr *bool,
	storage RegStorage,
) *RegProxy {
	dc := (&net.Dialer{
		Timeout:   *clientDialTimeout,
		KeepAlive: *clientKeepAliveInterval,
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
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:           http.ProxyFromEnvironment,
			DialContext:     dc,
			MaxIdleConns:    int(*clientMaxIdleConnections),
			IdleConnTimeout: *clientMaxIdleTimeout,
		},
		Timeout: *clientHttpTimeout,
	}
	rp := &RegProxy{
		storage: storage,
		client:  client,
	}
	sm := http.NewServeMux()
	sm.HandleFunc("/health", rp.health)
	sm.HandleFunc("/register", rp.register)
	sm.HandleFunc("/deregister", rp.deregister)
	sm.HandleFunc("/list", rp.list)
	sm.HandleFunc("/", rp.proxy)
	rp.handler = sm
	return rp
}

func main() {
	hostPtr := flag.String("host", "0.0.0.0", "The host to bind to")
	portPtr := flag.Int("port", 9876, "The port to bind to")
	serverReadTimeout := flag.Duration("server-read-timeout", 1*time.Second, "server read timeout")
	serverWriteTimeout := flag.Duration("server-write-timeout", 40*time.Second, "server write timeout")
	clientHttpTimeout := flag.Duration("client-http-timeout", 40*time.Second, "client timeout (for upstreams)")
	clientDialTimeout := flag.Duration("client-dial-timeout", 1*time.Second, "client dialer timeout")
	clientKeepAliveInterval := flag.Duration("client-keep-alive-interval", -1*time.Second, "client keep-alive interval")
	clientMaxIdleConnections := flag.Int64("client-max-idle-conns", 1, "client max idle connections (for connection pooling)")
	clientMaxIdleTimeout := flag.Duration("client-max-idle-timeout", 1*time.Second, "client idle connection timeout (for connection pooling)")
	useDnsCachePtr := flag.Bool("use-dns-cache", true, "use an internal DNS cache")
	dnsCacheRefresh := flag.Duration("dns-cache-refresh", 100*time.Hour, "interval for refrshing DNS cache")
	dnsLookupTimeout := flag.Duration("dns-lookup-timeout", 5*time.Second, "timeout for DNS lookups")
	registryStoreLocation := flag.String("storage-location", "memory", "registry data storage file location, or 'memory' for in-memory only")
	flag.Parse()

	log.Println("Starting regproxy with args")
	log.Println(os.Args)

	var storage RegStorage
	if *registryStoreLocation == "memory" {
		storage = &RegStorageMemory{upstreams: make(map[string]*url.URL)}
		log.Println("using in-memory storage")
	} else {
		st, err := NewRegStorageFile(*registryStoreLocation)
		if err != nil {
			log.Fatal(err)
		}
		storage = st
		log.Printf("using file storage at %s\n", *registryStoreLocation)
	}

	rp := NewRegProxy(
		clientHttpTimeout,
		clientDialTimeout,
		clientKeepAliveInterval,
		dnsCacheRefresh,
		dnsLookupTimeout,
		clientMaxIdleTimeout,
		clientMaxIdleConnections,
		useDnsCachePtr,
		storage,
	)

	srv := http.Server{
		Addr:         net.JoinHostPort(*hostPtr, strconv.Itoa(*portPtr)),
		Handler:      rp.handler,
		ReadTimeout:  *serverReadTimeout,
		WriteTimeout: *serverWriteTimeout,
	}
	log.Fatal(srv.ListenAndServe())
}
