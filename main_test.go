package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sync/errgroup"
	"io/ioutil"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"strconv"
	"testing"
	"time"
)

func withRegProxy(t *testing.T, f func(url string, t *testing.T)) {
	//serverReadTimeout := 1 * time.Second
	// serverWriteTimeout := 40 * time.Second
	clientHttpTimeout := 1 * time.Second
	clientDialTimeout := 1 * time.Second
	clientKeepAliveInterval := -1 * time.Second
	clientMaxIdleConnections := int64(1)
	clientMaxIdleTimeout := 1 * time.Second
	useDnsCache := true
	dnsCacheRefresh := 100 * time.Hour
	dnsLookupTimeout := 5 * time.Second
	rp := NewRegProxy(&clientHttpTimeout,
		&clientDialTimeout,
		&clientKeepAliveInterval,
		&dnsCacheRefresh,
		&dnsLookupTimeout,
		&clientMaxIdleTimeout,
		&clientMaxIdleConnections,
		&useDnsCache,
		&RegStorageMemory{upstreams: make(map[string]*url.URL)})
	srv := httptest.NewServer(rp.handler)
	defer srv.Close()
	f(srv.URL, t)
}

type registerTestCase struct {
	payload            string
	expectedHttpStatus int
}

func TestRegister(t *testing.T) {
	cases := []registerTestCase{
		{
			"{\"name\":\"foo\",\"callback\":\"baz\"}",
			204,
		},
		{
			"{flugelhorn}",
			400,
		},
	}
	for _, tcase := range cases {
		withRegProxy(t, func(url string, t *testing.T) {
			r, err := http.Post(url+"/register", "application/json", bytes.NewReader([]byte(tcase.payload)))
			if err != nil {
				t.Fatal(err)
			}
			if r.StatusCode != tcase.expectedHttpStatus {
				t.Fatalf("Wrong status code from /register %d expected %d", r.StatusCode, tcase.expectedHttpStatus)
			}
		})
	}
}

func TestConcurrentRegister(t *testing.T) {
	errorGroup, _ := errgroup.WithContext(context.Background())

	withRegProxy(t, func(url string, t *testing.T) {
		for i := 0; i < 100; i++ {
			request := fmt.Sprintf("{\"name\":\"foo-%d\",\"callback\":\"baz\"}", i)
			errorGroup.Go(func() error {
				r, err := http.Post(url+"/register", "application/json", bytes.NewReader([]byte(request)))
				if err != nil {
					return err
				}
				if r.StatusCode != 204 {
					return fmt.Errorf("Wrong status code from /register %d expected 204", r.StatusCode)
				}

				return nil
			})
		}

		resultingError := errorGroup.Wait()
		if resultingError != nil {
			t.Fatal(resultingError)
		}
	})
}

