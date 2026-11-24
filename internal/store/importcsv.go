package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"io"
	"strings"

	"pricetag/internal/barcode"
)

var headerAliases = map[string]string{
	"code": "code", "barcode": "code", "gtin": "code", "ean": "code", "ean13": "code", "upc": "code", "plu": "code", "itemcode": "code", "sku": "code", "barcodenumber": "code",
	"name": "name", "description": "name", "itemname": "name", "item": "name", "product": "name", "productname": "name", "itemdescription": "name",
	"price": "price", "sellingprice": "price", "retailprice": "price", "unitprice": "price", "saleprice": "price", "shelfprice": "price", "amount": "price",
	"size": "size", "pack": "size", "packsize": "size", "unit": "size", "weight": "size",
	"category": "category", "department": "category", "dept": "category", "group": "category", "class": "category",
}

func normHeader(h string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(h) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// undoFormulaGuard reverses the leading-apostrophe guard added on export.
func undoFormulaGuard(s string) string {
	if len(s) > 1 && s[0] == '\'' && strings.ContainsRune("=+-@\t\r", rune(s[1])) {
		return s[1:]
	}
	return s
}

func guardFormula(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// ParseCSV reads a price file exported from a POS / spreadsheet. It is lenient
// about what real exports look like: a UTF-8 BOM, comma / semicolon / tab
// separators, header names such as "Barcode", "Item Description" or "Selling
// Price", and prices written like "12,500" or "UGX 12500".
func ParseCSV(r io.Reader, decimals int) ([]ImportRow, []RowError, error) {
	br := bufio.NewReaderSize(r, 64*1024)
	if b, err := br.Peek(3); err == nil && bytes.Equal(b, []byte{0xEF, 0xBB, 0xBF}) {
		br.Discard(3)
	}
	first, _ := br.Peek(4096)
	line := first
	if i := bytes.IndexByte(first, '\n'); i >= 0 {
		line = first[:i]
	}
	comma := ','
	best := bytes.Count(line, []byte(","))
	for _, c := range []rune{';', '\t'} {
		if n := bytes.Count(line, []byte(string(c))); n > best {
			comma, best = c, n
		}
	}
	cr := csv.NewReader(br)
	cr.Comma = comma
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.TrimLeadingSpace = true
	head, err := cr.Read()
	if err == io.EOF {
		return nil, nil, invalid("empty_file", "the file is empty")
	} else if err != nil {
		return nil, nil, invalid("bad_csv", "cannot read the file as CSV: %v", err)
	}
	col := map[string]int{}
	for i, h := range head {
		if canon, ok := headerAliases[normHeader(h)]; ok {
			if _, dup := col[canon]; !dup {
				col[canon] = i
			}
		}
	}
	for _, need := range []string{"code", "name", "price"} {
		if _, ok := col[need]; !ok {
			return nil, nil, invalid("missing_column", "the first row must name a %q column (found: %s)", need, strings.Join(head, ", "))
		}
	}
	get := func(rec []string, k string) string {
		if i, ok := col[k]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var rows []ImportRow
	var errs []RowError
	rowNo := 1
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		rowNo++
		if err == nil {
			rowNo, _ = cr.FieldPos(0) // true file line, even when blank lines were skipped
		}
		if err != nil {
			errs = append(errs, RowError{Row: rowNo, Message: "unreadable line: " + err.Error()})
			var pe *csv.ParseError
			if errors.As(err, &pe) {
				continue
			}
			return nil, nil, err
		}
		if len(rows)+len(errs) >= MaxImportRows {
			return nil, nil, invalid("too_many_rows", "at most %d rows per import", MaxImportRows)
		}
		blank := true
		for _, f := range rec {
			if strings.TrimSpace(f) != "" {
				blank = false
				break
			}
		}
		if blank {
			continue
		}
		code := get(rec, "code")
		price, perr := ParseMoney(get(rec, "price"), decimals)
		if perr != nil {
			errs = append(errs, RowError{Row: rowNo, Code: code, Message: perr.Error()})
			continue
		}
		rows = append(rows, ImportRow{Row: rowNo, In: ProductInput{
			Code: code, Name: undoFormulaGuard(get(rec, "name")), Size: undoFormulaGuard(get(rec, "size")),
			Category: undoFormulaGuard(get(rec, "category")), Price: price,
		}})
	}
	return rows, errs, nil
}

// ExportCSV streams the whole catalogue in the same format ParseCSV reads.
func (s *Store) ExportCSV(ctx context.Context, w io.Writer) error {
	dec := s.Settings().Decimals
	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"code", "name", "size", "category", "price"}); err != nil {
		return err
	}
	rows, err := s.r.QueryContext(ctx, `SELECT code,name,size,category,price FROM products WHERE deleted=0 ORDER BY name COLLATE NOCASE, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var code, name, size, cat string
		var price int64
		if err := rows.Scan(&code, &name, &size, &cat, &price); err != nil {
			return err
		}
		if err := cw.Write([]string{barcode.Display(code), guardFormula(name), guardFormula(size), guardFormula(cat), FormatMoney(price, dec)}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	cw.Flush()
	return cw.Error()
}
