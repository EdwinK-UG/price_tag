package main

import (
	"bytes"
	"strings"
	"testing"

	"pricetag/internal/store"
)

// The generated file must import cleanly through the real importer.
func TestInventedFileImportsCleanly(t *testing.T) {
	var buf bytes.Buffer
	if err := write(&buf, invented(300)); err != nil {
		t.Fatal(err)
	}
	rows, errs, err := store.ParseCSV(&buf, 0)
	if err != nil || len(errs) != 0 || len(rows) != 300 {
		t.Fatalf("rows=%d errs=%v err=%v", len(rows), errs, err)
	}
	s, err := store.Create(t.TempDir(), "sample", store.DefaultSettings("Sample"), "o@example.com", "O", "password-1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	res, err := s.ImportProducts(t.Context(), "t", rows, false)
	if err != nil || res.Created != 300 || len(res.Errors) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestParseOpenFoodFactsKeepsOnlyValidProducts(t *testing.T) {
	js := `{"products":[
	 {"code":"4006381333931","product_name":"Maize Flour","quantity":"2 kg","brands":"Brand A","categories_tags":["en:cereals"]},
	 {"code":"4006381333932","product_name":"Bad check digit"},
	 {"code":"4006381333931","product_name":"Duplicate"},
	 {"code":"5901234123457","product_name":""},
	 {"code":"96385074","product_name":"Soda","brands":"","categories_tags":["en:beverages"]}]}`
	rows, err := parseOFF(strings.NewReader(js))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Name != "Brand A Maize Flour" || rows[0].Dept != "Pantry" || rows[1].Dept != "Drinks" {
		t.Fatalf("%+v", rows)
	}
	if _, err := parseOFF(strings.NewReader("<html>busy</html>")); err == nil {
		t.Fatal("an HTML error page must be reported, not parsed")
	}
}
