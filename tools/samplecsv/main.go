// Command samplecsv writes a realistic supermarket price file for testing the
// Import page and the POS connector. No public price list for Ugandan shops
// exists (Open Prices had zero UGX prices when this was written), so PRICES
// ARE MADE UP, but plausible for each kind of product.
//
//	go run ./tools/samplecsv -out my-shop-prices.csv                 # 300 invented products
//	go run ./tools/samplecsv -source openfoodfacts -country uganda    # real barcodes sold in Uganda
//
// The file uses headers like a till export ("Barcode", "Item Description",
// "Selling Price") and prices like "12,500", to exercise the importer the way
// a real shop file would.
package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type row struct {
	Code, Name, Size, Dept string
	Price                  int64
}

func checkDigit(body string) string {
	sum := 0
	for i := 0; i < len(body); i++ {
		d := int(body[len(body)-1-i] - '0')
		if i%2 == 0 {
			sum += 3 * d
		} else {
			sum += d
		}
	}
	return body + strconv.Itoa((10-sum%10)%10)
}

func validGTIN(s string) bool {
	if n := len(s); n != 8 && n != 12 && n != 13 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return checkDigit(s[:len(s)-1]) == s
}

// price picks a stable made-up price in [lo, hi], rounded to 100 shillings.
func price(key string, lo, hi int64) int64 {
	h := fnv.New32a()
	h.Write([]byte(key))
	p := lo + int64(h.Sum32())%(hi-lo+1)
	return max(100, p/100*100)
}

// ---- invented shop --------------------------------------------------------------

var base = []struct {
	name, dept string
	sizes      []string
	lo, hi     int64
}{
	{"Sugar", "Pantry", []string{"500 g", "1 kg", "2 kg", "5 kg"}, 2800, 26000},
	{"Maize flour", "Pantry", []string{"1 kg", "2 kg", "5 kg"}, 3000, 15000},
	{"Rice", "Pantry", []string{"1 kg", "2 kg", "5 kg", "10 kg"}, 4500, 48000},
	{"Wheat flour", "Pantry", []string{"1 kg", "2 kg"}, 4000, 9000},
	{"Cooking oil", "Pantry", []string{"500 ml", "1 L", "3 L", "5 L"}, 5000, 42000},
	{"Salt", "Pantry", []string{"500 g", "1 kg"}, 800, 2000},
	{"Beans", "Pantry", []string{"1 kg", "2 kg"}, 4000, 9500},
	{"Tea leaves", "Breakfast", []string{"100 g", "250 g", "500 g"}, 2000, 9000},
	{"Instant coffee", "Breakfast", []string{"50 g", "100 g", "200 g"}, 4500, 22000},
	{"Fresh milk", "Dairy", []string{"500 ml", "1 L"}, 1500, 3500},
	{"Long-life milk", "Dairy", []string{"500 ml", "1 L"}, 2000, 4500},
	{"Yoghurt", "Dairy", []string{"250 ml", "500 ml", "1 L"}, 2000, 8000},
	{"Eggs", "Dairy", []string{"tray of 30", "pack of 12"}, 5500, 15000},
	{"Bread", "Bakery", []string{"400 g", "600 g", "800 g"}, 3000, 6500},
	{"Bottled water", "Drinks", []string{"500 ml", "1 L", "1.5 L", "5 L"}, 800, 6000},
	{"Soda", "Drinks", []string{"300 ml", "500 ml", "1 L", "2 L"}, 1000, 6000},
	{"Juice", "Drinks", []string{"300 ml", "1 L", "2 L"}, 2000, 12000},
	{"Biscuits", "Snacks", []string{"100 g", "200 g", "500 g"}, 1000, 9000},
	{"Bathing soap", "Personal care", []string{"1 bar", "pack of 3"}, 2000, 9000},
	{"Toothpaste", "Personal care", []string{"50 ml", "100 ml", "150 ml"}, 2500, 9000},
	{"Washing powder", "Household", []string{"500 g", "1 kg", "2 kg"}, 4000, 16000},
	{"Toilet paper", "Household", []string{"4 rolls", "10 rolls"}, 5000, 14000},
	{"Dish soap", "Household", []string{"500 ml", "750 ml"}, 3500, 6000},
	{"Baby diapers", "Baby", []string{"pack of 24", "pack of 48"}, 22000, 65000},
	{"Notebook", "Stationery", []string{"48 pages", "96 pages"}, 1200, 3500},
}

var brands = []string{"Tropical", "Golden Hill", "Nile Valley", "Kibo", "Savanna", "Lake Star"}

