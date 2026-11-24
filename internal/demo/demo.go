// Package demo seeds a realistic sample supermarket so the system can be tried
// and tested in one command. All barcodes use the "2" prefix that GS1 reserves
// for in-store use, so they can never collide with a real product.
package demo

import (
	"context"
	"fmt"
	"os"
	"time"

	"pricetag/internal/barcode"
	"pricetag/internal/store"
)

const (
	OwnerEmail    = "owner@demo.example"
	OwnerPassword = "demo-password-1"
	EditorEmail   = "clerk@demo.example"
	EditorPass    = "demo-password-2"
)

type Item struct {
	Name, Size, Category string
	Price                int64 // UGX
}

// Items are generic grocery lines with plausible Ugandan shilling prices.
var Items = []Item{
	{"Sugar", "1 kg", "Pantry", 5500}, {"Sugar", "2 kg", "Pantry", 10500}, {"Maize flour (posho)", "2 kg", "Pantry", 6500},
	{"Rice", "1 kg", "Pantry", 5200}, {"Rice", "5 kg", "Pantry", 24500}, {"Wheat flour", "2 kg", "Pantry", 8500},
	{"Cooking oil", "1 L", "Pantry", 9000}, {"Cooking oil", "3 L", "Pantry", 25500}, {"Salt", "500 g", "Pantry", 1200},
	{"Beans", "1 kg", "Pantry", 4800}, {"Groundnuts", "500 g", "Pantry", 4200}, {"Spaghetti", "500 g", "Pantry", 3500},
	{"Tomato paste", "70 g", "Pantry", 1500}, {"Sardines in oil", "125 g", "Pantry", 3600}, {"Margarine", "500 g", "Pantry", 8200},
	{"Tea leaves", "100 g", "Breakfast", 2600}, {"Instant coffee", "50 g", "Breakfast", 5200}, {"Sliced bread", "600 g", "Bakery", 4500},
	{"Fresh milk", "500 ml", "Dairy", 1800}, {"Long-life milk", "1 L", "Dairy", 3700}, {"Yoghurt", "500 ml", "Dairy", 4300},
	{"Eggs", "tray of 30", "Dairy", 13500}, {"Butter", "250 g", "Dairy", 7800}, {"Bottled water", "500 ml", "Drinks", 1000},
	{"Bottled water", "1.5 L", "Drinks", 2000}, {"Soda", "500 ml", "Drinks", 2000}, {"Juice", "1 L", "Drinks", 5200},
	{"Energy drink", "300 ml", "Drinks", 2500}, {"Biscuits", "200 g", "Snacks", 2500}, {"Crisps", "100 g", "Snacks", 3000},
	{"Chocolate bar", "50 g", "Snacks", 3500}, {"Bathing soap", "1 bar", "Household", 3000}, {"Laundry bar soap", "1 kg", "Household", 6000},
	{"Washing powder", "1 kg", "Household", 7500}, {"Toothpaste", "100 ml", "Personal care", 4500}, {"Toothbrush", "1 piece", "Personal care", 2500},
	{"Toilet paper", "10 rolls", "Household", 12000}, {"Sanitary pads", "10 pack", "Personal care", 4000}, {"Baby diapers", "pack of 36", "Baby", 36000},
	{"Matches", "10 boxes", "Household", 1500}, {"Candles", "pack of 6", "Household", 3000}, {"Charcoal stove lighter", "1 piece", "Household", 2000},
	{"Hair oil", "250 ml", "Personal care", 6500}, {"Petroleum jelly", "100 g", "Personal care", 3200}, {"Dish soap", "750 ml", "Household", 4800},
	{"Bleach", "1 L", "Household", 3900}, {"Insecticide spray", "300 ml", "Household", 11500}, {"AA batteries", "pack of 4", "Household", 4500},
	{"Notebook", "A5 96 pages", "Stationery", 2200}, {"Ballpoint pens", "pack of 10", "Stationery", 5000}, {"Cooking gas lighter", "1 piece", "Household", 3500},
	{"Cornflakes", "500 g", "Breakfast", 9500}, {"Oats", "500 g", "Breakfast", 7200}, {"Honey", "500 g", "Pantry", 14500},
	{"Peanut butter", "400 g", "Pantry", 8800}, {"Jam", "450 g", "Pantry", 6900}, {"Tinned beans", "400 g", "Pantry", 3300},
}

