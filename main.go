package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var (
	upstreams map[string]string
)

func proxy(resp http.ResponseWriter, req *http.Request) {
	rc := make(chan *http.Response)
	ec := make(chan error)
	for name, callback := range upstreams {
		go func() {
			req2 := req.Clone(req.Context())
			req2.RequestURI = "" // Isn't allowed to be set on client requests
			
			// Re-target it to the upstream
			u, err := url.Parse(callback)
			if err != nil {
				ec <- err
				return
			}
			req2.URL.Host = u.Host
			req2.URL.Scheme = u.Scheme
			log.Printf("Forwarding request %s to upstream %s at %s", req2.URL.Path, name, callback)
			resp2, err := http.DefaultClient.Do(req2)
			
			if err != nil  {
				log.Printf("Error forwarding request %s to upstream %s at %s: %v", req2.URL.Path, name, callback, err)
				ec <- err
			} else if resp2.StatusCode < 200 || resp2.StatusCode > 399 {
				ec <- fmt.Errorf("Got unexpected status code %d from upstream", resp2.StatusCode)
			} else {
				log.Printf("Success forwarding request %s to upstream %s at %s: %v", req2.URL.Path, name, callback, resp2.StatusCode)
				rc <- resp2
			}
		}()
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
		case latest = <- rc:
		case e := <- ec:
			resp.WriteHeader(500)
			resp.Write([]byte(e.Error()))
			return
		case _ = <- dc:
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

func main() {
	hostPtr := flag.String("host", "0.0.0.0", "The host to bind to")
	portPtr := flag.Int("port", 9876, "The port to bind to")
	srtPtr := flag.Int("server-read-timeout", 1000, "server read timeout")
	swtPtr := flag.Int64("server-write-timeout", 40000, "server write timeout")
	ctPtr := flag.Int64("client-timeout", 40000, "client timeout (for upstreams)")
	flag.Parse()
	upstreams = make(map[string]string)
	http.HandleFunc("/register", register);
	http.HandleFunc("/", proxy);
	srt := time.Duration(*srtPtr) * time.Millisecond
	swt := time.Duration(*swtPtr) * time.Millisecond
	http.DefaultClient.Timeout = time.Duration(*ctPtr) * time.Millisecond
	srv := http.Server{
		Addr: net.JoinHostPort(*hostPtr, strconv.Itoa(*portPtr)),
		Handler: http.DefaultServeMux,
		ReadTimeout: srt,
		WriteTimeout: swt,
	}
	log.Fatal(srv.ListenAndServe())
}
