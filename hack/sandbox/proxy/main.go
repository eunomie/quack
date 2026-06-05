// Command proxy is a minimal forward proxy that only tunnels CONNECT requests to
// an explicit allow-list of hosts. Guest agent containers point HTTPS_PROXY at
// it; everything not on the list is refused, bounding egress.
package main

import (
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

func parseAllow(s string) map[string]bool {
	m := map[string]bool{}
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			m[strings.ToLower(h)] = true
		}
	}
	return m
}

func allowed(allow map[string]bool, hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return allow[strings.ToLower(host)]
}

func main() {
	allow := parseAllow(os.Getenv("ALLOW"))
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8888"
	}
	srv := &http.Server{
		Addr: addr,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodConnect || !allowed(allow, r.Host) {
				http.Error(w, "forbidden", http.StatusForbidden)
				log.Printf("DENY %s %s", r.Method, r.Host)
				return
			}
			dst, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
			if err != nil {
				http.Error(w, "bad gateway", http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
			hj, ok := w.(http.Hijacker)
			if !ok {
				dst.Close()
				return
			}
			src, _, err := hj.Hijack()
			if err != nil {
				dst.Close()
				return
			}
			go func() { io.Copy(dst, src); dst.Close() }()
			io.Copy(src, dst)
			src.Close()
		}),
	}
	log.Printf("egress proxy on %s, allow=%v", addr, allow)
	log.Fatal(srv.ListenAndServe())
}
