// Command pricetag runs the shop price-check service and its admin tools.
//
//	pricetag serve  [-addr :8080] [-data ./data]
//	pricetag store  create|list|backup|passwd ...
//	pricetag demo   [-slug demo]                 seed a sample supermarket
//	pricetag demo-tick -url ... -token ...       simulate live price changes
//	pricetag push   -url ... -token ... -file    reference POS connector
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"pricetag/internal/auth"
	"pricetag/internal/demo"
	"pricetag/internal/server"
	"pricetag/internal/store"
)

var version = "dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "serve":
		err = cmdServe(args)
	case "store":
		err = cmdStore(args)
	case "demo":
		err = cmdDemo(args)
	case "demo-tick":
		err = cmdTick(args)
	case "push":
		err = cmdPush(args)
	case "version", "-v", "--version":
		fmt.Println("pricetag", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`pricetag - shop price check service

Usage:
  pricetag serve      [-addr :8080] [-data ./data] [-trust-proxy] [-dev-tls]
  pricetag store create -slug S -name "Store name" -owner-email E [-owner-name N] [-password P]
  pricetag store list
  pricetag store backup -slug S -out FILE
  pricetag store passwd -slug S -email E [-password P]
  pricetag demo       [-slug demo]
  pricetag demo-tick  -url URL -slug S -token T [-every 3s]
  pricetag push       -url URL -slug S -token T -file prices.csv [-dry-run]

Environment (used when the matching flag is not given):
  PRICETAG_ADDR  PRICETAG_DATA  PRICETAG_OPERATOR_TOKEN  PRICETAG_TRUST_PROXY=1
`)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newLogger(json_ bool) *slog.Logger {
	if json_ {
		return slog.New(slog.NewJSONHandler(os.Stdout, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, nil))
}

// ---- serve ---------------------------------------------------------------------------

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", env("PRICETAG_ADDR", ":8080"), "listen address")
	data := fs.String("data", env("PRICETAG_DATA", "./data"), "directory holding one .db file per store")
	trust := fs.Bool("trust-proxy", env("PRICETAG_TRUST_PROXY", "") == "1", "trust X-Forwarded-* from a reverse proxy in front")
	access := fs.Bool("access-log", false, "log every request")
	certF := fs.String("tls-cert", "", "TLS certificate file (optional; use a reverse proxy like Caddy in production)")
	keyF := fs.String("tls-key", "", "TLS key file")
	devTLS := fs.Bool("dev-tls", false, "serve HTTPS with a throw-away self-signed certificate (lets a phone use the camera on your LAN)")
	jsonLog := fs.Bool("json-log", false, "JSON log lines")
	fs.Parse(args)

	log := newLogger(*jsonLog)
	if err := os.MkdirAll(*data, 0o750); err != nil {
		return err
	}
	srv, err := server.New(server.Config{DataDir: *data, TrustProxy: *trust, OperatorToken: os.Getenv("PRICETAG_OPERATOR_TOKEN"), AccessLog: *access, Logger: log})
	if err != nil {
		return err
	}
	hs := &http.Server{
		Addr: *addr, Handler: srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	useTLS := false
	switch {
	case *devTLS:
		cert, err := selfSigned()
		if err != nil {
			return err
		}
		hs.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
		useTLS = true
	case *certF != "" && *keyF != "":
		useTLS = true
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	scheme := "http"
	if useTLS {
		scheme = "https"
	}
	log.Info("listening", "addr", *addr, "scheme", scheme, "data", *data, "version", version)
	printReachable(scheme, *addr)
	go func() {
		if useTLS {
			errc <- hs.ListenAndServeTLS(*certF, *keyF)
		} else {
			errc <- hs.ListenAndServe()
		}
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	srv.Shutdown() // ends event streams so Shutdown below is quick
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return hs.Shutdown(sctx)
}

func printReachable(scheme, addr string) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	fmt.Printf("  open: %s://localhost:%s/\n", scheme, port)
	for _, ip := range lanIPs() {
		fmt.Printf("  on your network: %s://%s:%s/\n", scheme, ip, port)
	}
}

func lanIPs() []string {
	var out []string
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
			out = append(out, ipn.IP.String())
		}
	}
	return out
}