// Code returns the barcode of demo item i (0-based): an in-store 13-digit EAN.
func Code(i int) string { return barcode.WithCheckDigit(fmt.Sprintf("20%010d", i+1)) }

type Info struct {
	Slug, URL, AdminURL string
	Created             bool
}

// index finds a demo item by name and size, so offers don't depend on list order.
func index(name, size string) int {
	for i, it := range Items {
		if it.Name == name && it.Size == size {
			return i
		}
	}
	panic("demo item not found: " + name + " " + size)
}

// Seed creates the demo store if it does not exist yet (it never touches an
// existing one). It adds products, three offers (two live, one scheduled), a
// staff login and returns what the caller should print. A failed seed leaves
// nothing behind.
func Seed(dir, slug string) (info *Info, err error) {
	info = &Info{Slug: slug, URL: "/s/" + slug + "/", AdminURL: "/s/" + slug + "/admin/"}
	if store.Exists(dir, slug) {
		return info, nil
	}
	set := store.DefaultSettings("Kampala Demo Supermarket")
	set.Announcement = "Open 8am to 10pm every day. Free parking."
	st, err := store.Create(dir, slug, set, OwnerEmail, "Demo Owner", OwnerPassword)
	if err != nil {
		return nil, err
	}
	defer func() {
		st.Close()
		if err != nil {
			p := store.DBPath(dir, slug)
			os.Remove(p)
			os.Remove(p + "-wal")
			os.Remove(p + "-shm")
			info = nil
		}
	}()
	ctx := context.Background()
	if _, err = st.CreateUser(EditorEmail, "Demo Clerk", "editor", EditorPass); err != nil {
		return nil, err
	}
	rows := make([]store.ImportRow, len(Items))
	for i, it := range Items {
		rows[i] = store.ImportRow{Row: i + 1, In: store.ProductInput{Code: Code(i), Name: it.Name, Size: it.Size, Category: it.Category, Price: it.Price}}
	}
	if _, err = st.ImportProducts(ctx, "demo", rows, false); err != nil {
		return nil, err
	}
	now := time.Now()
	type deal struct {
		name, size string
		price      int64
	}
	offer := func(title, text string, from, to time.Time, deals ...deal) error {
		var pi []store.PromoItemInput
		for _, d := range deals {
			pi = append(pi, store.PromoItemInput{Code: Code(index(d.name, d.size)), Price: d.price})
		}
		_, err := st.CreatePromo(ctx, "demo", store.PromoInput{Title: title, Text: text, StartsAt: from.Unix(), EndsAt: to.Unix(), Enabled: true, Items: pi})
		return err
	}
	if err = offer("Weekend pantry deals", "Stock up on the basics. While stocks last.", now.Add(-time.Hour), now.Add(48*time.Hour),
		deal{"Sugar", "1 kg", 4900}, deal{"Rice", "1 kg", 4600}, deal{"Cooking oil", "1 L", 8200}, deal{"Maize flour (posho)", "2 kg", 5900}); err != nil {
		return nil, err
	}
	if err = offer("Fresh & dairy special", "Cold shelf favourites at lower prices.", now.Add(-time.Hour), now.Add(24*time.Hour),
		deal{"Fresh milk", "500 ml", 1500}, deal{"Yoghurt", "500 ml", 3800}, deal{"Eggs", "tray of 30", 12000}); err != nil {
		return nil, err
	}
	if err = offer("Back to school", "Notebooks, pens and snacks for the new term.", now.Add(72*time.Hour), now.Add(14*24*time.Hour),
		deal{"Notebook", "A5 96 pages", 1900}, deal{"Ballpoint pens", "pack of 10", 4200}, deal{"Biscuits", "200 g", 2200}); err != nil {
		return nil, err
	}
	info.Created = true
	return info, nil
}