func invented(n int) []row {
	var out []row
	i := 0
	for len(out) < n {
		b := base[i%len(base)]
		si := (i / len(base)) % len(b.sizes)
		size := b.sizes[si]
		brand := brands[(i/(len(base)*2))%len(brands)]
		name := brand + " " + strings.ToLower(b.name[:1]) + b.name[1:]
		name = strings.ToUpper(name[:1]) + name[1:]
		code := checkDigit(fmt.Sprintf("29%010d", i+1)) // GS1 in-store range: can never clash with a real product
		// smallest pack near the low end of the range, largest near the top, +-8% per brand
		span := b.hi - b.lo
		mid := b.lo
		if len(b.sizes) > 1 {
			mid = b.lo + span*int64(si)/int64(len(b.sizes)-1)
		}
		out = append(out, row{Code: code, Name: name, Size: size, Dept: b.dept, Price: price(code, mid*92/100, mid*108/100)})
		i++
	}
	return out
}

// ---- real barcodes from Open Food Facts ----------------------------------------------

type offProduct struct {
	Code       string   `json:"code"`
	Name       string   `json:"product_name"`
	Quantity   string   `json:"quantity"`
	Brands     string   `json:"brands"`
	Categories []string `json:"categories_tags"`
}

var deptByCategory = []struct {
	tag, dept string
	lo, hi    int64
}{
	{"beverages", "Drinks", 1000, 12000}, {"dairies", "Dairy", 1500, 9000}, {"breads", "Bakery", 3000, 7000},
	{"biscuits", "Snacks", 1000, 9000}, {"snacks", "Snacks", 1000, 9000}, {"cereals", "Pantry", 3000, 20000},
	{"oils", "Pantry", 5000, 40000}, {"sugars", "Pantry", 2800, 26000}, {"teas", "Breakfast", 2000, 9000},
	{"coffees", "Breakfast", 4500, 22000},
}

func classify(cats []string) (string, int64, int64) {
	for _, c := range cats {
		for _, d := range deptByCategory {
			if strings.Contains(c, d.tag) {
				return d.dept, d.lo, d.hi
			}
		}
	}
	return "Grocery", 1500, 15000
}

// parseOFF turns an Open Food Facts search response into rows, keeping only
// products with a valid retail barcode and a name.
func parseOFF(r io.Reader) ([]row, error) {
	var resp struct {
		Products []offProduct `json:"products"`
	}
	if err := json.NewDecoder(r).Decode(&resp); err != nil {
		return nil, fmt.Errorf("Open Food Facts did not return product data (the service may be busy; try again later): %w", err)
	}
	var out []row
	seen := map[string]bool{}
	for _, p := range resp.Products {
		name := strings.TrimSpace(p.Name)
		if name == "" || !validGTIN(p.Code) || seen[p.Code] {
			continue
		}
		seen[p.Code] = true
		if b := strings.TrimSpace(strings.Split(p.Brands, ",")[0]); b != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(b)) {
			name = b + " " + name
		}
		dept, lo, hi := classify(p.Categories)
		out = append(out, row{Code: p.Code, Name: name, Size: strings.TrimSpace(p.Quantity), Dept: dept, Price: price(p.Code, lo, hi)})
	}
	return out, nil
}

func fetchOFF(country string, n int) ([]row, error) {
	q := url.Values{}
	q.Set("countries_tags_en", country)
	q.Set("fields", "code,product_name,quantity,brands,categories_tags")
	q.Set("page_size", strconv.Itoa(min(n*2, 500)))
	req, _ := http.NewRequest("GET", "https://world.openfoodfacts.org/api/v2/search?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "PriceTagTestData/1.0")
	res, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("Open Food Facts answered HTTP %d (the service is often busy; try again in a few minutes, or use -source invented)", res.StatusCode)
	}
	rows, err := parseOFF(res.Body)
	if err != nil {
		return nil, err
	}
	if len(rows) > n {
		rows = rows[:n]
	}
	if len(rows) == 0 {
		return nil, errors.New("no usable products found for that country")
	}
	return rows, nil
}

func money(v int64) string {
	s := strconv.FormatInt(v, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func write(w io.Writer, rows []row) error {
	cw := csv.NewWriter(w)
	cw.Write([]string{"Barcode", "Item Description", "Pack", "Department", "Selling Price"})
	for _, r := range rows {
		cw.Write([]string{r.Code, r.Name, r.Size, r.Dept, money(r.Price)})
	}
	cw.Flush()
	return cw.Error()
}

func main() {
	out := flag.String("out", "sample-prices.csv", "file to write")
	n := flag.Int("n", 300, "number of products")
	source := flag.String("source", "invented", "invented | openfoodfacts")
	country := flag.String("country", "uganda", "country for -source openfoodfacts")
	flag.Parse()

	var rows []row
	var err error
	switch *source {
	case "invented":
		rows = invented(*n)
	case "openfoodfacts":
		rows, err = fetchOFF(*country, *n)
	default:
		err = fmt.Errorf("unknown -source %q", *source)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	f, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	defer f.Close()
	if err := write(f, rows); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d products to %s (prices are made up for testing)\n", len(rows), *out)
}