func selfSigned() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "pricetag dev"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(30 * 24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	for _, ip := range lanIPs() {
		tpl.IPAddresses = append(tpl.IPAddresses, net.ParseIP(ip))
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, nil
}

// ---- store admin ----------------------------------------------------------------------

func cmdStore(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: pricetag store create|list|backup|passwd")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("store "+sub, flag.ExitOnError)
	data := fs.String("data", env("PRICETAG_DATA", "./data"), "data directory")
	slug := fs.String("slug", "", "store address (lowercase letters, digits, hyphens)")
	name := fs.String("name", "", "store display name")
	email := fs.String("owner-email", "", "owner e-mail")
	ownerName := fs.String("owner-name", "Owner", "owner name")
	pw := fs.String("password", "", "password (generated when empty)")
	currency := fs.String("currency", "UGX", "currency code")
	decimals := fs.Int("decimals", 0, "currency decimals (UGX has none)")
	out := fs.String("out", "", "backup file to write")
	userEmail := fs.String("email", "", "user e-mail (passwd)")
	fs.Parse(rest)
	ctx := context.Background()

	switch sub {
	case "create":
		password, generated := *pw, false
		if password == "" {
			password, generated = auth.RandomPassword(14), true
		}
		set := store.DefaultSettings(*name)
		set.Currency, set.Decimals = *currency, *decimals
		st, err := store.Create(*data, *slug, set, *email, *ownerName, password)
		if err != nil {
			return err
		}
		st.Close()
		fmt.Printf("created store %q\n  shopper page: /s/%s/\n  admin page:   /s/%s/admin/\n  owner login:  %s\n", *slug, *slug, *slug, *email)
		if generated {
			fmt.Printf("  password:     %s   (shown once; the owner should change it)\n", password)
		}
		return nil
	case "list":
		reg := server.NewRegistry(*data, slog.New(slog.NewTextHandler(io.Discard, nil)))
		l, err := reg.List()
		for _, s := range l {
			fmt.Println(s)
		}
		return err
	case "backup":
		st, err := store.Open(*data, *slug)
		if err != nil {
			return err
		}
		defer st.Close()
		if *out == "" {
			*out = filepath.Join(".", fmt.Sprintf("%s-%s.db", *slug, time.Now().Format("20060102-150405")))
		}
		if err := st.Backup(ctx, *out); err != nil {
			return err
		}
		fmt.Println("backup written to", *out)
		return nil
	case "passwd":
		st, err := store.Open(*data, *slug)
		if err != nil {
			return err
		}
		defer st.Close()
		password, generated := *pw, false
		if password == "" {
			password, generated = auth.RandomPassword(14), true
		}
		if err := st.SetPasswordByEmail(ctx, *userEmail, password); err != nil {
			return err
		}
		fmt.Println("password updated; all sessions for that user were signed out")
		if generated {
			fmt.Println("  new password:", password)
		}
		return nil
	}
	return fmt.Errorf("unknown store command %q", sub)
}

// ---- demo ------------------------------------------------------------------------------

func cmdDemo(args []string) error {
	fs := flag.NewFlagSet("demo", flag.ExitOnError)
	data := fs.String("data", env("PRICETAG_DATA", "./data"), "data directory")
	slug := fs.String("slug", "demo", "demo store address")
	fs.Parse(args)
	info, err := demo.Seed(*data, *slug)
	if err != nil {
		return err
	}
	if info.Created {
		// An ingest key so `demo-tick` and `push` can be tried straight away.
		st, err := store.Open(*data, *slug)
		if err != nil {
			return err
		}
		tok, _, err := st.CreateToken(context.Background(), "demo", "demo connector")
		st.Close()
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(*data, *slug+".token"), []byte(tok+"\n"), 0o600); err != nil {
			return err
		}
	}
	state := "already exists (left untouched)"
	if info.Created {
		state = "created"
	}
	fmt.Printf("demo store %q %s\n  shopper page: %s\n  test barcodes: /s/%s/test-sheet\n  admin page:   %s\n  owner login:  %s / %s\n  clerk login:  %s / %s\n",
		*slug, state, info.URL, *slug, info.AdminURL, demo.OwnerEmail, demo.OwnerPassword, demo.EditorEmail, demo.EditorPass)
	if b, err := os.ReadFile(filepath.Join(*data, *slug+".token")); err == nil {
		fmt.Printf("  ingest key:   %s", b)
	}
	return nil
}

