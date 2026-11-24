// Command loadtest measures a running pricetag server: shopper lookup
// throughput and latency, a large nightly price-file import, and how fast a
// price change reaches thousands of connected phones.
//
//	pricetag serve -addr :8080 -data /tmp/lt -public-rps 1000000 -max-sse-per-ip 100000 &
//	pricetag store create -data /tmp/lt -slug bench -name Bench -owner-email o@x.co   (first run only; use the printed password)
//	go run ./tools/loadtest -url http://127.0.0.1:8080 -slug bench -email o@x.co -password ...
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

func withCheck(body string) string {
	sum := 0
	for i := 0; i < len(body); i++ {
		d := int(body[len(body)-1-i] - '0')
		if i%2 == 0 {
			sum += 3 * d
		} else {
			sum += d
		}
	}
	return body + string(rune('0'+(10-sum%10)%10))
}

func code(i int) string { return withCheck(fmt.Sprintf("23%010d", i)) }

func pct(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(float64(len(sorted))*p))]
}

func rss(pid int) string {
	if pid == 0 {
		return "n/a"
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return "n/a"
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "VmRSS:") {
			return strings.TrimSpace(strings.TrimPrefix(l, "VmRSS:"))
		}
	}
	return "n/a"
}

func main() {
	base := flag.String("url", "http://127.0.0.1:8080", "server")
	slug := flag.String("slug", "bench", "store")
	email := flag.String("email", "", "owner e-mail (to create an ingest key)")
	pass := flag.String("password", "", "owner password")
	token := flag.String("token", "", "existing ingest key (skips login)")
	products := flag.Int("products", 50000, "catalogue size to import")
	conc := flag.Int("conc", 128, "concurrent lookup clients")
	dur := flag.Duration("duration", 8*time.Second, "lookup phase length")
	subs := flag.Int("sse", 5000, "live (SSE) connections to open")
	pid := flag.Int("server-pid", 0, "server process id, to report its memory")
	flag.Parse()

	var lim syscall.Rlimit
	if syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim) == nil {
		lim.Cur = lim.Max
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}
	root := strings.TrimRight(*base, "/") + "/api/v1/s/" + *slug + "/"
	fmt.Printf("server memory at start: %s\n\n", rss(*pid))

	// --- token
	tok := *token
	if tok == "" {
		jar := &cookieJar{}
		hc := &http.Client{Jar: jar}
		req, _ := http.NewRequest("POST", root+"admin/login", strings.NewReader(fmt.Sprintf(`{"email":%q,"password":%q}`, *email, *pass)))
		req.Header.Set("X-PT-CSRF", "1")
		req.Header.Set("Content-Type", "application/json")
		res, err := hc.Do(req)
		if err != nil || res.StatusCode != 200 {
			fmt.Println("login failed", err)
			os.Exit(1)
		}
		req, _ = http.NewRequest("POST", root+"admin/tokens", strings.NewReader(`{"name":"loadtest"}`))
		req.Header.Set("X-PT-CSRF", "1")
		req.Header.Set("Content-Type", "application/json")
		res, err = hc.Do(req)
		if err != nil || res.StatusCode != 201 {
			fmt.Println("token failed", err)
			os.Exit(1)
		}
		var t struct{ Token string }
		json.NewDecoder(res.Body).Decode(&t)
		tok = t.Token
	}
	ingest := func(body []byte, ctype, q string) (time.Duration, map[string]any) {
		req, _ := http.NewRequest("POST", root+"ingest"+q, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", ctype)
		t0 := time.Now()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Println("ingest error:", err)
			os.Exit(1)
		}
		defer res.Body.Close()
		var out map[string]any
		json.NewDecoder(res.Body).Decode(&out)
		if res.StatusCode != 200 {
			fmt.Println("ingest status", res.StatusCode, out)
			os.Exit(1)
		}
		return time.Since(t0), out
	}

	// --- 1. nightly import
	var csv bytes.Buffer
	csv.WriteString("barcode,name,size,category,price\n")
	for i := 0; i < *products; i++ {
		fmt.Fprintf(&csv, "%s,Product number %d,500 g,Category %d,%d\n", code(i), i, i%40, 1000+(i%900)*50)
	}
	d, r := ingest(csv.Bytes(), "text/csv", "")
	fmt.Printf("IMPORT  %d products (%.1f MB CSV): %v  -> created %v, one version, one push event\n", *products, float64(csv.Len())/1e6, d.Round(time.Millisecond), r["created"])
	d, r = ingest(csv.Bytes(), "text/csv", "")
	fmt.Printf("IMPORT  same file again: %v -> unchanged %v (nothing pushed to phones)\n", d.Round(time.Millisecond), r["unchanged"])
	csv2 := bytes.ReplaceAll(csv.Bytes(), []byte(",1000\n"), []byte(",1050\n"))
	d, r = ingest(csv2, "text/csv", "")
	fmt.Printf("IMPORT  file with ~%v changed prices: %v\n\n", r["updated"], d.Round(time.Millisecond))

	// --- 2. lookups
	tr := &http.Transport{MaxIdleConnsPerHost: *conc, MaxConnsPerHost: *conc}
	hc := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	var ok, notOK, limited atomic.Int64
	var mu sync.Mutex
	var all []time.Duration
	stop := time.Now().Add(*dur)
	var wg sync.WaitGroup
	for g := 0; g < *conc; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rg := rand.New(rand.NewPCG(uint64(g), 7))
			var lat []time.Duration
			for time.Now().Before(stop) {
				t0 := time.Now()
				res, err := hc.Get(root + "items/" + code(rg.IntN(*products)))
				if err != nil {
					notOK.Add(1)
					continue
				}
				io.Copy(io.Discard, res.Body)
				res.Body.Close()
				switch res.StatusCode {
				case 200:
					ok.Add(1)
					lat = append(lat, time.Since(t0))
				case 429:
					limited.Add(1)
				default:
					notOK.Add(1)
				}
			}
			mu.Lock()
			all = append(all, lat...)
			mu.Unlock()
		}(g)
	}
	wg.Wait()
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	fmt.Printf("LOOKUP  %d clients for %v on a %d-product store (one machine, loopback)\n", *conc, *dur, *products)
	fmt.Printf("        %.0f requests/s   ok=%d errors=%d rate-limited=%d\n", float64(ok.Load())/dur.Seconds(), ok.Load(), notOK.Load(), limited.Load())
	fmt.Printf("        latency  p50 %v   p95 %v   p99 %v   max %v\n", pct(all, .5).Round(10*time.Microsecond), pct(all, .95).Round(10*time.Microsecond), pct(all, .99).Round(10*time.Microsecond), all[len(all)-1].Round(10*time.Microsecond))
	fmt.Printf("        server memory now: %s\n\n", rss(*pid))

	// --- 3. live fan-out
	if *subs > 0 {
		var ready, got atomic.Int64
		var firstMu sync.Mutex
		var arrivals []time.Time
		var changeSent atomic.Int64
		var wg2 sync.WaitGroup
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		sc := &http.Client{Transport: &http.Transport{DialContext: dialer.DialContext, MaxIdleConnsPerHost: 1}}
		for i := 0; i < *subs; i++ {
			wg2.Add(1)
			go func() {
				defer wg2.Done()
				res, err := sc.Get(root + "events")
				if err != nil || res.StatusCode != 200 {
					return
				}
				defer res.Body.Close()
				rd := bufio.NewReaderSize(res.Body, 4096)
				for {
					line, err := rd.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "event: hello") {
						ready.Add(1)
					}
					if strings.HasPrefix(line, "event: change") && changeSent.Load() > 0 {
						firstMu.Lock()
						arrivals = append(arrivals, time.Now())
						firstMu.Unlock()
						got.Add(1)
					}
				}
			}()
			if i%500 == 499 {
				time.Sleep(20 * time.Millisecond)
			}
		}
		for t0 := time.Now(); ready.Load() < int64(*subs) && time.Since(t0) < 20*time.Second; {
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Printf("LIVE    %d phones connected; server memory: %s\n", ready.Load(), rss(*pid))
		for round := 1; round <= 3; round++ {
			firstMu.Lock()
			arrivals = arrivals[:0]
			firstMu.Unlock()
			got.Store(0)
			body, _ := json.Marshal(map[string]any{"products": []map[string]any{{"code": code(1), "name": "Product number 1", "size": "500 g", "category": "Category 1", "price": 2000 + round}}})
			t0 := time.Now()
			changeSent.Store(t0.UnixNano())
			ingest(body, "application/json", "")
			want := ready.Load()
			for time.Since(t0) < 10*time.Second && got.Load() < want {
				time.Sleep(2 * time.Millisecond)
			}
			firstMu.Lock()
			ds := make([]time.Duration, len(arrivals))
			for i, a := range arrivals {
				ds[i] = a.Sub(t0)
			}
			firstMu.Unlock()
			sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
			fmt.Printf("        price change #%d reached %d/%d phones  p50 %v  p95 %v  last %v (includes the HTTP edit itself)\n", round, got.Load(), want, pct(ds, .5).Round(time.Millisecond), pct(ds, .95).Round(time.Millisecond), pct(ds, 1).Round(time.Millisecond))
		}
		fmt.Printf("        server memory with %d connections: %s\n", ready.Load(), rss(*pid))
		sc.CloseIdleConnections()
		os.Exit(0)
	}
}

type cookieJar struct {
	mu sync.Mutex
	c  []*http.Cookie
}

func (j *cookieJar) SetCookies(_ *urlT, cs []*http.Cookie) {
	j.mu.Lock()
	j.c = append(j.c, cs...)
	j.mu.Unlock()
}
func (j *cookieJar) Cookies(_ *urlT) []*http.Cookie { j.mu.Lock(); defer j.mu.Unlock(); return j.c }