func register(url string, u upstream, t *testing.T) {
	b, _ := json.Marshal(u)
	r, err := http.Post(url+"/register", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if r.StatusCode != 204 {
		t.Fatalf("Failed to register test callback")
	}
}

func TestHappyPath(t *testing.T) {
	withRegProxy(t, doTestHappyPath)
}

func doTestHappyPath(url string, t *testing.T) {
	// GIVEN
	testResponse := "foo"
	handler := http.HandlerFunc(func(rr http.ResponseWriter, req *http.Request) {
		rr.Write([]byte(testResponse))
	})
	testServer1 := httptest.NewServer(handler)
	testServer2 := httptest.NewServer(handler)
	defer testServer1.Close()
	defer testServer2.Close()
	us1 := upstream{
		Name:     "foo",
		Callback: testServer1.URL,
	}
	us2 := upstream{
		Name:     "bar",
		Callback: testServer2.URL,
	}
	register(url, us1, t)
	register(url, us2, t)

	// WHEN
	r, err := http.Get(url)

	// THEN
	if r.StatusCode != 200 {
		t.Errorf("expected 200, got %v", r.StatusCode)
	}
	if err != nil {
		t.Fatal(err)
	}
	_, err = ioutil.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	/*rbs := string(rb)
	if rbs != testResponse {
		t.Errorf("Expected %v, but got %v", testResponse, rbs)
	}*/
}

func TestOneFail(t *testing.T) {
	withRegProxy(t, func(url string, t *testing.T) {
		// GIVEN
		testResponse := "foo"
		handler := http.HandlerFunc(func(rr http.ResponseWriter, req *http.Request) {
			rr.Write([]byte(testResponse))
		})
		handlerErr := http.HandlerFunc(func(rr http.ResponseWriter, req *http.Request) {
			rr.WriteHeader(404)
			rr.Write([]byte("nope"))
		})
		testServer1 := httptest.NewServer(handler)
		testServer2 := httptest.NewServer(handlerErr)
		defer testServer1.Close()
		defer testServer2.Close()
		us1 := upstream{
			Name:     "foo",
			Callback: testServer1.URL,
		}
		us2 := upstream{
			Name:     "bar",
			Callback: testServer2.URL,
		}
		register(url, us1, t)
		register(url, us2, t)

		// WHEN
		r, err := http.Get(url)

		// THEN
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 404 {
			t.Errorf("Expected 404, got %v", r.StatusCode)
		}
		/*rbs := string(rb)
		if rbs != testResponse {
			t.Errorf("Expected %v, but got %v", testResponse, rbs)
		}*/
	})
}

func TestNoSuchHost(t *testing.T) {
	withRegProxy(t, func(url string, t *testing.T) {
		// GIVEN
		register(url, upstream{
			Name:     "foo",
			Callback: "http://seriously.not.a.top.level.domain",
		}, t)

		// WHEN
		r, err := http.Get(url)

		// THEN
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 500 {
			t.Errorf("Expected 500, got %v", r.StatusCode)
		}
	})
}

func TestTimeout(t *testing.T) {
	withRegProxy(t, func(url string, t *testing.T) {
		// GIVEN
		handler := http.HandlerFunc(func(rr http.ResponseWriter, req *http.Request) {
			time.Sleep(2 * time.Second)
			rr.Write([]byte("ok"))
		})
		handler2 := http.HandlerFunc(func(rr http.ResponseWriter, req *http.Request) {
			rr.Write([]byte("ok"))
		})
		testServer1 := httptest.NewServer(handler)
		testServer2 := httptest.NewServer(handler2)
		defer testServer1.Close()
		defer testServer2.Close()
		us1 := upstream{
			Name:     "foo",
			Callback: testServer1.URL,
		}
		us2 := upstream{
			Name:     "bar",
			Callback: testServer2.URL,
		}
		register(url, us2, t)
		register(url, us1, t)

		// WHEN
		r, err := http.Get(url)

		// THEN
		if err != nil {
			t.Fatal(err)
		}
		if r.StatusCode != 500 {
			t.Errorf("Expected 500, got %v", r.StatusCode)
		}
	})
}

func TestFileStorage(t *testing.T) {
	file := path.Join(os.TempDir(), "regproxy2_test_"+strconv.Itoa(int(rand.Uint32())))
	defer func(name string) {
		_ = os.Remove(name)
	}(file)

	clientHttpTimeout := 1 * time.Second
	clientDialTimeout := 1 * time.Second
	clientKeepAliveInterval := -1 * time.Second
	clientMaxIdleConnections := int64(1)
	clientMaxIdleTimeout := 1 * time.Second
	useDnsCache := true
	dnsCacheRefresh := 100 * time.Hour
	dnsLookupTimeout := 5 * time.Second
	st, err := NewRegStorageFile(file)
	if err != nil {
		t.Fatal(err)
	}
	rp := NewRegProxy(&clientHttpTimeout,
		&clientDialTimeout,
		&clientKeepAliveInterval,
		&dnsCacheRefresh,
		&dnsLookupTimeout,
		&clientMaxIdleTimeout,
		&clientMaxIdleConnections,
		&useDnsCache,
		st)
	srv := httptest.NewServer(rp.handler)
	defer srv.Close()
	doTestHappyPath(srv.URL, t)
}