// ---- push (reference POS connector) ---------------------------------------------------------

func post(url, token, ctype string, body io.Reader) ([]byte, int, error) {
	req, err := http.NewRequest("POST", url, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", ctype)
	res, err := (&http.Client{Timeout: 3 * time.Minute}).Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return b, res.StatusCode, nil
}

func cmdPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ExitOnError)
	base := fs.String("url", "http://localhost:8080", "server base URL")
	slug := fs.String("slug", "demo", "store address")
	token := fs.String("token", os.Getenv("PRICETAG_TOKEN"), "integration key (or PRICETAG_TOKEN)")
	file := fs.String("file", "", "price file (CSV) exported from the POS")
	dry := fs.Bool("dry-run", false, "check the file and show what would change, without saving")
	fs.Parse(args)
	if *file == "" || *token == "" {
		return errors.New("-file and -token are required")
	}
	f, err := os.Open(*file)
	if err != nil {
		return err
	}
	defer f.Close()
	u := strings.TrimRight(*base, "/") + "/api/v1/s/" + *slug + "/ingest"
	if *dry {
		u += "?dry_run=1"
	}
	b, code, err := post(u, *token, "text/csv", f)
	if err != nil {
		return err
	}
	fmt.Printf("%d %s\n", code, bytes.TrimSpace(b))
	if code != 200 {
		return fmt.Errorf("server refused the file (HTTP %d)", code)
	}
	return nil
}

// ---- demo-tick (live price change simulator) ------------------------------------------------------

func cmdTick(args []string) error {
	fs := flag.NewFlagSet("demo-tick", flag.ExitOnError)
	base := fs.String("url", "http://localhost:8080", "server base URL")
	slug := fs.String("slug", "demo", "store address")
	token := fs.String("token", os.Getenv("PRICETAG_TOKEN"), "integration key")
	data := fs.String("data", env("PRICETAG_DATA", "./data"), "data directory (to read the demo key if -token is empty)")
	every := fs.Duration("every", 3*time.Second, "time between price changes")
	count := fs.Int("count", 0, "stop after N changes (0 = until Ctrl-C)")
	fs.Parse(args)
	if *token == "" {
		if b, err := os.ReadFile(filepath.Join(*data, *slug+".token")); err == nil {
			*token = strings.TrimSpace(string(b))
		}
	}
	if *token == "" {
		return errors.New("-token is required")
	}
	root := strings.TrimRight(*base, "/") + "/api/v1/s/" + *slug
	res, err := http.Get(root + "/sample?n=40")
	if err != nil {
		return err
	}
	var sample struct {
		Items []struct {
			Code, Name, Size string
			Price            int64
		}
	}
	err = json.NewDecoder(res.Body).Decode(&sample)
	res.Body.Close()
	if err != nil || len(sample.Items) == 0 {
		return fmt.Errorf("could not read products from %s (is the store seeded?)", root)
	}
	fmt.Printf("changing a random price every %s; Ctrl-C to stop\n", *every)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	t := time.NewTicker(*every)
	defer t.Stop()
	for n := 0; *count == 0 || n < *count; n++ {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		it := &sample.Items[mrand.IntN(len(sample.Items))]
		pct := 0.05 + mrand.Float64()*0.10
		if mrand.IntN(2) == 0 {
			pct = -pct
		}
		np := int64(float64(it.Price)*(1+pct)/100) * 100 // keep prices on round 100 shillings
		if np < 100 {
			np = 100
		}
		body, _ := json.Marshal(map[string]any{"products": []map[string]any{{"code": it.Code, "name": it.Name, "size": it.Size, "price": np}}})
		b, code, err := post(root+"/ingest", *token, "application/json", bytes.NewReader(body))
		if err != nil || code != 200 {
			fmt.Printf("  push failed: %v %s\n", err, b)
			continue
		}
		fmt.Printf("  %-26s %-10s %7d -> %7d UGX\n", it.Name, it.Size, it.Price, np)
		it.Price = np
	}
	return nil
}
