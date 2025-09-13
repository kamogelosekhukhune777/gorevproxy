package main

import (
	"context"
	"crypto/tls"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sync/atomic"
	"time"
)

func newTransport() *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   5 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

func main() {
	target, _ := url.Parse("http://localhost:8081") // example backend
	transport := newTransport()

	// simple health check endpoint and readiness switching
	var ready int32 = 1
	mux := http.NewServeMux()

	mux.Handle("/healthz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	mux.Handle("/readyz", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&ready) == 1 {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("ready"))
			return
		}
		http.Error(w, "not ready", http.StatusServiceUnavailable)
	}))

	proxy := makeReverseProxy(target, transport)

	// main proxy handler with a simple rate-limit / concurrency guard could be added here
	mux.Handle("/", proxy)

	srv := &http.Server{
		Addr:         ":8443",
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// graceful shutdown
	idleConnsClosed := make(chan struct{})
	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, os.Interrupt)
		<-c
		atomic.StoreInt32(&ready, 0) // mark not ready for k8s readiness
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("HTTP server Shutdown: %v", err)
		}
		close(idleConnsClosed)
	}()

	log.Println("starting proxy on :8443")
	if err := srv.ListenAndServeTLS("server.crt", "server.key"); err != http.ErrServerClosed {
		log.Fatalf("ListenAndServeTLS: %v", err)
	}
	<-idleConnsClosed
	log.Println("server stopped")
}

func makeReverseProxy(target *url.URL, transport *http.Transport) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = transport

	// rewrite requests if needed
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		originalDirector(r)
		// set a header for tracing
		if r.Header.Get("X-Request-ID") == "" {
			r.Header.Set("X-Request-ID", time.Now().Format("20060102T150405.000000"))
		}
		// drop hop-by-hop headers we don't want forwarded
		r.Header.Del("Proxy-Connection")
	}

	// inspect/modify responses
	proxy.ModifyResponse = func(resp *http.Response) error {
		// add server header
		resp.Header.Set("Via", "MyGoProxy/1.0")
		return nil
	}

	// centralized error handling
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy error: %v %s %s", err, r.Method, r.URL)
		http.Error(w, "Bad Gateway", http.StatusBadGateway)
	}

	return proxy
}
